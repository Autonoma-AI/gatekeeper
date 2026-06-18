package config

import (
	"testing"
	"time"
)

func TestParseRoutes(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		routes, err := parseRoutes(`{"WEB.example.test":{"service":"web","port":3000}}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if up, ok := routes["web.example.test"]; !ok || up.Service != "web" || up.Port != 3000 {
			t.Fatalf("web route = %+v ok=%v", up, ok)
		}
	})
	for _, tt := range []struct{ name, raw string }{
		{"empty", ""},
		{"invalid json", `{nope}`},
		{"empty service", `{"h":{"service":"","port":80}}`},
		{"bad port", `{"h":{"service":"web","port":0}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseRoutes(tt.raw); err == nil {
				t.Fatalf("parseRoutes(%q) expected an error", tt.raw)
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

func TestLoadRequiresNamespaceAndRoutes(t *testing.T) {
	t.Run("missing namespace", func(t *testing.T) {
		t.Setenv("NAMESPACE", "")
		t.Setenv("ROUTES_JSON", `{"web.example.test":{"service":"web","port":3000}}`)
		if _, err := Load(); err == nil {
			t.Fatal("expected error for missing NAMESPACE")
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
