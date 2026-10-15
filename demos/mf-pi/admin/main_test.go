package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
	purged           []string
	purgeTemplates   []string
	resumeErr        error
	purgeErr         error

	// resumeBlock, when non-nil, makes ResumeActor wait until it is closed.
	// resumeCount (guarded by resumeMu) records how many resume calls have
	// started; both exist to test the gate's resume de-duplication.
	resumeBlock chan struct{}
	resumeMu    sync.Mutex
	resumeCount int
}

func (f *fakeControlClient) startedResumes() int {
	f.resumeMu.Lock()
	defer f.resumeMu.Unlock()
	return f.resumeCount
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
	f.resumeMu.Lock()
	f.resumeCount++
	block := f.resumeBlock
	f.resumeMu.Unlock()
	if block != nil {
		<-block
	}
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

func (f *fakeControlClient) PurgeActorVolumes(_ context.Context, req *ateapipb.PurgeActorVolumesRequest, _ ...grpc.CallOption) (*ateapipb.PurgeActorVolumesResponse, error) {
	if f.purgeErr != nil {
		return nil, f.purgeErr
	}
	f.purged = append(f.purged, req.GetActor().GetName())
	f.purgeTemplates = append(f.purgeTemplates, fmt.Sprintf("%s/%s", req.GetTemplate().GetAtespace(), req.GetTemplate().GetName()))
	return &ateapipb.PurgeActorVolumesResponse{PurgedVolumeIds: []string{req.GetActor().GetAtespace() + "-" + req.GetActor().GetName() + "-userdata"}}, nil
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

// fakeProviderRegistry is an in-memory providerRegistryStore for tests.
type fakeProviderRegistry struct {
	data map[string]registryProvider
	err  error
}

func newFakeProviderRegistry() *fakeProviderRegistry {
	return &fakeProviderRegistry{data: map[string]registryProvider{}}
}

func (f *fakeProviderRegistry) load(context.Context) error { return f.err }

func (f *fakeProviderRegistry) list() map[string]registryProvider {
	out := make(map[string]registryProvider, len(f.data))
	for k, v := range f.data {
		out[k] = v
	}
	return out
}

func (f *fakeProviderRegistry) set(_ context.Context, id string, p registryProvider) error {
	if f.err != nil {
		return f.err
	}
	f.data[id] = p
	return nil
}

func (f *fakeProviderRegistry) remove(_ context.Context, id string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if _, ok := f.data[id]; !ok {
		return false, nil
	}
	delete(f.data, id)
	return true, nil
}

func (f *fakeProviderRegistry) document() registryDocument {
	return registryDocument{Providers: f.list()}
}

// fakeKeyStore is an in-memory keyStore for tests.
type fakeKeyStore struct {
	keys map[string]providerKey
	err  error
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{keys: map[string]providerKey{}}
}

func (s *fakeKeyStore) Get(name string) (providerKey, bool) {
	k, ok := s.keys[name]
	return k, ok
}

func (s *fakeKeyStore) Set(name string, key providerKey) error {
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

// fakeExpiryStore is an in-memory expiryStore for tests.
type fakeExpiryStore struct {
	expiries map[string]string
	err      error
}

func newFakeExpiryStore() *fakeExpiryStore {
	return &fakeExpiryStore{expiries: map[string]string{}}
}

func (s *fakeExpiryStore) Get(name string) (string, bool) {
	e, ok := s.expiries[name]
	return e, ok
}

func (s *fakeExpiryStore) Set(name, expiresAt string) error {
	if s.err != nil {
		return s.err
	}
	s.expiries[name] = expiresAt
	return nil
}

func (s *fakeExpiryStore) Delete(name string) error {
	if s.err != nil {
		return s.err
	}
	delete(s.expiries, name)
	return nil
}

// fakeTierStore is an in-memory tierStore for tests.
type fakeTierStore struct {
	tiers map[string]string
	err   error
}

func newFakeTierStore() *fakeTierStore {
	return &fakeTierStore{tiers: map[string]string{}}
}

func (s *fakeTierStore) Get(name string) (string, bool) {
	t, ok := s.tiers[name]
	return t, ok
}

func (s *fakeTierStore) Set(name, tier string) error {
	if s.err != nil {
		return s.err
	}
	s.tiers[name] = tier
	return nil
}

func (s *fakeTierStore) Delete(name string) error {
	if s.err != nil {
		return s.err
	}
	delete(s.tiers, name)
	return nil
}

// fakeActorAuth is an in-memory actorAuthClient for tests. It records the
// set/clear calls per host and can be configured to fail. Stored credentials
// are tracked per (host, provider) because one provider is kept per user.
type fakeActorAuth struct {
	storedByHost map[string]map[string]string // host -> provider -> key
	setCalls     []string
	clearCalls   []string
	setErr       error
	clearErr     error
}

func newFakeActorAuth() *fakeActorAuth {
	return &fakeActorAuth{storedByHost: map[string]map[string]string{}}
}

// storeKey records a credential for host+providerID, creating the per-host map
// on first write so reads (isKeyStored, the assertions) never mutate the
// fixture and len(storedByHost) stays meaningful.
func (f *fakeActorAuth) storeKey(host, providerID, key string) {
	if f.storedByHost[host] == nil {
		f.storedByHost[host] = map[string]string{}
	}
	f.storedByHost[host][providerID] = key
}

func (f *fakeActorAuth) setPersonalKey(_ context.Context, host, providerID, apiKey string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, host+"|"+providerID)
	f.storeKey(host, providerID, apiKey)
	return nil
}

func (f *fakeActorAuth) clearPersonalKey(_ context.Context, host, providerID string) error {
	if f.clearErr != nil {
		return f.clearErr
	}
	f.clearCalls = append(f.clearCalls, host+"|"+providerID)
	if m := f.storedByHost[host]; m != nil {
		delete(m, providerID)
	}
	return nil
}

// isKeyStored reports whether the host holds a credential for providerID in the
// in-memory map, which setPersonalKey populates and clearPersonalKey empties —
// mirroring pi-web's source=="stored" vs "environment" distinction.
func (f *fakeActorAuth) isKeyStored(_ context.Context, host, providerID string) (bool, error) {
	_, ok := f.storedByHost[host][providerID]
	return ok, nil
}

func newTestServer(f *fakeControlClient) *server {
	return &server{
		atespace:          "mfpi",
		templateNamespace: "ate-demo-mf-pi",
		templateName:      "mf-pi",
		client:            f,
		passwords:         newFakePasswordStore(),
		keys:              newFakeKeyStore(),
		expiries:          newFakeExpiryStore(),
		tiers:             newFakeTierStore(),
		tierTemplates:     buildTierTemplates("mf-pi"),
		defaultTier:       "small",
		actors:            newFakeActorAuth(),
		providers:         defaultProviderOptions,
		providerRegistry:  newFakeProviderRegistry(),
		idleTimeout:       defaultIdleTimeout,
		idleTiers:         parseIdleTiers("small,mid"),
		idle:              newIdleTracker(),
		now:               func() time.Time { return fixedNow },
		lastAttempt:       make(map[string]time.Time),
		resuming:          make(map[string]bool),
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
	case path == "/api/providers":
		s.handleListProviders(rec, req)
	case path == "/internal/providers/models.json":
		s.handleInternalProviderModels(rec, req)
	case path == "/api/providers/discover":
		s.handleDiscoverProviderModels(rec, req)
	case strings.HasPrefix(path, "/api/providers/"):
		s.handleDeleteProvider(rec, req)
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
	if got, ok := store.Get("alice"); !ok || got.APIKey != "sk-test123" || got.ProviderID != defaultProviderID {
		t.Errorf("stored key = %+v, ok=%v; want apiKey=sk-test123 provider=%s", got, ok, defaultProviderID)
	}
	if len(f.resumed) != 1 || f.resumed[0] != "alice" {
		t.Errorf("resumed = %v, want [alice]", f.resumed)
	}
	if len(auth.setCalls) != 1 || auth.setCalls[0] != aliceHost+"|"+defaultProviderID {
		t.Errorf("setCalls = %v, want [%s]", auth.setCalls, aliceHost)
	}
	if got := auth.storedByHost[aliceHost][defaultProviderID]; got != "sk-test123" {
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
	if got, ok := store.Get("alice"); !ok || got.APIKey != "sk-x" {
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
	if got, ok := store.Get("alice"); !ok || got.APIKey != "sk-x" {
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
	store.Set("alice", providerKey{ProviderID: defaultProviderID, APIKey: "sk-x"})

	rec := doRequest(s, http.MethodDelete, "/api/users/alice/apikey", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auth.clearCalls) != 1 || auth.clearCalls[0] != aliceHost+"|"+defaultProviderID {
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
	store.Set("alice", providerKey{ProviderID: defaultProviderID, APIKey: "sk-x"})

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
	store.Set("alice", providerKey{ProviderID: defaultProviderID, APIKey: "sk-x"})
	auth.clearErr = errors.New("logout rejected")

	rec := doRequest(s, http.MethodDelete, "/api/users/alice/apikey", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	// Clear-first policy: a failed logout keeps the Secret entry for a retry.
	if got, ok := store.Get("alice"); !ok || got.APIKey != "sk-x" {
		t.Errorf("stored key = %q, ok=%v; want retained sk-x", got, ok)
	}
}

func TestHandleDeleteUserPurgesStoredKey(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	store.Set("alice", providerKey{ProviderID: defaultProviderID, APIKey: "sk-x"})

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

func TestHandleDeleteUserPurgesVolume(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	rec := doRequest(s, http.MethodDelete, "/api/users/alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(f.purged) != 1 || f.purged[0] != "alice" {
		t.Errorf("purged = %v, want [alice]", f.purged)
	}
	// The volume IDs come from the server's configured ActorTemplate.
	wantTemplate := s.templateNamespace + "/" + s.templateName
	if len(f.purgeTemplates) != 1 || f.purgeTemplates[0] != wantTemplate {
		t.Errorf("purgeTemplates = %v, want [%s]", f.purgeTemplates, wantTemplate)
	}
}

func TestHandleDeleteUserVolumePurgeFailureStillSucceeds(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	f.purgeErr = status.Error(codes.FailedPrecondition, "purge unavailable")

	rec := doRequest(s, http.MethodDelete, "/api/users/alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (purge is best-effort); body=%s", rec.Code, rec.Body.String())
	}
	if len(f.deleted) != 1 || f.deleted[0] != "alice" {
		t.Errorf("deleted = %v, want [alice]", f.deleted)
	}
}

func TestHandleListUsersReportsHasPersonalKey(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	addActor(f, "mfpi", "bob", "STATUS_SUSPENDED", fixedNow.Add(-2*time.Hour))
	s := newTestServer(f)
	s.keys.(*fakeKeyStore).Set("bob", providerKey{ProviderID: defaultProviderID, APIKey: "sk-bob"})

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
	store.Set("alice", providerKey{ProviderID: defaultProviderID, APIKey: "sk-x"})

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
	if len(auth.setCalls) != 1 || auth.setCalls[0] != aliceHost+"|"+defaultProviderID {
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
	store.Set("alice", providerKey{ProviderID: defaultProviderID, APIKey: "sk-x"})

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
	if len(auth.setCalls) != 1 || auth.setCalls[0] != aliceHost+"|"+defaultProviderID {
		t.Errorf("setCalls = %v, want [%s]", auth.setCalls, aliceHost)
	}
	if got := auth.storedByHost[aliceHost][defaultProviderID]; got != "sk-x" {
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

// TestReconcileInjectsKeyIntoRunningActor verifies the reconciler re-applies a
// stored key to a RUNNING actor that does not yet hold it — the recovery path
// for an actor resumed outside the admin flow (e.g. refresh-actor.sh).
func TestReconcileInjectsKeyIntoRunningActor(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	store.Set("alice", providerKey{ProviderID: defaultProviderID, APIKey: "sk-x"})

	s.reconcileKeys(context.Background())

	if len(auth.setCalls) != 1 || auth.setCalls[0] != aliceHost+"|"+defaultProviderID {
		t.Fatalf("setCalls = %v, want [%s]", auth.setCalls, aliceHost)
	}
	if got := auth.storedByHost[aliceHost][defaultProviderID]; got != "sk-x" {
		t.Errorf("injected key = %q, want sk-x", got)
	}
}

// TestReconcileSkipsNonRunningActor confirms the reconciler never drives the
// login flow against a non-RUNNING actor (which cannot accept it).
func TestReconcileSkipsNonRunningActor(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.keys.(*fakeKeyStore).Set("alice", providerKey{ProviderID: defaultProviderID, APIKey: "sk-x"})

	s.reconcileKeys(context.Background())

	auth := s.actors.(*fakeActorAuth)
	if len(auth.setCalls) != 0 {
		t.Errorf("setCalls = %v, want none for a SUSPENDED actor", auth.setCalls)
	}
}

// TestReconcileSkipsAlreadyStored confirms an actor that already holds the key
// is not re-driven, so the reconciler is idempotent and cheap.
func TestReconcileSkipsAlreadyStored(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	store.Set("alice", providerKey{ProviderID: defaultProviderID, APIKey: "sk-x"})
	auth.storeKey(aliceHost, defaultProviderID, "sk-x") // simulate already applied

	s.reconcileKeys(context.Background())

	if len(auth.setCalls) != 0 {
		t.Errorf("setCalls = %v, want none when already stored", auth.setCalls)
	}
}

// TestReconcileSkipsActorWithoutStoredKey confirms actors with no key in the
// Secret are left untouched.
func TestReconcileSkipsActorWithoutStoredKey(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	// No key stored for alice.

	s.reconcileKeys(context.Background())

	auth := s.actors.(*fakeActorAuth)
	if len(auth.setCalls) != 0 || len(auth.storedByHost) != 0 {
		t.Errorf("setCalls=%v stored=%v, want untouched", auth.setCalls, auth.storedByHost)
	}
}

// TestReconcileIgnoresOtherAtespace confirms only the configured atespace is
// reconciled.
func TestReconcileIgnoresOtherAtespace(t *testing.T) {
	f := newFake()
	addActor(f, "other", "zoe", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	store.Set("zoe", providerKey{ProviderID: defaultProviderID, APIKey: "sk-z"}) // key for the wrong atespace's actor name

	s.reconcileKeys(context.Background())

	auth := s.actors.(*fakeActorAuth)
	if len(auth.setCalls) != 0 {
		t.Errorf("setCalls = %v, want none (actor is in a different atespace)", auth.setCalls)
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
	// providerID is the provider the fake reports and drives. It starts at the
	// default and is updated from the request bodies, so tests can exercise any
	// provider exactly as pi-web would.
	providerID string
	// loggedOutProvider is the providerId of the last logout POST.
	loggedOutProvider string
}

func newPiWebRecorder() *piWebRecorder {
	return &piWebRecorder{interactiveHasPrompt: true, providerID: defaultProviderID}
}

// provider returns the provider the fake currently reports.
func (rec *piWebRecorder) provider() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.providerID == "" {
		return defaultProviderID
	}
	return rec.providerID
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
		providerID := rec.provider()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/machines/local/auth/providers":
			src := "environment"
			if responded {
				src = "stored"
			}
			fmt.Fprintf(w, `{"providers":[{"id":%q,"name":"Test Provider","authType":"api_key","status":{"configured":true,"source":%q}}]}`, providerID, src)
		case r.Method == http.MethodPost && r.URL.Path == "/api/machines/local/auth/api-key/interactive":
			var body struct {
				ProviderID string `json:"providerId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.mu.Lock()
			rec.providerID = body.ProviderID
			rec.mu.Unlock()
			if body.ProviderID == "" {
				providerID = defaultProviderID
			} else {
				providerID = body.ProviderID
			}
			if hasPrompt {
				fmt.Fprintf(w, `{"flowId":"f1","providerId":%q,"status":"running","prompt":{"requestId":"r1","message":"enter key","promptType":"secret"}}`, providerID)
			} else {
				fmt.Fprintf(w, `{"flowId":"f1","providerId":%q,"status":"running"}`, providerID)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/machines/local/auth/oauth/f1":
			// Before the respond the flow exposes its prompt; afterwards the
			// async login has reconciled and the flow reads "complete" (this
			// mirrors pi-web's OAuthLoginFlowService: respond resolves the
			// prompt and returns "running"; completion lands on a later poll).
			if responded {
				fmt.Fprintf(w, `{"flowId":"f1","providerId":%q,"status":"complete"}`, providerID)
			} else {
				fmt.Fprintf(w, `{"flowId":"f1","providerId":%q,"status":"running","prompt":{"requestId":"r1","message":"enter key","promptType":"secret"}}`, providerID)
			}
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
			fmt.Fprintf(w, `{"flowId":"f1","providerId":%q,"status":"running"}`, providerID)
		case r.Method == http.MethodPost && r.URL.Path == "/api/machines/local/auth/logout":
			var body struct {
				ProviderID string `json:"providerId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.mu.Lock()
			rec.responded = false
			rec.loggedOutProvider = body.ProviderID
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
	if err := c.setPersonalKey(context.Background(), aliceHost, defaultProviderID, "sk-abc"); err != nil {
		t.Fatalf("setPersonalKey: %v", err)
	}

	host, seq := recSnapshot(rec)
	if host != aliceHost {
		t.Errorf("Host header = %q, want %q", host, aliceHost)
	}
	want := []string{
		"GET /api/machines/local/auth/providers",
		"POST /api/machines/local/auth/api-key/interactive",
		"GET /api/machines/local/auth/oauth/f1", // poll for the prompt (no prompt in the interactive response)
		"POST /api/machines/local/auth/oauth/f1/respond",
		"GET /api/machines/local/auth/oauth/f1", // poll for async completion
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
	if err := c.setPersonalKey(context.Background(), aliceHost, defaultProviderID, "sk-abc"); err != nil {
		t.Fatalf("setPersonalKey: %v", err)
	}

	_, seq := recSnapshot(rec)
	want := []string{
		"GET /api/machines/local/auth/providers",
		"POST /api/machines/local/auth/api-key/interactive",
		"POST /api/machines/local/auth/oauth/f1/respond",
		"GET /api/machines/local/auth/oauth/f1", // poll for async completion
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
	if err := c.clearPersonalKey(context.Background(), aliceHost, defaultProviderID); err != nil {
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

// ---- Activation expiry + resource tier (docs/resource_quota_plan.md, Part D) ----

func TestHandleSetExpirySuccess(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.expiries.(*fakeExpiryStore)

	rec := doRequest(s, http.MethodPost, "/api/users/alice/expiry", `{"duration":"168h"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		ExpiresAt string `json:"expiresAt"`
	}](t, rec)
	want := fixedNow.Add(168 * time.Hour).UTC().Format(time.RFC3339)
	if resp.ExpiresAt != want {
		t.Errorf("expiresAt = %q, want %q", resp.ExpiresAt, want)
	}
	if got, ok := store.Get("alice"); !ok || got != want {
		t.Errorf("stored expiry = %q (ok=%v), want %q", got, ok, want)
	}
}

func TestHandleSetExpiryInvalidDuration(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	for _, dur := range []string{"bogus", "0", "-3h", ""} {
		rec := doRequest(s, http.MethodPost, "/api/users/alice/expiry", `{"duration":"`+dur+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("duration %q: status = %d, want 400; body=%s", dur, rec.Code, rec.Body.String())
		}
	}
	if _, ok := s.expiries.(*fakeExpiryStore).Get("alice"); ok {
		t.Errorf("expiry stored for invalid durations")
	}
}

func TestHandleSetExpiryMissingActor(t *testing.T) {
	f := newFake() // no alice actor
	s := newTestServer(f)
	rec := doRequest(s, http.MethodPost, "/api/users/alice/expiry", `{"duration":"24h"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleClearExpiryIdempotent(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.expiries.(*fakeExpiryStore)
	store.Set("alice", fixedNow.Add(time.Hour).UTC().Format(time.RFC3339))

	// First clear removes it.
	if rec := doRequest(s, http.MethodDelete, "/api/users/alice/expiry", ""); rec.Code != http.StatusOK {
		t.Fatalf("first clear status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Get("alice"); ok {
		t.Errorf("expiry still present after clear")
	}
	// Second clear is a no-op (idempotent).
	if rec := doRequest(s, http.MethodDelete, "/api/users/alice/expiry", ""); rec.Code != http.StatusOK {
		t.Fatalf("second clear status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestReconcileExpireSuspendExpiredRunner(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.expiries.(*fakeExpiryStore).Set("alice", fixedNow.Add(-time.Minute).UTC().Format(time.RFC3339))

	s.reconcileExpirations(context.Background())

	if len(f.suspended) != 1 || f.suspended[0] != "alice" {
		t.Errorf("suspended = %v, want [alice]", f.suspended)
	}
}

func TestReconcileExpireSkipsNotExpired(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.expiries.(*fakeExpiryStore).Set("alice", fixedNow.Add(time.Hour).UTC().Format(time.RFC3339))

	s.reconcileExpirations(context.Background())

	if len(f.suspended) != 0 {
		t.Errorf("suspended = %v, want none (not yet expired)", f.suspended)
	}
}

func TestReconcileExpireSkipsNonRunning(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.expiries.(*fakeExpiryStore).Set("alice", fixedNow.Add(-time.Minute).UTC().Format(time.RFC3339))

	s.reconcileExpirations(context.Background())

	if len(f.suspended) != 0 {
		t.Errorf("suspended = %v, want none for a SUSPENDED actor", f.suspended)
	}
}

func TestReconcileIdleSuspendsIdleMidTier(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-2*time.Hour))
	s := newTestServer(f)
	s.tiers.(*fakeTierStore).Set("alice", "mid")
	// Last input 2h ago, well past the 90m idle timeout.
	s.idle.touch("alice", fixedNow.Add(-2*time.Hour))

	s.reconcileIdle(context.Background())

	if len(f.suspended) != 1 || f.suspended[0] != "alice" {
		t.Errorf("suspended = %v, want [alice]", f.suspended)
	}
}

func TestReconcileIdleSkipsActive(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	// Touched 1 minute ago, within the idle window.
	s.idle.touch("alice", fixedNow.Add(-time.Minute))

	s.reconcileIdle(context.Background())

	if len(f.suspended) != 0 {
		t.Errorf("suspended = %v, want none (still active)", f.suspended)
	}
}

func TestReconcileIdleSkipsLargeTier(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-2*time.Hour))
	s := newTestServer(f)
	s.tiers.(*fakeTierStore).Set("alice", "large")
	s.idle.touch("alice", fixedNow.Add(-2*time.Hour)) // idle > timeout

	s.reconcileIdle(context.Background())

	if len(f.suspended) != 0 {
		t.Errorf("suspended = %v, want none for a large-tier actor", f.suspended)
	}
}

func TestReconcileIdleSkipsNoActivityRecord(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-2*time.Hour))
	s := newTestServer(f)
	// No idle touch recorded (e.g. admin just restarted): not considered idle.
	s.reconcileIdle(context.Background())

	if len(f.suspended) != 0 {
		t.Errorf("suspended = %v, want none without an activity record", f.suspended)
	}
}

func TestReconcileIdleSkipsNonRunning(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-2*time.Hour))
	s := newTestServer(f)
	s.idle.touch("alice", fixedNow.Add(-2*time.Hour))

	s.reconcileIdle(context.Background())

	if len(f.suspended) != 0 {
		t.Errorf("suspended = %v, want none for a SUSPENDED actor", f.suspended)
	}
}

func TestReconcileIdleSkipsIdleTimeoutUnset(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-2*time.Hour))
	s := newTestServer(f)
	s.idleTimeout = 0 // disabled
	s.idle.touch("alice", fixedNow.Add(-2*time.Hour))

	s.reconcileIdle(context.Background())

	if len(f.suspended) != 0 {
		t.Errorf("suspended = %v, want none when idle timeout is disabled", f.suspended)
	}
}

func TestHandleAuthRecordsInput(t *testing.T) {
	s := newTestServer(newFake())
	store := newFakePasswordStore()
	hash, _ := hashPassword("s3cret")
	store.Set("alice", hash)
	s.passwords = store

	rec := doAuth(s, authReq("/alice/", "", "alice", "s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	last, ok := s.idle.lastInput("alice")
	if !ok {
		t.Fatalf("no idle input recorded for alice after a successful auth")
	}
	if !last.Equal(fixedNow) {
		t.Errorf("last input = %v, want %v", last, fixedNow)
	}
}

func TestHandleAuthDoesNotRecordFailedAuth(t *testing.T) {
	s := newTestServer(newFake())
	store := newFakePasswordStore()
	hash, _ := hashPassword("s3cret")
	store.Set("alice", hash)
	s.passwords = store

	rec := doAuth(s, authReq("/alice/", "", "alice", "wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if _, ok := s.idle.lastInput("alice"); ok {
		t.Errorf("failed auth recorded as input for alice")
	}
}

func TestParseIdleTiers(t *testing.T) {
	if got := parseIdleTiers("small, mid"); !got["small"] || !got["mid"] || len(got) != 2 {
		t.Errorf("parseIdleTiers(small, mid) = %v, want {small, mid}", got)
	}
	if got := parseIdleTiers(""); len(got) != 0 {
		t.Errorf("parseIdleTiers(empty) = %v, want empty", got)
	}
	if got := parseIdleTiers(" large , "); !got["large"] || len(got) != 1 {
		t.Errorf("parseIdleTiers(large) = %v, want {large}", got)
	}
}

func TestHandleSetTierValid(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.tiers.(*fakeTierStore)

	rec := doRequest(s, http.MethodPost, "/api/users/alice/tier", `{"tier":"mid"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, ok := store.Get("alice"); !ok || got != "mid" {
		t.Errorf("stored tier = %q (ok=%v), want mid", got, ok)
	}
}

func TestHandleSetTierInvalid(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)

	for _, tier := range []string{"huge", "", "Small"} {
		rec := doRequest(s, http.MethodPost, "/api/users/alice/tier", `{"tier":"`+tier+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("tier %q: status = %d, want 400; body=%s", tier, rec.Code, rec.Body.String())
		}
	}
	if _, ok := s.tiers.(*fakeTierStore).Get("alice"); ok {
		t.Errorf("tier stored for invalid value")
	}
}

func TestHandleSetTierMissingActor(t *testing.T) {
	f := newFake() // no alice actor
	s := newTestServer(f)
	rec := doRequest(s, http.MethodPost, "/api/users/alice/tier", `{"tier":"mid"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateUserUsesTierTemplate(t *testing.T) {
	f := newFake()
	s := newTestServer(f)
	s.tiers.(*fakeTierStore).Set("alice", "mid")

	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(f.created) != 1 {
		t.Fatalf("created = %d actors, want 1", len(f.created))
	}
	if got := f.created[0].GetActorTemplateName(); got != "mf-pi-mid" {
		t.Errorf("actor template = %q, want mf-pi-mid", got)
	}
}

func TestHandleCreateUserDefaultTier(t *testing.T) {
	f := newFake()
	s := newTestServer(f) // no tier set -> defaultTier "small"

	rec := doRequest(s, http.MethodPost, "/api/users", `{"name":"alice"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := f.created[0].GetActorTemplateName(); got != "mf-pi-small" {
		t.Errorf("actor template = %q, want mf-pi-small (default tier)", got)
	}
}

func TestHandleListUsersReportsExpiryAndTier(t *testing.T) {
	f := newFake()
	f.atespaces["mfpi"] = &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "mfpi"}}
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	addActor(f, "mfpi", "bob", "STATUS_SUSPENDED", fixedNow.Add(-2*time.Hour))
	s := newTestServer(f)
	s.expiries.(*fakeExpiryStore).Set("alice", fixedNow.Add(time.Hour).UTC().Format(time.RFC3339))
	s.tiers.(*fakeTierStore).Set("bob", "large")

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
		switch u.Name {
		case "alice":
			if !u.HasExpiry {
				t.Errorf("alice hasExpiry = false, want true")
			}
			if want := fixedNow.Add(time.Hour).UTC().Format(time.RFC3339); u.Expiry != want {
				t.Errorf("alice expiry = %q, want %q", u.Expiry, want)
			}
			if u.Tier != "small" {
				t.Errorf("alice tier = %q, want default small", u.Tier)
			}
		case "bob":
			if u.HasExpiry {
				t.Errorf("bob hasExpiry = true, want false")
			}
			if u.Tier != "large" {
				t.Errorf("bob tier = %q, want large", u.Tier)
			}
		}
	}
}

func TestHandleDeleteUserCleansExpiryAndTier(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.expiries.(*fakeExpiryStore).Set("alice", fixedNow.Add(time.Hour).UTC().Format(time.RFC3339))
	s.tiers.(*fakeTierStore).Set("alice", "mid")

	rec := doRequest(s, http.MethodDelete, "/api/users/alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := s.expiries.(*fakeExpiryStore).Get("alice"); ok {
		t.Errorf("expiry not cleaned up after delete")
	}
	if _, ok := s.tiers.(*fakeTierStore).Get("alice"); ok {
		t.Errorf("tier not cleaned up after delete")
	}
}

// ---------------------------------------------------------------------------
// Multi-provider support: provider selection, switching, and storage.
// ---------------------------------------------------------------------------

func TestHandleSetAPIKeyExplicitProvider(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-or","providerId":"openai"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[map[string]string](t, rec)
	if resp["providerId"] != "openai" {
		t.Errorf("response providerId = %q, want openai", resp["providerId"])
	}
	if resp["message"] != "OpenAI Key 已设置" {
		t.Errorf("message = %q, want OpenAI Key 已设置", resp["message"])
	}
	if got, ok := store.Get("alice"); !ok || got.ProviderID != "openai" || got.APIKey != "sk-or" {
		t.Errorf("stored = %+v (ok=%v), want provider=openai key=sk-or", got, ok)
	}
	want := aliceHost + "|openai"
	if len(auth.setCalls) != 1 || auth.setCalls[0] != want {
		t.Errorf("setCalls = %v, want [%s]", auth.setCalls, want)
	}
	if len(auth.clearCalls) != 0 {
		t.Errorf("clearCalls = %v, want none", auth.clearCalls)
	}
}

func TestHandleSetAPIKeyDefaultsToDeepSeek(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-ds"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get("alice"); got.ProviderID != defaultProviderID {
		t.Errorf("stored provider = %q, want %q", got.ProviderID, defaultProviderID)
	}
}

func TestHandleSetAPIKeyUnsupportedProvider(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-x","providerId":"not-a-provider"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.keys) != 0 || len(auth.setCalls) != 0 || len(auth.clearCalls) != 0 {
		t.Errorf("rejected provider must touch nothing: store=%v set=%v clear=%v", store.keys, auth.setCalls, auth.clearCalls)
	}
}

// Setting a different provider switches: the previously stored provider is
// logged out of the actor before the new key is committed, so the old provider
// does not stay "configured" (and its models selectable) for the user.
func TestHandleSetAPIKeySwitchesProvider(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)

	store.Set("alice", providerKey{ProviderID: "deepseek", APIKey: "sk-old"})
	auth.storeKey(aliceHost, "deepseek", "sk-old")

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-new","providerId":"openai"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auth.clearCalls) != 1 || auth.clearCalls[0] != aliceHost+"|deepseek" {
		t.Errorf("clearCalls = %v, want [%s|deepseek]", auth.clearCalls, aliceHost)
	}
	if len(auth.setCalls) != 1 || auth.setCalls[0] != aliceHost+"|openai" {
		t.Errorf("setCalls = %v, want [%s|openai]", auth.setCalls, aliceHost)
	}
	if got, _ := store.Get("alice"); got.ProviderID != "openai" || got.APIKey != "sk-new" {
		t.Errorf("stored = %+v, want provider=openai key=sk-new", got)
	}
	if _, ok := auth.storedByHost[aliceHost]["deepseek"]; ok {
		t.Errorf("old provider credential still stored on actor: %v", auth.storedByHost[aliceHost])
	}
}

// A failed logout of the previous provider must not commit the new key: the
// stored record stays on the old provider so the operator can retry.
func TestHandleSetAPIKeySwitchClearFailureKeepsOldKey(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	store.Set("alice", providerKey{ProviderID: "deepseek", APIKey: "sk-old"})
	auth.storeKey(aliceHost, "deepseek", "sk-old")
	auth.clearErr = errors.New("logout boom")

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-new","providerId":"openai"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get("alice"); got.ProviderID != "deepseek" || got.APIKey != "sk-old" {
		t.Errorf("stored = %+v, want unchanged deepseek/sk-old", got)
	}
	if len(auth.setCalls) != 0 {
		t.Errorf("setCalls = %v, want none after a failed switch", auth.setCalls)
	}
}

// Re-setting the same provider must not log it out first.
func TestHandleSetAPIKeySameProviderDoesNotClear(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	store.Set("alice", providerKey{ProviderID: "openai", APIKey: "sk-old"})

	rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey", `{"apiKey":"sk-new","providerId":"openai"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auth.clearCalls) != 0 {
		t.Errorf("clearCalls = %v, want none when the provider is unchanged", auth.clearCalls)
	}
}

// Clear must log out the provider that is actually stored, not deepseek.
func TestHandleClearAPIKeyUsesStoredProvider(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	store := s.keys.(*fakeKeyStore)
	auth := s.actors.(*fakeActorAuth)
	store.Set("alice", providerKey{ProviderID: "openai", APIKey: "sk-or"})
	auth.storeKey(aliceHost, "openai", "sk-or")

	rec := doRequest(s, http.MethodDelete, "/api/users/alice/apikey", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auth.clearCalls) != 1 || auth.clearCalls[0] != aliceHost+"|openai" {
		t.Errorf("clearCalls = %v, want [%s|openai]", auth.clearCalls, aliceHost)
	}
	if _, ok := store.Get("alice"); ok {
		t.Errorf("stored key not purged")
	}
	resp := decode[map[string]string](t, rec)
	if resp["providerId"] != "openai" {
		t.Errorf("response providerId = %q, want openai", resp["providerId"])
	}
}

// Clearing a user with no stored key is an idempotent success and must not
// resume or touch the actor.
func TestHandleClearAPIKeyNoStoredKeyIsIdempotent(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_SUSPENDED", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	auth := s.actors.(*fakeActorAuth)

	rec := doRequest(s, http.MethodDelete, "/api/users/alice/apikey", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(auth.clearCalls) != 0 || len(f.resumed) != 0 {
		t.Errorf("actor touched with nothing stored: clear=%v resumed=%v", auth.clearCalls, f.resumed)
	}
}

func TestHandleListUsersReportsPersonalKeyProvider(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	addActor(f, "mfpi", "bob", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.keys.Set("alice", providerKey{ProviderID: "openai", APIKey: "sk-or"})

	rec := doRequest(s, http.MethodGet, "/api/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Users []userSummary `json:"users"`
	}](t, rec)
	byName := map[string]userSummary{}
	for _, u := range resp.Users {
		byName[u.Name] = u
	}
	if got := byName["alice"]; !got.HasPersonalKey || got.PersonalKeyProvider != "openai" {
		t.Errorf("alice = %+v, want hasPersonalKey with provider openai", got)
	}
	if got := byName["bob"]; got.HasPersonalKey || got.PersonalKeyProvider != "" {
		t.Errorf("bob = %+v, want no personal key", got)
	}
}

func TestHandleListProviders(t *testing.T) {
	s := newTestServer(newFake())
	rec := doRequest(s, http.MethodGet, "/api/providers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Providers []providerOption `json:"providers"`
	}](t, rec)
	if len(resp.Providers) != len(defaultProviderOptions) {
		t.Fatalf("providers = %d, want %d", len(resp.Providers), len(defaultProviderOptions))
	}
	if resp.Providers[0].ID != defaultProviderID || resp.Providers[0].Name == "" {
		t.Errorf("first provider = %+v, want %s with a display name", resp.Providers[0], defaultProviderID)
	}

}

func TestReconcileUsesStoredProvider(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	s.keys.Set("alice", providerKey{ProviderID: "openai", APIKey: "sk-or"})
	auth := s.actors.(*fakeActorAuth)

	s.reconcileKeys(context.Background())

	want := aliceHost + "|openai"
	if len(auth.setCalls) != 1 || auth.setCalls[0] != want {
		t.Fatalf("setCalls = %v, want [%s]", auth.setCalls, want)
	}
	if got := auth.storedByHost[aliceHost]["openai"]; got != "sk-or" {
		t.Errorf("injected key = %q, want sk-or", got)
	}
}

func TestParseStoredKey(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want providerKey
	}{
		{"json", `{"providerId":"openai","apiKey":"sk-or"}`, providerKey{ProviderID: "openai", APIKey: "sk-or"}},
		{"json without provider", `{"apiKey":"sk-or"}`, providerKey{ProviderID: defaultProviderID, APIKey: "sk-or"}},
		{"legacy raw key", "sk-legacy", providerKey{ProviderID: defaultProviderID, APIKey: "sk-legacy"}},
		{"legacy raw key that looks like a number", "12345", providerKey{ProviderID: defaultProviderID, APIKey: "12345"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseStoredKey(tc.raw); got != tc.want {
				t.Errorf("parseStoredKey(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseProviderOptions(t *testing.T) {
	fallback := []providerOption{{ID: "fallback", Name: "Fallback"}}
	cases := []struct {
		name string
		spec string
		want []providerOption
	}{
		{"blank falls back", "", fallback},
		{"only separators falls back", " , , ", fallback},
		{"id only uses id as name", "openai", []providerOption{{ID: "openai", Name: "openai"}}},
		{"id and name", "zai=Z.AI (GLM)", []providerOption{{ID: "zai", Name: "Z.AI (GLM)"}}},
		{"mixed with blanks", " openai , groq=Groq ,", []providerOption{
			{ID: "openai", Name: "openai"},
			{ID: "groq", Name: "Groq"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseProviderOptions(tc.spec, fallback)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				// providerOption now carries endpoint details (a slice), so it
				// is no longer comparable as a whole; this test only cares about
				// the parsed id/name.
				if got[i].ID != tc.want[i].ID || got[i].Name != tc.want[i].Name {
					t.Errorf("[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
	// The default spec must round-trip to the default list.
	if got := parseProviderOptions(defaultProviderSpec(), nil); len(got) != len(defaultProviderOptions) {
		t.Errorf("defaultProviderSpec round-trip = %d entries, want %d", len(got), len(defaultProviderOptions))
	}
}

func TestProviderLabelFallsBackToID(t *testing.T) {
	if got := providerLabel(defaultProviderOptions, "openai"); got != "OpenAI" {
		t.Errorf("providerLabel(openai) = %q, want OpenAI", got)
	}
	if got := providerLabel(defaultProviderOptions, "unknown-provider"); got != "unknown-provider" {
		t.Errorf("providerLabel(unknown) = %q, want the id itself", got)
	}
}

// secretKeyStore must round-trip the {providerId, apiKey} record through the
// Secret, and must still read keys written by the pre-multi-provider version
// (a bare key string).
func TestSecretKeyStoreRoundTrip(t *testing.T) {
	const ns, name = "ate-demo-mf-pi", "mfpi-user-provider-keys"
	mkSecret := func(data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       data,
		}
	}

	t.Run("write then reload", func(t *testing.T) {
		cs := fake.NewSimpleClientset(mkSecret(map[string][]byte{}))
		store := newSecretKeyStore(cs, ns, name)
		if err := store.load(context.Background()); err != nil {
			t.Fatalf("load: %v", err)
		}
		if err := store.Set("alice", providerKey{ProviderID: "openai", APIKey: "sk-or"}); err != nil {
			t.Fatalf("set: %v", err)
		}
		// A fresh store reading the same Secret must observe the same record.
		reloaded := newSecretKeyStore(cs, ns, name)
		if err := reloaded.load(context.Background()); err != nil {
			t.Fatalf("reload: %v", err)
		}
		got, ok := reloaded.Get("alice")
		if !ok || got.ProviderID != "openai" || got.APIKey != "sk-or" {
			t.Fatalf("reloaded = %+v (ok=%v), want openai/sk-or", got, ok)
		}
		if err := reloaded.Delete("alice"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, ok := reloaded.Get("alice"); ok {
			t.Errorf("entry survived delete")
		}
	})

	t.Run("legacy raw key", func(t *testing.T) {
		cs := fake.NewSimpleClientset(mkSecret(map[string][]byte{"alice": []byte("sk-legacy")}))
		store := newSecretKeyStore(cs, ns, name)
		if err := store.load(context.Background()); err != nil {
			t.Fatalf("load: %v", err)
		}
		got, ok := store.Get("alice")
		if !ok || got.ProviderID != defaultProviderID || got.APIKey != "sk-legacy" {
			t.Errorf("legacy entry = %+v (ok=%v), want %s/sk-legacy", got, ok, defaultProviderID)
		}
	})

	t.Run("missing secret is an empty store", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		store := newSecretKeyStore(cs, ns, name)
		if err := store.load(context.Background()); err != nil {
			t.Fatalf("load: %v", err)
		}
		if _, ok := store.Get("alice"); ok {
			t.Errorf("unexpected entry in an empty store")
		}
		// Writes need the pre-created Secret and must fail with a clear error.
		if err := store.Set("alice", providerKey{ProviderID: "openai", APIKey: "sk-or"}); err == nil {
			t.Errorf("Set on a missing Secret = nil, want an error")
		}
	})
}

// The HTTP client must drive whichever provider it is asked for and assert that
// provider's stored state, for both set and clear.
func TestHTTPActorAuthClientDrivesArbitraryProvider(t *testing.T) {
	rec := newPiWebRecorder()
	srv := newPiWebServer(rec)
	defer srv.Close()
	c := newHTTPActorAuthClient(srv.URL)

	if err := c.setPersonalKey(context.Background(), aliceHost, "openai", "sk-or"); err != nil {
		t.Fatalf("setPersonalKey: %v", err)
	}
	if got := rec.provider(); got != "openai" {
		t.Errorf("fake provider = %q, want openai", got)
	}
	if stored, err := c.isKeyStored(context.Background(), aliceHost, "openai"); err != nil || !stored {
		t.Errorf("isKeyStored(openai) = %v, %v; want true", stored, err)
	}

	if err := c.clearPersonalKey(context.Background(), aliceHost, "openai"); err != nil {
		t.Fatalf("clearPersonalKey: %v", err)
	}
	rec.mu.Lock()
	loggedOut := rec.loggedOutProvider
	rec.mu.Unlock()
	if loggedOut != "openai" {
		t.Errorf("logged out provider = %q, want openai", loggedOut)
	}
	if stored, err := c.isKeyStored(context.Background(), aliceHost, "openai"); err != nil || stored {
		t.Errorf("isKeyStored(openai) after clear = %v, %v; want false", stored, err)
	}
}

// TestLivePiWebProviderKey is an OPT-IN integration test that drives a real
// pi-web instance through the same httpActorAuthClient the admin uses: it stores
// a provider credential via pi-web's interactive api-key flow, verifies the
// provider then reads back as source=="stored", and clears it again. It is
// skipped unless the environment points it at a live instance, so the normal
// `go test` run stays hermetic.
//
//	MFPI_LIVE_PIWEB_URL   base URL of a reachable pi-web, e.g. http://127.0.0.1:8599
//	MFPI_LIVE_PROVIDER_KEY the API key to store (never logged)
//	MFPI_LIVE_PROVIDER_ID  provider id to exercise (default "siliconflow")
//	MFPI_LIVE_HOST         Host header to send (default: the URL's host[:port])
//	MFPI_LIVE_KEEP         if non-empty, leave the credential stored for a
//	                       follow-up real-inference check instead of clearing it
//
// A provider defined only in the agent dir's models.json (custom OpenAI-compatible
// endpoint) has no env-var credential, so a successful run also proves the
// admin can drive providers that exist purely as models.json entries.
func TestLivePiWebProviderKey(t *testing.T) {
	base := os.Getenv("MFPI_LIVE_PIWEB_URL")
	key := os.Getenv("MFPI_LIVE_PROVIDER_KEY")
	if base == "" || key == "" {
		t.Skip("set MFPI_LIVE_PIWEB_URL and MFPI_LIVE_PROVIDER_KEY to run against a live pi-web")
	}
	providerID := os.Getenv("MFPI_LIVE_PROVIDER_ID")
	if providerID == "" {
		providerID = "siliconflow"
	}
	host := os.Getenv("MFPI_LIVE_HOST")
	if host == "" {
		host = strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := newHTTPActorAuthClient(base)

	// Start from a clean slate: a credential left by an earlier run must not be
	// able to mask a failure here.
	if err := c.clearPersonalKey(ctx, host, providerID); err != nil {
		t.Fatalf("pre-clean clearPersonalKey(%s): %v", providerID, err)
	}
	if stored, err := c.isKeyStored(ctx, host, providerID); err != nil || stored {
		t.Fatalf("after pre-clean: stored=%v err=%v; want false/nil", stored, err)
	}

	if err := c.setPersonalKey(ctx, host, providerID, key); err != nil {
		t.Fatalf("setPersonalKey(%s): %v", providerID, err)
	}
	if stored, err := c.isKeyStored(ctx, host, providerID); err != nil || !stored {
		t.Fatalf("after setPersonalKey: stored=%v err=%v; want true/nil", stored, err)
	}
	t.Logf("pi-web stored the %s credential and reports source=stored", providerID)

	if os.Getenv("MFPI_LIVE_KEEP") != "" {
		t.Logf("MFPI_LIVE_KEEP set: leaving the credential in place for a real-inference check")
		return
	}
	if err := c.clearPersonalKey(ctx, host, providerID); err != nil {
		t.Fatalf("clearPersonalKey(%s): %v", providerID, err)
	}
	if stored, err := c.isKeyStored(ctx, host, providerID); err != nil || stored {
		t.Errorf("after clearPersonalKey: stored=%v err=%v; want false/nil", stored, err)
	}
}

// The provider registry endpoint serves the admin-managed registry as a
// models.json document, degrades to an empty registry, and stays read-only.
func TestHandleInternalProviderModels(t *testing.T) {
	s := newTestServer(newFake())
	reg := s.providerRegistry.(*fakeProviderRegistry)
	reg.data["siliconflow"] = registryProvider{
		Name:    "SiliconFlow",
		BaseURL: "https://api.siliconflow.cn/v1",
		API:     "openai-completions",
		Models:  []registryModel{{ID: "deepseek-ai/DeepSeek-V3.2"}},
	}

	rec := doRequest(s, http.MethodGet, "/internal/providers/models.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var doc registryDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	got, ok := doc.Providers["siliconflow"]
	if !ok || got.BaseURL != "https://api.siliconflow.cn/v1" || len(got.Models) != 1 {
		t.Errorf("served registry = %+v, want the stored siliconflow provider", doc.Providers)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	empty := doRequest(newTestServer(newFake()), http.MethodGet, "/internal/providers/models.json", "")
	var emptyDoc registryDocument
	if err := json.Unmarshal(empty.Body.Bytes(), &emptyDoc); err != nil {
		t.Fatalf("decode empty registry: %v; body=%s", err, empty.Body.String())
	}
	if len(emptyDoc.Providers) != 0 {
		t.Errorf("empty registry served %d providers", len(emptyDoc.Providers))
	}

	if post := doRequest(s, http.MethodPost, "/internal/providers/models.json", "{}"); post.Code != http.StatusNotFound {
		t.Errorf("POST status = %d, want 404", post.Code)
	}
}

func TestHandleCreateProvider(t *testing.T) {
	f := newFake()
	addActor(f, "mfpi", "alice", "STATUS_RUNNING", fixedNow.Add(-time.Hour))
	s := newTestServer(f)
	reg := s.providerRegistry.(*fakeProviderRegistry)

	rec := doRequest(s, http.MethodPost, "/api/providers",
		`{"name":"Internal Gateway","baseUrl":"https://gw.internal/v1","models":["qwen3-32b","llama-3.3-70b"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Provider providerOption `json:"provider"`
	}](t, rec)
	if resp.Provider.ID != "internal-gateway" || !resp.Provider.Custom {
		t.Errorf("provider = %+v, want id internal-gateway flagged custom", resp.Provider)
	}
	stored, ok := reg.data["internal-gateway"]
	if !ok {
		t.Fatalf("provider not persisted: %v", reg.data)
	}
	if stored.API != registryAPIDefault || len(stored.Models) != 2 {
		t.Errorf("stored = %+v, want api=%s and 2 models", stored, registryAPIDefault)
	}
	if stored.Models[0].Name != "qwen3-32b" {
		t.Errorf("model name = %q, want it defaulted to the id", stored.Models[0].Name)
	}

	// A custom provider id becomes selectable for per-user keys.
	if rec := doRequest(s, http.MethodPost, "/api/users/alice/apikey",
		`{"apiKey":"sk-x","providerId":"internal-gateway"}`); rec.Code != http.StatusOK {
		t.Errorf("setting a key for a custom provider: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// And it shows up in the UI list.
	list := decode[struct {
		Providers []providerOption `json:"providers"`
	}](t, doRequest(s, http.MethodGet, "/api/providers", ""))
	found := false
	for _, p := range list.Providers {
		if p.ID == "internal-gateway" && p.Custom {
			found = true
		}
	}
	if !found {
		t.Errorf("custom provider missing from %d options", len(list.Providers))
	}
}

func TestHandleCreateProviderValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty baseUrl", `{"name":"x","models":["m"]}`},
		{"relative baseUrl", `{"name":"x","baseUrl":"/v1","models":["m"]}`},
		{"unsupported scheme", `{"name":"x","baseUrl":"ftp://h/v1","models":["m"]}`},
		{"no models", `{"name":"x","baseUrl":"https://h/v1","models":[]}`},
		{"blank model", `{"name":"x","baseUrl":"https://h/v1","models":["  "]}`},
		{"bad id charset", `{"id":"Bad ID","name":"x","baseUrl":"https://h/v1","models":["m"]}`},
		{"shadows a builtin", `{"id":"openai","name":"x","baseUrl":"https://h/v1","models":["m"]}`},
		{"bad json", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(newFake())
			rec := doRequest(s, http.MethodPost, "/api/providers", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if n := len(s.providerRegistry.(*fakeProviderRegistry).data); n != 0 {
				t.Errorf("registry mutated on rejected input: %d entries", n)
			}
		})
	}
}

func TestHandleDeleteProvider(t *testing.T) {
	s := newTestServer(newFake())
	reg := s.providerRegistry.(*fakeProviderRegistry)
	reg.data["my-gw"] = registryProvider{Name: "My GW", BaseURL: "https://h/v1", Models: []registryModel{{ID: "m"}}}

	rec := doRequest(s, http.MethodDelete, "/api/providers/my-gw", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := reg.data["my-gw"]; ok {
		t.Errorf("provider still present after delete")
	}
	if rec := doRequest(s, http.MethodDelete, "/api/providers/my-gw", ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", rec.Code)
	}

	// Only DELETE may remove an endpoint: other methods must not.
	reg.data["keep-me"] = registryProvider{Name: "Keep", BaseURL: "https://h/v1", Models: []registryModel{{ID: "m"}}}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodGet} {
		if rec := doRequest(s, method, "/api/providers/keep-me", ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", method, rec.Code)
		}
		if _, ok := reg.data["keep-me"]; !ok {
			t.Fatalf("%s removed the provider; only DELETE may", method)
		}
	}
}

func TestProviderLabelAndOptionsIncludeCustom(t *testing.T) {
	s := newTestServer(newFake())
	s.providerRegistry.(*fakeProviderRegistry).data["my-gw"] = registryProvider{
		Name: "My Gateway", BaseURL: "https://h/v1", API: registryAPIDefault,
		Models: []registryModel{{ID: "m"}},
	}
	if got := providerLabel(s.providerOptions(), "my-gw"); got != "My Gateway" {
		t.Errorf("providerLabel(custom) = %q, want My Gateway", got)
	}
	opts := s.providerOptions()
	if len(opts) != len(defaultProviderOptions)+1 {
		t.Fatalf("options = %d, want %d", len(opts), len(defaultProviderOptions)+1)
	}
	last := opts[len(opts)-1]
	if last.ID != "my-gw" || !last.Custom {
		t.Errorf("last option = %+v, want the custom provider flagged", last)
	}
	// The UI needs the endpoint details inline to list custom endpoints.
	if last.BaseURL != "https://h/v1" || last.API != registryAPIDefault || len(last.Models) != 1 || last.Models[0].ID != "m" {
		t.Errorf("custom option details = %+v, want baseUrl/api/models exposed", last)
	}
	if opts[0].BaseURL != "" || opts[0].Models != nil {
		t.Errorf("built-in option must not carry endpoint details: %+v", opts[0])
	}
	if opts[0].Custom {
		t.Errorf("built-in options must not be flagged custom: %+v", opts[0])
	}
}

func TestSlugifyProviderID(t *testing.T) {
	cases := map[string]string{
		"Internal Gateway": "internal-gateway",
		"Z.AI  GLM":        "z-ai-glm",
		"":                 "custom",
		"!!!":              "custom",
		"  Leading  ":      "leading",
	}
	for in, want := range cases {
		if got := slugifyProviderID(in); got != want {
			t.Errorf("slugifyProviderID(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Declared model rows and endpoint model discovery.
// ---------------------------------------------------------------------------

func TestHandleCreateProviderAcceptsModelObjects(t *testing.T) {
	s := newTestServer(newFake())
	reg := s.providerRegistry.(*fakeProviderRegistry)

	rec := doRequest(s, http.MethodPost, "/api/providers", `{
		"name":"Acme Gateway","baseUrl":"https://gateway.acme.example/v1","api":"anthropic-messages",
		"models":[
			{"id":"acme-large","name":"Acme Large","contextWindow":131072,"maxTokens":32768,"input":["text","image"]},
			{"id":"acme-small"}
		]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored := reg.data["acme-gateway"]
	if len(stored.Models) != 2 {
		t.Fatalf("stored models = %+v, want 2", stored.Models)
	}
	first := stored.Models[0]
	if first.ID != "acme-large" || first.Name != "Acme Large" || first.ContextWindow != 131072 ||
		first.MaxTokens != 32768 || len(first.Input) != 2 {
		t.Errorf("first model = %+v, want the declared fields preserved", first)
	}
	if second := stored.Models[1]; second.Name != "acme-small" || second.ContextWindow != 0 {
		t.Errorf("second model = %+v, want name defaulted to id and no capacities", second)
	}
	resp := decode[struct {
		Provider providerOption `json:"provider"`
	}](t, rec)
	if resp.Provider.Models[0].ContextWindow != 131072 || resp.Provider.API != "anthropic-messages" {
		t.Errorf("response provider = %+v, want the model fields echoed back", resp.Provider)
	}
}

// Bare model-id strings stay accepted so an older caller keeps working.
func TestHandleCreateProviderAcceptsModelIDStrings(t *testing.T) {
	s := newTestServer(newFake())
	rec := doRequest(s, http.MethodPost, "/api/providers",
		`{"name":"gw","baseUrl":"https://h/v1","models":["m-one","m-two",""]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored := s.providerRegistry.(*fakeProviderRegistry).data["gw"]
	if len(stored.Models) != 2 {
		t.Errorf("models = %+v, want the two non-empty ids", stored.Models)
	}
}

func TestParseProviderModelsRejectsBadRows(t *testing.T) {
	cases := map[string]string{
		"zero context window":  `[{"id":"m","contextWindow":0}]`,
		"negative max tokens":  `[{"id":"m","maxTokens":-5}]`,
		"non-integer capacity": `[{"id":"m","contextWindow":1.5}]`,
		"empty id":             `[{"name":"x"}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var raw []json.RawMessage
			if err := json.Unmarshal([]byte(body), &raw); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			if _, err := parseProviderModels(raw); err == nil {
				t.Errorf("parseProviderModels(%s) = nil error, want a refusal", body)
			}
		})
	}
}

func TestValidateRegistryProviderRejectsUnknownInputModality(t *testing.T) {
	_, err := validateRegistryProvider("gw", registryProvider{
		BaseURL: "https://h/v1",
		Models:  []registryModel{{ID: "m", Input: []string{"audio"}}},
	}, builtinProviderIndex())
	if err == nil || !strings.Contains(err.Error(), "输入类型只能是 text 或 image") {
		t.Errorf("err = %v, want the modality refusal", err)
	}
}

// The probe reads an OpenAI-style `data` array, including the field aliases
// gateways actually publish.
func TestDiscoverProviderModelsOpenAIShape(t *testing.T) {
	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[
			{"id":"acme-large","name":"Acme Large","context_length":131072,"max_output_tokens":32768},
			{"id":"acme-mini","top_provider":{"max_completion_tokens":4096},"limit":{"context":8192}},
			{"id":"acme-tiny"}
		]}`)
	}))
	defer upstream.Close()

	s := newTestServer(newFake())
	rec := doRequest(s, http.MethodPost, "/api/providers/discover",
		`{"baseUrl":"`+upstream.URL+`/v1","api":"openai-completions","apiKey":"sk-probe"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/models" {
		t.Errorf("probed path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-probe" {
		t.Errorf("authorization = %q, want the probe key", gotAuth)
	}
	resp := decode[struct {
		Models []discoveredModel `json:"models"`
	}](t, rec)
	if len(resp.Models) != 3 {
		t.Fatalf("models = %+v, want 3", resp.Models)
	}
	if m := resp.Models[0]; m.ID != "acme-large" || m.Name != "Acme Large" || m.ContextWindow != 131072 || m.MaxTokens != 32768 {
		t.Errorf("first = %+v, want aliases read", m)
	}
	if m := resp.Models[1]; m.ContextWindow != 8192 || m.MaxTokens != 4096 {
		t.Errorf("second = %+v, want the nested limit/top_provider capacities", m)
	}
	if m := resp.Models[2]; m.Name != "acme-tiny" || m.ContextWindow != 0 {
		t.Errorf("third = %+v, want the name defaulted to the id", m)
	}
}

// An Anthropic-style `models` object lists at the root's /v1/models with the
// version header, and entries may be keyed without their own id.
func TestDiscoverProviderModelsAnthropicShape(t *testing.T) {
	var gotPath, gotVersion, gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotVersion, gotKey = r.URL.Path, r.Header.Get("anthropic-version"), r.Header.Get("x-api-key")
		_, _ = io.WriteString(w, `{"models":{"claude-x":{"display_name":"Claude X"}}}`)
	}))
	defer upstream.Close()

	s := newTestServer(newFake())
	rec := doRequest(s, http.MethodPost, "/api/providers/discover",
		`{"baseUrl":"`+upstream.URL+`/v1","api":"anthropic-messages","apiKey":"sk-a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/models" {
		t.Errorf("probed path = %q, want the root's /v1/models", gotPath)
	}
	if gotVersion != anthropicVersion || gotKey != "sk-a" {
		t.Errorf("headers = version %q key %q, want %q and the probe key", gotVersion, gotKey, anthropicVersion)
	}
	resp := decode[struct {
		Models []discoveredModel `json:"models"`
	}](t, rec)
	if len(resp.Models) != 1 || resp.Models[0].ID != "claude-x" || resp.Models[0].Name != "Claude X" {
		t.Errorf("models = %+v, want the key used as the id", resp.Models)
	}
}

// A present but empty listing is a successful "nothing listed", not a parse
// failure: the console says so and keeps hand entry available.
func TestDiscoverProviderModelsEmptyListing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()

	s := newTestServer(newFake())
	rec := doRequest(s, http.MethodPost, "/api/providers/discover", `{"baseUrl":"`+upstream.URL+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode[struct {
		Models []discoveredModel `json:"models"`
	}](t, rec)
	if len(resp.Models) != 0 {
		t.Errorf("models = %+v, want none", resp.Models)
	}
}

func TestDiscoverProviderModelsFailureModes(t *testing.T) {
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()
	unshaped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list"}`)
	}))
	defer unshaped.Close()

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantText string
	}{
		{"not json", `{`, http.StatusBadRequest, "请求体不是合法 JSON"},
		{"empty base url", `{"baseUrl":"  "}`, http.StatusBadRequest, "baseUrl 不能为空"},
		{"relative base url", `{"baseUrl":"/v1"}`, http.StatusBadRequest, "非法"},
		{"unlistable protocol", `{"baseUrl":"https://h/v1","api":"google-generative-ai"}`, http.StatusBadRequest, "不支持探测模型列表"},
		{"unauthorized", `{"baseUrl":"` + unauthorized.URL + `"}`, http.StatusBadGateway, "请检查 API Key"},
		{"unshaped reply", `{"baseUrl":"` + unshaped.URL + `"}`, http.StatusBadGateway, "请手工填写模型"},
		{"unreachable", `{"baseUrl":"http://127.0.0.1:1"}`, http.StatusBadGateway, "无法连接"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(newFake())
			rec := doRequest(s, http.MethodPost, "/api/providers/discover", tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantText) {
				t.Errorf("body = %s, want it to mention %q", rec.Body.String(), tc.wantText)
			}
		})
	}

	// Only POST probes; the route stays read-only otherwise.
	s := newTestServer(newFake())
	if rec := doRequest(s, http.MethodGet, "/api/providers/discover", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404", rec.Code)
	}
}

func TestListingURL(t *testing.T) {
	cases := []struct{ base, api, want string }{
		{"https://gw.example/v1", "openai-completions", "https://gw.example/v1/models"},
		{"https://gw.example/v1/", "openai-responses", "https://gw.example/v1/models"},
		{"https://gw.example/v1", "anthropic-messages", "https://gw.example/v1/models?limit=" + fmt.Sprint(anthropicModelPage)},
		{"https://gw.example", "anthropic-messages", "https://gw.example/v1/models?limit=" + fmt.Sprint(anthropicModelPage)},
		{"https://gw.example/openai/v1", "openai-completions", "https://gw.example/openai/v1/models"},
	}
	for _, tc := range cases {
		if got := listingURL(tc.base, tc.api); got != tc.want {
			t.Errorf("listingURL(%q, %q) = %q, want %q", tc.base, tc.api, got, tc.want)
		}
	}
}

// The registry round-trips through the admin's own structs: whatever an
// operator writes into the ConfigMap, the console must be able to read and
// write it back without silently dropping fields. (This is why the manifest
// seed is limited to the fields the console models.)
func TestRegistryDocumentRoundTrips(t *testing.T) {
	const seed = `{"providers":{"siliconflow":{"name":"SiliconFlow","baseUrl":"https://api.siliconflow.cn/v1",` +
		`"api":"openai-completions","models":[{"id":"m","name":"M","contextWindow":128000,"maxTokens":8192,"input":["text"]}]}}}`
	data, err := parseRegistryDocument(seed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rendered, err := marshalRegistryDocument(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again, err := parseRegistryDocument(rendered)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !reflect.DeepEqual(data, again) {
		t.Errorf("registry changed across a write:\n before=%+v\n after=%+v", data, again)
	}
	model := again["siliconflow"].Models[0]
	if model.Name != "M" || model.ContextWindow != 128000 || model.MaxTokens != 8192 || len(model.Input) != 1 {
		t.Errorf("model = %+v, want every declared field preserved", model)
	}
}

// TestLiveDiscoverModels is an OPT-IN check of the probe against a real
// endpoint, so the tolerant listing reader is exercised on a gateway's actual
// reply rather than on fixtures alone. Skipped unless pointed at one:
//
//	MFPI_LIVE_DISCOVER_URL  e.g. https://api.siliconflow.cn/v1
//	MFPI_LIVE_DISCOVER_KEY  API key for that endpoint (optional)
//	MFPI_LIVE_DISCOVER_API  protocol id (default openai-completions)
func TestLiveDiscoverModels(t *testing.T) {
	baseURL := os.Getenv("MFPI_LIVE_DISCOVER_URL")
	if baseURL == "" {
		t.Skip("set MFPI_LIVE_DISCOVER_URL to probe a real endpoint")
	}
	api := os.Getenv("MFPI_LIVE_DISCOVER_API")
	if api == "" {
		api = registryAPIDefault
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	models, err := probeModelListing(ctx, baseURL, api, os.Getenv("MFPI_LIVE_DISCOVER_KEY"))
	if err != nil {
		t.Fatalf("probe %s (%s): %v", baseURL, api, err)
	}
	if len(models) == 0 {
		t.Fatalf("probe listed no models; the endpoint may need a key")
	}
	t.Logf("listed %d models; first: %+v", len(models), models[0])
	for _, m := range models {
		if m.ID == "" {
			t.Fatalf("listed a model with no id: %+v", m)
		}
	}
}
