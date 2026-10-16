// Admin-managed provider registry for the mf-pi admin server.
//
// pi-web learns about providers beyond its built-in registry from
// <agentDir>/models.json, which is how a deployment offers arbitrary
// OpenAI-compatible endpoints (SiliconFlow, an internal gateway, ...). Two
// platform facts shape how that file is produced:
//
//   - An actor cannot be handed a ConfigMap: ActorTemplate volumes support only
//     durableDir and externalVolumeTemplate, and VolumeMount has no subPath, so
//     a single file cannot be mounted at <agentDir>/models.json.
//   - An actor cannot identify itself either: ActorTemplate env supports only
//     literal values and SecretKeyRef (no downward API), the sandbox hostname is
//     the fixed string "runsc", and workers are multiplexed so several actors
//     share a pod IP. The registry is therefore deployment-wide, not per user;
//     what stays per user is the credential (see apikey.go).
//
// So the actors pull the document over HTTP from the admin, exactly like
// managed skills, and the admin is the source of truth: the usermanagement UI
// adds and removes endpoints through /api/providers, and the registry is
// persisted in the ConfigMap the manifests pre-create.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// registryDataKey is the ConfigMap key holding the models.json document.
	registryDataKey = "provider-models.json"
	// registryAPIDefault is the pi-ai API implementation used for custom
	// providers unless the operator picks another one. It covers every
	// OpenAI-compatible server (vLLM, Moonshot, Qwen, OpenRouter, Zhipu,
	// Ollama, LM Studio, ...).
	registryAPIDefault = "openai-completions"
	// registryProviderIDRe is the provider/template-id charset pi-web and the
	// admin both accept; kept conservative so ids are safe in URLs and JSON.
	registryProviderIDRe = `^[a-z0-9][a-z0-9._-]*$`
)

var registryProviderIDPattern = regexp.MustCompile(registryProviderIDRe)

// registryModel is one model offered by a custom provider. Only the id is
// required; pi-web fills in sensible defaults for everything else. The fields
// mirror the subset of pi-ai's model definition the usermanagement UI edits.
type registryModel struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// ContextWindow and MaxTokens are token counts; zero means "not declared",
	// in which case pi-web inherits whatever the route or catalog provides.
	ContextWindow int `json:"contextWindow,omitempty"`
	MaxTokens     int `json:"maxTokens,omitempty"`
	// Input lists accepted modalities (text, image). Empty means "not
	// declared" so the route's own default applies.
	Input []string `json:"input,omitempty"`
}

// modelInputModalities are the modalities a declaration may name.
var modelInputModalities = map[string]bool{"text": true, "image": true}

// registryProvider is one custom OpenAI-compatible endpoint.
type registryProvider struct {
	Name    string          `json:"name,omitempty"`
	BaseURL string          `json:"baseUrl"`
	API     string          `json:"api,omitempty"`
	Models  []registryModel `json:"models"`
	// Pinned marks the model list as a closed set: while it stays pinned the
	// list cannot grow, and the console shows nothing outside it. It is an
	// admin-only field — see actorWireProvider for why it must never be written
	// into the document the actors read.
	Pinned bool `json:"pinned,omitempty"`
}

// actorWireProvider is one provider as pi-web reads it. pi-web validates
// models.json against a strict schema that rejects unknown provider keys, so an
// admin-only field leaking in here would invalidate the file the actors merge —
// hence a separate projection rather than sharing registryProvider.
type actorWireProvider struct {
	Name    string          `json:"name,omitempty"`
	BaseURL string          `json:"baseUrl"`
	API     string          `json:"api,omitempty"`
	Models  []registryModel `json:"models"`
}

// actorWireDocument is the models.json document served to actors.
type actorWireDocument struct {
	Providers map[string]actorWireProvider `json:"providers"`
}

// registryDocument is the on-disk/wire shape of the registry (a models.json
// document, so it can be served to pi-web verbatim).
type registryDocument struct {
	Providers map[string]registryProvider `json:"providers"`
}

// providerRegistryStore is the persistence surface for the registry.
// Implemented by configMapProviderRegistry in production and by a fake in tests.
type providerRegistryStore interface {
	// load refreshes the in-memory copy from the ConfigMap. A missing
	// ConfigMap is not an error (the store simply stays empty).
	load(ctx context.Context) error
	// list returns a copy of the registry keyed by provider id.
	list() map[string]registryProvider
	// set stores (creates or replaces) one provider.
	set(ctx context.Context, id string, p registryProvider) error
	// remove deletes one provider, reporting whether it existed.
	remove(ctx context.Context, id string) (bool, error)
	// document renders the registry as a models.json document.
	document() registryDocument
}

// configMapProviderRegistry persists the registry in a Kubernetes ConfigMap,
// mirroring it into memory so reads never hit the API server. Like the other
// stores it only ever Get→Updates: the manifest pre-creates the ConfigMap and
// the admin SA's RBAC grants get/update on it by name.
type configMapProviderRegistry struct {
	clientset kubernetes.Interface
	namespace string
	name      string

	mu   sync.RWMutex
	data map[string]registryProvider
}

func newConfigMapProviderRegistry(clientset kubernetes.Interface, namespace, name string) *configMapProviderRegistry {
	return &configMapProviderRegistry{
		clientset: clientset,
		namespace: namespace,
		name:      name,
		data:      map[string]registryProvider{},
	}
}

// parseRegistryDocument decodes a models.json document into providers. A blank
// document (or one without providers) is an empty registry, not an error, so an
// untouched seed ConfigMap is valid.
func parseRegistryDocument(raw string) (map[string]registryProvider, error) {
	if raw == "" {
		return map[string]registryProvider{}, nil
	}
	var doc registryDocument
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("registry is not valid JSON: %w", err)
	}
	out := make(map[string]registryProvider, len(doc.Providers))
	for id, p := range doc.Providers {
		out[id] = p
	}
	return out, nil
}

func marshalRegistryDocument(data map[string]registryProvider) (string, error) {
	if data == nil {
		data = map[string]registryProvider{}
	}
	b, err := json.MarshalIndent(registryDocument{Providers: data}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// marshalActorRegistryDocument renders the document the actors pull: provider
// definitions only, with every admin-only field projected away.
func marshalActorRegistryDocument(data map[string]registryProvider) (string, error) {
	wire := make(map[string]actorWireProvider, len(data))
	for id, p := range data {
		wire[id] = actorWireProvider{Name: p.Name, BaseURL: p.BaseURL, API: p.API, Models: p.Models}
	}
	b, err := json.MarshalIndent(actorWireDocument{Providers: wire}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

func (r *configMapProviderRegistry) load(ctx context.Context) error {
	cm, err := r.clientset.CoreV1().ConfigMaps(r.namespace).Get(ctx, r.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.mu.Lock()
			r.data = map[string]registryProvider{}
			r.mu.Unlock()
			return nil
		}
		return err
	}
	data, err := parseRegistryDocument(cm.Data[registryDataKey])
	if err != nil {
		return fmt.Errorf("configmap %s/%s: %w", r.namespace, r.name, err)
	}
	r.mu.Lock()
	r.data = data
	r.mu.Unlock()
	return nil
}

func (r *configMapProviderRegistry) list() map[string]registryProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]registryProvider, len(r.data))
	for k, v := range r.data {
		out[k] = v
	}
	return out
}

func (r *configMapProviderRegistry) document() registryDocument {
	return registryDocument{Providers: r.list()}
}

func (r *configMapProviderRegistry) set(ctx context.Context, id string, p registryProvider) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]registryProvider, len(r.data)+1)
	for k, v := range r.data {
		next[k] = v
	}
	next[id] = p
	if err := r.persist(ctx, next); err != nil {
		return err
	}
	r.data = next
	return nil
}

func (r *configMapProviderRegistry) remove(ctx context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.data[id]; !ok {
		return false, nil
	}
	next := make(map[string]registryProvider, len(r.data)-1)
	for k, v := range r.data {
		if k != id {
			next[k] = v
		}
	}
	if err := r.persist(ctx, next); err != nil {
		return false, err
	}
	r.data = next
	return true, nil
}

func (r *configMapProviderRegistry) persist(ctx context.Context, data map[string]registryProvider) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	raw, err := marshalRegistryDocument(data)
	if err != nil {
		return err
	}
	cm, err := r.clientset.CoreV1().ConfigMaps(r.namespace).Get(ctx, r.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("registry ConfigMap %s/%s not found; apply the mf-pi manifests to pre-create it", r.namespace, r.name)
		}
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[registryDataKey] = raw
	_, err = r.clientset.CoreV1().ConfigMaps(r.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// validateRegistryProvider checks an operator-supplied provider definition and
// returns the normalized form.
func validateRegistryProvider(id string, p registryProvider, reserved map[string]providerOption) (registryProvider, error) {
	if id == "" {
		return p, fmt.Errorf("provider id 不能为空")
	}
	if !registryProviderIDPattern.MatchString(id) {
		return p, fmt.Errorf("provider id %q 非法：需匹配 %s", id, registryProviderIDRe)
	}
	if _, clash := reserved[id]; clash {
		return p, fmt.Errorf("provider id %q 与内置 provider 冲突，请换一个", id)
	}
	if p.Name == "" {
		p.Name = id
	}
	if p.BaseURL == "" {
		return p, fmt.Errorf("baseUrl 不能为空")
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return p, fmt.Errorf("baseUrl %q 非法：需要 http:// 或 https:// 开头的完整地址", p.BaseURL)
	}
	if p.API == "" {
		p.API = registryAPIDefault
	}
	if len(p.Models) == 0 {
		return p, fmt.Errorf("至少要指定一个模型名")
	}
	models := make([]registryModel, 0, len(p.Models))
	seen := map[string]bool{}
	for _, m := range p.Models {
		if m.ID == "" {
			return p, fmt.Errorf("模型名不能为空")
		}
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if m.Name == "" {
			m.Name = m.ID
		}
		for _, modality := range m.Input {
			if !modelInputModalities[modality] {
				return p, fmt.Errorf("模型 %q 的输入类型只能是 text 或 image", m.ID)
			}
		}
		models = append(models, m)
	}
	p.Models = models
	return p, nil
}

// checkPinnedGrowth refuses a write that adds a model to an endpoint whose list
// is still pinned. The point of the flag is that the set is closed, and the
// console cannot be the only thing enforcing it: the API has to hold the line
// too. Extending the list is a deliberate two-step — save it unpinned, edit,
// pin it again — which is exactly the escape hatch the console documents.
func checkPinnedGrowth(id string, existing, next registryProvider) error {
	if !existing.Pinned || !next.Pinned {
		return nil
	}
	allowed := make(map[string]bool, len(existing.Models))
	for _, m := range existing.Models {
		allowed[m.ID] = true
	}
	for _, m := range next.Models {
		if !allowed[m.ID] {
			return fmt.Errorf("该端点已锁定模型列表，只能使用白名单内的模型：%s", m.ID)
		}
	}
	return nil
}

// modelCapacityError names the offending model in a capacity fault, so a form
// with several rows points at the row to fix.
func modelCapacityError(id string, contextWindow bool) error {
	if contextWindow {
		return fmt.Errorf("模型 %q 的上下文窗口必须是正整数", id)
	}
	return fmt.Errorf("模型 %q 的最大输出 token 数必须是正整数", id)
}

// handleInternalProviderModels serves the admin-managed registry to the actors'
// pull loop as a models.json document. Cluster-internal only: like
// /internal/skills/*, it must never be exposed through the gateway (nginx only
// routes /usermanagement/ and /<user>/).
func (s *server) handleInternalProviderModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	data := map[string]registryProvider{}
	if s.providerRegistry != nil {
		data = s.providerRegistry.list()
	}
	raw, err := marshalActorRegistryDocument(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = io.WriteString(w, raw)
}
