package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

func TestEmergencyLoginWorksWithoutPhotoLib(t *testing.T) {
	var output bytes.Buffer
	if err := generateEmergencyPassword(&output); err != nil {
		t.Fatal(err)
	}
	passwordLine, envLine, _ := strings.Cut(output.String(), "\n")
	password := strings.TrimPrefix(passwordLine, "应急密码 ")
	values, err := godotenv.Unmarshal(envLine)
	if err != nil || !validToken(password) {
		t.Fatal("could not load generated emergency credentials")
	}
	values["DEPLYT_PUBLIC_URL"] = "https://deploy.example.test"
	values["PHOTOLIB_PUBLIC_URL"] = "https://photos.example.test"
	auth, err := newConsoleAuth(values)
	if err != nil {
		t.Fatal(err)
	}
	offline := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("emergency authentication contacted PhotoLib")
		return nil, errors.New("PhotoLib is offline")
	})
	auth.photo.http.Transport = offline
	auth.client.Transport = offline
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/login", auth.loginView)
	mux.HandleFunc("POST /auth/emergency", auth.emergencyLogin(slog.New(slog.DiscardHandler)))
	mux.HandleFunc("GET /admin", auth.requireAdmin(func(w http.ResponseWriter, _ *http.Request, user administrator) {
		if user.ID != emergencyAccountID || user.Username != "应急管理员" {
			t.Error("missing emergency administrator identity")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /auth/logout", auth.requireAdmin(auth.logout))
	handler := consoleHeaders(mux)

	options := httptest.NewRecorder()
	handler.ServeHTTP(options, httptest.NewRequest(http.MethodGet, "/api/login", nil))
	if !strings.Contains(options.Body.String(), `"emergency":true`) || strings.Contains(options.Body.String(), password) {
		t.Fatal("incorrect emergency login options")
	}

	request := func(method, url, origin string, body io.Reader, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, url, body)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want || strings.Contains(w.Body.String(), password) {
			t.Fatalf("%s %s returned HTTP %d, want %d", method, url, w.Code, want)
		}
		return w
	}

	submit := func(origin, value string, previous *http.Cookie, status int) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"password": value})
		if err != nil {
			t.Fatal(err)
		}
		return request(http.MethodPost, "/auth/emergency", origin, bytes.NewReader(body), previous, status)
	}

	submit("https://elsewhere.test", password, nil, http.StatusForbidden)
	submit(auth.origin, "incorrect", nil, http.StatusUnauthorized)
	auth.emergency.verify.Lock()
	submit(auth.origin, password, nil, http.StatusTooManyRequests)
	auth.emergency.verify.Unlock()

	previous := httptest.NewRecorder()
	if !auth.saveSession(previous, httptest.NewRequest(http.MethodGet, "/", nil), adminSession{
		administrator: administrator{ID: "123", Username: "photo-user"},
		photo:         &consoleSession{tokens: photoLibTokens{AccessToken: "old-access", RefreshToken: "old-refresh"}},
	}, time.Hour) {
		t.Fatal("could not create the previous PhotoLib session")
	}
	oldCookie := previous.Result().Cookies()[0]
	signedIn := submit(auth.origin, password, oldCookie, http.StatusNoContent)
	cookies := signedIn.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value == oldCookie.Value || !cookies[0].Secure ||
		!cookies[0].HttpOnly || cookies[0].MaxAge != 1800 || signedIn.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing secure emergency session cookie")
	}
	request(http.MethodGet, "/admin", "", nil, oldCookie, http.StatusUnauthorized)
	request(http.MethodGet, "/admin", "", nil, cookies[0], http.StatusNoContent)
	request(http.MethodPost, "/auth/logout", auth.origin, nil, cookies[0], http.StatusNoContent)
	request(http.MethodGet, "/admin", "", nil, cookies[0], http.StatusUnauthorized)
}

func TestEmergencyLoginRequiresConfiguredHash(t *testing.T) {
	values := map[string]string{
		"DEPLYT_PUBLIC_URL":   "https://deploy.example.test",
		"PHOTOLIB_PUBLIC_URL": "https://photos.example.test",
	}
	auth, err := newConsoleAuth(values)
	if err != nil {
		t.Fatal(err)
	}
	options := httptest.NewRecorder()
	auth.loginView(options, httptest.NewRequest(http.MethodGet, "/api/login", nil))
	if strings.Contains(options.Body.String(), "emergency") {
		t.Fatal("emergency option is visible without a configured hash")
	}
	result := httptest.NewRecorder()
	auth.emergencyLogin(slog.New(slog.DiscardHandler))(result, httptest.NewRequest(http.MethodPost, "/auth/emergency", nil))
	if result.Code != http.StatusNotFound || len(result.Result().Cookies()) != 0 {
		t.Fatal("emergency login is enabled without a configured hash")
	}
	values["DEPLOY_EMERGENCY_PASSWORD_HASH"] = "incorrect"
	if _, err := newConsoleAuth(values); err == nil || strings.Contains(err.Error(), "incorrect") {
		t.Fatal("invalid emergency hash was accepted or disclosed")
	}
}
