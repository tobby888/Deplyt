package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	loginCookie   = "__Host-deplyt-login"
	sessionCookie = "__Host-deplyt-session"
	sessionLife   = 30 * time.Minute
)

type administrator struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type adminSession struct {
	administrator
	expires time.Time
}

type consoleAuth struct {
	origin   string
	photoLib string
	secret   string
	client   *http.Client
	mu       sync.Mutex
	sessions map[string]adminSession
}

func newConsoleAuth(values map[string]string) (*consoleAuth, error) {
	origin, err := httpsOrigin(values["DEPLYT_PUBLIC_URL"])
	if err != nil {
		return nil, errors.New("DEPLYT_PUBLIC_URL must be an HTTPS origin")
	}
	photoLib, err := httpsOrigin(values["PHOTOLIB_PUBLIC_URL"])
	if err != nil {
		return nil, errors.New("PHOTOLIB_PUBLIC_URL must be an HTTPS origin")
	}
	secret := values["DEPLYT_SSO_SECRET"]
	if len(secret) < 32 || strings.ContainsAny(secret, " \t\r\n") {
		return nil, errors.New("DEPLYT_SSO_SECRET must have at least 32 characters and no whitespace")
	}
	return &consoleAuth{
		origin: origin, photoLib: photoLib, secret: secret, sessions: make(map[string]adminSession),
		client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: rejectRedirect},
	}, nil
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func httpsOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid HTTPS origin")
	}
	host := strings.ToLower(u.Host)
	if u.Port() == "443" {
		host = strings.TrimSuffix(host, ":443")
	}
	return "https://" + host, nil
}

func randomToken() string {
	var data [32]byte
	rand.Read(data[:])
	return base64.RawURLEncoding.EncodeToString(data[:])
}

func validToken(token string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil && len(b) == 32 && len(token) == 43
}

func authCookie(w http.ResponseWriter, name, value string, seconds int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: seconds,
	})
}

func (a *consoleAuth) start(w http.ResponseWriter, r *http.Request) {
	state, verifier := randomToken(), randomToken()
	hash := sha256.Sum256([]byte(verifier))
	query := url.Values{"state": {state}, "challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}}
	authCookie(w, loginCookie, state+"."+verifier, 600)
	http.Redirect(w, r, a.photoLib+"/deplyt/authorize?"+query.Encode(), http.StatusSeeOther)
}

func (a *consoleAuth) callback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(loginCookie)
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	if err != nil || !validToken(code) || !validToken(state) {
		http.Redirect(w, r, "/?login=failed", http.StatusSeeOther)
		return
	}
	expectedState, verifier, _ := strings.Cut(cookie.Value, ".")
	if subtle.ConstantTimeCompare([]byte(expectedState), []byte(state)) != 1 || !validToken(verifier) {
		http.Redirect(w, r, "/?login=failed", http.StatusSeeOther)
		return
	}

	authCookie(w, loginCookie, "", -1)
	admin, err := a.exchange(r.Context(), code, verifier)
	if err != nil {
		http.Redirect(w, r, "/?login=failed", http.StatusSeeOther)
		return
	}
	now := time.Now()
	token := randomToken()
	a.mu.Lock()
	for key, session := range a.sessions {
		if !now.Before(session.expires) {
			delete(a.sessions, key)
		}
	}
	if old, err := r.Cookie(sessionCookie); err == nil {
		delete(a.sessions, old.Value)
	}
	a.sessions[token] = adminSession{administrator: admin, expires: now.Add(sessionLife)}
	a.mu.Unlock()
	authCookie(w, sessionCookie, token, int(sessionLife.Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *consoleAuth) exchange(ctx context.Context, code, verifier string) (administrator, error) {
	body, err := json.Marshal(map[string]string{"code": code, "verifier": verifier})
	if err != nil {
		return administrator{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.photoLib+"/api/v1/auth/deplyt/token", bytes.NewReader(body))
	if err != nil {
		return administrator{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Deplyt-Client-Secret", a.secret)
	response, err := a.client.Do(request)
	if err != nil {
		return administrator{}, err
	}
	defer response.Body.Close()
	var result struct {
		Code string
		Data administrator
	}
	if response.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result) != nil ||
		result.Code != "OK" || result.Data.ID == "" || result.Data.Username == "" {
		return administrator{}, errors.New("PhotoLib denied the authorization code")
	}
	return result.Data, nil
}

func (a *consoleAuth) admin(r *http.Request) (administrator, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return administrator{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[cookie.Value]
	if !ok || !time.Now().Before(session.expires) {
		delete(a.sessions, cookie.Value)
		return administrator{}, false
	}
	return session.administrator, true
}

func (a *consoleAuth) requireAdmin(next func(http.ResponseWriter, *http.Request, administrator)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin, ok := a.admin(r)
		if !ok {
			http.Error(w, "请使用 PhotoLib 管理员账号登录", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Origin") != a.origin {
			http.Error(w, "请求来源无效", http.StatusForbidden)
			return
		}
		next(w, r, admin)
	}
}

func (a *consoleAuth) logout(w http.ResponseWriter, r *http.Request, _ administrator) {
	cookie, _ := r.Cookie(sessionCookie)
	a.mu.Lock()
	delete(a.sessions, cookie.Value)
	a.mu.Unlock()
	authCookie(w, sessionCookie, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func consoleHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
