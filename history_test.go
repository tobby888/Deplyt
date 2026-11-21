package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRollbackReusesArchiveAndRecordsOperator(t *testing.T) {
	dir := t.TempDir()
	store := history(dir)
	target := deployment{
		ID: strings.Repeat("a", 26), Name: testJARName, Object: archivePrefix + testJARName,
		SHA256: strings.Repeat("1", 64), Status: "succeeded", Archived: true,
		Kind: "deploy", Run: "12345", Commit: strings.Repeat("a", 40), Started: time.Now().Add(-time.Hour),
	}
	if err := store.save(target); err != nil {
		t.Fatal(err)
	}

	var activated int
	var syncedTarget string
	relay := &relay{
		history: store, logger: slog.New(slog.DiscardHandler),
		downloadURL: func(_ context.Context, key string) (string, error) {
			if key != target.Object {
				t.Errorf("signed the wrong object %s", key)
			}
			return "https://oss.example/signed", nil
		},
		deploy: func(_ context.Context, jar artifact, id, url string, _ *slog.Logger) error {
			activated++
			if jar.sha256 != target.SHA256 || jar.name != target.Name ||
				id == target.ID || url != "https://oss.example/signed" {
				t.Error("rollback did not reuse the archived artifact with a fresh deployment ID")
			}
			return nil
		},
		deployWithDBSync: func(_ context.Context, _ artifact, _, _, restoreFrom string, _ *slog.Logger) error {
			activated++
			syncedTarget = restoreFrom
			return nil
		},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/deployments/"+target.ID+"/rollback", strings.NewReader(`{"restoreDatabase":true}`))
	request.SetPathValue("id", target.ID)
	w := httptest.NewRecorder()
	operator := administrator{ID: "10", Username: "alice"}
	relay.handleRollback(w, request, operator)
	if w.Code != http.StatusNoContent || activated != 1 || syncedTarget != target.ID {
		t.Fatalf("rollback failed: %d %s (syncedTarget=%s)", w.Code, w.Body, syncedTarget)
	}

	reopened, err := openHistory(map[string]string{"DEPLOY_STATE_DIR": dir})
	if err != nil {
		t.Fatal(err)
	}
	records, err := reopened.list()
	if err != nil || len(records) != 2 {
		t.Fatalf("expected two persisted records, got %d: %v", len(records), err)
	}
	rollback := records[0]
	if rollback.Kind != "rollback" || rollback.Status != "succeeded" ||
		rollback.Target != target.ID || !rollback.RestoreDatabase || rollback.Finished == nil ||
		rollback.Operator != operator || rollback.Commit != target.Commit {
		t.Errorf("unexpected rollback record: %+v", rollback)
	}

	rollback.Status = "running"
	if err := store.save(rollback); err != nil {
		t.Fatal(err)
	}
	if _, err := openHistory(map[string]string{"DEPLOY_STATE_DIR": dir}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.load(rollback.ID)
	if err != nil || pending.Status != "interrupted" {
		t.Fatalf("unfinished record was not recovered: %+v %v", pending, err)
	}
	request.SetPathValue("id", pending.ID)
	w = httptest.NewRecorder()
	relay.handleRollback(w, request, operator)
	if w.Code != http.StatusNotFound || activated != 1 {
		t.Fatal("an unconfirmed deployment was eligible for rollback")
	}
}
