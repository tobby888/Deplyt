package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	productionDir       = "/opt/Photolib"
	sshConnectTimeout   = 30 * time.Second
	sshTransferTimeout  = 12 * time.Minute
	sshRestartTimeout   = 7 * time.Minute
	sshRetryDelay       = 15 * time.Second
	sshDownloadAttempts = 3
)

//go:embed restart.sh
var restartScript string

type productionServer struct {
	address  string
	user     string
	password string
	hostKey  ssh.PublicKey
}

func (p productionServer) deploy(ctx context.Context, jar artifact, stageID, downloadURL string, logger *slog.Logger) error {
	return p.deployWithOptions(ctx, jar, stageID, downloadURL, "", logger)
}

func (p productionServer) deployWithOptions(ctx context.Context, jar artifact, stageID, downloadURL, restoreDBFrom string, logger *slog.Logger) error {
	// The presigned URL is sent over SSH stdin, outside the command, curl arguments, and logs.
	curlConfig := fmt.Sprintf("url = %q\n", downloadURL)
	format := jar.effectiveFormat()
	stagedExt := ".jar"
	if format == artifactFormatNative {
		stagedExt = ".tar.gz"
	}
	for attempt := 1; ; attempt++ {
		// An earlier attempt may still be terminating on the server after a timeout.
		// Each attempt uses its own path so that it cannot overwrite the next one.
		runID := fmt.Sprintf("%s-%d", stageID, attempt)
		stagedPath := fmt.Sprintf("%s/.upload-%s%s", productionDir, runID, stagedExt)
		command := fmt.Sprintf(`curl -q --silent --fail --proto '=https' --connect-timeout 20 --max-time 660 --config - --output '%s' || { echo 'OSS download failed' >&2; exit 1; }`,
			stagedPath)
		var input io.Reader = strings.NewReader(curlConfig)
		if jar.file != nil {
			if _, err := jar.file.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("rewind staged artifact: %w", err)
			}
			command, input = "cat > '"+stagedPath+"'", jar.file
		}
		command = fmt.Sprintf(`set -eu; umask 077; %s; printf '%%s  %%s\n' '%s' '%s' | sha256sum --check --status`,
			command, jar.sha256, stagedPath)
		output, err := p.run(ctx, sshTransferTimeout, command, input)
		if err == nil {
			logger.Info("production artifact transferred and verified", "attempt", attempt, "format", format)
			command := fmt.Sprintf("DEPLOY_DIR='%s' ARTIFACT_FORMAT='%s' ARTIFACT_NAME='%s' JAR_NAME='%s' RUN_ID='%s' bash -s",
				productionDir, format, jar.name, jar.name, runID)
			if restoreDBFrom != "" {
				command = fmt.Sprintf("DEPLOY_DIR='%s' ARTIFACT_FORMAT='%s' ARTIFACT_NAME='%s' JAR_NAME='%s' RUN_ID='%s' RESTORE_DB_FROM='%s' bash -s",
					productionDir, format, jar.name, jar.name, runID, restoreDBFrom)
			}
			script := strings.ReplaceAll(restartScript, "\r\n", "\n")
			output, err := p.run(ctx, sshRestartTimeout, command, strings.NewReader(script))
			if err != nil {
				return fmt.Errorf("switch artifact and restart photolib: %w; output: %s", err, output)
			}
			logger.Info("production restart completed", "output", output, "format", format)
			return nil
		}
		logger.Warn("production artifact transfer or verification failed", "attempt", attempt, "format", format, "error", err, "output", output)
		if _, err := p.run(ctx, sshConnectTimeout, "rm -f -- '"+stagedPath+"'", nil); err != nil {
			logger.Warn("remove incomplete production artifact", "path", stagedPath, "error", err)
		}
		if attempt == sshDownloadAttempts {
			return fmt.Errorf("transfer and verify artifact after %d attempts: %w", attempt, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sshRetryDelay):
		}
	}
}

func (p productionServer) run(ctx context.Context, timeout time.Duration, command string, stdin io.Reader) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	handshake, cancelHandshake := context.WithTimeout(ctx, sshConnectTimeout)
	defer cancelHandshake()
	conn, err := (&net.Dialer{}).DialContext(handshake, "tcp", p.address)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := handshake.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	algorithms := []string{p.hostKey.Type()}
	if p.hostKey.Type() == ssh.KeyAlgoRSA {
		algorithms = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	clientConfig := &ssh.ClientConfig{
		User:              p.user,
		Auth:              []ssh.AuthMethod{ssh.Password(p.password), ssh.KeyboardInteractive(p.answerPasswordPrompt)},
		HostKeyCallback:   ssh.FixedHostKey(p.hostKey),
		HostKeyAlgorithms: algorithms,
	}
	sshConn, channels, requests, err := ssh.NewClientConn(conn, p.address, clientConfig)
	if err != nil {
		return "", err
	}
	client := ssh.NewClient(sshConn, channels, requests)
	defer client.Close()
	cancelHandshake()
	deadline, _ = ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	session.Stdin = stdin
	output, err := session.CombinedOutput(command)
	if ctx.Err() != nil {
		return string(output), ctx.Err()
	}
	return string(output), err
}

func (p productionServer) answerPasswordPrompt(_, _ string, questions []string, echo []bool) ([]string, error) {
	if len(questions) == 0 {
		return nil, nil
	}
	if len(questions) != 1 || echo[0] {
		return nil, errors.New("unexpected SSH authentication prompts")
	}
	return []string{p.password}, nil
}
