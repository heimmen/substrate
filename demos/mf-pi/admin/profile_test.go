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
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// fakeProfileStore is an in-memory profileStore for tests.
type fakeProfileStore struct {
	mu          sync.Mutex
	profiles    map[string]fakeProfile
	ensureCalls []string
	putCalls    []string
	errEnsure   error
	errPut      error
	errHas      error
}

type fakeProfile struct {
	data []byte
	mod  time.Time
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

func (s *fakeProfileStore) GetProfile(_ context.Context, name string) (io.ReadCloser, time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[name]
	if !ok || p.data == nil {
		return nil, time.Time{}, false, nil
	}
	return io.NopCloser(bytes.NewReader(p.data)), p.mod, true, nil
}

func (s *fakeProfileStore) PutProfile(_ context.Context, name string, data []byte) (time.Time, error) {
	if s.errPut != nil {
		return time.Time{}, s.errPut
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCalls = append(s.putCalls, name)
	s.profiles[name] = fakeProfile{data: append([]byte(nil), data...), mod: fixedNow.Add(30 * time.Second)}
	return fixedNow.Add(30 * time.Second), nil
}

func (s *fakeProfileStore) HasProfile(_ context.Context, name string) (time.Time, bool, error) {
	if s.errHas != nil {
		return time.Time{}, false, s.errHas
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[name]
	if !ok || p.data == nil {
		return time.Time{}, false, nil
	}
	return p.mod, true, nil
}

func doInternal(s *server, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handleInternalActor(rec, req)
	return rec
}

func TestInternalProfileGate(t *testing.T) {
	// No Authorization header.
	if rec := doInternal(newTestServer(newFake()), http.MethodGet, "/internal/actor/alice/profile", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}
	// Wrong token.
	if rec := doInternal(newTestServer(newFake()), http.MethodGet, "/internal/actor/alice/profile", "wrong-token", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rec.Code)
	}
	// Server without a configured token fails closed.
	s := newTestServer(newFake())
	s.profileToken = ""
	if rec := doInternal(s, http.MethodGet, "/internal/actor/alice/profile", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty server token: status = %d, want 503", rec.Code)
	}
	// Valid token but no MinIO store configured.
	s = newTestServer(newFake())
	s.profiles = nil
	if rec := doInternal(s, http.MethodGet, "/internal/actor/alice/profile", "test-token", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil store: status = %d, want 503", rec.Code)
	}
}

func TestInternalProfilePutThenGetRoundTrip(t *testing.T) {
	s := newTestServer(newFake())
	store := s.profiles.(*fakeProfileStore)
	body := "tar-bytes-profile"

	rec := doInternal(s, http.MethodPut, "/internal/actor/alice/profile", "test-token", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.ensureCalls) != 1 || store.ensureCalls[0] != "alice" {
		t.Errorf("ensureCalls = %v, want [alice]", store.ensureCalls)
	}
	if len(store.putCalls) != 1 || store.putCalls[0] != "alice" {
		t.Errorf("putCalls = %v, want [alice]", store.putCalls)
	}
	resp := decode[struct {
		Bytes int `json:"bytes"`
	}](t, rec)
	if resp.Bytes != len(body) {
		t.Errorf("bytes = %d, want %d", resp.Bytes, len(body))
	}

	rec = doInternal(s, http.MethodGet, "/internal/actor/alice/profile", "test-token", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("GET body = %q, want %q", got, body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
}

func TestInternalProfileGetNoProfile(t *testing.T) {
	s := newTestServer(newFake())
	if rec := doInternal(s, http.MethodGet, "/internal/actor/alice/profile", "test-token", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("GET status = %d, want 204", rec.Code)
	}
}

func TestInternalProfilePutInvalidName(t *testing.T) {
	s := newTestServer(newFake())
	if rec := doInternal(s, http.MethodPut, "/internal/actor/Bad_User/profile", "test-token", "x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid name status = %d, want 400", rec.Code)
	}
	// Path traversal attempt.
	if rec := doInternal(s, http.MethodGet, "/internal/actor/alice/../evi/profile", "test-token", ""); rec.Code == http.StatusOK {
		t.Fatalf("path traversal unexpectedly succeeded")
	}
}

func TestInternalProfilePutTooLarge(t *testing.T) {
	old := maxProfileBytes
	maxProfileBytes = 1024
	defer func() { maxProfileBytes = old }()
	s := newTestServer(newFake())
	rec := doInternal(s, http.MethodPut, "/internal/actor/alice/profile", "test-token", strings.Repeat("x", 1025))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT oversized status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

func TestInternalProfilePutStoreError(t *testing.T) {
	s := newTestServer(newFake())
	store := s.profiles.(*fakeProfileStore)
	store.errPut = errors.New("minio unavailable")
	rec := doInternal(s, http.MethodPut, "/internal/actor/alice/profile", "test-token", "x")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("PUT store error status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
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
	if _, err := store.PutProfile(context.Background(), "alice", []byte("profile")); err != nil {
		t.Fatalf("seeding profile: %v", err)
	}

	if rec := doRequest(s, http.MethodDelete, "/api/users/alice", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Deleting a user must KEEP the MinIO bucket/object so a delete+recreate
	// reset can restore the profile on the next cold boot.
	p, ok := store.profiles["alice"]
	if !ok || string(p.data) != "profile" {
		t.Errorf("profile for alice lost after delete: %+v ok=%v", p, ok)
	}
}

func TestHandleListUsersReportsProfileSynced(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	addActor(f, "mfpi", "bob", "STATUS_SUSPENDED", fixedNow.Add(-2*time.Hour))
	s := newTestServer(f)
	store := s.profiles.(*fakeProfileStore)
	if _, err := store.PutProfile(context.Background(), "bob", []byte("x")); err != nil {
		t.Fatalf("seeding profile: %v", err)
	}

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
