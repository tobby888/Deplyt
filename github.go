package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func (c *console) deployWorkflow(w http.ResponseWriter, r *http.Request) {
	user, ok := c.requireDeployAccess(w, r)
	if !ok {
		return
	}
	if c.workflow == nil {
		renderPage(w, http.StatusServiceUnavailable, page{Title: "GitHub Actions 待配置", User: &user})
		return
	}
	link, err := c.workflow.start(context.WithoutCancel(r.Context()))
	if errors.Is(err, errDispatchInProgress) {
		renderPage(w, http.StatusConflict, page{
			Title: "触发请求正在处理", Message: "请先查看 GitHub Actions 中的运行记录。", User: &user, WorkflowURL: photoLibBuild,
		})
		return
	}
	if err != nil {
		c.logger.Error("manual PhotoLib workflow dispatch", "user_id", user.ID, "error", err)
		renderPage(w, http.StatusBadGateway, page{
			Title: "无法确认触发结果", Message: "请先查看 GitHub Actions 是否已创建运行记录。", User: &user, WorkflowURL: photoLibBuild,
		})
		return
	}
	c.logger.Info("manual PhotoLib workflow dispatched", "user_id", user.ID, "workflow_url", link)
	renderPage(w, http.StatusAccepted, page{
		Title: "已触发 PhotoLib 工作流", Message: "构建、测试和部署进度请在 GitHub Actions 中查看。", User: &user, WorkflowURL: link,
	})
}

func (c *console) requireDeployAccess(w http.ResponseWriter, r *http.Request) (photoLibUser, bool) {
	user, ok := c.authenticate(w, r)
	if !ok {
		return photoLibUser{}, false
	}
	if !c.access.allowed(user.ID) {
		renderPage(w, http.StatusForbidden, page{Title: "需要 Deplyt 访问权限", User: &user})
		return photoLibUser{}, false
	}
	return user, true
}
