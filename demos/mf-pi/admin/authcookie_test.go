package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedPassword gives the fake store a known password and returns its hash.
func seedPassword(t *testing.T, s *server, user, password string) string {
	t.Helper()
	store := newFakePasswordStore()
	hash, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	store.hashes[user] = hash
	s.passwords = store
	return hash
}

// The cookie is what actually gives us a controllable 720-minute window: Basic
// Auth has no server-side lifetime, the browser drops it on restart/sleep, and
// the user is prompted again. These tests pin the properties that make the
// cookie safe to accept without the password.
func TestAuthCookieRoundTrip(t *testing.T) {
	hash := "sha256$deadbeef"
	now := fixedNow
	exp := now.Add(720 * time.Minute)
	token := authCookieToken("alice", hash, exp)

	if !verifyAuthCookie(token, "alice", hash, now) {
		t.Fatal("a freshly minted cookie must verify")
	}
	// Another user's name must not be replayable, and the username is signed.
	if verifyAuthCookie(token, "bob", hash, now) {
		t.Error("cookie for alice verified as bob")
	}
	// Changing the password must invalidate outstanding cookies.
	if verifyAuthCookie(token, "alice", "sha256$other", now) {
		t.Error("cookie verified after the password hash changed")
	}
	// Expiry is enforced.
	if verifyAuthCookie(token, "alice", hash, exp.Add(time.Second)) {
		t.Error("expired cookie accepted")
	}
	// Tampering with the expiry invalidates the signature.
	tampered := strings.Replace(token, "|", "|1", 1)
	if verifyAuthCookie(tampered, "alice", hash, now) {
		t.Error("tampered cookie accepted")
	}
	for _, bad := range []string{"", "alice", "alice|123", "alice|nonce|x", "alice|99999999999|zz"} {
		if verifyAuthCookie(bad, "alice", hash, now) {
			t.Errorf("malformed cookie %q accepted", bad)
		}
	}
}

func TestAuthenticateAcceptsCookieWithoutBasic(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.authCookieTTL = 720 * time.Minute
	hash := seedPassword(t, s, "alice", "alice-pass")
	token := authCookieToken("alice", hash, fixedNow.Add(720*time.Minute))

	req := loadingReq("/alice/", "")
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: token})
	rec := httptest.NewRecorder()
	user, ok := s.authenticate(rec, req)
	if !ok || user != "alice" {
		t.Fatalf("cookie-only auth = (%q, %v), want (alice, true)", user, ok)
	}
	// No new cookie needed while more than half the lifetime remains.
	if got := rec.Header().Get(authCookieHeader); got != "" {
		t.Errorf("cookie re-issued too early: %q", got)
	}

	// Without the cookie and without Basic it must still refuse.
	rec2 := httptest.NewRecorder()
	if _, ok := s.authenticate(rec2, loadingReq("/alice/", "")); ok {
		t.Error("unauthenticated request accepted")
	}
	if rec2.Code == 0 || rec2.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("expected a 401 challenge, got code=%d header=%q", rec2.Code, rec2.Header().Get("WWW-Authenticate"))
	}
}

func TestAuthenticateIssuesCookieOnBasic(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.authCookieTTL = 720 * time.Minute
	seedPassword(t, s, "alice", "alice-pass")

	req := loadingReq("/alice/", "")
	req.SetBasicAuth("alice", "alice-pass")
	rec := httptest.NewRecorder()
	if _, ok := s.authenticate(rec, req); !ok {
		t.Fatal("basic auth rejected")
	}
	header := rec.Header().Get(authCookieHeader)
	if header == "" {
		t.Fatal("no session cookie header after Basic auth")
	}
	for _, want := range []string{authCookieName + "=", "Max-Age=43200", "HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(header, want) {
			t.Errorf("cookie header %q missing %q", header, want)
		}
	}
}

func TestAuthenticateCookieDisabled(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.authCookieTTL = 0 // MFPI_AUTH_COOKIE_TTL=0
	hash := seedPassword(t, s, "alice", "alice-pass")
	token := authCookieToken("alice", hash, fixedNow.Add(time.Hour))
	req := loadingReq("/alice/", "")
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: token})
	if _, ok := s.authenticate(httptest.NewRecorder(), req); ok {
		t.Error("cookie accepted although the feature is disabled")
	}

	// And Basic still works, without setting a cookie.
	req2 := loadingReq("/alice/", "")
	req2.SetBasicAuth("alice", "alice-pass")
	rec := httptest.NewRecorder()
	if _, ok := s.authenticate(rec, req2); !ok {
		t.Fatal("basic auth rejected with cookies disabled")
	}
	if got := rec.Header().Get(authCookieHeader); got != "" {
		t.Errorf("cookie header set although disabled: %q", got)
	}
}

func TestAuthCookieNameFromEnv(t *testing.T) {
	t.Setenv("AUTH_COOKIE_NAME", "")
	if got := authCookieNameFromEnv(); got != defaultAuthCookieName {
		t.Fatalf("empty AUTH_COOKIE_NAME = %q, want %q", got, defaultAuthCookieName)
	}
	t.Setenv("AUTH_COOKIE_NAME", "mfpi_auth_prod")
	if got := authCookieNameFromEnv(); got != "mfpi_auth_prod" {
		t.Fatalf("AUTH_COOKIE_NAME = %q, want mfpi_auth_prod", got)
	}
}
