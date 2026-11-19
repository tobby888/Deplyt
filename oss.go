package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

type ossDeployment struct {
	artifact *ossArtifact
	client   *http.Client
	relay    *relay
}

func (d *ossDeployment) run(ctx context.Context, runID string, logger *slog.Logger) error {
	source, err := d.artifact.download(ctx, d.client, d.relay.maxSize)
	if err != nil {
		return err
	}
	jar, stageErr := stageArtifact(source, d.artifact.name, d.artifact.checksum, d.relay.maxSize, logger)
	_ = source.Close()
	if stageErr != nil {
		return stageErr
	}
	defer jar.remove(logger)
	return d.relay.deployStaged(ctx, jar, runID, logger)
}

func (c *console) deployOSS(w http.ResponseWriter, r *http.Request) {
	user, ok := c.requireDeployAccess(w, r)
	if !ok {
		return
	}
	oss := c.oss
	if oss == nil {
		renderPage(w, http.StatusServiceUnavailable, page{Title: "OSS 部署待配置", User: &user})
		return
	}
	if !oss.relay.mu.TryLock() {
		renderPage(w, http.StatusConflict, page{Title: "已有部署在执行", User: &user})
		return
	}
	defer oss.relay.mu.Unlock()

	runID := "oss-" + rand.Text()
	logger := c.logger.With("run_id", runID, "user_id", user.ID, "artifact", oss.artifact.name)
	logger.Info("manual OSS deployment started")
	ctx := context.WithoutCancel(r.Context())
	if err := oss.run(ctx, runID, logger); err != nil {
		logger.Error("manual OSS deployment failed", "error", err)
		renderPage(w, http.StatusBadGateway, page{
			Title: "OSS 部署失败", Message: "请用部署编号在 Deplyt 服务日志中检查失败原因。", User: &user, RunID: runID,
		})
		return
	}
	renderPage(w, http.StatusOK, page{
		Title: "PhotoLib 已部署", Message: "OSS 制品已通过校验，生产服务已完成重启和状态检查。", User: &user, RunID: runID,
	})
}
