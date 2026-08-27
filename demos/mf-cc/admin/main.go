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

// mf-cc user-management server.
//
// Tiny stdlib HTTP server that proxies to the substrate ateapi gRPC service
// and exposes a small JSON API + embedded web UI for managing mf-cc users
// (each user is one Actor in the mfcc atespace):
//
//	GET    /api/users                 list users (mirrors list-users.sh)
//	POST   /api/users                 create a user (mirrors create-user.sh)
//	DELETE /api/users/{name}          delete a user (mirrors delete-user.sh)
//	POST   /api/users/{name}/password reset a user's access password
//	GET    /_mfcc_auth                nginx auth_request target: validate the
//	                                  per-user password for the requested user
//	GET    /                          serve the embedded admin UI
//	GET    /healthz                   readiness
//
// It authenticates in-cluster exactly like ate-controller/atenet-router: a
// projected service-account token (audience api.ate-system.svc) as Bearer
// credential and a projected ClusterTrustBundle as the server TLS roots.
// No RBAC beyond a valid SA token is required. See ADMIN_UI_PLAN.md.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/internal/ateapiauth"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

//go:embed index.html
var staticFS embed.FS

const (
	defaultPort              = "8080"
	defaultAtespace          = "mfcc"
	defaultTemplateNamespace = "ate-demo-mf-cc"
	defaultTemplateName      = "mf-cc"
	defaultAteapiAddr        = "dns:///api.ate-system.svc:443"
	defaultAteapiCAFile      = "/run/servicedns-ca/trust-bundle.pem"
	defaultAteapiTokenFile   = "/run/ateapi-token/token"
	ateapiServerName         = "api.ate-system.svc"

	// ConfigMap holding per-user password hashes (username -> hash).
	defaultPasswordsConfigMap = "mfcc-user-passwords"
	defaultPasswordsNamespace = "ate-demo-mf-cc"

	// passwordIterations is the PBKDF2 iteration count used to hash user
	// passwords. Demo-grade (stdlib-only); swap for bcrypt if desired.
	passwordIterations = 100_000

	// dns1123 is the same name rule Substrate enforces for actor names.
	dns1123 = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
)

var dns1123Re = regexp.MustCompile(dns1123)

// reservedPrefixRe matches the system path prefixes that nginx routes by the
// mfcc_user cookie rather than by a username in the URL. Used by the auth
// endpoint to decide whether the target user comes from the URL path or the
// cookie. Keep in sync with nginx.conf.
var reservedPrefixRe = regexp.MustCompile(`^/(api|assets|ws|sdk|callback|auth|proxy|preview-fs|local-file|health|usermanagement)(/|$)`)

// userPathRe extracts the leading username from a request path like /alice/...
var userPathRe = regexp.MustCompile(`^/([a-z0-9-]+)(/|$)`)

// controlClient is the subset of ateapipb.ControlClient the server needs,
// kept as an interface so handlers are unit-testable with a fake.
type controlClient interface {
	ListActors(ctx context.Context, req *ateapipb.ListActorsRequest, opts ...grpc.CallOption) (*ateapipb.ListActorsResponse, error)
	GetActor(ctx context.Context, req *ateapipb.GetActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error)
	CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error)
	ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest, opts ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error)
	DeleteActor(ctx context.Context, req *ateapipb.DeleteActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error)
	SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest, opts ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error)
	GetAtespace(ctx context.Context, req *ateapipb.GetAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error)
	CreateAtespace(ctx context.Context, req *ateapipb.CreateAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error)
}

// passwordStore persists per-user password hashes. Implemented by
// configMapPasswordStore in production and by a fake in tests.
type passwordStore interface {
	// Get returns the stored hash for the user, and whether one exists.
	Get(name string) (hash string, ok bool)
	// Set stores the hash for the user.
	Set(name, hash string) error
	// Delete removes any stored hash for the user (idempotent).
	Delete(name string) error
}

// configMapPasswordStore stores password hashes in a Kubernetes ConfigMap,
// mirroring them into an in-memory map for fast auth checks.
type configMapPasswordStore struct {
	clientset kubernetes.Interface
	namespace string
	name      string

	mu   sync.RWMutex
	data map[string]string // username -> hash
}

func newConfigMapPasswordStore(clientset kubernetes.Interface, namespace, name string) *configMapPasswordStore {
	return &configMapPasswordStore{
		clientset: clientset,
		namespace: namespace,
		name:      name,
		data:      map[string]string{},
	}
}

// load populates the in-memory map from the ConfigMap. A missing ConfigMap is
// treated as an empty store (not an error).
func (s *configMapPasswordStore) load(ctx context.Context) error {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = make(map[string]string, len(cm.Data))
	for k, v := range cm.Data {
		s.data[k] = v
	}
	return nil
}

func (s *configMapPasswordStore) Get(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.data[name]
	return h, ok
}

func (s *configMapPasswordStore) Set(name, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]string, len(s.data)+1)
	for k, v := range s.data {
		next[k] = v
	}
	next[name] = hash
	if err := s.persist(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *configMapPasswordStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[name]; !ok {
		return nil
	}
	next := make(map[string]string, len(s.data)-1)
	for k, v := range s.data {
		if k != name {
			next[k] = v
		}
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// persist writes data to the ConfigMap, creating it if absent.
func (s *configMapPasswordStore) persist(data map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace},
			Data:       data,
		}, metav1.CreateOptions{})
		return err
	}
	cm.Data = data
	_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// generatePassword returns a random password (32 hex chars).
func generatePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashPassword derives a salted, iterated hash from a plaintext password and
// encodes it as "salt$iterations$hash" (hex). Demo-grade PBKDF2-HMAC-SHA256.
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := pbkdf2SHA256([]byte(password), salt, passwordIterations, sha256.Size)
	return fmt.Sprintf("%x$%d$%x", salt, passwordIterations, dk), nil
}

// checkPassword reports whether password matches the encoded hash.
func checkPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 {
		return false
	}
	salt, err := hex.DecodeString(parts[0])
	if err != nil {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	want, err := hex.DecodeString(parts[2])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2SHA256 implements PBKDF2 (RFC 2898) with HMAC-SHA256 for a single
// output block (keyLen <= 32). It is the standard construction and avoids a
// third-party dependency for this demo.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	// U1 = HMAC(password, salt || INT_32_BE(1))
	prf.Write(salt)
	var block [4]byte
	binary.BigEndian.PutUint32(block[:], 1)
	prf.Write(block[:])
	u := prf.Sum(nil)
	t := append([]byte(nil), u...)
	for i := 1; i < iter; i++ {
		prf.Reset()
		prf.Write(u)
		u = prf.Sum(nil)
		for j := range t {
			t[j] ^= u[j]
		}
	}
	return t[:keyLen]
}

type server struct {
	atespace          string
	templateNamespace string
	templateName      string
	client            controlClient
	passwords         passwordStore
	now               func() time.Time
}

// userSummary is the JSON shape the UI renders.
type userSummary struct {
	Name     string `json:"name"`
	Template string `json:"template"`
	Status   string `json:"status"`
	AteomPod string `json:"ateomPod"`
	IP       string `json:"ip"`
	Version  int64  `json:"version"`
	Age      string `json:"age"`
}

func (s *server) summarize(a *ateapipb.Actor) userSummary {
	pod := "<none>"
	if a.GetAteomPodNamespace() != "" {
		pod = a.GetAteomPodNamespace() + "/" + a.GetAteomPodName()
	}
	age := ""
	if ts := a.GetMetadata().GetCreateTime(); ts != nil {
		age = duration.HumanDuration(s.now().Sub(ts.AsTime()))
	}
	return userSummary{
		Name:     a.GetMetadata().GetName(),
		Template: a.GetActorTemplateNamespace() + "/" + a.GetActorTemplateName(),
		Status:   a.GetStatus().String(),
		AteomPod: pod,
		IP:       a.GetAteomPodIp(),
		Version:  a.GetMetadata().GetVersion(),
		Age:      age,
	}
}

func (s *server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListUsers(w, r)
	case http.MethodPost:
		s.handleCreateUser(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleUserSubresource dispatches /api/users/{name} (DELETE) and
// /api/users/{name}/password (POST).
func (s *server) handleUserSubresource(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/users/"), "/")
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(rest, "/password"):
		name := strings.TrimSuffix(rest, "/password")
		s.handleResetPassword(w, r, name)
	case r.Method == http.MethodDelete:
		s.handleDeleteUser(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleListUsers mirrors list-users.sh: ListActors in the configured atespace.
func (s *server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var all []*ateapipb.Actor
	pageToken := ""
	for {
		resp, err := s.client.ListActors(ctx, &ateapipb.ListActorsRequest{
			Atespace:  s.atespace,
			PageSize:  1000,
			PageToken: pageToken,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "列出用户失败: " + err.Error()})
			return
		}
		all = append(all, resp.GetActors()...)
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}

	users := make([]userSummary, 0, len(all))
	for _, a := range all {
		users = append(users, s.summarize(a))
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Name < users[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"atespace": s.atespace, "users": users})
}

// handleCreateUser mirrors create-user.sh: auto-create the atespace, then
// idempotently create-or-reuse the actor (history preserved on reuse).
func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if !dns1123Re.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("非法用户名 %q：必须匹配 DNS-1123：%s", name, dns1123),
		})
		return
	}

	// Resume can block while the workload boots, so allow more time than the
	// plain create path.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	// Ensure the atespace exists before creating actors in it.
	if _, err := s.client.GetAtespace(ctx, &ateapipb.GetAtespaceRequest{Atespace: &ateapipb.ObjectRef{Name: s.atespace}}); err != nil {
		if status.Code(err) != codes.NotFound {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询 atespace 失败: " + err.Error()})
			return
		}
		if _, err := s.client.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
			Atespace: &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: s.atespace}},
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建 atespace 失败: " + err.Error()})
			return
		}
	}

	// Idempotent create-or-reuse: if the actor exists, keep its history.
	ref := &ateapipb.ObjectRef{Atespace: s.atespace, Name: name}
	if _, err := s.client.GetActor(ctx, &ateapipb.GetActorRequest{Actor: ref}); err == nil {
		writeJSON(w, http.StatusOK, map[string]string{"message": "用户已存在，复用现有会话", "name": name})
		return
	} else if status.Code(err) != codes.NotFound {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询用户失败: " + err.Error()})
		return
	}

	actor, err := s.client.CreateActor(ctx, &ateapipb.CreateActorRequest{
		Actor: &ateapipb.Actor{
			Metadata:               &ateapipb.ResourceMetadata{Atespace: s.atespace, Name: name},
			ActorTemplateNamespace: s.templateNamespace,
			ActorTemplateName:      s.templateName,
		},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建用户失败: " + err.Error()})
		return
	}

	// Assign the user's access password now, before the actor is resumed.
	password, err := s.assignPassword(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成访问密码失败: " + err.Error()})
		return
	}

	// Resume immediately so the user's agent page is openable without the
	// lazy first-request 503. If resume fails (e.g. no free workers), the
	// actor still exists and the page will lazily resume on first access.
	resumed, err := s.client.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: ref})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"message":  "创建成功，但立即恢复失败（可稍后打开页面触发恢复）：" + err.Error(),
			"user":     s.summarize(actor),
			"password": password,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":  "创建成功",
		"user":     s.summarize(resumed.GetActor()),
		"password": password,
	})
}

// assignPassword generates a new password for the user, stores its hash and
// returns the plaintext (to be shown once).
func (s *server) assignPassword(name string) (string, error) {
	password, err := generatePassword()
	if err != nil {
		return "", err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return "", err
	}
	if err := s.passwords.Set(name, hash); err != nil {
		return "", err
	}
	return password, nil
}

// handleResetPassword generates a fresh password for an existing user and
// returns it. Used by the UI so a lost one-time password is recoverable.
func (s *server) handleResetPassword(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || strings.Contains(name, "/") || !dns1123Re.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法用户名"})
		return
	}
	password, err := s.assignPassword(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "重置密码失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message":  "密码已重置",
		"name":     name,
		"password": password,
	})
}

// handleDeleteUser mirrors the fixed delete-user.sh: suspend first (the API
// rejects deleting a RUNNING actor), then delete.
func (s *server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/users/"), "/")
	if name == "" || strings.Contains(name, "/") || !dns1123Re.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法用户名"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	ref := &ateapipb.ObjectRef{Atespace: s.atespace, Name: name}
	if _, err := s.client.GetActor(ctx, &ateapipb.GetActorRequest{Actor: ref}); err != nil {
		if status.Code(err) == codes.NotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询用户失败: " + err.Error()})
		return
	}

	// Suspend is idempotent: no-op when already SUSPENDED.
	if _, err := s.client.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: ref}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "挂起用户失败: " + err.Error()})
		return
	}
	if _, err := s.client.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "删除用户失败: " + err.Error()})
		return
	}
	if err := s.passwords.Delete(name); err != nil {
		// The actor is gone; a stale hash is harmless. Log rather than fail.
		log.Printf("deleting password for %q failed: %v", name, err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "删除成功", "name": name})
}

// handleAuth is the nginx auth_request target. It decides the target user from
// the original request URI (user-scoped path) or the mfcc_user cookie (system
// paths), then requires HTTP Basic credentials where the username matches the
// target user and the password matches the stored hash. Users without a stored
// password are denied until the admin assigns one via the management UI.
func (s *server) handleAuth(w http.ResponseWriter, r *http.Request) {
	username := targetUser(r)
	if username == "" {
		unauthorized(w, "missing user")
		return
	}
	hash, ok := s.passwords.Get(username)
	if !ok {
		// No password assigned: deny access until the admin generates one.
		unauthorized(w, "no password assigned")
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != username || !checkPassword(hash, pass) {
		unauthorized(w, "invalid credentials")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// targetUser resolves the user a request is addressed to. For user-scoped
// paths (/alice/...) it is the leading path segment; for system paths
// (/api/..., /ws/...) and the fallback it is the mfcc_user cookie.
func targetUser(r *http.Request) string {
	if uri := r.Header.Get("X-Original-URI"); uri != "" && !reservedPrefixRe.MatchString(uri) {
		if m := userPathRe.FindStringSubmatch(uri); m != nil {
			return m[1]
		}
	}
	return r.Header.Get("X-Mfcc-User")
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="mfcc agent", charset="UTF-8"`)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": msg})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "atespace": s.atespace})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// serverConfig carries runtime configuration (env-overridable).
type serverConfig struct {
	atespace           string
	templateNamespace  string
	templateName       string
	ateapiAddr         string
	ateapiCAFile       string
	ateapiTokenFile    string
	passwordsConfigMap string
	passwordsNamespace string
}

func serverConfigFromEnv() serverConfig {
	return serverConfig{
		atespace:           envOr("ATESPACE", defaultAtespace),
		templateNamespace:  envOr("ACTOR_TEMPLATE_NAMESPACE", defaultTemplateNamespace),
		templateName:       envOr("ACTOR_TEMPLATE_NAME", defaultTemplateName),
		ateapiAddr:         envOr("ATEAPI_ADDR", defaultAteapiAddr),
		ateapiCAFile:       envOr("ATEAPI_CA_FILE", defaultAteapiCAFile),
		ateapiTokenFile:    envOr("ATEAPI_TOKEN_FILE", defaultAteapiTokenFile),
		passwordsConfigMap: envOr("PASSWORDS_CONFIGMAP", defaultPasswordsConfigMap),
		passwordsNamespace: envOr("PASSWORDS_NAMESPACE", defaultPasswordsNamespace),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// dialAteAPI opens a gRPC connection to the ateapi Control service using the
// in-cluster auth pattern shared with ate-controller/atenet-router.
func dialAteAPI(cfg serverConfig) (ateapipb.ControlClient, *grpc.ClientConn, error) {
	dialOpts, err := ateapiauth.DialOptions(ateapiauth.ClientConfig{
		UseTokenAuth: true,
		CAFile:       cfg.ateapiCAFile,
		ServerName:   ateapiServerName,
		TokenFile:    cfg.ateapiTokenFile,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("building ateapi dial options: %w", err)
	}
	conn, err := grpc.NewClient(cfg.ateapiAddr, dialOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("creating grpc connection to ateapi: %w", err)
	}
	return ateapipb.NewControlClient(conn), conn, nil
}

func main() {
	cfg := serverConfigFromEnv()

	client, conn, err := dialAteAPI(cfg)
	if err != nil {
		log.Fatalf("dial ateapi: %v", err)
	}
	defer conn.Close()

	// In-cluster Kubernetes client for the password ConfigMap store.
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("building in-cluster k8s config: %v", err)
	}
	k8s, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatalf("building k8s client: %v", err)
	}
	passwords := newConfigMapPasswordStore(k8s, cfg.passwordsNamespace, cfg.passwordsConfigMap)
	loadCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := passwords.load(loadCtx); err != nil {
		cancel()
		log.Fatalf("loading password store: %v", err)
	}
	cancel()

	srv := &server{
		atespace:          cfg.atespace,
		templateNamespace: cfg.templateNamespace,
		templateName:      cfg.templateName,
		client:            client,
		passwords:         passwords,
		now:               time.Now,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/users", srv.handleUsers)
	mux.HandleFunc("/api/users/", srv.handleUserSubresource)
	mux.HandleFunc("/_mfcc_auth", srv.handleAuth)
	mux.HandleFunc("/healthz", srv.handleHealthz)

	addr := "0.0.0.0:" + envOr("PORT", defaultPort)
	log.Printf("mfcc-admin serving on %s (atespace=%s template=%s/%s)",
		addr, cfg.atespace, cfg.templateNamespace, cfg.templateName)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
