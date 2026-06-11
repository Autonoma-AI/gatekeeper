package config

import (
	"testing"
	"time"
)

func TestParseRoutes(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		routes, err := parseRoutes(`{"WEB.preview.test":{"service":"web","port":3000},"api.preview.test":{"service":"api","port":8080}}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Hosts are lowercased.
		if up, ok := routes["web.preview.test"]; !ok || up.Service != "web" || up.Port != 3000 {
			t.Fatalf("web route = %+v ok=%v", up, ok)
		}
	})

	tests := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"invalid json", `{not json}`},
		{"empty service", `{"h":{"service":"","port":80}}`},
		{"zero port", `{"h":{"service":"web","port":0}}`},
		{"port too high", `{"h":{"service":"web","port":70000}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseRoutes(tt.raw); err == nil {
				t.Fatalf("parseRoutes(%q) expected error, got nil", tt.raw)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Run("success with defaults", func(t *testing.T) {
		t.Setenv("NAMESPACE", "preview-acme-pr-7")
		t.Setenv("BYPASS_TOKEN", "tok")
		t.Setenv("COOKIE_DOMAIN", "preview.example.com")
		t.Setenv("APP_URL", "https://app.example.com/")
		t.Setenv("ROUTES_JSON", `{"web.preview.test":{"service":"web","port":3000}}`)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.IdleTimeout != 30*time.Minute {
			t.Errorf("IdleTimeout default = %v, want 30m", cfg.IdleTimeout)
		}
		if cfg.AppURL != "https://app.example.com" {
			t.Errorf("AppURL = %q, want trailing slash trimmed", cfg.AppURL)
		}
		if cfg.Port != 8080 || cfg.SelfAppLabel != "gatekeeper" {
			t.Errorf("unexpected defaults: port=%d selfApp=%q", cfg.Port, cfg.SelfAppLabel)
		}
	})

	t.Run("missing required vars", func(t *testing.T) {
		t.Setenv("NAMESPACE", "")
		t.Setenv("BYPASS_TOKEN", "")
		t.Setenv("COOKIE_DOMAIN", "")
		t.Setenv("APP_URL", "")
		t.Setenv("ROUTES_JSON", `{"web.preview.test":{"service":"web","port":3000}}`)

		if _, err := Load(); err == nil {
			t.Fatal("Load expected error for missing required vars, got nil")
		}
	})

	t.Run("invalid duration", func(t *testing.T) {
		t.Setenv("NAMESPACE", "ns")
		t.Setenv("BYPASS_TOKEN", "tok")
		t.Setenv("COOKIE_DOMAIN", "d")
		t.Setenv("APP_URL", "https://app.example.com")
		t.Setenv("ROUTES_JSON", `{"web.preview.test":{"service":"web","port":3000}}`)
		t.Setenv("IDLE_TIMEOUT", "not-a-duration")

		if _, err := Load(); err == nil {
			t.Fatal("Load expected error for invalid IDLE_TIMEOUT, got nil")
		}
	})
}
