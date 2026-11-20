package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
)

type githubCachedDetail struct {
	Revision string          `json:"revision"`
	Data     json.RawMessage `json:"data"`
}

func githubDetailID(name string, item json.RawMessage) (id, revision string, reusable bool) {
	var identity struct {
		SHA        string `json:"sha"`
		Number     int64  `json:"number"`
		ID         int64  `json:"id"`
		Updated    string `json:"updated_at"`
		RunAttempt int    `json:"run_attempt"`
		Status     string `json:"status"`
	}
	if json.Unmarshal(item, &identity) != nil {
		return "", "", false
	}
	switch {
	case name == "commits" && commitPattern.MatchString(identity.SHA):
		return identity.SHA, identity.SHA, true
	case name == "pulls" && identity.Number > 0:
		return strconv.FormatInt(identity.Number, 10), identity.Updated, identity.Updated != ""
	case name == "actions" && identity.ID > 0:
		return strconv.FormatInt(identity.ID, 10), fmt.Sprintf("%s/%d", identity.Updated, identity.RunAttempt),
			identity.Status == "completed" && identity.Updated != ""
	}
	return "", "", false
}

func (p githubPull) detail(ctx context.Context, name, id string) (json.RawMessage, error) {
	switch name {
	case "commits":
		return p.commitDetail(ctx, id)
	case "pulls":
		return p.pullDetail(ctx, id)
	case "actions":
		return p.jobDetails(ctx, id)
	}
	return nil, errors.New("未知的 GitHub 数据类型")
}

func (p githubPull) commitDetail(ctx context.Context, id string) (json.RawMessage, error) {
	type fileDetail struct {
		Filename  string `json:"filename"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	}
	type commitDetail struct {
		Commit struct {
			Committer struct {
				Name string `json:"name"`
			} `json:"committer"`
		} `json:"commit"`
		Stats struct {
			Additions int `json:"additions"`
			Deletions int `json:"deletions"`
		} `json:"stats"`
		Files []fileDetail `json:"files"`
	}
	var result commitDetail
	first := true
	err := p.pages(ctx, "/commits/"+id, url.Values{"per_page": {"100"}}, func(body io.Reader) error {
		var page commitDetail
		if err := json.NewDecoder(body).Decode(&page); err != nil {
			return err
		}
		if first {
			result.Commit, result.Stats = page.Commit, page.Stats
			first = false
		}
		result.Files = append(result.Files, page.Files...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func (p githubPull) pullDetail(ctx context.Context, id string) (json.RawMessage, error) {
	var result struct {
		Commits      int    `json:"commits"`
		ChangedFiles int    `json:"changed_files"`
		Additions    int    `json:"additions"`
		Deletions    int    `json:"deletions"`
		MergedAt     string `json:"merged_at"`
	}
	response, err := p.fetch(ctx, "/pulls/"+id, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&result); err != nil {
		return nil, errors.New("无法读取 GitHub PR 详情")
	}
	return json.Marshal(result)
}

func (p githubPull) jobDetails(ctx context.Context, id string) (json.RawMessage, error) {
	type job struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
	}
	var result struct {
		Jobs []job `json:"jobs"`
	}
	result.Jobs = []job{}
	err := p.pages(ctx, "/actions/runs/"+id+"/jobs", url.Values{"per_page": {"100"}}, func(body io.Reader) error {
		var page struct {
			Jobs []job `json:"jobs"`
		}
		if err := json.NewDecoder(body).Decode(&page); err != nil {
			return err
		}
		if page.Jobs == nil {
			return errors.New("missing GitHub job list")
		}
		result.Jobs = append(result.Jobs, page.Jobs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}
