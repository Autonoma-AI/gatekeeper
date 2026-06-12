package auth

import "fmt"

// AuthCallbackPage returns the HTML served at the auth callback path. It reads
// ?token and ?next from the URL, sets the session cookie, then redirects to
// ?next. This is the hand-back point for a redirect-based login: the external
// login authenticates the user and issues the token, then bounces the browser
// here to set the cookie on this host.
//
// cookieName is the cookie to set. cookieDomain is optional: when set the cookie
// is scoped to ".<domain>" (shared across subdomains); when empty it is a
// host-only cookie.
func AuthCallbackPage(cookieName, cookieDomain string) string {
	domainAttr := ""
	if cookieDomain != "" {
		domainAttr = "; domain=." + cookieDomain
	}
	script := fmt.Sprintf(
		`(function(){var p=new URLSearchParams(location.search);var t=p.get("token");var n=p.get("next")||"/";if(t){document.cookie="%s="+encodeURIComponent(t)+"; path=/; max-age=86400; secure; samesite=lax%s"}location.replace(n)})()`,
		cookieName, domainAttr,
	)
	return "<html><body><script>" + script + "</script></body></html>"
}
