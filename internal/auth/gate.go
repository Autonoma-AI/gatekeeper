// Package auth implements Gatekeeper's optional request authentication: a shared
// token presented via a configurable header or cookie. When no token is
// configured the gate is disabled and every request passes. Unauthenticated
// requests are either redirected to a configured login URL or rejected with 401.
package auth

import (
	"crypto/subtle"
	"net/http"
	"net/url"
)

// Gate authorizes requests against a single shared token.
type Gate struct {
	token    string
	header   string
	cookie   string
	loginURL string
}

// NewGate builds a Gate. An empty token disables authentication. header and
// cookie name where the token is read from; loginURL (optional) is where
// unauthenticated browsers are redirected.
func NewGate(token, header, cookie, loginURL string) *Gate {
	return &Gate{token: token, header: header, cookie: cookie, loginURL: loginURL}
}

// Enabled reports whether authentication is active (a token is configured).
func (g *Gate) Enabled() bool { return g.token != "" }

// Authorized reports whether the request may proceed. Always true when auth is
// disabled; otherwise the token must match the configured header or cookie.
func (g *Gate) Authorized(r *http.Request) bool {
	if !g.Enabled() {
		return true
	}
	if g.tokenMatches(r.Header.Get(g.header)) {
		return true
	}
	if c, err := r.Cookie(g.cookie); err == nil && g.tokenMatches(c.Value) {
		return true
	}
	return false
}

func (g *Gate) tokenMatches(candidate string) bool {
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(g.token)) == 1
}

// LoginRedirect returns the URL to send an unauthenticated browser to, and
// whether a login URL is configured at all. requestURI should be the full
// request URI (path + query), e.g. r.URL.RequestURI(). When false is returned,
// the caller should respond 401 instead.
func (g *Gate) LoginRedirect(scheme, host, requestURI string) (string, bool) {
	if g.loginURL == "" {
		return "", false
	}
	target := scheme + "://" + host + requestURI
	return g.loginURL + "?redirect=" + url.QueryEscape(target), true
}
