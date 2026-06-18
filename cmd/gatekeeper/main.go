// Command gatekeeper is a per-namespace reverse proxy that authenticates preview
// traffic, scales the namespace's workloads to zero when idle, and wakes them on
// demand while holding the triggering request until the backend is ready.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/autonoma-ai/gatekeeper/internal/activity"
	"github.com/autonoma-ai/gatekeeper/internal/auth"
	"github.com/autonoma-ai/gatekeeper/internal/config"
	"github.com/autonoma-ai/gatekeeper/internal/idle"
	"github.com/autonoma-ai/gatekeeper/internal/power"
	"github.com/autonoma-ai/gatekeeper/internal/proxy"
	"github.com/autonoma-ai/gatekeeper/internal/routing"
	"github.com/autonoma-ai/gatekeeper/internal/scaler"
)

const (
	initStateTimeout = 15 * time.Second
	shutdownTimeout  = 20 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("gatekeeper exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)
	log.Info("starting gatekeeper",
		"namespace", cfg.Namespace,
		"port", cfg.Port,
		"authEnabled", cfg.AuthEnabled(),
		"scaleToZero", cfg.ScaleToZeroEnabled(),
		"idleTimeout", cfg.IdleTimeout.String(),
		"wakeTimeout", cfg.WakeTimeout.String(),
		"targetSelector", cfg.TargetSelector,
		"routes", len(cfg.Routes),
	)

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster kube config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}

	// Wire components.
	table := routing.NewTable(cfg.Namespace, cfg.Routes)
	gate := auth.NewGate(cfg.AuthToken, cfg.AuthHeader, cfg.AuthCookie, cfg.LoginURL)
	callbackHTML := auth.AuthCallbackPage(cfg.AuthCookie, cfg.CookieDomain)
	sc := scaler.New(clientset, cfg.Namespace, cfg.TargetSelector, cfg.SelfName, cfg.WakeReplicasAnnotation, log)
	pw := power.New(sc, cfg.WakeTimeout, log)
	tracker := activity.NewTracker()

	// Root context cancelled on SIGTERM/SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Seed awake/asleep state from the cluster. Best-effort: on failure we assume
	// awake and reconcile on the first request / idle tick.
	initCtx, cancelInit := context.WithTimeout(ctx, initStateTimeout)
	if err := pw.Init(initCtx); err != nil {
		log.Warn("could not determine initial power state; assuming awake", "err", err)
	}
	cancelInit()

	handler := proxy.NewHandler(table, gate, pw, sc, tracker, callbackHTML, cfg.AuthCallbackPath, cfg.HealthPath, cfg.WakeTimeout, log)

	loop := idle.New(tracker, pw, cfg.IdleTimeout, cfg.IdleCheckInterval, log)
	go loop.Run(ctx)

	server := &http.Server{
		Addr:    ":" + strconv.Itoa(cfg.Port),
		Handler: handler,
		// No Read/Write timeouts: long-lived proxied responses and websocket
		// upgrades must not be cut off. ReadHeaderTimeout guards header reads only.
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	log.Info("gatekeeper stopped")
	return nil
}

func newLogger(level string) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
