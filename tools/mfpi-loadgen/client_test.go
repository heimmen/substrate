// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type authRecorder struct {
	mu         sync.Mutex
	basicSeen  []bool
	requireBsc bool
}

func (a *authRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hasBasic := r.Header.Get("Authorization") != ""
		a.mu.Lock()
		a.basicSeen = append(a.basicSeen, hasBasic)
		require := a.requireBsc
		a.mu.Unlock()

		if hasBasic {
			http.SetCookie(w, &http.Cookie{Name: "mfpi_auth", Value: "ok", Path: "/"})
			w.WriteHeader(http.StatusOK)
			return
		}
		if !require {
			if c, err := r.Cookie("mfpi_auth"); err == nil && c.Value == "ok" {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
}

func (a *authRecorder) seen() []bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]bool(nil), a.basicSeen...)
}

func (a *authRecorder) setRequireBasic(v bool) {
	a.mu.Lock()
	a.requireBsc = v
	a.mu.Unlock()
}

func TestCookieModeSendsBasicOnlyOnce(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c, err := newUserClient(srv.URL, UserCred{Name: "alice", Password: "pw"}, authCookie, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, code, _, err := c.do(ctx, http.MethodGet, "/alice/", nil, "application/json", "", 2*time.Second); err != nil || code != 200 {
			t.Fatalf("request %d: code=%d err=%v", i, code, err)
		}
	}
	got := rec.seen()
	want := []bool{true, false, false}
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("basic headers = %v, want %v", got, want)
		}
	}
}

func TestCookieModeRearmsBasicAfter401(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c, err := newUserClient(srv.URL, UserCred{Name: "alice", Password: "pw"}, authCookie, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, code, _, _ := c.do(ctx, http.MethodGet, "/alice/", nil, "application/json", "", 2*time.Second); code != 200 {
		t.Fatalf("bootstrap code = %d", code)
	}
	rec.setRequireBasic(true)
	if _, code, _, _ := c.do(ctx, http.MethodGet, "/alice/", nil, "application/json", "", 2*time.Second); code != 401 {
		t.Fatalf("expected 401 after cookie invalidation, got %d", code)
	}
	if _, code, _, _ := c.do(ctx, http.MethodGet, "/alice/", nil, "application/json", "", 2*time.Second); code != 200 {
		t.Fatalf("expected recovery via Basic, got %d", code)
	}
	got := rec.seen()
	want := []bool{true, false, true}
	if len(got) != len(want) {
		t.Fatalf("basic headers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("basic headers = %v, want %v", got, want)
		}
	}
}

func TestBasicModeAlwaysSendsBasic(t *testing.T) {
	rec := &authRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	c, err := newUserClient(srv.URL, UserCred{Name: "alice", Password: "pw"}, authBasic, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, code, _, _ := c.do(ctx, http.MethodGet, "/alice/", nil, "application/json", "", 2*time.Second); code != 200 {
			t.Fatalf("request %d code = %d", i, code)
		}
	}
	for i, b := range rec.seen() {
		if !b {
			t.Fatalf("request %d did not send Basic", i)
		}
	}
}

func TestParseThinkRange(t *testing.T) {
	var lo, hi time.Duration
	if err := parseThinkRange("3s-10s", &lo, &hi); err != nil {
		t.Fatal(err)
	}
	if lo != 3*time.Second || hi != 10*time.Second {
		t.Errorf("got %v-%v", lo, hi)
	}
	if err := parseThinkRange("2s", &lo, &hi); err != nil {
		t.Fatal(err)
	}
	if lo != 2*time.Second || hi != 2*time.Second {
		t.Errorf("single value: got %v-%v", lo, hi)
	}
	if err := parseThinkRange("10s-1s", &lo, &hi); err == nil {
		t.Error("expected an error for min > max")
	}
	if err := parseThinkRange("nope", &lo, &hi); err == nil {
		t.Error("expected an error for invalid duration")
	}
}

func TestAuthModeFor(t *testing.T) {
	if authModeFor("basic", 1) != authBasic {
		t.Error("basic mode mismatch")
	}
	if authModeFor("cookie", 1) != authCookie {
		t.Error("cookie mode mismatch")
	}
	if authModeFor("mixed", 0) != authBasic || authModeFor("mixed", 1) != authCookie {
		t.Error("mixed mode should alternate per VU index")
	}
}

func TestBootstrapProbesAPIInsteadOfNavigation(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/alice/":
			// The gateway rewrites a not-ready actor to a 200 loading page for
			// navigations; the readiness probe must not trust this.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html>mfpi-page=loading</html>"))
		case "/alice/api/machines/local/status":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"isStreaming":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := newUserClient(srv.URL, UserCred{Name: "alice", Password: "pw"}, authCookie, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := c.bootstrap(context.Background(), 2*time.Second)
	if err != nil || code != http.StatusOK {
		t.Fatalf("bootstrap = %d, %v", code, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) == 0 || hits[0] != "/alice/api/machines/local/status" {
		t.Fatalf("first probe = %v, want the API readiness probe", hits)
	}
}

func TestBootstrapRetries503(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"isStreaming":false}`))
	}))
	defer srv.Close()

	c, err := newUserClient(srv.URL, UserCred{Name: "alice", Password: "pw"}, authCookie, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := c.bootstrap(context.Background(), 10*time.Second)
	if err != nil || code != http.StatusOK {
		t.Fatalf("bootstrap = %d, %v", code, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts < 3 {
		t.Fatalf("attempts = %d, want >= 3", attempts)
	}
}
