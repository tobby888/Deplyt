package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
