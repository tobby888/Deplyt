package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDependabotAlertsUseCursorPagination(t *testing.T) {
	const nextCursor = "opaque+cursor/="
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.URL.Path != photoLibRepository+"/dependabot/alerts" || query.Has("page") || query.Get("per_page") != "20" {
			http.Error(w, "invalid Dependabot pagination", http.StatusBadRequest)
			return
		}
		switch query.Get("after") {
		case "":
			w.Header().Set("Link", `<https://api.github.com/repositories/123/dependabot/alerts?per_page=20&after=`+
				url.QueryEscape(nextCursor)+`>; rel="next"`)
			_, _ = io.WriteString(w, `[{"number":21}]`)
		case nextCursor:
			w.Header().Set("Link", `<https://api.github.com/repositories/123/dependabot/alerts?before=other>; rel="prev"`)
			_, _ = io.WriteString(w, `[{"number":1}]`)
		default:
			http.Error(w, "invalid cursor", http.StatusBadRequest)
		}
	}))
	defer api.Close()
	repo := newGitHub(testToken)
	repo.base = api.URL

	for _, tc := range []struct {
		query    string
		expected []string
	}{
		{"page=1", []string{`"number":21`, `"hasNext":true`, `"nextCursor":"` + nextCursor + `"`}},
		{"page=2&after=" + url.QueryEscape(nextCursor), []string{`"number":1`, `"hasNext":false`}},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/github/dependabot?"+tc.query, nil)
		r.SetPathValue("section", "dependabot")
		w := httptest.NewRecorder()
		repo.list(w, r, administrator{})
		if w.Code != http.StatusOK {
			t.Fatalf("query %q returned %d: %s", tc.query, w.Code, w.Body)
		}
		for _, expected := range tc.expected {
			if !strings.Contains(w.Body.String(), expected) {
				t.Fatalf("query %q missing %q: %s", tc.query, expected, w.Body)
			}
		}
	}
}

func TestGitHubFeedsKeepTokenServerSideAndRedactSecrets(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.URL.Query().Get("page") != "2" {
			t.Error("GitHub request did not use the server token and selected page")
		}
		w.Header().Set("Link", `<https://api.github.com/page/3>; rel="next"`)
		switch r.URL.Path {
		case photoLibRepository + "/actions/runs":
			_, _ = io.WriteString(w, `{"total_count":5,"workflow_runs":[{"id":12,"conclusion":"success"}]}`)
		case photoLibRepository + "/secret-scanning/alerts":
			_, _ = io.WriteString(w, `[{"number":3,"secret":"must-not-leave-server","state":"open","secret_type_display_name":"Test Token"}]`)
		default:
			t.Error("unexpected GitHub endpoint")
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	repo := newGitHub(testToken)
	repo.base = api.URL
	for section, expected := range map[string]string{"actions": `"conclusion":"success"`, "secrets": `"number":3`} {
		r := httptest.NewRequest(http.MethodGet, "/api/github/"+section+"?page=2", nil)
		r.SetPathValue("section", section)
		w := httptest.NewRecorder()
		repo.list(w, r, administrator{})
		data := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(data, expected) ||
			!strings.Contains(data, `"hasNext":true`) || strings.Contains(data, "must-not-leave-server") ||
			strings.Contains(data, testToken) {
			t.Fatalf("unexpected %s response: %d %s", section, w.Code, data)
		}
	}
}
