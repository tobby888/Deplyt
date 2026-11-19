package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

type ossArtifact struct {
	url      string
	name     string
	token    string
	checksum []byte
}

func (s *relay) handleOSS(artifact *ossArtifact, client *http.Client) func(http.ResponseWriter, *http.Request, consoleIdentity) {
	return func(w http.ResponseWriter, r *http.Request, user consoleIdentity) {
		if artifact == nil {
			http.Error(w, "请先配置 OSS 制品", http.StatusServiceUnavailable)
			return
		}
		if !s.mu.TryLock() {
			http.Error(w, "已有部署或回滚正在进行", http.StatusConflict)
			return
		}
		defer s.mu.Unlock()
		logger := s.logger.With("user_id", user.ID, "artifact", artifact.name)
		ctx := context.WithoutCancel(r.Context())
		source, err := artifact.download(ctx, client, s.maxSize)
		if err != nil {
			logger.Error("download manual OSS artifact", "error", err)
			http.Error(w, "无法读取 OSS 制品，请查看 Deplyt 日志", http.StatusBadGateway)
			return
		}
		staged, stageErr := stageArtifact(source, artifact.name, artifact.checksum, s.maxSize, logger)
		_ = source.Close()
		if stageErr != nil {
			logger.Warn("validate manual OSS artifact", "error", stageErr)
			http.Error(w, "OSS 制品校验失败，请查看 Deplyt 日志", http.StatusBadGateway)
			return
		}
		defer staged.remove(logger)
		if s.publish(w, ctx, staged, "", "", user.administrator) {
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func loadOSSArtifact(values map[string]string) (*ossArtifact, error) {
	rawURL, token := values["OSS_ARTIFACT_URL"], values["OSS_ACCESS_TOKEN"]
	name, digest := values["OSS_ARTIFACT_NAME"], values["OSS_ARTIFACT_SHA256"]
	if rawURL == "" && token == "" && name == "" && digest == "" {
		return nil, nil
	}
	endpoint, err := parseHTTPSEndpoint(rawURL)
	if err != nil {
		return nil, errors.New("OSS_ARTIFACT_URL must be an HTTPS object URL")
	}
	if name == "" {
		name = path.Base(endpoint.Path)
	}
	if !jarNamePattern.MatchString(name) {
		return nil, errors.New("OSS artifact name must match a PhotoLib JAR filename")
	}
	checksum, err := hex.DecodeString(digest)
	if err != nil || len(checksum) != sha256.Size {
		return nil, errors.New("OSS_ARTIFACT_SHA256 must be the JAR's SHA-256 digest")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("OSS_ACCESS_TOKEN must not contain whitespace")
	}
	return &ossArtifact{url: rawURL, name: name, token: token, checksum: checksum}, nil
}

func (a ossArtifact) download(ctx context.Context, client *http.Client, maxSize int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, nil)
	if err != nil {
		return nil, errors.New("could not construct OSS download request")
	}
	req.Header.Set("Accept-Encoding", "identity")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	res, err := client.Do(req)
	if err != nil {
		// HTTP client errors can include the full URL, including a presigned credential.
		return nil, errors.New("could not download the OSS object")
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		return nil, fmt.Errorf("OSS object endpoint returned HTTP %d", res.StatusCode)
	}
	if res.ContentLength > maxSize {
		_ = res.Body.Close()
		return nil, errors.New("OSS object exceeds the JAR size limit")
	}
	return res.Body, nil
}
