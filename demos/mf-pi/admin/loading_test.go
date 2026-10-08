package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// actorStatus converts a proto enum name (e.g. "STATUS_SUSPENDED") to its value.
func actorStatus(name string) ateapipb.Actor_Status {
	return ateapipb.Actor_Status(ateapipb.Actor_Status_value[name])
}

// loadingReq builds the request nginx sends to /_mfpi_loading: the original
// URI (X-Original-URI) and, for the bare-origin fallback, the mfpi_user cookie
// (X-Mfpi-User).
func loadingReq(originalURI, cookieUser string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/_mfpi_loading", nil)
	req.Header.Set("Accept", "text/html")
	if originalURI != "" {
		req.Header.Set("X-Original-URI", originalURI)
	}
	if cookieUser != "" {
		req.Header.Set("X-Mfpi-User", cookieUser)
	}
	return req
}

func doLoading(s *server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handleLoadingPage(rec, req)
	return rec
}

func TestLoadingPageSuspendedActor(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	rec := doLoading(s, loadingReq("/alice/", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "正在唤醒") {
		t.Errorf("body does not say the agent is waking:\n%s", body)
	}
	// The username is injected into a JS context; html/template must quote it.
	if !strings.Contains(body, `var user = "alice";`) {
		t.Errorf("username not rendered as a JS string literal")
	}
	if !strings.Contains(body, `href="/alice/"`) {
		t.Errorf("retry link does not point back at the user's page")
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("page does not auto-refresh")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestLoadingPageResumingActor(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RESUMING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	rec := doLoading(s, loadingReq("/alice/", ""))
	if body := rec.Body.String(); !strings.Contains(body, "正在唤醒") {
		t.Errorf("RESUMING actor should say waking; body:\n%s", body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("resuming page should auto-refresh")
	}
}

func TestLoadingPageRunningActor(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	rec := doLoading(s, loadingReq("/alice/", ""))
	if body := rec.Body.String(); !strings.Contains(body, "正在启动") {
		t.Errorf("RUNNING actor booting its web server should say starting; body:\n%s", body)
	}
}

func TestLoadingPageMissingActorIsAnError(t *testing.T) {
	s := newTestServer(newFake()) // no actor

	rec := doLoading(s, loadingReq("/bob/", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "用户不存在") {
		t.Errorf("missing user should be reported; body:\n%s", body)
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("missing user must not auto-refresh")
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want unset for a missing user", got)
	}
}

func TestLoadingPageResolvesUserFromCookie(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	// Bare-origin fallback: no path user, only the cookie. No original URI
	// either, so the retry link falls back to the canonical entry point.
	rec := doLoading(s, loadingReq("", "alice"))
	body := rec.Body.String()
	if !strings.Contains(body, "正在唤醒") {
		t.Errorf("cookie-resolved actor should render the loading page; body:\n%s", body)
	}
	if !strings.Contains(body, `href="/alice/"`) {
		t.Errorf("retry link should default to /<user>/; body:\n%s", body)
	}
}

func TestLoadingPageUsesOriginalURIForRetry(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	rec := doLoading(s, loadingReq("/alice/deep/link", ""))
	if body := rec.Body.String(); !strings.Contains(body, `href="/alice/deep/link"`) {
		t.Errorf("retry link should preserve the original deep link; body:\n%s", body)
	}
}

func TestLoadingPageIgnoresInvalidCookieUser(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	// The mfpi_user cookie is client-controlled; a non-DNS-1123 value must be
	// dropped rather than queried or rendered.
	rec := doLoading(s, loadingReq("", `"><script>alert(1)</script>`))
	body := rec.Body.String()
	if strings.Contains(body, "alert(1)") {
		t.Errorf("invalid cookie user was rendered:\n%s", body)
	}
	if !strings.Contains(body, "正在准备你的 Agent") {
		t.Errorf("expected the generic loading copy; body:\n%s", body)
	}
}

func TestLoadingCopyForStatus(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{"STATUS_RESUMING", "正在唤醒"},
		{"STATUS_SUSPENDED", "正在唤醒"},
		{"STATUS_RUNNING", "正在启动"},
		{"STATUS_PAUSED", "正在切换状态"},
		{"STATUS_CRASHED", "正在重启"},
		{"STATUS_UNSPECIFIED", "正在载入"},
	}
	for _, tc := range tests {
		title, message := loadingCopyForStatus(actorStatus(tc.status))
		if !strings.Contains(title, tc.want) {
			t.Errorf("%s: title = %q, want it to contain %q", tc.status, title, tc.want)
		}
		if message == "" {
			t.Errorf("%s: empty message", tc.status)
		}
	}
}
