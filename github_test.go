package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubSyncFetchesAllPagesAndKeepsCache(t *testing.T) {
	const cursor = "opaque+cursor/="
	sha := strings.Repeat("a", 40)
	var calls, details atomic.Int32
	var secondPass atomic.Bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("missing server GitHub token")
		}
		query := r.URL.Query()
		switch r.URL.Path {
		case photoLibRepository + "/commits":
			_, _ = fmt.Fprintf(w, `[{"sha":%q}]`, sha)
		case photoLibRepository + "/commits/" + sha:
			details.Add(1)
			if query.Get("page") == "2" {
				_, _ = io.WriteString(w, `{"files":[{"filename":"second.go"}]}`)
			} else {
				w.Header().Set("Link", `<https://api.github.com/repositories/123/commits/`+sha+`?page=2>; rel="next"`)
				_, _ = io.WriteString(w, `{"stats":{"additions":2},"files":[{"filename":"first.go"}]}`)
			}
		case photoLibRepository + "/pulls":
			if query.Get("state") != "all" {
				t.Error("sync did not request all PR states")
			}
			_, _ = io.WriteString(w, `[{"number":8,"updated_at":"2026-11-20T00:00:00Z"}]`)
		case photoLibRepository + "/pulls/8":
			details.Add(1)
			_, _ = io.WriteString(w, `{"changed_files":3}`)
		case photoLibRepository + "/actions/runs":
			_, _ = io.WriteString(w, `{"workflow_runs":[{"id":12,"status":"completed","updated_at":"2026-11-20T00:00:00Z"}]}`)
		case photoLibRepository + "/actions/runs/12/jobs":
			details.Add(1)
			if query.Get("page") == "2" {
				_, _ = io.WriteString(w, `{"jobs":[{"id":3,"name":"second job"}]}`)
			} else {
				w.Header().Set("Link", `<https://api.github.com/repositories/123/actions/runs/12/jobs?page=2>; rel="next"`)
				_, _ = io.WriteString(w, `{"jobs":[{"id":2,"name":"first job"}]}`)
			}
		case photoLibRepository + "/dependabot/alerts":
			if query.Has("page") || query.Get("per_page") != "100" {
				t.Error("invalid Dependabot pagination")
			}
			switch query.Get("after") {
			case "":
				w.Header().Set("Link", `<https://api.github.com/repositories/123/dependabot/alerts?after=`+
					url.QueryEscape(cursor)+`>; rel="next"`)
				alerts := make([]map[string]int, 20)
				for i := range alerts {
					alerts[i] = map[string]int{"number": i + 1}
				}
				writeJSON(w, alerts)
			case cursor:
				if secondPass.Load() {
					http.Error(w, "unavailable", http.StatusForbidden)
					return
				}
				_, _ = io.WriteString(w, `[{"number":20},{"number":21}]`)
			default:
				t.Error("unexpected Dependabot cursor")
				http.Error(w, "invalid cursor", http.StatusBadRequest)
			}
		case photoLibRepository + "/code-scanning/alerts":
			_, _ = io.WriteString(w, `[]`)
		case photoLibRepository + "/secret-scanning/alerts":
			if query.Get("hide_secret") != "true" {
				t.Error("secret scanning request did not request redaction")
			}
			_, _ = io.WriteString(w, `[{"number":3,"secret":"must-not-be-stored","state":"open","secret_type_display_name":"Test Token"}]`)
		default:
			t.Error("unexpected GitHub endpoint")
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	repo := newGitHub(testToken)
	repo.base = api.URL
	dir := t.TempDir()
	logger := slog.New(slog.DiscardHandler)
	cache := openGitHubSync(repo, dir, logger)
	cache.refresh(t.Context(), githubPull{repo: repo})
	status := cache.status()
	if status.Running || status.FinishedAt.IsZero() || status.NextAt.Sub(status.StartedAt) != 3*time.Hour || len(status.Issues) != 0 {
		t.Fatalf("unexpected sync status: %+v", status)
	}
	if details.Load() != 5 {
		t.Fatalf("expected five detail requests, got %d", details.Load())
	}

	reopened := openGitHubSync(repo, dir, logger)
	beforeRead := calls.Load()
	for _, tc := range []struct {
		section, query string
		expected       []string
	}{
		{"commits", "page=1", []string{`"filename":"first.go"`, `"filename":"second.go"`}},
		{"pulls", "page=1", []string{`"changed_files":3`}},
		{"actions", "page=1", []string{`"name":"first job"`, `"name":"second job"`}},
		{"dependabot", "page=1", []string{`"hasNext":true`}},
		{"dependabot", "page=2", []string{`"items":[{"number":21}]`, `"hasNext":false`}},
		{"secrets", "page=1", []string{`"number":3`, `"secret_type_display_name":"Test Token"`}},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/github/"+tc.section+"?"+tc.query, nil)
		r.SetPathValue("section", tc.section)
		w := httptest.NewRecorder()
		reopened.list(w, r, administrator{})
		body := w.Body.String()
		if w.Code != http.StatusOK || strings.Contains(body, testToken) || strings.Contains(body, "must-not-be-stored") {
			t.Fatalf("unexpected response for %s: %d", tc.section, w.Code)
		}
		for _, text := range tc.expected {
			if !strings.Contains(body, text) {
				t.Fatalf("%s missing %q in %s", tc.section, text, body)
			}
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/github/actions/12", nil)
	r.SetPathValue("section", "actions")
	r.SetPathValue("id", "12")
	w := httptest.NewRecorder()
	reopened.detail(w, r, administrator{})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "second job") || calls.Load() != beforeRead {
		t.Fatal("reading the saved cache made an upstream request or lost a detail page")
	}
	saved, err := os.ReadFile(filepath.Join(dir, "github", "secrets.json"))
	if err != nil || strings.Contains(string(saved), "must-not-be-stored") {
		t.Fatal("raw credential reached the disk cache")
	}

	previous := reopened.snapshot("dependabot")
	secondPass.Store(true)
	reopened.refresh(t.Context(), githubPull{repo: repo})
	if details.Load() != 5 {
		t.Fatal("unchanged details were fetched again")
	}
	snapshot := reopened.snapshot("dependabot")
	if len(snapshot.Items) != 21 || snapshot.UpdatedAt != previous.UpdatedAt || reopened.status().Issues["dependabot"] == "" {
		t.Fatal("failure on the second page replaced a successful snapshot")
	}
}

func TestGitHubSyncStartsAutomaticallyAndCoalescesManualRequests(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case photoLibRepository + "/commits":
			started <- struct{}{}
			select {
			case <-r.Context().Done():
				return
			case <-release:
			}
		case photoLibRepository + "/actions/runs":
			_, _ = io.WriteString(w, `{"workflow_runs":[]}`)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	defer api.Close()
	repo := newGitHub(testToken)
	repo.base = api.URL
	cache := openGitHubSync(repo, t.TempDir(), slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	waitForStart := func() {
		t.Helper()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the sync never started")
		}
	}
	requestSync := func() githubSyncStatus {
		t.Helper()
		w := httptest.NewRecorder()
		cache.syncNow(w, httptest.NewRequest(http.MethodPost, "/api/github/sync", nil), administrator{})
		var status githubSyncStatus
		if w.Code != http.StatusOK || json.NewDecoder(w.Body).Decode(&status) != nil {
			t.Fatal("invalid sync response")
		}
		return status
	}
	waitForStart()
	if !requestSync().Running || len(cache.wake) != 0 {
		t.Fatal("manual request did not join the running sync")
	}
	release <- struct{}{}
	for deadline := time.Now().Add(5 * time.Second); cache.status().Running; {
		if time.Now().After(deadline) {
			t.Fatal("the automatic sync did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !requestSync().Running {
		t.Fatal("manual request did not start a new sync")
	}
	waitForStart()
	release <- struct{}{}
}
