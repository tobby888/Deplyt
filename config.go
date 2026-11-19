package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
	"golang.org/x/crypto/ssh"
)

type config struct {
	listenAddr string
	token      string
	production productionServer
	archive    jarArchive
	manualOSS  *ossArtifact
	adminID    int64
	values     map[string]string
}

func loadConfig(path string) (config, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		// A dotenv parse error can contain the original line, including secrets.
		return config{}, fmt.Errorf("cannot read or parse %s", path)
	}
	for _, key := range []string{"DEPLOY_TOKEN", "PRODUCTION_SSH_HOST", "PRODUCTION_SSH_PASSWORD", "PRODUCTION_SSH_HOST_KEY"} {
		if values[key] == "" {
			return config{}, fmt.Errorf("%s is required in %s", key, path)
		}
	}
	token := values["DEPLOY_TOKEN"]
	if len(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
		return config{}, errors.New("DEPLOY_TOKEN must have at least 32 characters and no whitespace")
	}
	port := values["PRODUCTION_SSH_PORT"]
	if port == "" {
		port = "22"
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return config{}, errors.New("PRODUCTION_SSH_PORT must be between 1 and 65535")
	}
	hostKey, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(values["PRODUCTION_SSH_HOST_KEY"]))
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return config{}, errors.New("PRODUCTION_SSH_HOST_KEY must contain one OpenSSH host public key")
	}
	user := values["PRODUCTION_SSH_USER"]
	if user == "" {
		user = "root"
	}
	addr := values["DEPLOY_LISTEN_ADDR"]
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	manual, err := loadOSSArtifact(values)
	if err != nil {
		return config{}, err
	}
	var archive jarArchive
	if manual == nil || values["OSS_ACCESS_KEY_ID"] != "" || values["OSS_ACCESS_KEY_SECRET"] != "" || values["OSS_ECS_RAM_ROLE"] != "" {
		archive, err = newArchive(values)
		if err != nil {
			return config{}, err
		}
	}
	var adminID int64
	if value := values["DEPLOY_ADMIN_PHOTOLIB_ID"]; value != "" {
		adminID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || adminID <= 0 {
			return config{}, errors.New("DEPLOY_ADMIN_PHOTOLIB_ID must be a positive PhotoLib user ID")
		}
	}
	if strings.ContainsAny(values["GITHUB_ACTIONS_TOKEN"], " \t\r\n") {
		return config{}, errors.New("GITHUB_ACTIONS_TOKEN must not contain whitespace")
	}
	return config{
		archive:    archive,
		manualOSS:  manual,
		adminID:    adminID,
		values:     values,
		listenAddr: addr,
		token:      token,
		production: productionServer{
			address:  net.JoinHostPort(values["PRODUCTION_SSH_HOST"], port),
			user:     user,
			password: values["PRODUCTION_SSH_PASSWORD"],
			hostKey:  hostKey,
		},
	}, nil
}

func parseHTTPSEndpoint(value string) (*url.URL, error) {
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" ||
		endpoint.User != nil || endpoint.Fragment != "" {
		return nil, errors.New("expected an HTTPS URL")
	}
	return endpoint, nil
}
