package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"golang.org/x/crypto/ssh"
)

const (
	testToken   = "test-deployment-token-with-sufficient-length"
	testJARName = "photolib-backend-0.1.0-SNAPSHOT.jar"
)

func deploymentRequest(data []byte) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/deploy/photolib", bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-Artifact-Name", testJARName)
	request.Header.Set("X-Artifact-SHA256", fmt.Sprintf("%x", sha256.Sum256(data)))
	request.Header.Set("X-Deploy-Run-ID", "12345")
	return request
}

func TestRelayRejectsInvalidUploads(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*relay, *http.Request)
		status int
	}{
		{"wrong token", func(_ *relay, r *http.Request) { r.Header.Del("Authorization") }, http.StatusUnauthorized},
		{"path traversal", func(_ *relay, r *http.Request) { r.Header.Set("X-Artifact-Name", "../"+testJARName) }, http.StatusBadRequest},
		{"declared size limit", func(s *relay, _ *http.Request) { s.maxSize = 1 }, http.StatusRequestEntityTooLarge},
		{"streaming size limit", func(s *relay, r *http.Request) { s.maxSize = 1; r.ContentLength = -1 }, http.StatusRequestEntityTooLarge},
		{"wrong hash", func(_ *relay, r *http.Request) { r.Header.Set("X-Artifact-SHA256", strings.Repeat("0", 64)) }, http.StatusUnprocessableEntity},
		{"matching hash for invalid jar", func(_ *relay, _ *http.Request) {}, http.StatusUnprocessableEntity},
		{"deployment in progress", func(s *relay, _ *http.Request) { s.mu.Lock() }, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &relay{token: testToken, maxSize: maxJARSize, logger: slog.New(slog.DiscardHandler)}
			request := deploymentRequest([]byte("not a JAR"))
			test.change(s, request)
			response := httptest.NewRecorder()
			s.handleDeploy(response, request)
			if response.Code != test.status {
				t.Fatalf("got %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func makeTestJAR(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	for _, name := range []string{
		"META-INF/MANIFEST.MF",
		"BOOT-INF/classes/static/index.html",
		"BOOT-INF/classes/native/linux-x86_64/libphotolib-image.so",
		"BOOT-INF/classes/native/windows-x86_64/photolib-image.dll",
	} {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, "test data"); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestRelayArchivesToOSSAndSwitchesThroughSSH(t *testing.T) {
	data := makeTestJAR(t)
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	const password = "test $ssh # password"
	configFile := filepath.Join(t.TempDir(), ".env")
	port := listener.Addr().(*net.TCPAddr).Port
	contents := fmt.Sprintf("DEPLOY_TOKEN=%s\nPRODUCTION_SSH_HOST=127.0.0.1\nPRODUCTION_SSH_PORT=%d\nPRODUCTION_SSH_PASSWORD='%s'\nPRODUCTION_SSH_HOST_KEY='%s'\nOSS_REGION=cn-hangzhou\nOSS_BUCKET=photolib-test\nOSS_PRODUCTION_ENDPOINT=https://oss-cn-hangzhou-internal.aliyuncs.com\nOSS_ACCESS_KEY_ID=test-key\nOSS_ACCESS_KEY_SECRET=test-secret\n", testToken, port, password, bytes.TrimSpace(ssh.MarshalAuthorizedKey(key.PublicKey())))
	if err := os.WriteFile(configFile, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(configFile)
	if err != nil {
		t.Fatal(err)
	}

	server := &ssh.ServerConfig{PasswordCallback: func(conn ssh.ConnMetadata, given []byte) (*ssh.Permissions, error) {
		if conn.User() != "root" || string(given) != password {
			return nil, fmt.Errorf("unexpected SSH credentials")
		}
		return nil, nil
	}}
	server.AddHostKey(key)
	result := make(chan error, 1)
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	tcp := listener.(*net.TCPListener)
	_ = tcp.SetDeadline(time.Now().Add(5 * time.Second))
	go func() { result <- verifySSHDeployment(listener, server, digest) }()

	cfg.archive.uploadClient = oss.NewClient(oss.LoadDefaultConfig().WithRegion("cn-hangzhou").
		WithUseAccelerateEndpoint(true).
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test-key", "test-secret")).
		WithHttpClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(r.Body)
			if r.Method != http.MethodPut || r.URL.Host != "photolib-test.oss-accelerate.aliyuncs.com" ||
				!strings.HasPrefix(r.URL.Path, "/"+archivePrefix+"photolib-backend-12345-") ||
				!strings.HasSuffix(r.URL.Path, "-"+digest+".jar") ||
				r.Header.Get("x-oss-meta-sha256") != digest || r.Header.Get("x-oss-forbid-overwrite") != "true" ||
				r.Header.Get("x-oss-object-acl") != "private" || !bytes.Equal(body, data) || err != nil {
				t.Errorf("unexpected OSS upload: %s %s", r.Method, r.URL)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
		})}))
	handler := &relay{token: cfg.token, maxSize: maxJARSize, logger: slog.New(slog.DiscardHandler), history: history(t.TempDir()),
		archive: cfg.archive.upload, downloadURL: cfg.archive.downloadURL, deploy: cfg.production.deploy}
	response := httptest.NewRecorder()
	handler.handleDeploy(response, deploymentRequest(data))
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("got %d: %s", response.Code, response.Body.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func verifySSHDeployment(listener net.Listener, server *ssh.ServerConfig, digest string) error {
	var runID, jarName string
	if err := expectSSHCommand(listener, server, func(command string, body []byte) error {
		match := regexp.MustCompile(`\.upload-([A-Za-z0-9-]+)\.jar`).FindStringSubmatch(command)
		if len(match) != 2 || !strings.Contains(command, digest) ||
			!strings.Contains(command, "curl -q") || strings.Contains(command, "Signature") {
			return fmt.Errorf("unexpected download command: %q", command)
		}
		runID = match[1]
		url, err := strconv.Unquote(strings.TrimSpace(strings.TrimPrefix(string(body), "url = ")))
		if err != nil || !strings.HasPrefix(url, "https://photolib-test.oss-cn-hangzhou-internal.aliyuncs.com/"+archivePrefix) ||
			!strings.Contains(url, "x-oss-signature=") {
			return fmt.Errorf("expected presigned OSS URL on SSH stdin")
		}
		jarName = "photolib-backend-12345-" + strings.TrimSuffix(runID, "-1") + "-" + digest + ".jar"
		if !strings.Contains(url, "/"+archivePrefix+jarName+"?") {
			return fmt.Errorf("download name does not match deployment name")
		}
		return nil
	}); err != nil {
		_ = listener.Close()
		return err
	}
	return expectSSHCommand(listener, server, func(command string, body []byte) error {
		want := fmt.Sprintf("DEPLOY_DIR='/opt/Photolib' JAR_NAME='%s' RUN_ID='%s' bash -s", jarName, runID)
		if command != want || string(body) != restartScript {
			return fmt.Errorf("unexpected restart: %q", command)
		}
		return nil
	})
}

func expectSSHCommand(listener net.Listener, config *ssh.ServerConfig, verify func(string, []byte) error) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	sshConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return err
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(requests)
	newChannel := <-channels
	if newChannel == nil || newChannel.ChannelType() != "session" {
		return fmt.Errorf("expected an SSH session")
	}
	channel, sessionRequests, err := newChannel.Accept()
	if err != nil {
		return err
	}
	defer channel.Close()
	request := <-sessionRequests
	if request == nil || request.Type != "exec" {
		return fmt.Errorf("expected an SSH command")
	}
	var payload struct{ Command string }
	if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
		return err
	}
	if err := request.Reply(true, nil); err != nil {
		return err
	}
	body, err := io.ReadAll(channel)
	if err != nil {
		return err
	}
	if err := verify(payload.Command, body); err != nil {
		return err
	}
	_, err = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
	return err
}

func TestSSHRejectsUntrustedHost(t *testing.T) {
	newKey := func() ssh.Signer {
		t.Helper()
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ssh.NewSignerFromKey(private)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	server := &ssh.ServerConfig{NoClientAuth: true}
	server.AddHostKey(newKey())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() { done <- expectSSHCommand(listener, server, nil) }()
	production := productionServer{address: listener.Addr().String(), hostKey: newKey().PublicKey(), user: "root"}
	if _, err := production.run(context.Background(), 5*time.Second, "true", nil); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("expected host key mismatch, got %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("unexpected successful SSH connection")
	}
}
