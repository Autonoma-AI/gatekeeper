package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Tests the Rewrite logic (target selection, Host preservation, X-Forwarded-Proto)
// against a real upstream, injecting the target the way the handler does.
func TestReverseProxyForwardsRequestAndSetsHeaders(t *testing.T) {
	var gotHost, gotProto, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "hello upstream")
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := newReverseProxy(http.DefaultTransport, testLogger())

	req := httptest.NewRequest(http.MethodGet, "http://app.preview.test/some/path", nil)
	req = req.WithContext(withTarget(req.Context(), target))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello upstream" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "hello upstream")
	}
	if gotProto != "https" {
		t.Errorf("upstream X-Forwarded-Proto = %q, want https", gotProto)
	}
	if gotHost != "app.preview.test" {
		t.Errorf("upstream Host = %q, want app.preview.test (public host preserved)", gotHost)
	}
	if gotPath != "/some/path" {
		t.Errorf("upstream path = %q, want /some/path", gotPath)
	}
}
