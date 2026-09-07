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
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// fakeProfileStore is an in-memory profileStore for tests. It only implements
// the read-only badge surface (EnsureBucket + HasProfile) — the compile-time
// narrowing proves the server has no profile push/pull broker anymore.
type fakeProfileStore struct {
	mu          sync.Mutex
	profiles    map[string]fakeProfile
	ensureCalls []string
	errEnsure   error
	errHas      error
}

type fakeProfile struct {
	mod time.Time
}

func newFakeProfileStore() *fakeProfileStore {
	return &fakeProfileStore{profiles: map[string]fakeProfile{}}
}

func (s *fakeProfileStore) EnsureBucket(_ context.Context, name string) error {
	if s.errEnsure != nil {
		return s.errEnsure
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCalls = append(s.ensureCalls, name)
	if _, ok := s.profiles[name]; !ok {
		s.profiles[name] = fakeProfile{}
	}
	return nil
}

func (s *fakeProfileStore) HasProfile(_ context.Context, name string) (time.Time, bool, error) {
	if s.errHas != nil {
		return time.Time{}, false, s.errHas
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[name]
	if !ok || p.mod.IsZero() {
		return time.Time{}, false, nil
	}
	return p.mod, true, nil
}

// seedProfile marks a user as having a synced profile at a fixed time.
func (s *fakeProfileStore) seedProfile(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles[name] = fakeProfile{mod: fixedNow.Add(30 * time.Second)}
}

func TestHandleCreateUserEnsuresProfileBucket(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	s := newTestServer(f)
	store := s.profiles.(*fakeProfileStore)

	if rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`); rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.ensureCalls) != 1 || store.ensureCalls[0] != "alice" {
		t.Errorf("ensureCalls = %v, want [alice]", store.ensureCalls)
	}
}

func TestHandleCreateUserBucketFailureDoesNotFailCreate(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	s := newTestServer(f)
	s.profiles.(*fakeProfileStore).errEnsure = errors.New("minio down")
	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200 despite bucket failure; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := f.actors["mfpi/alice"]; !ok {
		t.Errorf("actor mfpi/alice not created despite bucket failure")
	}
	resp := decode[map[string]any](t, rec)
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "创建成功") {
		t.Errorf("message = %q, want to mention 创建成功", msg)
	}
}

func TestHandleDeleteUserKeepsProfile(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.profiles.(*fakeProfileStore)
	store.seedProfile("alice")

	if rec := doRequest(s, http.MethodDelete, "/api/users/alice", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Deleting a user must KEEP the MinIO bucket/object so a delete+recreate
	// reset can restore the profile automatically on the next mount.
	if p, ok := store.profiles["alice"]; !ok || p.mod.IsZero() {
		t.Errorf("profile for alice lost after delete: %+v ok=%v", p, ok)
	}
}

func TestHandleListUsersReportsProfileSynced(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	addActor(f, "mfpi", "bob", "STATUS_SUSPENDED", fixedNow.Add(-2*time.Hour))
	s := newTestServer(f)
	s.profiles.(*fakeProfileStore).seedProfile("bob")

	rec := doRequest(s, http.MethodGet, "/api/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Users []userSummary `json:"users"`
	}](t, rec)
	if len(resp.Users) != 2 {
		t.Fatalf("len(users) = %d, want 2", len(resp.Users))
	}
	for _, u := range resp.Users {
		want := u.Name == "bob"
		if u.ProfileSynced != want {
			t.Errorf("user %s profileSynced = %v, want %v", u.Name, u.ProfileSynced, want)
		}
		if u.Name == "bob" {
			wantTime := fixedNow.Add(30 * time.Second).UTC().Format(time.RFC3339)
			if u.ProfileLastSync != wantTime {
				t.Errorf("bob profileLastSync = %q, want %q", u.ProfileLastSync, wantTime)
			}
		}
	}
}

func TestHandleListUsersProfileStoreErrorDegrades(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.profiles.(*fakeProfileStore).errHas = errors.New("minio down")

	rec := doRequest(s, http.MethodGet, "/api/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 despite MinIO error; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Users []userSummary `json:"users"`
	}](t, rec)
	if len(resp.Users) != 1 || resp.Users[0].ProfileSynced {
		t.Errorf("profileSynced should degrade to false on MinIO error: %+v", resp.Users)
	}
}
