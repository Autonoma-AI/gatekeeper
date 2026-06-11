package auth

import "fmt"

// PreviewAuthPage returns the HTML served at /preview-auth on the preview host.
// It mirrors the page the old nginx proxy served: read ?session and ?next from
// the URL, set the pk_session cookie on the parent preview domain, then redirect
// to ?next. The token is hex so encodeURIComponent is a no-op, but it is kept
// for byte-for-byte parity with the issuer in apps/ui (preview-auth.tsx).
//
// cookieDomain is trusted operator config (COOKIE_DOMAIN), not user input, so it
// is interpolated directly. The cookie is scoped to ".<domain>" so it is sent to
// every app hostname under the preview domain.
func PreviewAuthPage(cookieDomain string) string {
	script := fmt.Sprintf(
		`(function(){var p=new URLSearchParams(location.search);var s=p.get("session");var n=p.get("next")||"/";if(s){document.cookie="pk_session="+encodeURIComponent(s)+"; path=/; domain=.%s; max-age=86400; secure; samesite=lax"}location.replace(n)})()`,
		cookieDomain,
	)
	return "<html><body><script>" + script + "</script></body></html>"
}
