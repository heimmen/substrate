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
//	GET    /api/users        list users (mirrors list-users.sh)
//	POST   /api/users        create a user (mirrors create-user.sh)
//	DELETE /api/users/{name} delete a user (mirrors delete-user.sh)
//	GET    /                 serve the embedded admin UI
//	GET    /healthz          readiness
//
// It authenticates in-cluster exactly like ate-controller/atenet-router: a
// projected service-account token (audience api.ate-system.svc) as Bearer
// credential and a projected ClusterTrustBundle as the server TLS roots.
// No RBAC beyond a valid SA token is required. See ADMIN_UI_PLAN.md.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/internal/ateapiauth"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/duration"
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

	// dns1123 is the same name rule Substrate enforces for actor names.
	dns1123 = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
)

var dns1123Re = regexp.MustCompile(dns1123)

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

type server struct {
	atespace          string
	templateNamespace string
	templateName      string
	client            controlClient
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

	// Resume immediately so the user's agent page is openable without the
	// lazy first-request 503. If resume fails (e.g. no free workers), the
	// actor still exists and the page will lazily resume on first access.
	resumed, err := s.client.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: ref})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"message": "创建成功，但立即恢复失败（可稍后打开页面触发恢复）：" + err.Error(),
			"user":    s.summarize(actor),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "创建成功", "user": s.summarize(resumed.GetActor())})
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
	writeJSON(w, http.StatusOK, map[string]string{"message": "删除成功", "name": name})
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
	atespace          string
	templateNamespace string
	templateName      string
	ateapiAddr        string
	ateapiCAFile      string
	ateapiTokenFile   string
}

func serverConfigFromEnv() serverConfig {
	return serverConfig{
		atespace:          envOr("ATESPACE", defaultAtespace),
		templateNamespace: envOr("ACTOR_TEMPLATE_NAMESPACE", defaultTemplateNamespace),
		templateName:      envOr("ACTOR_TEMPLATE_NAME", defaultTemplateName),
		ateapiAddr:        envOr("ATEAPI_ADDR", defaultAteapiAddr),
		ateapiCAFile:      envOr("ATEAPI_CA_FILE", defaultAteapiCAFile),
		ateapiTokenFile:   envOr("ATEAPI_TOKEN_FILE", defaultAteapiTokenFile),
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

	srv := &server{
		atespace:          cfg.atespace,
		templateNamespace: cfg.templateNamespace,
		templateName:      cfg.templateName,
		client:            client,
		now:               time.Now,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/users", srv.handleUsers)
	mux.HandleFunc("/api/users/", srv.handleDeleteUser)
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
