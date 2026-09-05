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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeControlClient is an in-memory ateapipb.ControlClient implementation.
type fakeControlClient struct {
	actors           map[string]*ateapipb.Actor
	atespaces        map[string]*ateapipb.Atespace
	created          []*ateapipb.Actor
	resumed          []string
	suspended        []string
	deleted          []string
	atespacesCreated []string
	resumeErr        error
}

func newFake() *fakeControlClient {
	return &fakeControlClient{
		actors:    map[string]*ateapipb.Actor{},
		atespaces: map[string]*ateapipb.Atespace{},
	}
}

func actKey(atespace, name string) string { return atespace + "/" + name }

func (f *fakeControlClient) ListActors(_ context.Context, req *ateapipb.ListActorsRequest, _ ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	var out []*ateapipb.Actor
	for _, a := range f.actors {
		if a.GetMetadata().GetAtespace() == req.GetAtespace() {
			out = append(out, a)
		}
	}
	return &ateapipb.ListActorsResponse{Actors: out}, nil
}

func (f *fakeControlClient) GetActor(_ context.Context, req *ateapipb.GetActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	a, ok := f.actors[actKey(req.GetActor().GetAtespace(), req.GetActor().GetName())]
	if !ok {
		return nil, status.Error(codes.NotFound, "actor not found")
	}
	return a, nil
}

func (f *fakeControlClient) CreateActor(_ context.Context, req *ateapipb.CreateActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	a := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace:   req.GetActor().GetMetadata().GetAtespace(),
			Name:       req.GetActor().GetMetadata().GetName(),
			Version:    1,
			CreateTime: timestamppb.New(fixedNow.Add(-5 * time.Minute)),
			UpdateTime: timestamppb.New(fixedNow),
		},
		ActorTemplateNamespace: req.GetActor().GetActorTemplateNamespace(),
		ActorTemplateName:      req.GetActor().GetActorTemplateName(),
		Status:                 ateapipb.Actor_STATUS_SUSPENDED,
	}
	f.actors[actKey(a.GetMetadata().GetAtespace(), a.GetMetadata().GetName())] = a
	f.created = append(f.created, a)
	return a, nil
}

func (f *fakeControlClient) ResumeActor(_ context.Context, req *ateapipb.ResumeActorRequest, _ ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	a, ok := f.actors[actKey(req.GetActor().GetAtespace(), req.GetActor().GetName())]
	if !ok {
		return nil, status.Error(codes.NotFound, "actor not found")
	}
	a.Status = ateapipb.Actor_STATUS_RUNNING
	f.resumed = append(f.resumed, a.GetMetadata().GetName())
	return &ateapipb.ResumeActorResponse{Actor: a}, nil
}

func (f *fakeControlClient) SuspendActor(_ context.Context, req *ateapipb.SuspendActorRequest, _ ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	a, ok := f.actors[actKey(req.GetActor().GetAtespace(), req.GetActor().GetName())]
	if !ok {
		return nil, status.Error(codes.NotFound, "actor not found")
	}
	a.Status = ateapipb.Actor_STATUS_SUSPENDED
	f.suspended = append(f.suspended, a.GetMetadata().GetName())
	return &ateapipb.SuspendActorResponse{Actor: a}, nil
}

func (f *fakeControlClient) DeleteActor(_ context.Context, req *ateapipb.DeleteActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	k := actKey(req.GetActor().GetAtespace(), req.GetActor().GetName())
	a, ok := f.actors[k]
	if !ok {
		return nil, status.Error(codes.NotFound, "actor not found")
	}
	delete(f.actors, k)
	f.deleted = append(f.deleted, a.GetMetadata().GetName())
	return a, nil
}

func (f *fakeControlClient) GetAtespace(_ context.Context, req *ateapipb.GetAtespaceRequest, _ ...grpc.CallOption) (*ateapipb.Atespace, error) {
	a, ok := f.atespaces[req.GetAtespace().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "atespace not found")
	}
	return a, nil
}

func (f *fakeControlClient) CreateAtespace(_ context.Context, req *ateapipb.CreateAtespaceRequest, _ ...grpc.CallOption) (*ateapipb.Atespace, error) {
	name := req.GetAtespace().GetMetadata().GetName()
	a := &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: name}}
	f.atespaces[name] = a
	f.atespacesCreated = append(f.atespacesCreated, name)
	return a, nil
}

var fixedNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

// fakePasswordStore is an in-memory passwordStore for tests.
type fakePasswordStore struct {
	hashes map[string]string
}

func newFakePasswordStore() *fakePasswordStore {
	return &fakePasswordStore{hashes: map[string]string{}}
}

func (s *fakePasswordStore) Get(name string) (string, bool) {
	h, ok := s.hashes[name]
	return h, ok
}

func (s *fakePasswordStore) Set(name, hash string) error {
	s.hashes[name] = hash
	return nil
}

func (s *fakePasswordStore) Delete(name string) error {
	delete(s.hashes, name)
	return nil
}

// fakeKeyStore is an in-memory keyStore for tests.
type fakeKeyStore struct {
	keys map[string]string
	err  error
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{keys: map[string]string{}}
}

func (s *fakeKeyStore) Get(name string) (string, bool) {
	k, ok := s.keys[name]
	return k, ok
}

func (s *fakeKeyStore) Set(name, key string) error {
	if s.err != nil {
		return s.err
	}
	s.keys[name] = key
	return nil
}

func (s *fakeKeyStore) Delete(name string) error {
	if s.err != nil {
		return s.err
	}
	delete(s.keys, name)
	return nil
}

// fakeActorAuth is an in-memory actorAuthClient for tests. It records the
// set/clear calls per host and can be configured to fail.
type fakeActorAuth struct {
	storedByHost map[string]string // host -> key
	setCalls     []string
	clearCalls   []string
	setErr       error
	clearErr     error
}

func newFakeActorAuth() *fakeActorAuth {
	return &fakeActorAuth{storedByHost: map[string]string{}}
}

func (f *fakeActorAuth) setPersonalKey(_ context.Context, host, apiKey string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, host)
	f.storedByHost[host] = apiKey
	return nil
}

func (f *fakeActorAuth) clearPersonalKey(_ context.Context, host string) error {
	if f.clearErr != nil {
		return f.clearErr
	}
	f.clearCalls = append(f.clearCalls, host)
	delete(f.storedByHost, host)
	return nil
}

func newTestServer(f *fakeControlClient) *server {
	return &server{
		atespace:          "mfpi",
		templateNamespace: "ate-demo-mf-pi",
		templateName:      "mf-pi",
		client:            f,
		passwords:         newFakePasswordStore(),
		keys:              newFakeKeyStore(),
		actors:            newFakeActorAuth(),
		now:               func() time.Time { return fixedNow },
	}
}

func addActor(f *fakeControlClient, atespace, name, status string, createTime time.Time) {
	f.actors[actKey(atespace, name)] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace:   atespace,
			Name:       name,
			Version:    3,
			CreateTime: timestamppb.New(createTime),
		},
		ActorTemplateNamespace: "ate-demo-mf-pi",
		ActorTemplateName:      "mf-pi",
		Status:                 ateapipb.Actor_Status(ateapipb.Actor_Status_value[status]),
	}
}

func doRequest(s *server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	switch {
	case method == http.MethodGet && path == "/api/users":
		s.handleListUsers(rec, req)
	case method == http.MethodPost && path == "/api/users":
		s.handleCreateUser(rec, req)
	case strings.HasPrefix(path, "/api/users/"):
		s.handleUserSubresource(rec, req)
	default:
		http.NotFound(rec, req)
	}
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return v
}

func TestHandleListUsers(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	addActor(f, "mfpi", "bob", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-5*time.Minute))
	addActor(f, "other", "zoe", "STATUS_RUNNING", fixedNow.Add(-time.Hour)) // different atespace

	s := newTestServer(f)
	rec := doRequest(s, http.MethodGet, "/api/users", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decode[struct {
		Atespace string        `json:"atespace"`
		Users    []userSummary `json:"users"`
	}](t, rec)

	if resp.Atespace != "mfpi" {
		t.Errorf("atespace = %q, want mfpi", resp.Atespace)
	}
	if len(resp.Users) != 2 {
		t.Fatalf("len(users) = %d, want 2", len(resp.Users))
	}
	// Sorted by name.
	if resp.Users[0].Name != "alice" || resp.Users[1].Name != "bob" {
		t.Errorf("users order = %s, %s; want alice, bob", resp.Users[0].Name, resp.Users[1].Name)
	}
	if resp.Users[0].Status != "STATUS_RUNNING" {
		t.Errorf("alice status = %q, want STATUS_RUNNING", resp.Users[0].Status)
	}
	if resp.Users[0].Template != "ate-demo-mf-pi/mf-pi" {
		t.Errorf("alice template = %q, want ate-demo-mf-pi/mf-pi", resp.Users[0].Template)
	}
	if resp.Users[1].Status != "STATUS_SUSPENDED" {
		t.Errorf("bob status = %q, want STATUS_SUSPENDED", resp.Users[1].Status)
	}
	if resp.Users[0].AteomPod == "" || resp.Users[0].Age == "" {
		t.Errorf("alice summary missing fields: %+v", resp.Users[0])
	}
}

func TestHandleCreateUserInvalidName(t *testing.T) {
	f := newFake()
	s := newTestServer(f)
	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"Bad_Name"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(f.created) != 0 {
		t.Errorf("actor created for invalid name")
	}
}

func TestHandleCreateUserAutoCreatesAtespace(t *testing.T) {
	f := newFake() // no atespace yet
	s := newTestServer(f)
	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(f.atespacesCreated) != 1 || f.atespacesCreated[0] != "mfpi" {
		t.Errorf("atespacesCreated = %v, want [mfpi]", f.atespacesCreated)
	}
	if _, ok := f.actors["mfpi/alice"]; !ok {
		t.Errorf("actor mfpi/alice was not created")
	}
	if len(f.resumed) != 1 || f.resumed[0] != "alice" {
		t.Errorf("resumed = %v, want [alice]", f.resumed)
	}
	if got := f.actors["mfpi/alice"].GetStatus(); got != ateapipb.Actor_STATUS_RUNNING {
		t.Errorf("alice status = %v, want STATUS_RUNNING", got)
	}
	resp := decode[map[string]any](t, rec)
	if resp["message"] != "创建成功" {
		t.Errorf("message = %v, want 创建成功", resp["message"])
	}
}

func TestHandleCreateUserResumesActor(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	s := newTestServer(f)
	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Message string      `json:"message"`
		User    userSummary `json:"user"`
	}](t, rec)
	if resp.Message != "创建成功" {
		t.Errorf("message = %q, want 创建成功", resp.Message)
	}
	if resp.User.Status != "STATUS_RUNNING" {
		t.Errorf("user status = %q, want STATUS_RUNNING", resp.User.Status)
	}
}

func TestHandleCreateUserResumeFailureStillSucceeds(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	f.resumeErr = status.Error(codes.FailedPrecondition, "no free workers available")
	s := newTestServer(f)
	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := f.actors["mfpi/alice"]; !ok {
		t.Errorf("actor mfpi/alice was not created despite resume failure")
	}
	resp := decode[struct {
		Message string      `json:"message"`
		User    userSummary `json:"user"`
	}](t, rec)
	if !strings.Contains(resp.Message, "立即恢复失败") {
		t.Errorf("message = %q, want to mention resume failure", resp.Message)
	}
	if resp.User.Status != "STATUS_SUSPENDED" {
		t.Errorf("user status = %q, want STATUS_SUSPENDED", resp.User.Status)
	}
}

func TestHandleCreateUserDuplicateReuses(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(f.created) != 0 {
		t.Errorf("duplicate create attempted; created=%d", len(f.created))
	}
	resp := decode[map[string]any](t, rec)
	if resp["message"] != "用户已存在，复用现有会话" {
		t.Errorf("message = %v, want 用户已存在，复用现有会话", resp["message"])
	}
}

func TestHandleDeleteUserMissing(t *testing.T) {
	f := newFake()
	s := newTestServer(f)
	rec := doRequest(s, http.MethodDelete, "/api/users/nobody", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteUserSuspendsThenDeletes(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	rec := doRequest(s, http.MethodDelete, "/api/users/alice", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(f.suspended) != 1 || f.suspended[0] != "alice" {
		t.Errorf("suspended = %v, want [alice]", f.suspended)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "alice" {
		t.Errorf("deleted = %v, want [alice]", f.deleted)
	}
	if _, ok := f.actors["mfpi/alice"]; ok {
		t.Errorf("actor mfpi/alice still present after delete")
	}
}

func TestHandleCreateUserReturnsPassword(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	s := newTestServer(f)
	store := newFakePasswordStore()
	s.passwords = store

	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Password string `json:"password"`
	}](t, rec)
	if resp.Password == "" {
		t.Fatalf("password not returned in create response")
	}
	hash, ok := store.Get("alice")
	if !ok {
		t.Fatalf("password hash not stored for alice")
	}
	if !checkPassword(hash, resp.Password) {
		t.Errorf("stored hash does not match returned password")
	}
}

func TestHandleCreateUserDuplicateNoNewPassword(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := newFakePasswordStore()
	s.passwords = store

	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[map[string]any](t, rec)
	if _, hasPw := resp["password"]; hasPw {
		t.Errorf("unexpected password in duplicate-create response: %v", resp["password"])
	}
	if _, ok := store.Get("alice"); ok {
		t.Errorf("password assigned on duplicate create")
	}
}

func TestHandleResetPassword(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := newFakePasswordStore()
	s.passwords = store

	oldHash, _ := hashPassword("old-secret")
	store.Set("alice", oldHash)

	rec := doRequest(s, http.MethodPost, "/api/users/alice/password", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Password string `json:"password"`
	}](t, rec)
	if resp.Password == "" {
		t.Fatalf("new password not returned")
	}
	hash, ok := store.Get("alice")
	if !ok {
		t.Fatalf("password hash missing after reset")
	}
	if hash == oldHash {
		t.Errorf("hash unchanged after reset")
	}
	if checkPassword(oldHash, resp.Password) {
		t.Errorf("old hash still accepts new password")
	}
	if !checkPassword(hash, resp.Password) {
		t.Errorf("new hash does not accept new password")
	}
}

func authReq(path, cookieUser, user, pass string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/_mfpi_auth", nil)
	if path != "" {
		req.Header.Set("X-Original-URI", path)
	}
	if cookieUser != "" {
		req.Header.Set("X-Mfpi-User", cookieUser)
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	return req
}

func doAuth(s *server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handleAuth(rec, req)
	return rec
}

func TestHandleAuthDeniesUserWithoutPassword(t *testing.T) {
	s := newTestServer(newFake())
	rec := doAuth(s, authReq("/alice/", "", "alice", "whatever"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no stored password -> deny)", rec.Code)
	}
}

func TestHandleAuthCorrectPassword(t *testing.T) {
	s := newTestServer(newFake())
	store := newFakePasswordStore()
	hash, _ := hashPassword("s3cret")
	store.Set("alice", hash)
	s.passwords = store

	rec := doAuth(s, authReq("/alice/", "", "alice", "s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAuthWrongPassword(t *testing.T) {
	s := newTestServer(newFake())
	store := newFakePasswordStore()
	hash, _ := hashPassword("s3cret")
	store.Set("alice", hash)
	s.passwords = store

	rec := doAuth(s, authReq("/alice/", "", "alice", "wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleAuthMissingCredentials(t *testing.T) {
	s := newTestServer(newFake())
	store := newFakePasswordStore()
	hash, _ := hashPassword("s3cret")
	store.Set("alice", hash)
	s.passwords = store

	rec := doAuth(s, authReq("/alice/", "", "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleAuthUsernameMismatch(t *testing.T) {
	s := newTestServer(newFake())
	store := newFakePasswordStore()
	hash, _ := hashPassword("s3cret")
	store.Set("alice", hash)
	s.passwords = store

	// bob presents alice's password for alice's path -> reject.
	rec := doAuth(s, authReq("/alice/", "", "bob", "s3cret"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleAuthUserScopedPathResolvesUserFromPath(t *testing.T) {
	s := newTestServer(newFake())
	store := newFakePasswordStore()
	hash, _ := hashPassword("s3cret")
	store.Set("alice", hash)
	s.passwords = store

	// pi-web issues every request relative to /<username>/, so the target
	// user always comes from the URL path, even for API/WebSocket paths.
	rec := doAuth(s, authReq("/alice/api/machines/local/sessions/events", "", "alice", "s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAuthFallbackUsesCookie(t *testing.T) {
	s := newTestServer(newFake())
	store := newFakePasswordStore()
	hash, _ := hashPassword("s3cret")
	store.Set("alice", hash)
	s.passwords = store

	// Bare-origin fallback (favicon etc.): the user comes from the cookie.
	rec := doAuth(s, authReq("/favicon.ico", "alice", "alice", "s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAuthMissingUser(t *testing.T) {
	s := newTestServer(newFake())
	rec := doAuth(s, authReq("", "", "alice", "s3cret"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHashPasswordCheckPassword(t *testing.T) {
	hash, err := hashPassword("hello世界")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if !checkPassword(hash, "hello世界") {
		t.Errorf("checkPassword returned false for correct password")
	}
	if checkPassword(hash, "hello世界x") {
		t.Errorf("checkPassword returned true for wrong password")
	}
	if checkPassword("garbage", "hello世界") {
		t.Errorf("checkPassword returned true for malformed hash")
	}
	if checkPassword("a$b$c", "hello世界") {
		t.Errorf("checkPassword returned true for invalid hex hash")
	}
}

const aliceHost = "alice.mfpi.actors.resources.substrate.ate.dev"

func TestHandleSetAPIKeySuspendedResumesThenInjects(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-test123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, ok := store.Get("alice"); !ok || got != "sk-test123" {
		t.Errorf("stored key = %q, ok=%v; want sk-test123", got, ok)
	}
	if len(f.resumed) != 1 || f.resumed[0] != "alice" {
		t.Errorf("resumed = %v, want [alice]", f.resumed)
	}
	if len(auth.setCalls) != 1 || auth.setCalls[0] != aliceHost {
		t.Errorf("setCalls = %v, want [%s]", auth.setCalls, aliceHost)
	}
	if got := auth.storedByHost[aliceHost]; got != "sk-test123" {
		t.Errorf("injected key = %q, want sk-test123", got)
	}
}

func TestHandleSetAPIKeyRunningDoesNotResume(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	auth := s.actors.(*fakeActorAuth)

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-test123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(f.resumed) != 0 {
		t.Errorf("resumed = %v, want none (actor already running)", f.resumed)
	}
	if len(auth.setCalls) != 1 {
		t.Errorf("setCalls = %v, want one call", auth.setCalls)
	}
}

func TestHandleSetAPIKeyInvalidName(t *testing.T) {
	f := newFake()
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)

	rec := doRequest(s, http.MethodPost, "/api/users/Bad_Name/apikey", `{"apiKey":"sk-x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.keys) != 0 || len(auth.setCalls) != 0 {
		t.Errorf("invalid name must not touch stores or actor: store=%v setCalls=%v", store.keys, auth.setCalls)
	}
}

func TestHandleSetAPIKeyEmptyKey(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.keys) != 0 || len(auth.setCalls) != 0 {
		t.Errorf("empty key must not touch stores or actor")
	}
}

func TestHandleSetAPIKeyMissingActor(t *testing.T) {
	f := newFake()
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)

	rec := doRequest(s, http.MethodPost, "/api/users/nobody/apikey", `{"apiKey":"sk-x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.keys) != 0 {
		t.Errorf("key stored for a missing actor: %v", store.keys)
	}
}

func TestHandleSetAPIKeyStoreFailure(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	store.err = errors.New("rbac denied")

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-x"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if len(auth.setCalls) != 0 {
		t.Errorf("actor driven despite store failure: setCalls=%v", auth.setCalls)
	}
}

func TestHandleSetAPIKeyInjectFailureKeepsStoredKey(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	auth.setErr = errors.New("web never became ready")

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-x"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	// Store-first policy: the key is retained even though injection failed.
	if got, ok := store.Get("alice"); !ok || got != "sk-x" {
		t.Errorf("stored key = %q, ok=%v; want retained sk-x", got, ok)
	}
}

func TestHandleSetAPIKeyResumeFailureKeepsStoredKey(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	f.resumeErr = errors.New("no free workers")
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-x"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if got, ok := store.Get("alice"); !ok || got != "sk-x" {
		t.Errorf("stored key = %q, ok=%v; want retained sk-x", got, ok)
	}
	if len(auth.setCalls) != 0 {
		t.Errorf("actor driven despite resume failure: setCalls=%v", auth.setCalls)
	}
}

func TestHandleClearAPIKeyHappyPath(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	store.Set("alice", "sk-x")

	rec := doRequest(s, http.MethodDelete, "/api/users/alice/apikey", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auth.clearCalls) != 1 || auth.clearCalls[0] != aliceHost {
		t.Errorf("clearCalls = %v, want [%s]", auth.clearCalls, aliceHost)
	}
	if _, ok := store.Get("alice"); ok {
		t.Errorf("stored key still present after successful clear")
	}
}

func TestHandleClearAPIKeyMissingActorPurgesStore(t *testing.T) {
	f := newFake()
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	store.Set("alice", "sk-x")

	rec := doRequest(s, http.MethodDelete, "/api/users/alice/apikey", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auth.clearCalls) != 0 {
		t.Errorf("clearPersonalKey driven for a missing actor: %v", auth.clearCalls)
	}
	if _, ok := store.Get("alice"); ok {
		t.Errorf("stored key not purged for missing actor")
	}
}

func TestHandleClearAPIKeyLogoutFailureKeepsStoredKey(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	store.Set("alice", "sk-x")
	auth.clearErr = errors.New("logout rejected")

	rec := doRequest(s, http.MethodDelete, "/api/users/alice/apikey", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	// Clear-first policy: a failed logout keeps the Secret entry for a retry.
	if got, ok := store.Get("alice"); !ok || got != "sk-x" {
		t.Errorf("stored key = %q, ok=%v; want retained sk-x", got, ok)
	}
}

func TestHandleDeleteUserPurgesStoredKey(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	store.Set("alice", "sk-x")

	rec := doRequest(s, http.MethodDelete, "/api/users/alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(f.deleted) != 1 || f.deleted[0] != "alice" {
		t.Errorf("deleted = %v, want [alice]", f.deleted)
	}
	if _, ok := store.Get("alice"); ok {
		t.Errorf("stored key not purged on user delete")
	}
}

func TestHandleListUsersReportsHasPersonalKey(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	addActor(f, "mfpi", "bob", "STATUS_SUSPENDED", fixedNow.Add(-2*time.Hour))
	s := newTestServer(f)
	s.keys.(*fakeKeyStore).Set("bob", "sk-bob")

	rec := doRequest(s, http.MethodGet, "/api/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Users []userSummary `json:"users"`
	}](t, rec)
	if len(resp.Users) != 2 {
		t.Fatalf("len(users) = %d, want 2", len(resp.Users))
	}
	for _, u := range resp.Users {
		want := u.Name == "bob"
		if u.HasPersonalKey != want {
			t.Errorf("user %s hasPersonalKey = %v, want %v", u.Name, u.HasPersonalKey, want)
		}
	}
}

func TestHandleCreateUserDuplicateReplaysStoredKey(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	store.Set("alice", "sk-x")

	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[map[string]any](t, rec)
	if !strings.Contains(resp["message"].(string), "用户已存在，复用现有会话") {
		t.Errorf("message = %v, want to mention reuse", resp["message"])
	}
	// Duplicate-with-stored-key resumes the SUSPENDED actor and replays the key.
	if len(f.resumed) != 1 || f.resumed[0] != "alice" {
		t.Errorf("resumed = %v, want [alice]", f.resumed)
	}
	if len(auth.setCalls) != 1 || auth.setCalls[0] != aliceHost {
		t.Errorf("setCalls = %v, want [%s]", auth.setCalls, aliceHost)
	}
}

func TestHandleCreateUserFreshReplaysStoredKey(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	// Actor deleted earlier (key purge failed); re-creating should replay.
	store.Set("alice", "sk-x")

	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[map[string]any](t, rec)
	if resp["message"] != "创建成功" {
		t.Errorf("message = %v, want 创建成功", resp["message"])
	}
	// One resume from the create handler, one replay injection.
	if len(f.resumed) != 1 || f.resumed[0] != "alice" {
		t.Errorf("resumed = %v, want [alice]", f.resumed)
	}
	if len(auth.setCalls) != 1 || auth.setCalls[0] != aliceHost {
		t.Errorf("setCalls = %v, want [%s]", auth.setCalls, aliceHost)
	}
	if got := auth.storedByHost[aliceHost]; got != "sk-x" {
		t.Errorf("injected key = %q, want sk-x", got)
	}
}

func TestActorHostname(t *testing.T) {
	if got := actorHostname("alice", "mfpi"); got != aliceHost {
		t.Errorf("actorHostname = %q, want %q", got, aliceHost)
	}
	if got := actorHostname("alice", "mfpi-test"); got != "alice.mfpi-test.actors.resources.substrate.ate.dev" {
		t.Errorf("actorHostname(test) = %q", got)
	}
}

// piWebRecorder coordinates a fake pi-web /api/machines/local/auth server used
// to exercise httpActorAuthClient end to end.
type piWebRecorder struct {
	mu                   sync.Mutex
	host                 string
	seq                  []string
	responded            bool
	interactiveHasPrompt bool // false => POST .../interactive returns no prompt (forces flow poll)
	lastRequestID        string
	lastValue            string
}

func newPiWebRecorder() *piWebRecorder {
	return &piWebRecorder{interactiveHasPrompt: true}
}

// newPiWebServer stands in for an actor's pi-web auth endpoints. Providers
// report source "stored" only after a respond (or before a logout).
func newPiWebServer(rec *piWebRecorder) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.host = r.Host
		rec.seq = append(rec.seq, r.Method+" "+r.URL.Path)
		responded := rec.responded
		hasPrompt := rec.interactiveHasPrompt
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/machines/local/auth/providers":
			src := "environment"
			if responded {
				src = "stored"
			}
			fmt.Fprintf(w, `{"providers":[{"id":"deepseek","name":"DeepSeek","authType":"api_key","status":{"configured":true,"source":%q}}]}`, src)
		case r.Method == http.MethodPost && r.URL.Path == "/api/machines/local/auth/api-key/interactive":
			if hasPrompt {
				fmt.Fprint(w, `{"flowId":"f1","providerId":"deepseek","status":"running","prompt":{"requestId":"r1","message":"enter key","promptType":"secret"}}`)
			} else {
				fmt.Fprint(w, `{"flowId":"f1","providerId":"deepseek","status":"running"}`)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/machines/local/auth/oauth/f1":
			fmt.Fprint(w, `{"flowId":"f1","providerId":"deepseek","status":"running","prompt":{"requestId":"r1","message":"enter key","promptType":"secret"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/machines/local/auth/oauth/f1/respond":
			var body struct {
				RequestID string `json:"requestId"`
				Value     string `json:"value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.mu.Lock()
			rec.lastRequestID = body.RequestID
			rec.lastValue = body.Value
			rec.responded = true
			rec.mu.Unlock()
			fmt.Fprint(w, `{"flowId":"f1","providerId":"deepseek","status":"complete"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/machines/local/auth/logout":
			rec.mu.Lock()
			rec.responded = false
			rec.mu.Unlock()
			fmt.Fprint(w, `{"accepted":true}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func recSnapshot(rec *piWebRecorder) (host string, seq []string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.host, append([]string(nil), rec.seq...)
}

func TestHTTPActorAuthClientSetWithoutPromptPollsFlow(t *testing.T) {
	rec := newPiWebRecorder()
	rec.interactiveHasPrompt = false
	srv := newPiWebServer(rec)
	defer srv.Close()

	c := newHTTPActorAuthClient(srv.URL)
	if err := c.setPersonalKey(context.Background(), aliceHost, "sk-abc"); err != nil {
		t.Fatalf("setPersonalKey: %v", err)
	}

	host, seq := recSnapshot(rec)
	if host != aliceHost {
		t.Errorf("Host header = %q, want %q", host, aliceHost)
	}
	want := []string{
		"GET /api/machines/local/auth/providers",
		"POST /api/machines/local/auth/api-key/interactive",
		"GET /api/machines/local/auth/oauth/f1",
		"POST /api/machines/local/auth/oauth/f1/respond",
		"GET /api/machines/local/auth/providers",
	}
	if len(seq) != len(want) {
		t.Fatalf("request seq = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Errorf("seq[%d] = %q, want %q", i, seq[i], want[i])
		}
	}
	rec.mu.Lock()
	requestID, value := rec.lastRequestID, rec.lastValue
	rec.mu.Unlock()
	if requestID != "r1" || value != "sk-abc" {
		t.Errorf("respond body = (requestId=%q value=%q), want (r1 sk-abc)", requestID, value)
	}
}

func TestHTTPActorAuthClientSetWithImmediatePromptSkipsPoll(t *testing.T) {
	rec := newPiWebRecorder() // interactiveHasPrompt defaults true
	srv := newPiWebServer(rec)
	defer srv.Close()

	c := newHTTPActorAuthClient(srv.URL)
	if err := c.setPersonalKey(context.Background(), aliceHost, "sk-abc"); err != nil {
		t.Fatalf("setPersonalKey: %v", err)
	}

	_, seq := recSnapshot(rec)
	want := []string{
		"GET /api/machines/local/auth/providers",
		"POST /api/machines/local/auth/api-key/interactive",
		"POST /api/machines/local/auth/oauth/f1/respond",
		"GET /api/machines/local/auth/providers",
	}
	if len(seq) != len(want) {
		t.Fatalf("request seq = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Errorf("seq[%d] = %q, want %q", i, seq[i], want[i])
		}
	}
}

func TestHTTPActorAuthClientClearDrivesLogout(t *testing.T) {
	rec := newPiWebRecorder()
	rec.responded = true // start with a stored credential
	srv := newPiWebServer(rec)
	defer srv.Close()

	c := newHTTPActorAuthClient(srv.URL)
	if err := c.clearPersonalKey(context.Background(), aliceHost); err != nil {
		t.Fatalf("clearPersonalKey: %v", err)
	}

	host, seq := recSnapshot(rec)
	if host != aliceHost {
		t.Errorf("Host header = %q, want %q", host, aliceHost)
	}
	want := []string{
		"GET /api/machines/local/auth/providers",
		"POST /api/machines/local/auth/logout",
		"GET /api/machines/local/auth/providers",
	}
	if len(seq) != len(want) {
		t.Fatalf("request seq = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Errorf("seq[%d] = %q, want %q", i, seq[i], want[i])
		}
	}
}
