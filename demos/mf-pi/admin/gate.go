// Readiness gate for the mf-pi loading interstitial.
//
// Background: when a user opens an actor that is SUSPENDED, the router's
// ext_proc does NOT answer 503 — it blocks (retrying for up to 15s) until the
// resume completes and then proxies the request. A healthy suspended actor
// therefore returns the real page after a few seconds of silence, and nginx
// sees no 5xx to turn into the loading page. The user just watches a blank tab.
//
// To give immediate feedback, nginx runs this gate as its auth_request for
// user-scoped requests. The gate is an auth check plus a readiness check:
//
//	401  missing/invalid credentials (browser shows the Basic Auth prompt)
//	403  credentials fine, but the actor is not RUNNING yet
//	200  actor is RUNNING (nginx proceeds to proxy)
//
// On 403 it also kicks off the resume in the background, so the next pass (the
// loading page auto-refreshes every few seconds) finds the actor up. nginx maps
// the 403 to the loading page for browser navigations and to a raw 503 for
// API/WebSocket clients (see nginx.conf).
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// resumeTriggerTimeout bounds a background resume kicked off by the gate. The
// router allows 15s for its own resume; give the admin a little more so a slow
// Full-snapshot restore still completes server-side.
const resumeTriggerTimeout = 3 * time.Minute

// handleGate is the nginx auth_request target for user-scoped requests. It
// authenticates the user (like handleAuth) and additionally refuses with 403
// while the actor is not RUNNING, starting a resume in the background.
func (s *server) handleGate(w http.ResponseWriter, r *http.Request) {
	username, ok := s.authenticate(w, r)
	if !ok {
		return // authenticate already wrote 401
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	actor, err := s.client.GetActor(ctx, &ateapipb.GetActorRequest{
		Actor: &ateapipb.ObjectRef{Atespace: s.atespace, Name: username},
	})
	switch {
	case err == nil:
		if actor.GetStatus() == ateapipb.Actor_STATUS_RUNNING {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Not ready: wake it up and let the caller show the loading page.
		s.triggerResume(username)
		w.WriteHeader(http.StatusForbidden)
	case status.Code(err) == codes.NotFound:
		// No such actor: the loading page reports "user does not exist".
		w.WriteHeader(http.StatusForbidden)
	default:
		// ateapi is unhappy. Don't wall the user off: let the request through
		// and let the proxy/router sort it out (it may still resume).
		log.Printf("loading-gate: status lookup for %q failed: %v", username, err)
		w.WriteHeader(http.StatusOK)
	}
}

// triggerResume starts a resume for the actor in the background unless one is
// already in flight. It never blocks: the gate must answer immediately so the
// loading page renders without waiting for the resume. Failures are logged and
// retried by a later gate call (the loading page refreshes every few seconds).
func (s *server) triggerResume(name string) {
	s.resumeMu.Lock()
	if s.resuming[name] {
		s.resumeMu.Unlock()
		return
	}
	s.resuming[name] = true
	s.resumeMu.Unlock()

	go func() {
		defer func() {
			s.resumeMu.Lock()
			delete(s.resuming, name)
			s.resumeMu.Unlock()
		}()
		// Detached from the request context: the browser may navigate away or
		// the auto-refresh may fire while the resume is still running, and the
		// resume must not be cancelled with it.
		ctx, cancel := context.WithTimeout(context.Background(), resumeTriggerTimeout)
		defer cancel()
		ref := &ateapipb.ObjectRef{Atespace: s.atespace, Name: name}
		if _, err := s.client.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: ref}); err != nil {
			log.Printf("loading-gate: background resume of %q failed: %v", name, err)
		}
	}()
}
