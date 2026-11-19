package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	pairingCookie = "__Host-deplyt-pairing"
	pairingTTL    = 10 * time.Minute
	memberTTL     = 8 * time.Hour
)

type accessLevel int

const (
	authenticatedAccess accessLevel = iota
	grantedAccess
	adminAccess
)

type consoleIdentity struct {
	administrator
	Admin   bool `json:"admin"`
	Allowed bool `json:"allowed"`
}

type pairing struct {
	mu       sync.Mutex
	deadline time.Time
	data     photoLibPairing
	pollAt   time.Time
}

type consoleSession struct {
	mu        sync.Mutex
	expiresAt time.Time
	tokens    photoLibTokens
}

type loginView struct {
	SSO        bool   `json:"sso"`
	Emergency  bool   `json:"emergency,omitempty"`
	PairingURL string `json:"pairingURL,omitempty"`
	Code       string `json:"code,omitempty"`
}

func (a *consoleAuth) writeLogin(w http.ResponseWriter, p *pairing) {
	view := loginView{SSO: a.secret != "", Emergency: a.emergency != nil}
	if p != nil && p.data.DeviceCode != "" {
		view.PairingURL = a.photo.authorizationURL(p.data.RequestID)
		view.Code = p.data.UserCode
	}
	writeJSON(w, view)
}

func (a *consoleAuth) loginView(w http.ResponseWriter, r *http.Request) {
	p := a.findPairing(r)
	if p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
	}
	a.writeLogin(w, p)
}

func (a *consoleAuth) startPairing(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != a.origin {
		http.Error(w, "请求来源无效", http.StatusForbidden)
		return
	}
	if a.findPairing(r) != nil {
		a.loginView(w, r)
		return
	}
	id := randomToken()
	now := time.Now()
	p := &pairing{deadline: now.Add(pairingTTL)}
	p.mu.Lock()
	defer p.mu.Unlock()
	a.mu.Lock()
	for key, value := range a.pairings {
		if !now.Before(value.deadline) {
			delete(a.pairings, key)
		}
	}
	if len(a.pairings) >= 256 {
		a.mu.Unlock()
		http.Error(w, "待批准的登录较多，请稍后重试", http.StatusTooManyRequests)
		return
	}
	a.pairings[id] = p
	a.mu.Unlock()
	input := map[string]string{"clientName": "Deplyt", "deviceLabel": "自动化部署平台"}
	err := a.photo.call(r.Context(), "/mcp/authorizations", input, "", &p.data)
	if err != nil || p.data.RequestID == "" || p.data.UserCode == "" ||
		p.data.DeviceCode == "" || p.data.ExpiresIn <= 0 {
		a.removePairing(id)
		http.Error(w, "PhotoLib 未能创建配对，请稍后重试", http.StatusBadGateway)
		return
	}
	authCookie(w, pairingCookie, id, int(min(p.data.ExpiresIn, int64(pairingTTL/time.Second))))
	a.writeLogin(w, p)
}

func (a *consoleAuth) finishPairing(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != a.origin {
		http.Error(w, "请求来源无效", http.StatusForbidden)
		return
	}
	p := a.findPairing(r)
	if p == nil {
		http.Error(w, "配对已过期，请重新登录", http.StatusUnauthorized)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.data.DeviceCode == "" {
		http.Error(w, "配对已结束，请重新登录", http.StatusUnauthorized)
		return
	}
	if time.Now().Before(p.pollAt) {
		http.Error(w, "请稍候再检查批准结果", http.StatusTooManyRequests)
		return
	}
	p.pollAt = time.Now().Add(time.Duration(max(2, min(30, p.data.Interval))) * time.Second)
	ctx := context.WithoutCancel(r.Context())
	input := map[string]string{"requestId": p.data.RequestID, "deviceCode": p.data.DeviceCode}
	var tokens photoLibTokens
	err := a.photo.call(ctx, "/mcp/token", input, "", &tokens)
	if err == nil && tokens.Status == "PENDING" {
		writeJSON(w, map[string]bool{"approved": false})
		return
	}
	if err != nil && !errors.Is(err, errPhotoLibSession) {
		http.Error(w, "暂时无法读取 PhotoLib 批准结果", http.StatusBadGateway)
		return
	}
	a.removePairing(cookieValue(r, pairingCookie))
	p.data.DeviceCode = ""
	authCookie(w, pairingCookie, "", -1)
	if err != nil || tokens.Status != "APPROVED" || !validPhotoTokens(tokens) {
		http.Error(w, "配对已失效或被拒绝，请重新登录", http.StatusUnauthorized)
		return
	}
	user, err := a.photo.user(ctx, tokens.AccessToken)
	if err != nil {
		a.revokePhotoToken(ctx, tokens.RefreshToken)
		http.Error(w, "无法验证 PhotoLib 账号", http.StatusUnauthorized)
		return
	}
	if err := a.access.register(user); err != nil {
		a.revokePhotoToken(ctx, tokens.RefreshToken)
		http.Error(w, "无法保存 PhotoLib 账号", http.StatusInternalServerError)
		return
	}
	session := adminSession{
		administrator: administrator{ID: strconv.FormatInt(user.ID, 10), Username: user.Username},
		privileged:    user.ID == a.access.admin,
		photo:         &consoleSession{},
	}
	session.photo.setTokens(tokens)
	if !a.setSession(w, r, session, memberTTL) {
		a.revokePhotoToken(ctx, tokens.RefreshToken)
		http.Error(w, "会话较多，请稍后登录", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]bool{"approved": true})
}

func (a *consoleAuth) findPairing(r *http.Request) *pairing {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pairings[cookieValue(r, pairingCookie)]
	if p != nil && time.Now().Before(p.deadline) {
		return p
	}
	return nil
}

func (a *consoleAuth) removePairing(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pairings, id)
}

func (a *consoleAuth) setSession(w http.ResponseWriter, r *http.Request, session adminSession, ttl time.Duration) bool {
	a.dropSession(r)
	return a.saveSession(w, r, session, ttl)
}

func (a *consoleAuth) saveSession(w http.ResponseWriter, r *http.Request, session adminSession, ttl time.Duration) bool {
	now, token := time.Now(), randomToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, cookieValue(r, sessionCookie))
	for id, active := range a.sessions {
		if !now.Before(active.expires) {
			delete(a.sessions, id)
		}
	}
	if len(a.sessions) >= 1024 {
		return false
	}
	session.expires = now.Add(ttl)
	a.sessions[token] = session
	authCookie(w, sessionCookie, token, int(ttl/time.Second))
	return true
}

func (a *consoleAuth) identity(r *http.Request) (consoleIdentity, error) {
	id := cookieValue(r, sessionCookie)
	a.mu.Lock()
	session, ok := a.sessions[id]
	a.mu.Unlock()
	if !ok || !time.Now().Before(session.expires) {
		return consoleIdentity{}, errPhotoLibSession
	}
	if session.photo != nil {
		user, err := a.verifyPhotoSession(r.Context(), session.photo)
		if err != nil {
			return consoleIdentity{}, err
		}
		if strconv.FormatInt(user.ID, 10) != session.ID {
			return consoleIdentity{}, errPhotoLibSession
		}
		session.Username = user.Username
	}
	userID, _ := strconv.ParseInt(session.ID, 10, 64)
	allowed := session.privileged || (userID > 0 && a.access != nil && a.access.allowed(userID))
	return consoleIdentity{administrator: session.administrator, Admin: session.privileged, Allowed: allowed}, nil
}

func (a *consoleAuth) verifyPhotoSession(ctx context.Context, session *consoleSession) (photoLibUser, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.tokens.AccessToken == "" {
		return photoLibUser{}, errPhotoLibSession
	}
	if time.Until(session.expiresAt) <= 15*time.Second {
		var refreshed photoLibTokens
		input := map[string]string{"refreshToken": session.tokens.RefreshToken}
		if err := a.photo.call(context.WithoutCancel(ctx), "/mcp/token/refresh", input, "", &refreshed); err != nil {
			return photoLibUser{}, err
		}
		if !validPhotoTokens(refreshed) {
			return photoLibUser{}, errPhotoLibSession
		}
		session.setTokens(refreshed)
	}
	return a.photo.user(ctx, session.tokens.AccessToken)
}

func validPhotoTokens(tokens photoLibTokens) bool {
	return tokens.AccessToken != "" && tokens.RefreshToken != "" && tokens.ExpiresIn > 0
}

func (s *consoleSession) setTokens(tokens photoLibTokens) {
	s.tokens = tokens
	s.expiresAt = time.Now().Add(time.Duration(min(tokens.ExpiresIn, 3600)) * time.Second)
}

func (a *consoleAuth) dropSession(r *http.Request) {
	id := cookieValue(r, sessionCookie)
	a.mu.Lock()
	session := a.sessions[id]
	delete(a.sessions, id)
	a.mu.Unlock()
	if session.photo != nil {
		session.photo.mu.Lock()
		token := session.photo.tokens.RefreshToken
		session.photo.tokens = photoLibTokens{}
		session.photo.mu.Unlock()
		a.revokePhotoToken(context.WithoutCancel(r.Context()), token)
	}
}

func (a *consoleAuth) revokePhotoToken(ctx context.Context, token string) {
	_ = a.photo.call(ctx, "/mcp/token/revoke", map[string]string{"refreshToken": token}, "", nil)
}

func (a *consoleAuth) updateAccess(logger *slog.Logger) func(http.ResponseWriter, *http.Request, administrator) {
	return func(w http.ResponseWriter, r *http.Request, admin administrator) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		action := r.PathValue("action")
		if err != nil || id <= 0 || (action != "grant" && action != "revoke") {
			http.NotFound(w, r)
			return
		}
		if err := a.access.update(id, action == "grant"); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.Error(w, "未找到可管理的 PhotoLib 账号", http.StatusNotFound)
			} else {
				logger.Error("save access change", "user_id", id, "error", err)
				http.Error(w, "权限保存失败", http.StatusInternalServerError)
			}
			return
		}
		logger.Info("Deplyt access updated", "admin_id", admin.ID, "user_id", id, "action", action)
		w.WriteHeader(http.StatusNoContent)
	}
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}
