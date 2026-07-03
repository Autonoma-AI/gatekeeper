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
	// by the time Load returns. Empty in discovery mode, where routes come
	// from namespace annotations instead.
	Routes map[string]routing.Upstream

	// NamespaceSelector, when set, enables discovery mode: Gatekeeper watches
	// Namespaces matching this label selector and reads each one's routes from
	// RoutesAnnotation. Mutually exclusive with ROUTES_JSON.
	NamespaceSelector string
	// RoutesAnnotation holds a namespace's routes as the same JSON shape as
	// ROUTES_JSON values, except entries must NOT name a namespace - a
	// namespace's annotation can only route into that namespace.
	RoutesAnnotation string
	// IdleTimeoutAnnotation optionally overrides IdleTimeout per namespace
	// (Go duration; <= 0 disables auto-sleep for that namespace).
	IdleTimeoutAnnotation string

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
	// ReadyPath serves readiness, distinct from HealthPath (liveness) so that
	// readiness can be gated (e.g. on discovery cache sync) without ever
	// affecting liveness. Readiness is deliberately NOT leadership-gated: a
	// Deployment whose standby pods are permanently unready cannot complete a
	// rollout, so traffic is steered to the leader by a pod label instead.
	ReadyPath string
	LogLevel  string

	// LeaderElection enables active-passive HA: replicas elect a leader via a
	// Lease named LeaseName in PodNamespace, and only the leader - labeled
	// gatekeeper.dev/role=leader, which the Service selects on - receives
	// traffic, seeds power state, and runs the idle loop. The others stand by.
	LeaderElection bool
	// PodName is this pod's name (inject via the downward API): the election
	// identity and the pod the leader label is applied to.
	PodName   string
	LeaseName string
}

// AuthEnabled reports whether request authentication is active.
func (c *Config) AuthEnabled() bool { return c.AuthToken != "" }

// ScaleToZeroEnabled reports whether idle scale-to-zero is active. A zero (or
// negative) IDLE_TIMEOUT disables it: the namespace is never auto-slept, though
// requests still wake a namespace that is already asleep.
func (c *Config) ScaleToZeroEnabled() bool { return c.IdleTimeout > 0 }

// DiscoveryEnabled reports whether namespaces are discovered by label instead
// of routed statically via ROUTES_JSON.
func (c *Config) DiscoveryEnabled() bool { return c.NamespaceSelector != "" }

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
		ReadyPath:              stringEnv("READY_PATH", "/readyz"),
		LogLevel:               stringEnv("LOG_LEVEL", "info"),
		PodName:                os.Getenv("POD_NAME"),
		LeaseName:              stringEnv("LEASE_NAME", "gatekeeper"),
		NamespaceSelector:      os.Getenv("NAMESPACE_SELECTOR"),
		RoutesAnnotation:       stringEnv("ROUTES_ANNOTATION", "gatekeeper.dev/routes"),
		IdleTimeoutAnnotation:  stringEnv("IDLE_TIMEOUT_ANNOTATION", "gatekeeper.dev/idle-timeout"),
	}

	var err error
	if cfg.LeaderElection, err = boolEnv("LEADER_ELECTION", false); err != nil {
		return nil, err
	}
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

	if rawRoutes := os.Getenv("ROUTES_JSON"); cfg.DiscoveryEnabled() {
		if strings.TrimSpace(rawRoutes) != "" {
			return nil, errors.New("NAMESPACE_SELECTOR and ROUTES_JSON are mutually exclusive: routes come from namespace annotations in discovery mode")
		}
	} else if cfg.Routes, err = parseRoutes(rawRoutes, cfg.Namespace); err != nil {
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
	if !c.DiscoveryEnabled() && len(c.Routes) == 0 {
		return errors.New("ROUTES_JSON must define at least one host -> upstream mapping (or set NAMESPACE_SELECTOR for discovery)")
	}
	// Discovery counts as scale-to-zero-capable regardless of the global
	// IDLE_TIMEOUT: per-namespace annotations can (re-)enable it.
	if (c.ScaleToZeroEnabled() || c.DiscoveryEnabled()) && c.IdleCheckInterval <= 0 {
		return errors.New("IDLE_CHECK_INTERVAL must be > 0 when scale-to-zero is enabled (IDLE_TIMEOUT > 0)")
	}
	if c.ReadyPath == c.HealthPath {
		return errors.New("READY_PATH and HEALTH_PATH must differ (liveness must stay unconditionally OK)")
	}
	if c.LeaderElection && c.PodName == "" {
		return errors.New("POD_NAME is required when LEADER_ELECTION is enabled (inject it via the downward API)")
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

func boolEnv(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid boolean for %s (want e.g. \"true\", \"false\"): %w", key, err)
	}
	return b, nil
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
