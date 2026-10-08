package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// gateReq builds the request nginx sends to /_mfpi_gate for user path
// /<user>/...: it carries the original URI and Basic credentials.
func gateReq(originalURI, user, pass string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/_mfpi_gate", nil)
	if originalURI != "" {
		req.Header.Set("X-Original-URI", originalURI)
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	return req
}

func doGate(s *server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handleGate(rec, req)
	return rec
}

// gateServer returns a test server with alice's password set to "s3cret".
func gateServer(f *fakeControlClient) *server {
	s := newTestServer(f)
	store := newFakePasswordStore()
	hash, _ := hashPassword("s3cret")
	store.Set("alice", hash)
	s.passwords = store
	return s
}

func TestGateAllowsRunningActor(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := gateServer(f)

	rec := doGate(s, gateReq("/alice/", "alice", "s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a RUNNING actor", rec.Code)
	}
	if got := f.startedResumes(); got != 0 {
		t.Errorf("resume started %d times for a RUNNING actor, want 0", got)
	}
}

func TestGateForbidsSuspendedActorAndResumes(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := gateServer(f)

	rec := doGate(s, gateReq("/alice/", "alice", "s3cret"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a SUSPENDED actor", rec.Code)
	}
	// The resume is started in the background; wait for it to be observed.
	waitFor(t, time.Second, func() bool { return f.startedResumes() == 1 })
}

func TestGateForbidsResumingActor(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RESUMING", fixedNow.Add(-time.Hour))
	s := gateServer(f)

	rec := doGate(s, gateReq("/alice/", "alice", "s3cret"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a RESUMING actor", rec.Code)
	}
}

func TestGateForbidsUnknownActor(t *testing.T) {
	f := newFake() // no alice
	// Set a password so auth passes and we reach the status check.
	s := gateServer(f)

	rec := doGate(s, gateReq("/alice/", "alice", "s3cret"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an unknown actor (loading page shows 'not found')", rec.Code)
	}
	if got := f.startedResumes(); got != 0 {
		t.Errorf("resume started %d times for an unknown actor, want 0", got)
	}
}

func TestGateRejectsBadCredentials(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := gateServer(f)

	rec := doGate(s, gateReq("/alice/", "alice", "wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for bad credentials", rec.Code)
	}
}

func TestGateRejectsUserWithoutPassword(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f) // no password stored

	rec := doGate(s, gateReq("/alice/", "alice", "s3cret"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no password assigned)", rec.Code)
	}
}

func TestGateRejectsMissingUser(t *testing.T) {
	f := newFake()
	s := gateServer(f)

	rec := doGate(s, gateReq("", "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no resolvable user)", rec.Code)
	}
}

func TestGateRecordsIdleInput(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := gateServer(f)

	if rec := doGate(s, gateReq("/alice/", "alice", "s3cret")); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if _, ok := s.idle.lastInput("alice"); !ok {
		t.Errorf("successful gate did not record idle input")
	}
}

func TestGateDeduplicatesResumes(t *testing.T) {
	f := newFake()
	f.resumeBlock = make(chan struct{})
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := gateServer(f)

	// Three gate passes (as the loading page's 3s auto-refresh would produce)
	// must start exactly one background resume while the first is in flight.
	for i := 0; i < 3; i++ {
		if rec := doGate(s, gateReq("/alice/", "alice", "s3cret")); rec.Code != http.StatusForbidden {
			t.Fatalf("pass %d: status = %d, want 403", i, rec.Code)
		}
	}
	waitFor(t, time.Second, func() bool { return f.startedResumes() >= 1 })
	if got := f.startedResumes(); got != 1 {
		t.Errorf("resume calls = %d, want 1 (deduplicated)", got)
	}
	close(f.resumeBlock)
}

// waitFor polls cond until it is true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
