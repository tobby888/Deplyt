package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	relay := &relay{
		token:   cfg.token,
		deploy:  cfg.production.deploy,
		logger:  logger,
		maxSize: maxJARSize,
	}
	mux := http.NewServeMux()
	registerWebAssets(mux, &branding{
		origin: cfg.photoLibOrigin,
		client: outboundClient(3 * time.Second),
	})
	if cfg.adminID != 0 {
		access, err := openAccessRegistry(cfg.stateDir, cfg.photoLibOrigin, cfg.adminID)
		if err != nil {
			return err
		}
		photoLib := photoLibClient{origin: cfg.photoLibOrigin, http: outboundClient(8 * time.Second)}
		console := newConsole(photoLib, access, logger)
		if cfg.githubToken != "" {
			console.workflow = &workflowDispatch{endpoint: photoLibAPI, token: cfg.githubToken, client: outboundClient(12 * time.Second)}
		}
		if cfg.ossArtifact != nil {
			console.oss = &ossDeployment{artifact: cfg.ossArtifact, client: outboundClient(uploadTimeout), relay: relay}
		}
		console.register(mux)
	} else {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
			renderPage(w, http.StatusServiceUnavailable, page{
				Title:   "控制台待配置",
				Message: "请设置 PhotoLib 站点和 Deplyt 管理员账号。",
			})
		})
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/deploy/photolib", relay.handleDeploy)
	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           http.NewCrossOriginProtection().Handler(mux),
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
