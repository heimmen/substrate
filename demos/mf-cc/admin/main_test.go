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
	"net/http"
	"net/http/httptest"
	"strings"
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

func newTestServer(f *fakeControlClient) *server {
	return &server{
		atespace:          "mfcc",
		templateNamespace: "ate-demo-mf-cc",
		templateName:      "mf-cc",
		client:            f,
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
		ActorTemplateNamespace: "ate-demo-mf-cc",
		ActorTemplateName:      "mf-cc",
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
	case method == http.MethodDelete && strings.HasPrefix(path, "/api/users/"):
		s.handleDeleteUser(rec, req)
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
	f.atespaces["mfcc"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfcc"}}
	addActor(f, "mfcc", "bob", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	addActor(f, "mfcc", "alice", "STATUS_RUNNING", fixedNow.Add(-5*time.Minute))
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

	if resp.Atespace != "mfcc" {
		t.Errorf("atespace = %q, want mfcc", resp.Atespace)
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
	if resp.Users[0].Template != "ate-demo-mf-cc/mf-cc" {
		t.Errorf("alice template = %q, want ate-demo-mf-cc/mf-cc", resp.Users[0].Template)
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
	if len(f.atespacesCreated) != 1 || f.atespacesCreated[0] != "mfcc" {
		t.Errorf("atespacesCreated = %v, want [mfcc]", f.atespacesCreated)
	}
	if _, ok := f.actors["mfcc/alice"]; !ok {
		t.Errorf("actor mfcc/alice was not created")
	}
	if len(f.resumed) != 1 || f.resumed[0] != "alice" {
		t.Errorf("resumed = %v, want [alice]", f.resumed)
	}
	if got := f.actors["mfcc/alice"].GetStatus(); got != ateapipb.Actor_STATUS_RUNNING {
		t.Errorf("alice status = %v, want STATUS_RUNNING", got)
	}
	resp := decode[map[string]any](t, rec)
	if resp["message"] != "创建成功" {
		t.Errorf("message = %v, want 创建成功", resp["message"])
	}
}

func TestHandleCreateUserResumesActor(t *testing.T) {
	f := newFake()
	f.atespaces["mfcc"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfcc"}}
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
	f.atespaces["mfcc"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfcc"}}
	f.resumeErr = status.Error(codes.FailedPrecondition, "no free workers available")
	s := newTestServer(f)
	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := f.actors["mfcc/alice"]; !ok {
		t.Errorf("actor mfcc/alice was not created despite resume failure")
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
	addActor(f, "mfcc", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
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
	addActor(f, "mfcc", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
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
	if _, ok := f.actors["mfcc/alice"]; ok {
		t.Errorf("actor mfcc/alice still present after delete")
	}
}
