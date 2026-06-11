package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/autonoma-ai/gatekeeper/internal/auth"
	"github.com/autonoma-ai/gatekeeper/internal/routing"
)

const proxyTestToken = "tok-123"

type fakeWaker struct {
	asleep      bool
	ensureCalls int
}

func (f *fakeWaker) Asleep() bool                      { return f.asleep }
func (f *fakeWaker) EnsureAwake(context.Context) error { f.ensureCalls++; return nil }

type fakeReadiness struct{ calls int }

func (f *fakeReadiness) WaitForReady(context.Context, string) error { f.calls++; return nil }

type fakeToucher struct{ calls int }

func (f *fakeToucher) Touch() { f.calls++ }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestHandler(waker *fakeWaker, rd *fakeReadiness, tr *fakeToucher) *Handler {
	table := routing.NewTable("test-ns", map[string]routing.Upstream{
		"app.preview.test": {Service: "web", Port: 3000},
	})
	gate := auth.NewGate(proxyTestToken, "https://app.example.com")
	return NewHandler(table, gate, waker, rd, tr, "<html>auth-page</html>", "/gatekeeper-health", 2*time.Second, testLogger())
}

func TestHandlerHealthIsUnauthenticated(t *testing.T) {
	h := newTestHandler(&fakeWaker{}, &fakeReadiness{}, &fakeToucher{})
	req := httptest.NewRequest(http.MethodGet, "http://app.preview.test/gatekeeper-health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("health = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

func TestHandlerPreviewAuthPageIsUnauthenticated(t *testing.T) {
	h := newTestHandler(&fakeWaker{}, &fakeReadiness{}, &fakeToucher{})
	req := httptest.NewRequest(http.MethodGet, "http://app.preview.test/preview-auth?session=x&next=/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "auth-page") {
		t.Fatalf("preview-auth = %d %q", rec.Code, rec.Body.String())
	}
}

func TestHandlerUnknownHostIs404(t *testing.T) {
	h := newTestHandler(&fakeWaker{}, &fakeReadiness{}, &fakeToucher{})
	req := httptest.NewRequest(http.MethodGet, "http://nope.preview.test/", nil)
	req.AddCookie(&http.Cookie{Name: "pk_session", Value: proxyTestToken})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown host = %d, want 404", rec.Code)
	}
}

func TestHandlerUnauthorizedRedirectsToLogin(t *testing.T) {
	tr := &fakeToucher{}
	h := newTestHandler(&fakeWaker{}, &fakeReadiness{}, tr)
	req := httptest.NewRequest(http.MethodGet, "http://app.preview.test/dashboard", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://app.example.com/preview-auth?redirect=") {
		t.Fatalf("Location = %q", loc)
	}
	if tr.calls != 0 {
		t.Fatalf("unauthorized request must not record activity, got %d touches", tr.calls)
	}
}

func TestHandlerAuthorizedAwakeTouchesAndDoesNotWake(t *testing.T) {
	waker := &fakeWaker{asleep: false}
	rd := &fakeReadiness{}
	tr := &fakeToucher{}
	h := newTestHandler(waker, rd, tr)

	// Short context: the upstream is an unreachable cluster DNS name, so the
	// proxy attempt fails fast rather than hanging the test.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://app.preview.test/", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: "pk_session", Value: proxyTestToken})
	h.ServeHTTP(httptest.NewRecorder(), req)

	if tr.calls != 1 {
		t.Errorf("Touch calls = %d, want 1", tr.calls)
	}
	if waker.ensureCalls != 0 {
		t.Errorf("EnsureAwake calls = %d, want 0 (already awake)", waker.ensureCalls)
	}
}

func TestHandlerAuthorizedAsleepWakesAndWaitsBeforeProxy(t *testing.T) {
	waker := &fakeWaker{asleep: true}
	rd := &fakeReadiness{}
	tr := &fakeToucher{}
	h := newTestHandler(waker, rd, tr)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://app.preview.test/", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: "pk_session", Value: proxyTestToken})
	h.ServeHTTP(httptest.NewRecorder(), req)

	if tr.calls != 1 {
		t.Errorf("Touch calls = %d, want 1", tr.calls)
	}
	if waker.ensureCalls != 1 {
		t.Errorf("EnsureAwake calls = %d, want 1", waker.ensureCalls)
	}
	if rd.calls != 1 {
		t.Errorf("WaitForReady calls = %d, want 1", rd.calls)
	}
}
