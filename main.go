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
	registerWebAssets(mux)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		renderPage(w, http.StatusOK, page{
			Title:   "PhotoLib 部署",
			Message: "通过 Deplyt 将 PhotoLib 发布到生产环境。",
		})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/deploy/photolib", relay.handleDeploy)
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
