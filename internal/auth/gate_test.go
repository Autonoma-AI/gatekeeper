package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testToken  = "abc123deadbeef"
	testHeader = "X-Gatekeeper-Token"
	testCookie = "gatekeeper_session"
)

func TestGateDisabledWhenNoToken(t *testing.T) {
	g := NewGate("", testHeader, testCookie, "")
	if g.Enabled() {
		t.Fatal("expected gate disabled with empty token")
	}
	r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	if !g.Authorized(r) {
		t.Fatal("a disabled gate must authorize all requests")
	}
}

func TestGateAuthorized(t *testing.T) {
	g := NewGate(testToken, testHeader, testCookie, "")
	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   bool
	}{
		{"valid header", func(r *http.Request) { r.Header.Set(testHeader, testToken) }, true},
		{"valid cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: testCookie, Value: testToken}) }, true},
		{"wrong header", func(r *http.Request) { r.Header.Set(testHeader, "nope") }, false},
		{"wrong cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: testCookie, Value: "nope"}) }, false},
		{"no creds", func(r *http.Request) {}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
			tt.mutate(r)
			if got := g.Authorized(r); got != tt.want {
				t.Fatalf("Authorized() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGateLoginRedirect(t *testing.T) {
	t.Run("with login url", func(t *testing.T) {
		g := NewGate(testToken, testHeader, testCookie, "https://login.example.com")
		loc, ok := g.LoginRedirect("https", "abc.example.test", "/foo?bar=1")
		if !ok {
			t.Fatal("expected redirect when login URL is set")
		}
		if !strings.HasPrefix(loc, "https://login.example.com?redirect=") {
			t.Fatalf("unexpected location %q", loc)
		}
		if !strings.Contains(loc, "https%3A%2F%2Fabc.example.test%2Ffoo%3Fbar%3D1") {
			t.Fatalf("redirect target not URL-encoded in %q", loc)
		}
	})
	t.Run("without login url", func(t *testing.T) {
		g := NewGate(testToken, testHeader, testCookie, "")
		if _, ok := g.LoginRedirect("https", "h", "/"); ok {
			t.Fatal("expected no redirect when login URL is unset")
		}
	})
}
