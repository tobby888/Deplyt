package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPhotoLibLoginAndDynamicAccess(t *testing.T) {
	var pairingCount atomic.Int64
	photoLib := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api/v1/auth/mcp/authorizations":
			id := pairingCount.Add(1)
			data = map[string]any{
				"requestId": fmt.Sprint(id), "deviceCode": fmt.Sprintf("device-%d", id),
				"userCode": "PAIR-CODE", "expiresIn": 600, "interval": 2,
			}
		case "/api/v1/auth/mcp/token":
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil ||
				input["deviceCode"] != "device-"+input["requestId"] {
				http.Error(w, "invalid device", http.StatusBadRequest)
				return
			}
			data = map[string]any{
				"status": "APPROVED", "accessToken": "token-" + input["requestId"],
				"refreshToken": "refresh-" + input["requestId"], "expiresIn": 900,
			}
		case "/api/v1/auth/me":
			switch r.Header.Get("Authorization") {
			case "Bearer token-1":
				data = photoLibUser{ID: 11, Username: "photo-owner"}
			case "Bearer token-2":
				data = photoLibUser{ID: 27, Username: "photo-member", DisplayName: "<Photo Member>"}
			default:
				http.Error(w, "invalid session", http.StatusUnauthorized)
				return
			}
		case "/api/v1/auth/mcp/token/revoke":
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "OK", "data": data})
	}))
	defer photoLib.Close()

	dir := t.TempDir()
	access, err := openAccessRegistry(dir, photoLib.URL, 11)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	c := newConsole(photoLibClient{photoLib.URL, photoLib.Client()}, access, slog.New(slog.DiscardHandler))
	c.register(mux)
	server := httptest.NewTLSServer(http.NewCrossOriginProtection().Handler(mux))
	defer server.Close()

	admin, member := consoleBrowser(t, server), consoleBrowser(t, server)
	browseConsole := func(t *testing.T, browser *http.Client, method, path, origin string, want int, contains string) string {
		t.Helper()
		return requestConsole(t, browser, method, server.URL+path, origin, want, contains)
	}
	browseConsole(t, member, http.MethodGet, "/", "", http.StatusSeeOther, "")
	for _, browser := range []*http.Client{admin, member} {
		browseConsole(t, browser, http.MethodPost, "/login/start", "", http.StatusSeeOther, "")
		body := browseConsole(t, browser, http.MethodGet, "/login", "", http.StatusOK, "PAIR-CODE")
		if strings.Contains(body, "device-1") || strings.Contains(body, "device-2") ||
			!strings.Contains(body, photoLib.URL+"/mcp/authorize?request=") {
			t.Fatal("unexpected PhotoLib pairing page")
		}
		browseConsole(t, browser, http.MethodPost, "/login/finish", "", http.StatusSeeOther, "")
	}
	browseConsole(t, member, http.MethodGet, "/", "", http.StatusForbidden, "等待授权")
	browseConsole(t, admin, http.MethodGet, "/", "", http.StatusOK, "&lt;Photo Member&gt;")
	browseConsole(t, member, http.MethodPost, "/access/27/grant", "", http.StatusForbidden, "")
	browseConsole(t, admin, http.MethodPost, "/access/999/grant", "", http.StatusNotFound, "")
	browseConsole(t, admin, http.MethodPost, "/access/27/grant", "https://other.example", http.StatusForbidden, "")
	browseConsole(t, admin, http.MethodPost, "/access/27/grant", server.URL, http.StatusSeeOther, "")
	browseConsole(t, member, http.MethodGet, "/", "", http.StatusOK, "PhotoLib 部署")

	reloaded, err := openAccessRegistry(dir, photoLib.URL, 11)
	if err != nil || !reloaded.allowed(27) {
		t.Fatalf("grant did not survive registry reload: %v", err)
	}
	browseConsole(t, admin, http.MethodPost, "/access/27/revoke", server.URL, http.StatusSeeOther, "")
	browseConsole(t, member, http.MethodGet, "/", "", http.StatusForbidden, "等待授权")
	browseConsole(t, member, http.MethodPost, "/logout", server.URL, http.StatusSeeOther, "")
	browseConsole(t, member, http.MethodGet, "/", "", http.StatusSeeOther, "")
}

func consoleBrowser(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := outboundClient(0)
	client.Transport = server.Client().Transport
	client.Jar = jar
	return client
}

func requestConsole(t *testing.T, client *http.Client, method, endpoint, origin string, want int, contains string) string {
	t.Helper()
	req, err := http.NewRequest(method, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want || !strings.Contains(string(data), contains) {
		t.Fatalf("%s %s returned %d, want %d containing %q\n%s", method, endpoint, res.StatusCode, want, contains, data)
	}
	return string(data)
}
