package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

const testToken = "abc123deadbeef"

func TestGateAuthorized(t *testing.T) {
	gate := NewGate(testToken, "https://app.example.com")

	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   bool
	}{
		{
			name:   "valid bypass header",
			mutate: func(r *http.Request) { r.Header.Set("X-Previewkit-Bypass", testToken) },
			want:   true,
		},
		{
			name:   "valid session cookie",
			mutate: func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "pk_session", Value: testToken}) },
			want:   true,
		},
		{
			name:   "wrong header token",
			mutate: func(r *http.Request) { r.Header.Set("X-Previewkit-Bypass", "nope") },
			want:   false,
		},
		{
			name:   "wrong cookie token",
			mutate: func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "pk_session", Value: "nope"}) },
			want:   false,
		},
		{
			name:   "no credentials",
			mutate: func(r *http.Request) {},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://x.preview.test/", nil)
			tt.mutate(r)
			if got := gate.Authorized(r); got != tt.want {
				t.Fatalf("Authorized() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGateRedirectLocation(t *testing.T) {
	gate := NewGate(testToken, "https://app.example.com")
	got := gate.RedirectLocation("abc.preview.test", "/foo?bar=1")

	want := "https://app.example.com/preview-auth?redirect=" +
		url.QueryEscape("https://abc.preview.test/foo?bar=1")
	if got != want {
		t.Fatalf("RedirectLocation() =\n  %q\nwant\n  %q", got, want)
	}
}
