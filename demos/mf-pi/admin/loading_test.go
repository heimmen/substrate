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
	addActorSnapshot(f, "mfpi", "alice") // returning user: suspended after use, has a snapshot
	s := newTestServer(f)

	rec := doLoading(s, loadingReq("/alice/", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "正在唤醒") {
		t.Errorf("body does not say the agent is waking:\n%s", body)
	}
	// Values injected into JS are escaped by html/template. The retry URL comes
	// from X-Original-URI, which is client-controlled, so this is the escaping
	// boundary too.
	if !strings.Contains(body, `var retryURL = "/alice/";`) {
		t.Errorf("retry URL not rendered as a JS string literal")
	}
	if !strings.Contains(body, `href="/alice/"`) {
		t.Errorf("retry link does not point back at the user's page")
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}
	// Retries happen in place, so the elapsed counter cannot be restarted by a
	// page reload; the meta refresh survives only as a no-JS fallback.
	if !strings.Contains(body, "fetch(retryURL") {
		t.Errorf("page does not retry in place")
	}
	if strings.Contains(body, "sessionStorage") {
		t.Errorf("counter must not depend on sessionStorage across page reloads")
	}
	assertRefreshIsNoScriptFallback(t, body)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// assertRefreshIsNoScriptFallback checks that the page's only meta refresh is
// the <noscript> fallback. A page-level meta refresh reloads the document every
// 3s, and a reload is what used to restart the "已等待 N 秒" counter — which is
// exactly the bug this guards against.
func assertRefreshIsNoScriptFallback(t *testing.T, body string) {
	t.Helper()
	noscript := strings.Index(body, "<noscript>")
	refresh := strings.Index(body, `http-equiv="refresh"`)
	if refresh == -1 {
		t.Fatalf("no auto-refresh at all: no-JS users would be stuck on the page")
	}
	if noscript == -1 || refresh < noscript {
		t.Errorf("meta refresh must live inside <noscript> only; a document-level refresh restarts the elapsed counter")
	}
}

// TestLoadingPageCounterIsMonotonic pins the contract behind the visible timer:
// within one wait episode the page must never reload itself, because the counter
// only lives in page memory. Retries are issued with fetch() instead.
func TestLoadingPageCounterIsMonotonic(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	body := doLoading(s, loadingReq("/alice/", "")).Body.String()
	if n := strings.Count(body, "http-equiv="); n != 1 {
		t.Errorf("found %d http-equiv declarations, want exactly 1 (the noscript fallback)", n)
	}
	if !strings.Contains(body, "setInterval(tick, 1000)") {
		t.Errorf("elapsed counter is not ticked every second")
	}
	// The episode start must be the page's own load time, never something
	// restored from storage that could be stale or missing.
	if !strings.Contains(body, "var startedAt = Date.now();") {
		t.Errorf("counter start must be taken from the current page load")
	}
	if strings.Contains(body, "sessionStorage") || strings.Contains(body, "localStorage") {
		t.Errorf("counter must not be carried across page loads")
	}
	for _, want := range []string{"fetch(retryURL", "looksReady", "mfpi-page"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing in-place retry piece %q", want)
		}
	}
}

// TestLoadingPageRetryActionIsUnambiguous pins the wording of the retry control
// and the promise it makes. The old label («立即重试») left it unclear whether
// retrying reconnects (keeping what was already generated), discards it, or
// force-reloads the page; the button now says what it does and a fine print
// states that generated content is kept.
func TestLoadingPageRetryActionIsUnambiguous(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	body := doLoading(s, loadingReq("/alice/", "")).Body.String()

	if strings.Contains(body, "立即重试") {
		t.Errorf("ambiguous label 立即重试 must not come back")
	}
	if !strings.Contains(body, ">重试连接</a>") {
		t.Errorf("retry control should read 重试连接")
	}
	// The button must be an in-place retry, not a page navigation: the JS binds
	// to this id and calls retry() after preventDefault().
	if !strings.Contains(body, `id="retry-now"`) {
		t.Errorf("retry control is missing id=retry-now (no in-place binding)")
	}
	if !strings.Contains(body, "retryNow.addEventListener('click'") {
		t.Errorf("retry control is not wired to an in-place retry")
	}
	if !strings.Contains(body, "已生成的内容会保留") {
		t.Errorf("missing the statement that generated content is kept")
	}
	// It stays usable without JS (plain link to the original URL).
	if !strings.Contains(body, `href="/alice/"`) {
		t.Errorf("retry control lost its no-JS fallback href")
	}
}

func TestLoadingPageResumingActor(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RESUMING", fixedNow.Add(-time.Hour))
	addActorSnapshot(f, "mfpi", "alice") // returning user: waking from a snapshot
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
	addActorSnapshot(f, "mfpi", "alice")
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
		status      string
		hasSnapshot bool
		want        string
	}{
		// Coming back: there is a snapshot to restore, so "waking" is accurate.
		{"STATUS_RESUMING", true, "正在唤醒"},
		{"STATUS_SUSPENDED", true, "正在唤醒"},
		// First ever start: nothing to restore, so do not say "waking".
		{"STATUS_RESUMING", false, "欢迎使用"},
		{"STATUS_SUSPENDED", false, "欢迎使用"},
		{"STATUS_RUNNING", false, "正在启动"},
		{"STATUS_RUNNING", true, "正在启动"},
		{"STATUS_PAUSED", false, "正在切换状态"},
		{"STATUS_CRASHED", false, "正在重启"},
		{"STATUS_UNSPECIFIED", false, "正在载入"},
	}
	for _, tc := range tests {
		title, message := loadingCopyForStatus(actorStatus(tc.status), tc.hasSnapshot)
		if !strings.Contains(title, tc.want) {
			t.Errorf("%s (snapshot=%v): title = %q, want it to contain %q",
				tc.status, tc.hasSnapshot, title, tc.want)
		}
		if message == "" {
			t.Errorf("%s (snapshot=%v): empty message", tc.status, tc.hasSnapshot)
		}
	}
}

// TestLoadingPageFirstUseDoesNotSayWaking is the user-visible regression guard: a
// brand-new actor has no snapshot, so its interstitial must not claim the agent
// is being woken (new users found "I never used this, why is it waking?").
func TestLoadingPageFirstUseDoesNotSayWaking(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "newbie", "STATUS_RESUMING", fixedNow) // no snapshot
	s := newTestServer(f)

	body := doLoading(s, loadingReq("/newbie/", "")).Body.String()
	if strings.Contains(body, "正在唤醒") {
		t.Errorf("first-use page must not say 正在唤醒; body:\n%s", body)
	}
	if !strings.Contains(body, "欢迎使用") {
		t.Errorf("first-use page should welcome the user; body:\n%s", body)
	}
}

// TestLoadingPageReturningUserSaysWaking is the counterpart: a suspended actor
// that does have a snapshot is genuinely being restored.
func TestLoadingPageReturningUserSaysWaking(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "returning", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	addActorSnapshot(f, "mfpi", "returning")
	s := newTestServer(f)

	body := doLoading(s, loadingReq("/returning/", "")).Body.String()
	if !strings.Contains(body, "正在唤醒") {
		t.Errorf("returning user should see 正在唤醒; body:\n%s", body)
	}
}
