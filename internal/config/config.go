// Package config loads and validates Gatekeeper's runtime configuration from
// the environment. Everything is read once at startup; the rest of the program
// receives values, never the environment.
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
	Port int

	// Namespace is the default namespace for ROUTES_JSON entries that don't
	// name one. Optional when every entry carries its own namespace.
	Namespace string
	// PodNamespace is the namespace Gatekeeper itself runs in (inject via the
	// downward API); SELF_NAME is only excluded from scaling there. Falls back
	// to Namespace, which is where legacy single-namespace deployments run;
	// required when Namespace is empty so self-exclusion never silently lapses.
	PodNamespace string

	// Routes maps an incoming hostname to the in-cluster Service that serves
	// it. Every entry's namespace is filled in (explicitly or from Namespace)
	// by the time Load returns.
	Routes map[string]routing.Upstream

	// Auth is OFF when AuthToken is empty: Gatekeeper then acts as a plain
	// scale-to-zero reverse proxy. When set, every request must present the token
	// via AuthHeader or AuthCookie.
	AuthToken  string
	AuthHeader string
	AuthCookie string
	// LoginURL, if set, is where unauthenticated browser requests are redirected
	// (with the original URL appended as ?redirect=). When empty, unauthenticated
	// requests get a 401 instead.
	LoginURL string
	// AuthCallbackPath serves a tiny page that reads ?token=&next=, sets the cookie,
	// and redirects to next - the hand-back point for redirect-based logins.
	AuthCallbackPath string
	// CookieDomain, if set, scopes the cookie to ".<domain>" so it is shared across
	// subdomains; empty means a host-only cookie.
	CookieDomain string

	// IdleTimeout is how long the namespace may be idle before being scaled to
	// zero. Zero (or negative) disables scale-to-zero: see ScaleToZeroEnabled.
	IdleTimeout       time.Duration
	IdleCheckInterval time.Duration
	WakeTimeout       time.Duration

	// TargetSelector is the label selector for workloads Gatekeeper scales. Empty
	// selects every Deployment/StatefulSet in the namespace. SelfName is always
	// excluded so Gatekeeper never scales itself.
	TargetSelector         string
	SelfName               string
	WakeReplicasAnnotation string
	// DependsOnAnnotation is the annotation key whose comma-separated value lists
	// the workloads a workload depends on. Gatekeeper wakes workloads in dependency
	// order (dependencies first), so an app is not started before its database.
	DependsOnAnnotation string

	HealthPath string
	LogLevel   string
}

// AuthEnabled reports whether request authentication is active.
func (c *Config) AuthEnabled() bool { return c.AuthToken != "" }

// ScaleToZeroEnabled reports whether idle scale-to-zero is active. A zero (or
// negative) IDLE_TIMEOUT disables it: the namespace is never auto-slept, though
// requests still wake a namespace that is already asleep.
func (c *Config) ScaleToZeroEnabled() bool { return c.IdleTimeout > 0 }

// Load reads configuration from the environment and validates it. It returns an
// error (rather than panicking) so the entrypoint can log and exit cleanly.
func Load() (*Config, error) {
	cfg := &Config{
		Port:                   intEnv("PORT", 8080),
		Namespace:              os.Getenv("NAMESPACE"),
		AuthToken:              os.Getenv("AUTH_TOKEN"),
		AuthHeader:             stringEnv("AUTH_HEADER", "X-Gatekeeper-Token"),
		AuthCookie:             stringEnv("AUTH_COOKIE", "gatekeeper_session"),
		LoginURL:               strings.TrimRight(os.Getenv("LOGIN_URL"), "/"),
		AuthCallbackPath:       stringEnv("AUTH_CALLBACK_PATH", "/_gatekeeper/auth"),
		CookieDomain:           os.Getenv("COOKIE_DOMAIN"),
		TargetSelector:         stringEnv("TARGET_SELECTOR", "gatekeeper.dev/scale-to-zero=true"),
		SelfName:               stringEnv("SELF_NAME", "gatekeeper"),
		WakeReplicasAnnotation: stringEnv("WAKE_REPLICAS_ANNOTATION", "gatekeeper.dev/wake-replicas"),
		DependsOnAnnotation:    stringEnv("DEPENDS_ON_ANNOTATION", "gatekeeper.dev/depends-on"),
		HealthPath:             stringEnv("HEALTH_PATH", "/healthz"),
		LogLevel:               stringEnv("LOG_LEVEL", "info"),
	}

	var err error
	if cfg.IdleTimeout, err = durationEnv("IDLE_TIMEOUT", 30*time.Minute); err != nil {
		return nil, err
	}
	if cfg.IdleCheckInterval, err = durationEnv("IDLE_CHECK_INTERVAL", 30*time.Second); err != nil {
		return nil, err
	}
	if cfg.WakeTimeout, err = durationEnv("WAKE_TIMEOUT", 5*time.Minute); err != nil {
		return nil, err
	}

	if cfg.PodNamespace = os.Getenv("POD_NAMESPACE"); cfg.PodNamespace == "" {
		cfg.PodNamespace = cfg.Namespace
	}

	if cfg.Routes, err = parseRoutes(os.Getenv("ROUTES_JSON"), cfg.Namespace); err != nil {
		return nil, err
	}

	if err = cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	// Without its own namespace Gatekeeper cannot exclude itself from scaling:
	// a selector matching the Gatekeeper pod would then scale it to zero.
	if c.PodNamespace == "" {
		return errors.New("POD_NAMESPACE is required when NAMESPACE is not set (inject it via the downward API)")
	}
	if len(c.Routes) == 0 {
		return errors.New("ROUTES_JSON must define at least one host -> upstream mapping")
	}
	if c.ScaleToZeroEnabled() && c.IdleCheckInterval <= 0 {
		return errors.New("IDLE_CHECK_INTERVAL must be > 0 when scale-to-zero is enabled (IDLE_TIMEOUT > 0)")
	}
	return nil
}

// parseRoutes parses the ROUTES_JSON map of hostname -> {namespace, service,
// port}. Entries without a namespace get defaultNamespace (the NAMESPACE env),
// so single-namespace configs stay as they always were.
func parseRoutes(raw, defaultNamespace string) (map[string]routing.Upstream, error) {
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
		if up.Namespace == "" {
			if defaultNamespace == "" {
				return nil, fmt.Errorf("ROUTES_JSON entry %q has no namespace and NAMESPACE is not set", host)
			}
			up.Namespace = defaultNamespace
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
