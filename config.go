package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
	"golang.org/x/crypto/ssh"
)

type config struct {
	listenAddr     string
	token          string
	production     productionServer
	photoLibOrigin string
	adminID        int64
	stateDir       string
	githubToken    string
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
	origin := values["PHOTOLIB_BASE_URL"]
	if origin != "" {
		site, err := parseHTTPSEndpoint(origin)
		if err != nil || (site.Path != "" && site.Path != "/") || site.RawQuery != "" || site.ForceQuery {
			return config{}, errors.New("PHOTOLIB_BASE_URL must be an HTTPS origin")
		}
		origin = "https://" + strings.ToLower(site.Host)
	}
	var adminID int64
	if setting := values["DEPLOY_ADMIN_PHOTOLIB_ID"]; setting != "" {
		adminID, err = strconv.ParseInt(setting, 10, 64)
		if err != nil || adminID <= 0 || origin == "" {
			return config{}, errors.New("DEPLOY_ADMIN_PHOTOLIB_ID requires a positive PhotoLib user ID and PHOTOLIB_BASE_URL")
		}
	}
	stateDir := values["DEPLOY_STATE_DIR"]
	if stateDir == "" {
		stateDir = os.Getenv("STATE_DIRECTORY")
	}
	if stateDir == "" {
		stateDir = "state"
	}
	githubToken := values["GITHUB_ACTIONS_TOKEN"]
	if strings.ContainsAny(githubToken, " \t\r\n") {
		return config{}, errors.New("GITHUB_ACTIONS_TOKEN must not contain whitespace")
	}
	return config{
		listenAddr:     addr,
		token:          token,
		photoLibOrigin: origin,
		adminID:        adminID,
		stateDir:       stateDir,
		githubToken:    githubToken,
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
