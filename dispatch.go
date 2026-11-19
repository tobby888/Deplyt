package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	photoLibGitHub = "https://github.com/tobby888/Photolib"
	photoLibBuild  = photoLibGitHub + "/actions/workflows/build.yml"
	photoLibAPI    = "https://api.github.com/repos/tobby888/Photolib/actions/workflows/build.yml/dispatches"
)

var errDispatchInProgress = errors.New("a deployment workflow was just submitted")

type workflowDispatch struct {
	endpoint string
	token    string
	client   *http.Client
	mu       sync.Mutex
	hold     time.Time
}

func (d *workflowDispatch) start(ctx context.Context) (string, error) {
	if !d.mu.TryLock() {
		return "", errDispatchInProgress
	}
	defer d.mu.Unlock()
	if time.Now().Before(d.hold) {
		return "", errDispatchInProgress
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, strings.NewReader(`{"ref":"master"}`))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	// Also suppress immediate retries when the response is lost after dispatch.
	defer func() { d.hold = time.Now().Add(10 * time.Second) }()
	res, err := d.client.Do(req)
	if err != nil {
		return "", errors.New("could not confirm GitHub workflow dispatch")
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		var result struct {
			RunID int64 `json:"workflow_run_id"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&result); err == nil && result.RunID > 0 {
			return photoLibGitHub + "/actions/runs/" + strconv.FormatInt(result.RunID, 10), nil
		}
		return photoLibBuild, nil
	case http.StatusNoContent:
		return photoLibBuild, nil
	default:
		return "", fmt.Errorf("GitHub returned HTTP %d", res.StatusCode)
	}
}

func (d *workflowDispatch) handle(logger *slog.Logger) func(http.ResponseWriter, *http.Request, consoleIdentity) {
	return func(w http.ResponseWriter, r *http.Request, user consoleIdentity) {
		if d.token == "" {
			http.Error(w, "请先配置 GitHub Actions 触发令牌", http.StatusServiceUnavailable)
			return
		}
		link, err := d.start(context.WithoutCancel(r.Context()))
		if errors.Is(err, errDispatchInProgress) {
			http.Error(w, "触发请求正在处理，请查看 GitHub Actions 的运行记录", http.StatusConflict)
			return
		}
		if err != nil {
			logger.Error("manual PhotoLib workflow dispatch", "user_id", user.ID, "error", err)
			http.Error(w, "无法确认触发结果，请先查看 GitHub Actions 的运行记录", http.StatusBadGateway)
			return
		}
		logger.Info("manual PhotoLib workflow dispatched", "user_id", user.ID, "workflow_url", link)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"url": link})
	}
}
