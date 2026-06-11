// Package auth implements the preview-environment access gate. A request is
// authorized when it carries the environment's bypass token, either as the
// x-previewkit-bypass header (service-to-service callers) or the pk_session
// cookie (browsers). Unauthorized browser requests are redirected to the main
// app's /preview-auth page, which issues the token after checking org
// membership and bounces back to set the cookie.
package auth

import (
	"crypto/subtle"
	"net/http"
	"net/url"
)

const (
	bypassHeader  = "X-Previewkit-Bypass"
	sessionCookie = "pk_session"
)

// Gate authorizes requests against a single environment's bypass token.
type Gate struct {
	bypassToken string
	appURL      string
}

// NewGate builds a Gate. bypassToken is the plaintext per-environment token;
// appURL is the main app origin used to build the login redirect.
func NewGate(bypassToken, appURL string) *Gate {
	return &Gate{bypassToken: bypassToken, appURL: appURL}
}

// Authorized reports whether the request carries the bypass token via the
// x-previewkit-bypass header or the pk_session cookie.
func (g *Gate) Authorized(r *http.Request) bool {
	if g.tokenMatches(r.Header.Get(bypassHeader)) {
		return true
	}
	if c, err := r.Cookie(sessionCookie); err == nil && g.tokenMatches(c.Value) {
		return true
	}
	return false
}

func (g *Gate) tokenMatches(candidate string) bool {
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(g.bypassToken)) == 1
}

// RedirectLocation builds the URL an unauthenticated browser is sent to. The
// main app authenticates the user, checks org membership, issues the token, and
// redirects back to /preview-auth on this host to set the cookie. requestURI
// should be the full request URI (path + query), e.g. r.URL.RequestURI().
func (g *Gate) RedirectLocation(host, requestURI string) string {
	target := "https://" + host + requestURI
	return g.appURL + "/preview-auth?redirect=" + url.QueryEscape(target)
}
