package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestConsoleSSOAndOriginCheck(t *testing.T) {
	var exchanges int
	var challenge string
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		var body map[string]string
		if r.URL.Path != "/api/v1/auth/deplyt/token" || r.Header.Get("X-Deplyt-Client-Secret") != testToken ||
			json.NewDecoder(r.Body).Decode(&body) != nil || !validToken(body["code"]) {
			t.Error("unexpected PhotoLib token request")
		}
		digest := sha256.Sum256([]byte(body["verifier"]))
		if challenge != base64.RawURLEncoding.EncodeToString(digest[:]) {
			t.Error("exchanged verifier does not match the login challenge")
		}
		writeJSON(w, map[string]any{"code": "OK", "data": administrator{ID: "42", Username: "admin"}})
	}))
	defer provider.Close()
	auth, err := newConsoleAuth(map[string]string{
		"DEPLYT_PUBLIC_URL": "https://deploy.example.test", "PHOTOLIB_PUBLIC_URL": provider.URL, "DEPLYT_SSO_SECRET": testToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	auth.client = provider.Client()
	start := httptest.NewRecorder()
	auth.start(start, httptest.NewRequest(http.MethodGet, "/auth/start", nil))
	redirect, err := url.Parse(start.Header().Get("Location"))
	if err != nil || redirect.Path != "/deplyt/authorize" {
		t.Fatal("missing PhotoLib redirect")
	}
	challenge = redirect.Query().Get("challenge")
	state := redirect.Query().Get("state")
	login := start.Result().Cookies()[0]
	if login.Name != loginCookie || !login.HttpOnly || !login.Secure || login.SameSite != http.SameSiteLaxMode {
		t.Fatal("login cookie is not browser-bound")
	}

	callback := func(state string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+state+"&code="+randomToken(), nil)
		r.AddCookie(login)
		w := httptest.NewRecorder()
		auth.callback(w, r)
		return w
	}
	callback(randomToken())
	if exchanges != 0 {
		t.Fatal("callback accepted the wrong state")
	}

	result := callback(state)
	if result.Header().Get("Location") != "/" || exchanges != 1 {
		t.Fatal("callback did not establish the session")
	}
	session := result.Result().Cookies()[1]
	protected := auth.requireAdmin(func(w http.ResponseWriter, _ *http.Request, admin administrator) {
		if admin.ID != "42" {
			t.Error("missing PhotoLib identity")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	request := func(handler http.HandlerFunc, cookie *http.Cookie, origin string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/rollback", strings.NewReader(""))
		r.Header.Set("Origin", origin)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if request(protected, nil, auth.origin) != http.StatusUnauthorized ||
		request(protected, session, "https://elsewhere.example.test") != http.StatusForbidden ||
		request(protected, session, auth.origin) != http.StatusNoContent ||
		request(auth.requireAdmin(auth.logout), session, auth.origin) != http.StatusNoContent ||
		request(protected, session, auth.origin) != http.StatusUnauthorized {
		t.Fatal("unexpected admin session or CSRF behavior")
	}
}
