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

const (
	testToken  = "tok-123"
	testHeader = "X-Gatekeeper-Token"
	testCookie = "gatekeeper_session"
	testHost   = "app.example.test"
)

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

func handlerWith(gate *auth.Gate, waker *fakeWaker, rd *fakeReadiness, tr *fakeToucher) *Handler {
	table := routing.NewTable("test-ns", map[string]routing.Upstream{
		testHost: {Service: "web", Port: 3000},
	})
	return NewHandler(table, gate, waker, rd, tr, "<html>callback</html>", "/_gatekeeper/auth", "/healthz", 2*time.Second, testLogger())
}

func enabledGate(loginURL string) *auth.Gate {
	return auth.NewGate(testToken, testHeader, testCookie, loginURL)
}
func disabledGate() *auth.Gate { return auth.NewGate("", testHeader, testCookie, "") }

func TestHealthIsUnauthenticated(t *testing.T) {
	h := handlerWith(enabledGate(""), &fakeWaker{}, &fakeReadiness{}, &fakeToucher{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/healthz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("health = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAuthCallbackPathServesPageUnauthenticated(t *testing.T) {
	h := handlerWith(enabledGate(""), &fakeWaker{}, &fakeReadiness{}, &fakeToucher{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/_gatekeeper/auth?token=x&next=/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "callback") {
		t.Fatalf("callback = %d %q", rec.Code, rec.Body.String())
	}
}

func TestUnknownHostIs404(t *testing.T) {
	h := handlerWith(disabledGate(), &fakeWaker{}, &fakeReadiness{}, &fakeToucher{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://nope.example.test/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAuthDisabledPassesThrough(t *testing.T) {
	tr := &fakeToucher{}
	h := handlerWith(disabledGate(), &fakeWaker{asleep: false}, &fakeReadiness{}, tr)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil).WithContext(ctx)
	h.ServeHTTP(rec, req)
	// Not gated: it proceeds to touch + proxy (upstream unreachable -> 502).
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusFound {
		t.Fatalf("auth-disabled request should not be gated, got %d", rec.Code)
	}
	if tr.calls != 1 {
		t.Fatalf("Touch calls = %d, want 1", tr.calls)
	}
}

func TestUnauthorizedRedirectsWhenLoginURLSet(t *testing.T) {
	tr := &fakeToucher{}
	h := handlerWith(enabledGate("https://login.example.test"), &fakeWaker{}, &fakeReadiness{}, tr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/dashboard", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://login.example.test?redirect=") {
		t.Fatalf("Location = %q", loc)
	}
	if tr.calls != 0 {
		t.Fatalf("unauthorized request must not record activity, got %d", tr.calls)
	}
}

func TestUnauthorizedIs401WhenNoLoginURL(t *testing.T) {
	h := handlerWith(enabledGate(""), &fakeWaker{}, &fakeReadiness{}, &fakeToucher{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthorizedAwakeTouchesAndDoesNotWake(t *testing.T) {
	waker := &fakeWaker{asleep: false}
	tr := &fakeToucher{}
	h := handlerWith(enabledGate(""), waker, &fakeReadiness{}, tr)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil).WithContext(ctx)
	req.Header.Set(testHeader, testToken)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if tr.calls != 1 || waker.ensureCalls != 0 {
		t.Fatalf("touch=%d ensure=%d, want 1/0", tr.calls, waker.ensureCalls)
	}
}

func TestAuthorizedAsleepWakesBeforeProxy(t *testing.T) {
	waker := &fakeWaker{asleep: true}
	rd := &fakeReadiness{}
	tr := &fakeToucher{}
	h := handlerWith(enabledGate(""), waker, rd, tr)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: testCookie, Value: testToken})
	h.ServeHTTP(httptest.NewRecorder(), req)
	if tr.calls != 1 || waker.ensureCalls != 1 || rd.calls != 1 {
		t.Fatalf("touch=%d ensure=%d ready=%d, want 1/1/1", tr.calls, waker.ensureCalls, rd.calls)
	}
}
