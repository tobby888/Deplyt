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

func TestSwitchBetweenJARAndNativeDeployments(t *testing.T) {
	dir := t.TempDir()
	store := history(dir)
	commit := strings.Repeat("c", 40)
	jarTarget := deployment{
		ID: strings.Repeat("a", 26), Name: testJARName, OriginalName: testJARName,
		Object: archivePrefix + testJARName, SHA256: strings.Repeat("1", 64),
		Status: "succeeded", Archived: true, Kind: "deploy", Run: "555", Commit: commit,
		Started: time.Now().Add(-2 * time.Minute),
	}
	nativeTarget := deployment{
		ID: strings.Repeat("b", 26), Name: testNativeName, OriginalName: testNativeName,
		Object: archivePrefix + testNativeName, SHA256: strings.Repeat("2", 64),
		Format: artifactFormatNative, Status: "archived", Archived: true, Kind: "deploy", Run: "555", Commit: commit,
		Started: time.Now().Add(-time.Minute),
	}
	if err := store.save(jarTarget); err != nil {
		t.Fatal(err)
	}
	if err := store.save(nativeTarget); err != nil {
		t.Fatal(err)
	}

	var deployed []artifact
	relay := &relay{
		history: store, logger: slog.New(slog.DiscardHandler),
		downloadURL: func(_ context.Context, key string) (string, error) {
			return "https://oss.example/" + key, nil
		},
		deploy: func(_ context.Context, art artifact, _, _ string, _ *slog.Logger) error {
			deployed = append(deployed, art)
			return nil
		},
	}
	operator := administrator{ID: "1", Username: "admin"}

	// Archived-only native target must reject restoreDatabase=true because no snapshot exists.
	reqBad := httptest.NewRequest(http.MethodPost, "/api/deployments/"+nativeTarget.ID+"/switch", strings.NewReader(`{"restoreDatabase":true}`))
	reqBad.SetPathValue("id", nativeTarget.ID)
	w := httptest.NewRecorder()
	relay.handleSwitch(w, reqBad, operator)
	if w.Code != http.StatusBadRequest || len(deployed) != 0 {
		t.Fatalf("expected 400 for DB restore on archived-only target, got %d: %s", w.Code, w.Body.String())
	}

	// Switch to archived native image bundle.
	reqNative := httptest.NewRequest(http.MethodPost, "/api/deployments/"+nativeTarget.ID+"/switch", nil)
	reqNative.SetPathValue("id", nativeTarget.ID)
	w = httptest.NewRecorder()
	relay.handleSwitch(w, reqNative, operator)
	if w.Code != http.StatusNoContent || len(deployed) != 1 {
		t.Fatalf("switch to native failed: %d %s", w.Code, w.Body.String())
	}
	if deployed[0].format != artifactFormatNative || deployed[0].name != testNativeName || deployed[0].sha256 != nativeTarget.SHA256 {
		t.Fatalf("unexpected native artifact deployed: %+v", deployed[0])
	}

	// Switch back to legacy JAR record (which had empty Format on disk).
	reqJAR := httptest.NewRequest(http.MethodPost, "/api/deployments/"+jarTarget.ID+"/switch", strings.NewReader(`{}`))
	reqJAR.SetPathValue("id", jarTarget.ID)
	w = httptest.NewRecorder()
	relay.handleSwitch(w, reqJAR, operator)
	if w.Code != http.StatusNoContent || len(deployed) != 2 {
		t.Fatalf("switch back to JAR failed: %d %s", w.Code, w.Body.String())
	}
	if deployed[1].format != artifactFormatJAR || deployed[1].name != testJARName || deployed[1].sha256 != jarTarget.SHA256 {
		t.Fatalf("unexpected JAR artifact deployed: %+v", deployed[1])
	}

	records, err := store.list()
	if err != nil || len(records) != 4 {
		t.Fatalf("expected 4 persisted records, got %d: %v", len(records), err)
	}
	if records[0].Kind != "switch" || records[0].Format != artifactFormatJAR || records[0].Target != jarTarget.ID || records[0].Status != "succeeded" {
		t.Fatalf("unexpected switch-to-JAR record: %+v", records[0])
	}
	if records[1].Kind != "switch" || records[1].Format != artifactFormatNative || records[1].Target != nativeTarget.ID || records[1].Status != "succeeded" {
		t.Fatalf("unexpected switch-to-native record: %+v", records[1])
	}
}
