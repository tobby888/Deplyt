package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPairedPhotoLibAccountRequiresDeplytGrant(t *testing.T) {
	const memberID = int64(2012345678901234567)
	memberIDText := strconv.FormatInt(memberID, 10)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api/v1/auth/mcp/authorizations":
			data = photoLibPairing{RequestID: "pairing-id", DeviceCode: "private-device-code", UserCode: "PAIR-CODE", ExpiresIn: 600, Interval: 2}
		case "/api/v1/auth/mcp/token":
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["deviceCode"] != "private-device-code" {
				t.Error("missing server-side device code")
			}
			data = photoLibTokens{Status: "APPROVED", AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 900}
		case "/api/v1/auth/me":
			if r.Header.Get("Authorization") != "Bearer access" {
				t.Error("missing PhotoLib access token")
			}
			// PhotoLib emits Snowflake IDs above JavaScript's safe integer range as strings.
			data = map[string]string{"id": memberIDText, "username": "member"}
		default:
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"code": "OK", "data": data})
	}))
	defer provider.Close()

	auth, err := newConsoleAuth(map[string]string{"DEPLYT_PUBLIC_URL": "https://deploy.example.test", "PHOTOLIB_PUBLIC_URL": provider.URL})
	if err != nil {
		t.Fatal(err)
	}
	auth.photo.http = provider.Client()
	dir := t.TempDir()
	auth.access, err = openAccessRegistry(dir, provider.URL, 11)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	github := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["ref"] != "master" ||
			r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("unexpected GitHub dispatch request")
		}
		_, _ = w.Write([]byte(`{"workflow_run_id":1234}`))
	}))
	defer github.Close()
	dispatch := &workflowDispatch{endpoint: github.URL, token: "test-token", client: github.Client()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/pairing", auth.startPairing)
	mux.HandleFunc("POST /auth/pairing/finish", auth.finishPairing)
	mux.HandleFunc("POST /api/access/{id}/{action}", auth.requireAdmin(auth.updateAccess(logger)))
	mux.HandleFunc("POST /api/deployments/workflow", auth.protect(grantedAccess, dispatch.handle(logger)))
	post := func(path, origin string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.Header.Set("Origin", origin)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s returned %d, want %d: %s", path, w.Code, want, w.Body)
		}
		return w
	}

	origin := auth.origin
	post("/api/deployments/workflow", origin, nil, http.StatusUnauthorized)
	post("/auth/pairing", "https://elsewhere.test", nil, http.StatusForbidden)
	login := post("/auth/pairing", origin, nil, http.StatusOK)
	if strings.Contains(login.Body.String(), "private-device-code") || !strings.Contains(login.Body.String(), "PAIR-CODE") {
		t.Fatal("unexpected pairing data in browser")
	}
	result := post("/auth/pairing/finish", origin, login.Result().Cookies()[0], http.StatusOK)
	member := result.Result().Cookies()[1]
	post("/api/deployments/workflow", origin, member, http.StatusForbidden)
	post("/api/access/"+memberIDText+"/grant", origin, member, http.StatusForbidden)

	adminPage := httptest.NewRecorder()
	if !auth.setSession(adminPage, httptest.NewRequest(http.MethodGet, "/", nil),
		adminSession{administrator: administrator{ID: "11", Username: "admin"}, privileged: true}, time.Minute) {
		t.Fatal("could not create admin session")
	}
	admin := adminPage.Result().Cookies()[0]
	post("/api/access/999/grant", origin, admin, http.StatusNotFound)
	post("/api/access/"+memberIDText+"/grant", origin, admin, http.StatusNoContent)
	res := post("/api/deployments/workflow", origin, member, http.StatusAccepted)
	if !strings.Contains(res.Body.String(), "/actions/runs/1234") {
		t.Fatalf("missing run link: %s", res.Body)
	}
	reloaded, err := openAccessRegistry(dir, provider.URL, 11)
	if err != nil || !reloaded.allowed(memberID) {
		t.Fatalf("grant did not survive restart: %v", err)
	}
	post("/api/access/"+memberIDText+"/revoke", origin, admin, http.StatusNoContent)
	post("/api/deployments/workflow", origin, member, http.StatusForbidden)
}
