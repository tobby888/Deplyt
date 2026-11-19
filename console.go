package main

import (
	"context"
	"crypto/rand"
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
	sessionCookie = "__Host-deplyt-session"
	pairingTTL    = 10 * time.Minute
	sessionTTL    = 8 * time.Hour
)

type pairing struct {
	mu       sync.Mutex
	deadline time.Time
	data     photoLibPairing
	pollAt   time.Time
}

type consoleSession struct {
	mu        sync.Mutex
	deadline  time.Time
	expiresAt time.Time
	userID    int64
	tokens    photoLibTokens
}

type console struct {
	photo    photoLibClient
	access   *accessRegistry
	logger   *slog.Logger
	workflow *workflowDispatch
	oss      *ossDeployment
	mu       sync.Mutex
	pairing  map[string]*pairing
	session  map[string]*consoleSession
}

func newConsole(photo photoLibClient, access *accessRegistry, logger *slog.Logger) *console {
	return &console{
		photo: photo, access: access, logger: logger,
		pairing: make(map[string]*pairing),
		session: make(map[string]*consoleSession),
	}
}

func (c *console) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", c.home)
	mux.HandleFunc("GET /login", c.login)
	mux.HandleFunc("POST /login/start", c.startLogin)
	mux.HandleFunc("POST /login/finish", c.finishLogin)
	mux.HandleFunc("POST /logout", c.logout)
	mux.HandleFunc("POST /access/{id}/{action}", c.updateAccess)
	mux.HandleFunc("POST /deploy/workflow", c.deployWorkflow)
	mux.HandleFunc("POST /deploy/oss", c.deployOSS)
}

func (c *console) home(w http.ResponseWriter, r *http.Request) {
	user, ok := c.authenticate(w, r)
	if !ok {
		return
	}
	data := page{Title: "PhotoLib 部署", User: &user, Admin: user.ID == c.access.admin}
	if !c.access.allowed(user.ID) {
		data.Title = "等待授权"
		data.Message = "已识别你的 PhotoLib 账号。管理员可以在 Deplyt 授予访问权限。获准后刷新此页。"
		renderPage(w, http.StatusForbidden, data)
		return
	}
	if data.Admin {
		data.Accounts = c.access.list()
	}
	data.Deployment = true
	data.WorkflowEnabled = c.workflow != nil
	if c.oss != nil {
		data.OSSArtifact = c.oss.artifact.name
	}
	renderPage(w, http.StatusOK, data)
}

func (c *console) login(w http.ResponseWriter, r *http.Request) {
	pending := c.findPairing(cookieValue(r, pairingCookie))
	if pending == nil {
		renderPage(w, http.StatusOK, page{Title: "登录 Deplyt", Login: true})
		return
	}
	pending.mu.Lock()
	defer pending.mu.Unlock()
	renderPage(w, http.StatusOK, c.pairingPage(pending.data, ""))
}

func (c *console) pairingPage(data photoLibPairing, message string) page {
	return page{
		Title: "在 PhotoLib 批准登录", Message: message,
		PairingURL: c.photo.authorizationURL(data.RequestID), PairingCode: data.UserCode,
	}
}

func (c *console) startLogin(w http.ResponseWriter, r *http.Request) {
	if c.findPairing(cookieValue(r, pairingCookie)) != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	id := rand.Text()
	pending := &pairing{deadline: time.Now().Add(pairingTTL)}
	pending.mu.Lock()
	defer pending.mu.Unlock()
	c.mu.Lock()
	for key, p := range c.pairing {
		if !time.Now().Before(p.deadline) {
			delete(c.pairing, key)
		}
	}
	if len(c.pairing) >= 256 {
		c.mu.Unlock()
		renderPage(w, http.StatusTooManyRequests, page{Title: "请稍后登录", Login: true, Message: "当前待批准的登录较多。"})
		return
	}
	c.pairing[id] = pending
	c.mu.Unlock()

	input := map[string]string{"clientName": "Deplyt", "deviceLabel": "自动化部署平台"}
	err := c.photo.call(r.Context(), "/mcp/authorizations", input, "", &pending.data)
	if err != nil || pending.data.RequestID == "" || pending.data.UserCode == "" ||
		pending.data.DeviceCode == "" || pending.data.ExpiresIn <= 0 {
		c.dropPairing(id)
		renderPage(w, http.StatusBadGateway, page{Title: "暂时无法登录", Login: true, Message: "PhotoLib 未能创建配对，请稍后重试。"})
		return
	}
	maxAge := min(pending.data.ExpiresIn, int64(pairingTTL/time.Second))
	setCookie(w, pairingCookie, id, int(maxAge))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (c *console) finishLogin(w http.ResponseWriter, r *http.Request) {
	id := cookieValue(r, pairingCookie)
	pending := c.findPairing(id)
	if pending == nil {
		setCookie(w, pairingCookie, "", -1)
		renderPage(w, http.StatusUnauthorized, page{Title: "请重新登录", Login: true})
		return
	}
	pending.mu.Lock()
	defer pending.mu.Unlock()
	if pending.data.DeviceCode == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if time.Now().Before(pending.pollAt) {
		renderPage(w, http.StatusTooManyRequests, c.pairingPage(pending.data, "请稍候再检查批准结果。"))
		return
	}
	pending.pollAt = time.Now().Add(time.Duration(max(2, min(30, pending.data.Interval))) * time.Second)
	input := map[string]string{"requestId": pending.data.RequestID, "deviceCode": pending.data.DeviceCode}
	var tokens photoLibTokens
	ctx := context.WithoutCancel(r.Context())
	err := c.photo.call(ctx, "/mcp/token", input, "", &tokens)
	if err == nil && tokens.Status == "PENDING" {
		renderPage(w, http.StatusAccepted, c.pairingPage(pending.data, "请在 PhotoLib 输入配对码并批准后，再检查结果。"))
		return
	}
	if err != nil && !errors.Is(err, errPhotoLibSession) {
		renderPage(w, http.StatusBadGateway, c.pairingPage(pending.data, "读取 PhotoLib 批准结果失败，请稍后重试。"))
		return
	}
	c.dropPairing(id)
	pending.data.DeviceCode = ""
	setCookie(w, pairingCookie, "", -1)
	if err != nil || tokens.Status != "APPROVED" || !validTokens(tokens) {
		renderPage(w, http.StatusUnauthorized, page{Title: "请重新登录", Login: true, Message: "配对已失效或被拒绝。"})
		return
	}
	user, err := c.photo.user(ctx, tokens.AccessToken)
	if err != nil {
		c.revokePhotoToken(ctx, tokens.RefreshToken)
		renderPage(w, http.StatusUnauthorized, page{Title: "无法验证 PhotoLib 账号", Login: true, Message: "请确认你的 PhotoLib 账号可用后重新登录。"})
		return
	}
	if err := c.access.register(user); err != nil {
		c.revokePhotoToken(ctx, tokens.RefreshToken)
		c.logger.Error("save verified account", "user_id", user.ID, "error", err)
		renderPage(w, http.StatusInternalServerError, page{Title: "无法保存账号", Login: true})
		return
	}
	sessionID := c.openSession(user.ID, tokens)
	if sessionID == "" {
		c.revokePhotoToken(ctx, tokens.RefreshToken)
		renderPage(w, http.StatusServiceUnavailable, page{Title: "请稍后登录", Login: true})
		return
	}
	c.closeSession(ctx, cookieValue(r, sessionCookie))
	setCookie(w, sessionCookie, sessionID, int(sessionTTL/time.Second))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (c *console) findPairing(id string) *pairing {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pairing[id]
	if p != nil && time.Now().Before(p.deadline) {
		return p
	}
	return nil
}

func (c *console) dropPairing(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pairing, id)
}

func (c *console) openSession(userID int64, tokens photoLibTokens) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for id, session := range c.session {
		if !now.Before(session.deadline) {
			delete(c.session, id)
		}
	}
	if len(c.session) >= 1024 {
		return ""
	}
	id := rand.Text()
	session := &consoleSession{userID: userID, deadline: now.Add(sessionTTL)}
	session.setTokens(tokens)
	c.session[id] = session
	return id
}

func (c *console) authenticate(w http.ResponseWriter, r *http.Request) (photoLibUser, bool) {
	id := cookieValue(r, sessionCookie)
	c.mu.Lock()
	session := c.session[id]
	c.mu.Unlock()
	if session != nil {
		user, err := c.verifySession(r.Context(), session)
		if err == nil {
			return user, true
		}
		if !errors.Is(err, errPhotoLibSession) {
			renderPage(w, http.StatusBadGateway, page{Title: "暂时无法验证账号", Message: "PhotoLib 暂时不可用，请稍后刷新。"})
			return photoLibUser{}, false
		}
		c.closeSession(context.WithoutCancel(r.Context()), id)
	}
	setCookie(w, sessionCookie, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
	return photoLibUser{}, false
}

func (c *console) verifySession(ctx context.Context, session *consoleSession) (photoLibUser, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if !time.Now().Before(session.deadline) || session.tokens.AccessToken == "" {
		return photoLibUser{}, errPhotoLibSession
	}
	if time.Until(session.expiresAt) <= 15*time.Second {
		var refreshed photoLibTokens
		input := map[string]string{"refreshToken": session.tokens.RefreshToken}
		// PhotoLib rotates the refresh token. Retain the replacement if the browser disconnects.
		if err := c.photo.call(context.WithoutCancel(ctx), "/mcp/token/refresh", input, "", &refreshed); err != nil {
			return photoLibUser{}, err
		}
		if !validTokens(refreshed) {
			return photoLibUser{}, errPhotoLibSession
		}
		session.setTokens(refreshed)
	}
	user, err := c.photo.user(ctx, session.tokens.AccessToken)
	if err == nil && user.ID != session.userID {
		return photoLibUser{}, errPhotoLibSession
	}
	return user, err
}

func validTokens(tokens photoLibTokens) bool {
	return tokens.AccessToken != "" && tokens.RefreshToken != "" && tokens.ExpiresIn > 0
}

func (s *consoleSession) setTokens(tokens photoLibTokens) {
	s.tokens = tokens
	s.expiresAt = time.Now().Add(time.Duration(min(tokens.ExpiresIn, 3600)) * time.Second)
}

func (c *console) logout(w http.ResponseWriter, r *http.Request) {
	c.closeSession(context.WithoutCancel(r.Context()), cookieValue(r, sessionCookie))
	setCookie(w, sessionCookie, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (c *console) closeSession(ctx context.Context, id string) {
	c.mu.Lock()
	session := c.session[id]
	delete(c.session, id)
	c.mu.Unlock()
	if session != nil {
		session.mu.Lock()
		refreshToken := session.tokens.RefreshToken
		session.tokens = photoLibTokens{}
		session.mu.Unlock()
		c.revokePhotoToken(ctx, refreshToken)
	}
}

func (c *console) revokePhotoToken(ctx context.Context, token string) {
	if err := c.photo.call(ctx, "/mcp/token/revoke", map[string]string{"refreshToken": token}, "", nil); err != nil {
		c.logger.Warn("could not revoke Deplyt's PhotoLib pairing")
	}
}

func (c *console) updateAccess(w http.ResponseWriter, r *http.Request) {
	user, ok := c.authenticate(w, r)
	if !ok {
		return
	}
	if user.ID != c.access.admin {
		renderPage(w, http.StatusForbidden, page{Title: "需要 Deplyt 管理员权限"})
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	action := r.PathValue("action")
	if err != nil || id <= 0 || (action != "grant" && action != "revoke") {
		http.NotFound(w, r)
		return
	}
	if err := c.access.update(id, action == "grant"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			renderPage(w, http.StatusNotFound, page{Title: "未找到可管理的 PhotoLib 账号"})
		} else {
			c.logger.Error("save access change", "user_id", id, "error", err)
			renderPage(w, http.StatusInternalServerError, page{Title: "权限保存失败"})
		}
		return
	}
	c.logger.Info("Deplyt access updated", "admin_id", user.ID, "user_id", id, "action", action)
	http.Redirect(w, r, "/#access", http.StatusSeeOther)
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, MaxAge: maxAge,
		Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}
