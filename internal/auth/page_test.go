package auth

import (
	"strings"
	"testing"
)

func TestPreviewAuthPage(t *testing.T) {
	html := PreviewAuthPage("preview.example.com")

	mustContain := []string{
		"pk_session",
		"domain=.preview.example.com",
		"max-age=86400",
		"secure",
		"samesite=lax",
		"encodeURIComponent",
		"location.replace",
		`p.get("session")`,
		`p.get("next")`,
	}
	for _, frag := range mustContain {
		if !strings.Contains(html, frag) {
			t.Errorf("preview-auth page missing %q\n---\n%s", frag, html)
		}
	}
}
