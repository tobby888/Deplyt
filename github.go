package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const photoLibRepository = "/repos/tobby888/Photolib"

type githubRepository struct {
	token  string
	base   string
	client *http.Client
}

func newGitHub(token string) githubRepository {
	return githubRepository{
		token: token, base: "https://api.github.com",
		client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: rejectRedirect},
	}
}

type githubFeed struct {
	path    string
	actions bool
	query   string
}

var githubFeeds = map[string]githubFeed{
	"commits":    {path: "/commits"},
	"pulls":      {path: "/pulls", query: "state=all&sort=updated&direction=desc"},
	"actions":    {path: "/actions/runs", actions: true},
	"dependabot": {path: "/dependabot/alerts"},
	"code":       {path: "/code-scanning/alerts"},
	"secrets":    {path: "/secret-scanning/alerts"},
}

func (g githubRepository) list(w http.ResponseWriter, r *http.Request, _ administrator) {
	name := r.PathValue("section")
	feed, ok := githubFeeds[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}
	if n, err := strconv.Atoi(page); err != nil || n < 1 || n > 10000 {
		http.Error(w, "页码无效", http.StatusBadRequest)
		return
	}
	query, _ := url.ParseQuery(feed.query)
	query.Set("per_page", "20")
	if name == "dependabot" {
		// Dependabot paginates with the cursor in GitHub's Link header.
		if after := r.URL.Query().Get("after"); after != "" {
			query.Set("after", after)
		}
	} else {
		query.Set("page", page)
	}
	response := g.fetch(w, r, feed.path+"?"+query.Encode())
	if response == nil {
		return
	}
	defer response.Body.Close()

	body := io.LimitReader(response.Body, 8<<20)
	var items []json.RawMessage
	var err error
	if feed.actions {
		var data struct {
			Runs []json.RawMessage `json:"workflow_runs"`
		}
		err = json.NewDecoder(body).Decode(&data)
		items = data.Runs
	} else {
		err = json.NewDecoder(body).Decode(&items)
	}
	if err != nil {
		http.Error(w, "无法读取 GitHub 响应", http.StatusBadGateway)
		return
	}
	// The Secret Scanning API includes the credential itself. Only allow display fields.
	if name == "secrets" {
		for i, raw := range items {
			var alert struct {
				Number     int    `json:"number"`
				State      string `json:"state"`
				Type       string `json:"secret_type_display_name"`
				Created    string `json:"created_at"`
				Updated    string `json:"updated_at"`
				Resolution string `json:"resolution"`
				Validity   string `json:"validity"`
				HTMLURL    string `json:"html_url"`
			}
			if err := json.Unmarshal(raw, &alert); err != nil {
				http.Error(w, "无法读取 GitHub Secret Scanning 响应", http.StatusBadGateway)
				return
			}
			items[i], _ = json.Marshal(alert)
		}
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	result := map[string]any{
		"items": items, "hasNext": strings.Contains(response.Header.Get("Link"), `rel="next"`),
	}
	if name == "dependabot" {
		cursor := dependabotNextCursor(response.Header.Values("Link"))
		result["nextCursor"] = cursor
		result["hasNext"] = cursor != ""
	}
	writeJSON(w, result)
}

func dependabotNextCursor(headers []string) string {
	for _, header := range headers {
		for entry := range strings.SplitSeq(header, ",") {
			entry, ok := strings.CutPrefix(strings.TrimSpace(entry), "<")
			if !ok {
				continue
			}
			target, attributes, ok := strings.Cut(entry, ">")
			if !ok || !strings.Contains(attributes, `rel="next"`) {
				continue
			}
			link, err := url.Parse(target)
			if err == nil {
				return link.Query().Get("after")
			}
		}
	}
	return ""
}

func (g githubRepository) detail(w http.ResponseWriter, r *http.Request, _ administrator) {
	id, section := r.PathValue("id"), r.PathValue("section")
	var path string
	switch {
	case section == "commits" && commitPattern.MatchString(id):
		path = "/commits/" + id
	case section == "pulls" && runIDPattern.MatchString(id):
		path = "/pulls/" + id
	case section == "actions" && runIDPattern.MatchString(id):
		path = "/actions/runs/" + id + "/jobs?per_page=100"
	default:
		http.NotFound(w, r)
		return
	}
	response := g.fetch(w, r, path)
	if response == nil {
		return
	}
	defer response.Body.Close()
	var data json.RawMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&data); err != nil {
		http.Error(w, "无法读取 GitHub 详情", http.StatusBadGateway)
		return
	}
	writeJSON(w, data)
}

func (g githubRepository) fetch(w http.ResponseWriter, r *http.Request, path string) *http.Response {
	if g.token == "" {
		http.Error(w, "Deplyt 尚未配置 GITHUB_TOKEN", http.StatusServiceUnavailable)
		return nil
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, g.base+photoLibRepository+path, nil)
	if err != nil {
		http.Error(w, "无法创建 GitHub 请求", http.StatusBadGateway)
		return nil
	}
	request.Header.Set("Authorization", "Bearer "+g.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := g.client.Do(request)
	if err != nil {
		http.Error(w, "无法连接 GitHub", http.StatusBadGateway)
		return nil
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		message := fmt.Sprintf("GitHub 请求失败（HTTP %d）", response.StatusCode)
		switch {
		case response.StatusCode == http.StatusTooManyRequests || response.Header.Get("X-RateLimit-Remaining") == "0":
			message = "GitHub 请求已达限额，请稍后重试"
		case response.StatusCode == http.StatusUnauthorized:
			message = "GitHub 未接受当前 PAT，请检查令牌是否有效"
		case response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound:
			message += "，请检查 PAT 权限及仓库是否已启用相应功能"
		}
		http.Error(w, message, http.StatusBadGateway)
		return nil
	}
	return response
}
