package auth

import (
	"strings"
	"testing"
)

func TestAuthCallbackPageWithDomain(t *testing.T) {
	html := AuthCallbackPage("gatekeeper_session", "example.com")
	for _, frag := range []string{
		`document.cookie="gatekeeper_session="`,
		"domain=.example.com",
		"max-age=86400",
		"secure",
		"samesite=lax",
		"encodeURIComponent",
		"location.replace",
		`p.get("token")`,
		`p.get("next")`,
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("callback page missing %q\n%s", frag, html)
		}
	}
}

func TestAuthCallbackPageHostOnlyWhenNoDomain(t *testing.T) {
	html := AuthCallbackPage("sess", "")
	if strings.Contains(html, "domain=") {
		t.Errorf("expected a host-only cookie (no domain attribute), got:\n%s", html)
	}
}
