package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const (
	maxJARSize      = 512 << 20
	uploadTimeout   = 12 * time.Minute
	deployTimeout   = 45 * time.Minute
	responseTimeout = time.Hour
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("relay stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig(".env")
	if err != nil {
		return err
	}
	auth, err := newConsoleAuth(cfg.values)
	if err != nil {
		return err
	}
	if cfg.adminID == 0 && auth.secret == "" {
		return errors.New("configure DEPLOY_ADMIN_PHOTOLIB_ID or DEPLYT_SSO_SECRET to enable console administration")
	}
	store, err := openHistory(cfg.values)
	if err != nil {
		return err
	}
	auth.access, err = openAccessRegistry(string(store), auth.photoLib, cfg.adminID)
	if err != nil {
		return err
	}
	relay := &relay{
		history: store,
		token:   cfg.token,
		deploy:  cfg.production.deploy,
		logger:  logger,
		maxSize: maxJARSize,
	}
	if cfg.archive.client != nil {
		relay.archive = cfg.archive.upload
		relay.downloadURL = cfg.archive.downloadURL
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/deploy/photolib", relay.handleDeploy)
	console := http.NewServeMux()
	github := newGitHub(cfg.values["GITHUB_TOKEN"])
	dispatch := &workflowDispatch{endpoint: photoLibAPI, token: cfg.values["GITHUB_ACTIONS_TOKEN"], client: outboundClient(12 * time.Second)}
	brand := &branding{origin: auth.photoLib, client: outboundClient(3 * time.Second)}
	console.HandleFunc("GET /brand-icon", brand.serve)
	console.HandleFunc("GET /auth/start", auth.start)
	console.HandleFunc("GET /auth/callback", auth.callback)
	console.HandleFunc("GET /api/login", auth.loginView)
	console.HandleFunc("POST /auth/pairing", auth.startPairing)
	console.HandleFunc("POST /auth/pairing/finish", auth.finishPairing)
	console.HandleFunc("POST /auth/logout", auth.protect(authenticatedAccess, func(w http.ResponseWriter, r *http.Request, user consoleIdentity) {
		auth.logout(w, r, user.administrator)
	}))
	console.HandleFunc("GET /api/session", auth.protect(authenticatedAccess, func(w http.ResponseWriter, _ *http.Request, user consoleIdentity) {
		type sessionView struct {
			consoleIdentity
			Workflow bool   `json:"workflow"`
			Artifact string `json:"artifact,omitempty"`
			Archive  bool   `json:"archive"`
		}
		view := sessionView{consoleIdentity: user}
		if user.Allowed {
			view.Workflow = dispatch.token != ""
			view.Archive = relay.downloadURL != nil
			if cfg.manualOSS != nil {
				view.Artifact = cfg.manualOSS.name
			}
		}
		writeJSON(w, view)
	}))
	console.HandleFunc("GET /api/access", auth.requireAdmin(func(w http.ResponseWriter, _ *http.Request, _ administrator) {
		list := make([]map[string]any, 0)
		for _, account := range auth.access.list() {
			list = append(list, map[string]any{
				"id": strconv.FormatInt(account.ID, 10), "username": account.Username,
				"displayName": account.DisplayName, "granted": account.Granted,
			})
		}
		writeJSON(w, list)
	}))
	console.HandleFunc("POST /api/access/{id}/{action}", auth.requireAdmin(auth.updateAccess(logger)))
	console.HandleFunc("GET /api/deployments", auth.protect(grantedAccess, func(w http.ResponseWriter, r *http.Request, user consoleIdentity) {
		relay.handleHistory(w, r, user.administrator)
	}))
	console.HandleFunc("POST /api/deployments/workflow", auth.protect(grantedAccess, dispatch.handle(logger)))
	console.HandleFunc("POST /api/deployments/oss", auth.protect(grantedAccess, relay.handleOSS(cfg.manualOSS, outboundClient(uploadTimeout))))
	console.HandleFunc("POST /api/deployments/{id}/rollback", auth.requireAdmin(relay.handleRollback))
	console.HandleFunc("GET /api/github/{section}", auth.requireAdmin(github.list))
	console.HandleFunc("GET /api/github/{section}/{id}", auth.requireAdmin(github.detail))
	if err := registerUI(console); err != nil {
		return err
	}
	mux.Handle("/", consoleHeaders(console))
	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       uploadTimeout,
		WriteTimeout:      responseTimeout,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("relay listening", "address", cfg.listenAddr)
		serverErr <- server.ListenAndServe()
	}()
	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		stop()
		shutdown, cancel := context.WithTimeout(context.Background(), uploadTimeout+deployTimeout+time.Minute)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return err
		}
		if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
