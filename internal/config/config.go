// Package config loads and validates Gatekeeper's runtime configuration from
// the environment. All configuration is read once at startup; the rest of the
// program receives values, never the environment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/autonoma-ai/gatekeeper/internal/routing"
)

// Config is the fully-validated runtime configuration.
type Config struct {
	Port         int
	Namespace    string
	BypassToken  string
	CookieDomain string
	AppURL       string

	// Routes maps an incoming hostname to the in-cluster Service that serves it.
	Routes map[string]routing.Upstream

	IdleTimeout       time.Duration
	IdleCheckInterval time.Duration
	WakeTimeout       time.Duration

	// SelfAppLabel is the value of the `app` label on Gatekeeper's own
	// Deployment. The scaler uses it to exclude itself from scale-down, since
	// Gatekeeper also carries the managed-by label.
	SelfAppLabel string
	// ManagedLabelSelector selects every previewkit-managed workload.
	ManagedLabelSelector string
	HealthPath           string
	LogLevel             string
}

// Load reads configuration from the environment and validates it. It returns an
// error (rather than panicking) so the entrypoint can log and exit cleanly.
func Load() (*Config, error) {
	cfg := &Config{
		Port:                 intEnv("PORT", 8080),
		Namespace:            os.Getenv("NAMESPACE"),
		BypassToken:          os.Getenv("BYPASS_TOKEN"),
		CookieDomain:         os.Getenv("COOKIE_DOMAIN"),
		AppURL:               strings.TrimRight(os.Getenv("APP_URL"), "/"),
		SelfAppLabel:         stringEnv("SELF_APP_LABEL", "gatekeeper"),
		ManagedLabelSelector: stringEnv("MANAGED_LABEL_SELECTOR", "previewkit.dev/managed-by=previewkit"),
		HealthPath:           stringEnv("HEALTH_PATH", "/gatekeeper-health"),
		LogLevel:             stringEnv("LOG_LEVEL", "info"),
	}

	var err error
	if cfg.IdleTimeout, err = durationEnv("IDLE_TIMEOUT", 30*time.Minute); err != nil {
		return nil, err
	}
	if cfg.IdleCheckInterval, err = durationEnv("IDLE_CHECK_INTERVAL", 30*time.Second); err != nil {
		return nil, err
	}
	if cfg.WakeTimeout, err = durationEnv("WAKE_TIMEOUT", 90*time.Second); err != nil {
		return nil, err
	}

	if cfg.Routes, err = parseRoutes(os.Getenv("ROUTES_JSON")); err != nil {
		return nil, err
	}

	if err = cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	missing := make([]string, 0, 4)
	if c.Namespace == "" {
		missing = append(missing, "NAMESPACE")
	}
	if c.BypassToken == "" {
		missing = append(missing, "BYPASS_TOKEN")
	}
	if c.CookieDomain == "" {
		missing = append(missing, "COOKIE_DOMAIN")
	}
	if c.AppURL == "" {
		missing = append(missing, "APP_URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if len(c.Routes) == 0 {
		return errors.New("ROUTES_JSON must define at least one host -> upstream mapping")
	}
	return nil
}

// parseRoutes parses the ROUTES_JSON map of hostname -> {service, port}.
func parseRoutes(raw string) (map[string]routing.Upstream, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("ROUTES_JSON is required")
	}
	var parsed map[string]routing.Upstream
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("ROUTES_JSON is not valid JSON: %w", err)
	}
	routes := make(map[string]routing.Upstream, len(parsed))
	for host, up := range parsed {
		if up.Service == "" {
			return nil, fmt.Errorf("ROUTES_JSON entry %q has an empty service", host)
		}
		if up.Port <= 0 || up.Port > 65535 {
			return nil, fmt.Errorf("ROUTES_JSON entry %q has an invalid port %d", host, up.Port)
		}
		routes[strings.ToLower(host)] = up
	}
	return routes, nil
}

func stringEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration for %s (want e.g. \"30m\", \"90s\"): %w", key, err)
	}
	return d, nil
}
