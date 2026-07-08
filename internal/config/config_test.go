package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseRoutes(t *testing.T) {
	t.Run("valid with defaulted namespace", func(t *testing.T) {
		routes, err := parseRoutes(`{"WEB.example.test":{"service":"web","port":3000}}`, "ns")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		up, ok := routes["web.example.test"]
		if !ok || up.Service != "web" || up.Port != 3000 {
			t.Fatalf("web route = %+v ok=%v", up, ok)
		}
		if up.Namespace != "ns" {
			t.Fatalf("namespace = %q, want defaulted %q", up.Namespace, "ns")
		}
	})
	t.Run("explicit namespace wins over default", func(t *testing.T) {
		routes, err := parseRoutes(`{"h":{"namespace":"other","service":"web","port":80}}`, "ns")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := routes["h"].Namespace; got != "other" {
			t.Fatalf("namespace = %q, want %q", got, "other")
		}
	})
	for _, tt := range []struct{ name, raw, defaultNS string }{
		{"empty", "", "ns"},
		{"invalid json", `{nope}`, "ns"},
		{"empty service", `{"h":{"service":"","port":80}}`, "ns"},
		{"bad port", `{"h":{"service":"web","port":0}}`, "ns"},
		{"no namespace anywhere", `{"h":{"service":"web","port":80}}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseRoutes(tt.raw, tt.defaultNS); err == nil {
				t.Fatalf("parseRoutes(%q, %q) expected an error", tt.raw, tt.defaultNS)
			}
		})
	}
}

func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NAMESPACE", "ns")
	t.Setenv("ROUTES_JSON", `{"web.example.test":{"service":"web","port":3000}}`)
}

func TestLoadDefaults(t *testing.T) {
	setMinimalEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthEnabled() {
		t.Error("auth should be disabled when AUTH_TOKEN is empty")
	}
	if cfg.AuthHeader != "X-Gatekeeper-Token" || cfg.AuthCookie != "gatekeeper_session" {
		t.Errorf("auth header/cookie defaults = %q / %q", cfg.AuthHeader, cfg.AuthCookie)
	}
	if cfg.TargetSelector != "gatekeeper.dev/scale-to-zero=true" {
		t.Errorf("TargetSelector default = %q", cfg.TargetSelector)
	}
	if cfg.SelfName != "gatekeeper" || cfg.WakeReplicasAnnotation != "gatekeeper.dev/wake-replicas" {
		t.Errorf("self/annotation defaults = %q / %q", cfg.SelfName, cfg.WakeReplicasAnnotation)
	}
	if cfg.DependsOnAnnotation != "gatekeeper.dev/depends-on" {
		t.Errorf("DependsOnAnnotation default = %q", cfg.DependsOnAnnotation)
	}
	if cfg.HealthPath != "/healthz" || cfg.AuthCallbackPath != "/_gatekeeper/auth" {
		t.Errorf("path defaults = %q / %q", cfg.HealthPath, cfg.AuthCallbackPath)
	}
	if cfg.IdleTimeout != 30*time.Minute || cfg.Port != 8080 {
		t.Errorf("idle/port defaults = %v / %d", cfg.IdleTimeout, cfg.Port)
	}
	if cfg.WakeTimeout != 5*time.Minute {
		t.Errorf("WakeTimeout default = %v, want 5m", cfg.WakeTimeout)
	}
}

func TestLoadNotFoundPage(t *testing.T) {
	// notFoundPagePath is a fixed convention, not an env var; point it at a
	// temp file for the duration of each subtest and restore it after.
	withNotFoundPagePath := func(t *testing.T, path string) {
		t.Helper()
		orig := notFoundPagePath
		notFoundPagePath = path
		t.Cleanup(func() { notFoundPagePath = orig })
	}

	t.Run("nothing mounted means built-in default", func(t *testing.T) {
		setMinimalEnv(t)
		withNotFoundPagePath(t, filepath.Join(t.TempDir(), "404.html"))
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.NotFoundHTML != "" {
			t.Errorf("NotFoundHTML = %q, want empty (built-in default)", cfg.NotFoundHTML)
		}
	})

	t.Run("reads file content when present", func(t *testing.T) {
		setMinimalEnv(t)
		path := filepath.Join(t.TempDir(), "404.html")
		if err := os.WriteFile(path, []byte("<html>custom 404</html>"), 0o600); err != nil {
			t.Fatal(err)
		}
		withNotFoundPagePath(t, path)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.NotFoundHTML != "<html>custom 404</html>" {
			t.Errorf("NotFoundHTML = %q", cfg.NotFoundHTML)
		}
	})

	// A file that exists but can't be read (e.g. bad permissions) is a real
	// misconfiguration of whatever is mounted, and must fail startup rather
	// than silently serving the default page.
	t.Run("unreadable existing file is a hard error", func(t *testing.T) {
		setMinimalEnv(t)
		path := filepath.Join(t.TempDir(), "404.html")
		if err := os.WriteFile(path, []byte("x"), 0o000); err != nil {
			t.Fatal(err)
		}
		withNotFoundPagePath(t, path)
		if _, err := Load(); err == nil {
			t.Fatal("Load succeeded with an unreadable notFoundPagePath")
		}
	})
}

func TestLoadAuthEnabled(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("AUTH_TOKEN", "secret")
	t.Setenv("LOGIN_URL", "https://login.example.com/")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AuthEnabled() {
		t.Error("auth should be enabled when AUTH_TOKEN is set")
	}
	if cfg.LoginURL != "https://login.example.com" {
		t.Errorf("LoginURL trailing slash not trimmed: %q", cfg.LoginURL)
	}
}

func TestLoadNamespaceRequirements(t *testing.T) {
	t.Run("route without namespace needs NAMESPACE", func(t *testing.T) {
		t.Setenv("NAMESPACE", "")
		t.Setenv("ROUTES_JSON", `{"web.example.test":{"service":"web","port":3000}}`)
		if _, err := Load(); err == nil {
			t.Fatal("expected error: route has no namespace and NAMESPACE is unset")
		}
	})
	t.Run("per-route namespaces make NAMESPACE optional", func(t *testing.T) {
		t.Setenv("NAMESPACE", "")
		t.Setenv("POD_NAMESPACE", "system")
		t.Setenv("ROUTES_JSON", `{"web.example.test":{"namespace":"ns-a","service":"web","port":3000}}`)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.Routes["web.example.test"].Namespace; got != "ns-a" {
			t.Fatalf("route namespace = %q, want ns-a", got)
		}
	})
	t.Run("without NAMESPACE, POD_NAMESPACE is required", func(t *testing.T) {
		t.Setenv("NAMESPACE", "")
		t.Setenv("POD_NAMESPACE", "")
		t.Setenv("ROUTES_JSON", `{"web.example.test":{"namespace":"ns-a","service":"web","port":3000}}`)
		if _, err := Load(); err == nil {
			t.Fatal("expected error: self-exclusion needs POD_NAMESPACE (or NAMESPACE)")
		}
	})
	t.Run("missing routes", func(t *testing.T) {
		t.Setenv("NAMESPACE", "ns")
		t.Setenv("ROUTES_JSON", "")
		if _, err := Load(); err == nil {
			t.Fatal("expected error for missing ROUTES_JSON")
		}
	})
}

func TestLoadPodNamespace(t *testing.T) {
	t.Run("falls back to NAMESPACE", func(t *testing.T) {
		setMinimalEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.PodNamespace != "ns" {
			t.Fatalf("PodNamespace = %q, want fallback %q", cfg.PodNamespace, "ns")
		}
	})
	t.Run("explicit POD_NAMESPACE wins", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("POD_NAMESPACE", "system")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.PodNamespace != "system" {
			t.Fatalf("PodNamespace = %q, want %q", cfg.PodNamespace, "system")
		}
	})
}

func TestScaleToZeroEnabled(t *testing.T) {
	t.Run("default idle timeout enables scale-to-zero", func(t *testing.T) {
		setMinimalEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.ScaleToZeroEnabled() {
			t.Error("scale-to-zero should be enabled with the default IDLE_TIMEOUT")
		}
	})
	t.Run("zero idle timeout disables scale-to-zero", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("IDLE_TIMEOUT", "0")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.IdleTimeout != 0 {
			t.Errorf("IdleTimeout = %v, want 0", cfg.IdleTimeout)
		}
		if cfg.ScaleToZeroEnabled() {
			t.Error("scale-to-zero should be disabled when IDLE_TIMEOUT is 0")
		}
	})
}

func TestLoadInvalidDuration(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("IDLE_TIMEOUT", "not-a-duration")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid IDLE_TIMEOUT")
	}
}

func TestLeaderElectionConfig(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		setMinimalEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.LeaderElection {
			t.Error("LeaderElection should default to false")
		}
		if cfg.LeaseName != "gatekeeper" || cfg.ReadyPath != "/readyz" {
			t.Errorf("lease/ready defaults = %q / %q", cfg.LeaseName, cfg.ReadyPath)
		}
	})
	t.Run("enabled requires POD_NAME", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("LEADER_ELECTION", "true")
		if _, err := Load(); err == nil {
			t.Fatal("expected error: LEADER_ELECTION without POD_NAME")
		}
	})
	t.Run("enabled with POD_NAME loads", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("LEADER_ELECTION", "true")
		t.Setenv("POD_NAME", "gatekeeper-abc12")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.LeaderElection || cfg.PodName != "gatekeeper-abc12" {
			t.Errorf("LeaderElection=%v PodName=%q", cfg.LeaderElection, cfg.PodName)
		}
	})
	t.Run("invalid boolean errors", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("LEADER_ELECTION", "yes-please")
		if _, err := Load(); err == nil {
			t.Fatal("expected error for invalid LEADER_ELECTION")
		}
	})
	t.Run("READY_PATH must differ from HEALTH_PATH", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("READY_PATH", "/healthz")
		if _, err := Load(); err == nil {
			t.Fatal("expected error: READY_PATH colliding with HEALTH_PATH")
		}
	})
}

func TestDiscoveryConfig(t *testing.T) {
	t.Run("selector enables discovery without routes", func(t *testing.T) {
		t.Setenv("POD_NAMESPACE", "system")
		t.Setenv("NAMESPACE_SELECTOR", "gatekeeper.dev/managed=true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.DiscoveryEnabled() || len(cfg.Routes) != 0 {
			t.Fatalf("DiscoveryEnabled=%v routes=%d", cfg.DiscoveryEnabled(), len(cfg.Routes))
		}
		if cfg.RoutesAnnotation != "gatekeeper.dev/routes" || cfg.IdleTimeoutAnnotation != "gatekeeper.dev/idle-timeout" {
			t.Errorf("annotation defaults = %q / %q", cfg.RoutesAnnotation, cfg.IdleTimeoutAnnotation)
		}
	})
	t.Run("selector and ROUTES_JSON are mutually exclusive", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("NAMESPACE_SELECTOR", "gatekeeper.dev/managed=true")
		if _, err := Load(); err == nil {
			t.Fatal("expected error: NAMESPACE_SELECTOR with ROUTES_JSON")
		}
	})
	t.Run("discovery requires a positive check interval even with IDLE_TIMEOUT=0", func(t *testing.T) {
		t.Setenv("POD_NAMESPACE", "system")
		t.Setenv("NAMESPACE_SELECTOR", "gatekeeper.dev/managed=true")
		t.Setenv("IDLE_TIMEOUT", "0")
		t.Setenv("IDLE_CHECK_INTERVAL", "0")
		if _, err := Load(); err == nil {
			t.Fatal("expected error: per-namespace overrides can re-enable sleep, so the loop must tick")
		}
	})
}

func TestIdleCheckInterval(t *testing.T) {
	t.Run("zero interval with scale-to-zero enabled is a config error", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("IDLE_CHECK_INTERVAL", "0")
		if _, err := Load(); err == nil {
			t.Fatal("expected error: the idle loop cannot tick on a zero interval")
		}
	})
	t.Run("zero interval is fine when scale-to-zero is disabled", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv("IDLE_TIMEOUT", "0")
		t.Setenv("IDLE_CHECK_INTERVAL", "0")
		if _, err := Load(); err != nil {
			t.Fatalf("Load: %v (IDLE_TIMEOUT=0 deployments may zero the interval too)", err)
		}
	})
}
