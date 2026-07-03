// Command gatekeeper is a reverse proxy for one or more namespaces that
// authenticates preview traffic, scales each namespace's workloads to zero when
// idle, and wakes them on demand while holding the triggering request until the
// backend is ready.
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
	"github.com/autonoma-ai/gatekeeper/internal/registry"
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
		"defaultNamespace", cfg.Namespace,
		"podNamespace", cfg.PodNamespace,
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
	// The client default (QPS 5) throttles wake readiness polling as soon as a
	// few namespaces wake concurrently; raise it well clear of that.
	restCfg.QPS = 50
	restCfg.Burst = 100
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}

	// Wire components: one Env (scaler, power manager, activity tracker) per
	// managed namespace, built by the registry from the routing table.
	factory := func(ns string) *registry.Env {
		nsLog := log.With("namespace", ns)
		// Gatekeeper must never scale itself, but only its own namespace can
		// contain it: elsewhere a workload merely sharing the name is managed.
		self := ""
		if ns == cfg.PodNamespace {
			self = cfg.SelfName
		}
		sc := scaler.New(clientset, ns, cfg.TargetSelector, self, cfg.WakeReplicasAnnotation, cfg.DependsOnAnnotation, nsLog)
		return &registry.Env{
			Namespace:   ns,
			Power:       power.New(sc, cfg.WakeTimeout, nsLog),
			Readiness:   sc,
			Activity:    activity.NewTracker(),
			IdleTimeout: cfg.IdleTimeout,
		}
	}
	reg := registry.New(factory)
	reg.Rebuild(cfg.Routes)

	gate := auth.NewGate(cfg.AuthToken, cfg.AuthHeader, cfg.AuthCookie, cfg.LoginURL)
	callbackHTML := auth.AuthCallbackPage(cfg.AuthCookie, cfg.CookieDomain)

	// Root context cancelled on SIGTERM/SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Seed each namespace's awake/asleep state from the cluster. Best-effort: on
	// failure we assume awake and reconcile on the first request / idle tick.
	initCtx, cancelInit := context.WithTimeout(ctx, initStateTimeout)
	for _, env := range reg.Envs() {
		if err := env.Power.Init(initCtx); err != nil {
			log.Warn("could not determine initial power state; assuming awake",
				"namespace", env.Namespace, "err", err)
		}
	}
	cancelInit()

	handler := proxy.NewHandler(reg, gate, callbackHTML, cfg.AuthCallbackPath, cfg.HealthPath, cfg.WakeTimeout, log)

	// With scale-to-zero disabled every Env's idle timeout is 0, so the loop
	// could never sleep anything; don't start it (this also keeps the legacy
	// IDLE_TIMEOUT=0 + IDLE_CHECK_INTERVAL=0 config working, as before).
	if cfg.ScaleToZeroEnabled() {
		go idle.New(reg, cfg.IdleCheckInterval, log).Run(ctx)
	} else {
		log.Info("scale-to-zero disabled (idle timeout <= 0); idle loop not started")
	}

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
