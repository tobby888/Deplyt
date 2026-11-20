package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const photoLibRepository = "/repos/tobby888/Photolib"

type githubRepository struct {
	token  string
	base   string
	client *http.Client
}

func newGitHub(token string) githubRepository {
	return githubRepository{token: token, base: "https://api.github.com", client: outboundClient(15 * time.Second)}
}

type githubFeed struct {
	path  string
	query string
}

var githubFeeds = map[string]githubFeed{
	"commits":    {path: "/commits"},
	"pulls":      {path: "/pulls", query: "state=all&sort=updated&direction=desc"},
	"actions":    {path: "/actions/runs"},
	"dependabot": {path: "/dependabot/alerts"},
	"code":       {path: "/code-scanning/alerts"},
	"secrets":    {path: "/secret-scanning/alerts", query: "hide_secret=true"},
}

type githubPull struct {
	repo githubRepository
	pace *rate.Limiter
}

type githubRequestError struct {
	message string
	stop    bool
}

func (e *githubRequestError) Error() string { return e.message }

func stopGitHubPull(err error) bool {
	var requestError *githubRequestError
	return errors.As(err, &requestError) && requestError.stop
}

func (p githubPull) fetch(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	if p.pace != nil {
		if err := p.pace.Wait(ctx); err != nil {
			return nil, err
		}
	}
	endpoint := p.repo.base + photoLibRepository + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("无法创建 GitHub 请求")
	}
	request.Header.Set("Authorization", "Bearer "+p.repo.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := p.repo.client.Do(request)
	if err != nil {
		return nil, &githubRequestError{"无法连接 GitHub", true}
	}
	if response.StatusCode == http.StatusOK {
		return response, nil
	}
	response.Body.Close()
	message := fmt.Sprintf("GitHub 请求失败（HTTP %d）", response.StatusCode)
	switch {
	case response.StatusCode == http.StatusTooManyRequests ||
		(response.StatusCode == http.StatusForbidden && (response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != "")):
		return nil, &githubRequestError{"GitHub 请求已达限额，请稍后重试", true}
	case response.StatusCode == http.StatusUnauthorized:
		return nil, &githubRequestError{"GitHub 未接受当前 PAT，请检查令牌是否有效", true}
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusForbidden:
		message += "，请检查 PAT 权限及仓库功能设置"
	}
	return nil, &githubRequestError{message, response.StatusCode >= 500}
}

func (p githubPull) pages(ctx context.Context, path string, query url.Values, read func(io.Reader) error) error {
	seen := make(map[string]bool)
	defaults := query
	for {
		key := query.Encode()
		if seen[key] {
			return errors.New("GitHub 返回了重复的分页链接")
		}
		seen[key] = true
		response, err := p.fetch(ctx, path, query)
		if err != nil {
			return err
		}
		decodeErr := read(io.LimitReader(response.Body, 16<<20))
		response.Body.Close()
		if decodeErr != nil {
			return errors.New("无法读取 GitHub 响应")
		}
		next, err := githubNextPage(response.Header.Values("Link"))
		if err != nil || next == nil {
			return err
		}
		// Keep the configured repository and endpoint even when GitHub links by repository ID.
		for key, values := range defaults {
			if !next.Has(key) {
				next[key] = values
			}
		}
		query = next
	}
}

func githubNextPage(headers []string) (url.Values, error) {
	for _, header := range headers {
		for entry := range strings.SplitSeq(header, ",") {
			target, attributes, ok := strings.Cut(strings.TrimSpace(entry), ">")
			if !ok || !strings.Contains(attributes, `rel="next"`) {
				continue
			}
			address, ok := strings.CutPrefix(target, "<")
			if !ok {
				return nil, errors.New("GitHub 返回了无效的分页链接")
			}
			link, err := url.Parse(address)
			if err != nil || link.RawQuery == "" {
				return nil, errors.New("GitHub 返回了无效的分页链接")
			}
			return url.ParseQuery(link.RawQuery)
		}
	}
	return nil, nil
}

func (p githubPull) list(ctx context.Context, name string) ([]json.RawMessage, error) {
	feed := githubFeeds[name]
	query, _ := url.ParseQuery(feed.query)
	query.Set("per_page", "100")

	items := []json.RawMessage{}
	seen := make(map[string]bool)
	err := p.pages(ctx, feed.path, query, func(body io.Reader) error {
		var page []json.RawMessage
		decoder := json.NewDecoder(body)
		if name == "actions" {
			var output struct {
				Runs []json.RawMessage `json:"workflow_runs"`
			}
			if err := decoder.Decode(&output); err != nil {
				return err
			}
			page = output.Runs
		} else if err := decoder.Decode(&page); err != nil {
			return err
		}
		if page == nil {
			return errors.New("missing GitHub list")
		}
		for _, item := range page {
			var key struct {
				SHA    string `json:"sha"`
				ID     int64  `json:"id"`
				Number int64  `json:"number"`
			}
			if err := json.Unmarshal(item, &key); err != nil {
				return err
			}
			id := key.SHA
			if id == "" {
				number := max(key.ID, key.Number)
				if number <= 0 {
					return errors.New("missing GitHub record ID")
				}
				id = strconv.FormatInt(number, 10)
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			if name == "secrets" {
				sanitized, err := githubSecretMetadata(item)
				if err != nil {
					return err
				}
				item = sanitized
			}
			items = append(items, item)
		}
		return nil
	})
	return items, err
}

func githubSecretMetadata(item json.RawMessage) (json.RawMessage, error) {
	// Keep only fields rendered in the console before persisting the alert.
	var alert struct {
		Number     int64  `json:"number"`
		State      string `json:"state"`
		Type       string `json:"secret_type_display_name"`
		Created    string `json:"created_at"`
		Updated    string `json:"updated_at"`
		Resolution string `json:"resolution"`
		Validity   string `json:"validity"`
		HTMLURL    string `json:"html_url"`
	}
	if err := json.Unmarshal(item, &alert); err != nil {
		return nil, err
	}
	return json.Marshal(alert)
}
