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

// Per-user DeepSeek API-key plumbing for the mf-pi admin server.
//
// The platform cannot inject a per-actor env var or Secret: container env is
// frozen into a Full snapshot at suspend and restored verbatim on resume, and
// there is no external path into an actor's filesystem. Instead pi-web re-reads
// its credential file (<agentDir>/auth.json) on every model call, and a stored
// credential outranks the DEEPSEEK_API_KEY env. So a per-user key is set by
// driving pi-web's own api-key login flow over the atenet router, which writes
// auth.json inside the actor. Since each mf-pi user is one actor, this is
// exactly per-user. Keys are also persisted to a pre-created Secret so the UI
// can show set/unset and keys survive actor delete+recreate.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// keyStore persists per-user provider API keys. Implemented by secretKeyStore
// in production and by a fake in tests.
type keyStore interface {
	// Get returns the stored key for the user, and whether one exists.
	Get(name string) (key string, ok bool)
	// Set stores (overwrites) the key for the user.
	Set(name, key string) error
	// Delete removes any stored key for the user (idempotent).
	Delete(name string) error
}

// secretKeyStore stores per-user keys in a Kubernetes Secret (username -> key),
// mirroring them into an in-memory map. It mirrors configMapPasswordStore but
// is backed by a Secret and only ever Get→Updates (never Creates): the admin
// SA's RBAC grants get/update on the pre-created Secret and cannot create it.
type secretKeyStore struct {
	clientset kubernetes.Interface
	namespace string
	name      string

	mu   sync.RWMutex
	data map[string]string // username -> key
}

func newSecretKeyStore(clientset kubernetes.Interface, namespace, name string) *secretKeyStore {
	return &secretKeyStore{
		clientset: clientset,
		namespace: namespace,
		name:      name,
		data:      map[string]string{},
	}
}

// load populates the in-memory map from the Secret. A missing Secret is
// treated as an empty store (not an error) so the server can start before the
// manifests have created it.
func (s *secretKeyStore) load(ctx context.Context) error {
	sec, err := s.clientset.CoreV1().Secrets(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = make(map[string]string, len(sec.Data))
	for k, v := range sec.Data {
		s.data[k] = string(v)
	}
	return nil
}

func (s *secretKeyStore) Get(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.data[name]
	return k, ok
}

func (s *secretKeyStore) Set(name, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]string, len(s.data)+1)
	for k, v := range s.data {
		next[k] = v
	}
	next[name] = key
	if err := s.persist(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *secretKeyStore) Delete(name string) error {
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

// persist writes the keys to the Secret. It only ever Get→Updates: the admin
// SA is not granted create, and the Secret is pre-created in the manifests.
// On NotFound it returns a clear error telling the operator to apply the
// manifests.
func (s *secretKeyStore) persist(data map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sec, err := s.clientset.CoreV1().Secrets(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("key Secret %s/%s not found; apply the mf-pi manifests to pre-create it", s.namespace, s.name)
		}
		return err
	}
	dataBytes := make(map[string][]byte, len(data))
	for k, v := range data {
		dataBytes[k] = []byte(v)
	}
	sec.Data = dataBytes
	_, err = s.clientset.CoreV1().Secrets(s.namespace).Update(ctx, sec, metav1.UpdateOptions{})
	return err
}

// actorAuthClient drives a per-user key into an actor's pi-web auth store.
// host is the actor's router hostname (see actorHostname).
type actorAuthClient interface {
	setPersonalKey(ctx context.Context, host, apiKey string) error
	clearPersonalKey(ctx context.Context, host string) error
}

// httpActorAuthClient implements actorAuthClient over the atenet router. Each
// request is addressed to the router (base) with the per-actor Host header set,
// so the router forwards it to the target actor's pi-web on port 80. Paths
// carry no /<user>/ prefix (that prefix is what nginx strips upstream).
type httpActorAuthClient struct {
	base string
	hc   *http.Client
}

func newHTTPActorAuthClient(base string) *httpActorAuthClient {
	return &httpActorAuthClient{
		base: base,
		// No global client timeout: the per-request context bounds the whole
		// flow. Only per-hop transport timeouts guard against a wedged dial or
		// an upstream that never sends response headers.
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       30 * time.Second,
			},
		},
	}
}

// actorHostSuffix is the router host suffix for an actor: a Host header of
// <name>.<atespace>.actors.resources.substrate.ate.dev routes to the actor's
// workload. Kept next to actorHostname (its only consumer); both are shared
// across the mf-pi prod (atespace mfpi) and test (atespace mfpi-test) servers.
const actorHostSuffix = ".actors.resources.substrate.ate.dev"

// actorHostname builds the router Host header for a user's actor. The atespace
// is required (prod uses mfpi, test uses mfpi-test).
func actorHostname(name, atespace string) string {
	return name + "." + atespace + actorHostSuffix
}

// setPersonalKey drives pi-web's api-key login flow for the deepseek provider
// and verifies the key is stored.
func (c *httpActorAuthClient) setPersonalKey(ctx context.Context, host, apiKey string) error {
	if err := c.waitForWeb(ctx, host); err != nil {
		return err
	}

	var state authFlowState
	if err := c.doJSON(ctx, host, http.MethodPost, "/api/machines/local/auth/api-key/interactive", map[string]string{"providerId": "deepseek"}, &state); err != nil {
		return fmt.Errorf("start api-key login: %w", err)
	}
	if state.FlowID == "" {
		return fmt.Errorf("api-key login flow missing flowId")
	}

	if state.Prompt != nil {
		return c.respondAndVerify(ctx, host, state.FlowID, state.Prompt.RequestID, apiKey)
	}
	prompt, err := c.pollForRequestID(ctx, host, state.FlowID)
	if err != nil {
		return err
	}
	return c.respondAndVerify(ctx, host, state.FlowID, prompt.RequestID, apiKey)
}

func (c *httpActorAuthClient) clearPersonalKey(ctx context.Context, host string) error {
	if err := c.waitForWeb(ctx, host); err != nil {
		return err
	}
	var resp struct {
		Accepted bool `json:"accepted"`
	}
	if err := c.doJSON(ctx, host, http.MethodPost, "/api/machines/local/auth/logout", map[string]string{"providerId": "deepseek"}, &resp); err != nil {
		return fmt.Errorf("logout deepseek: %w", err)
	}
	if !resp.Accepted {
		return fmt.Errorf("deepseek logout not accepted")
	}
	return c.verifyProvider(ctx, host, "")
}

// respondAndVerify submits the api key to the flow and checks the resulting
// provider state is "stored".
func (c *httpActorAuthClient) respondAndVerify(ctx context.Context, host, flowID, requestID, apiKey string) error {
	var state authFlowState
	if err := c.doJSON(ctx, host, http.MethodPost, "/api/machines/local/auth/oauth/"+flowID+"/respond", map[string]string{
		"requestId": requestID,
		"value":     apiKey,
	}, &state); err != nil {
		return fmt.Errorf("respond to api-key login: %w", err)
	}
	if state.Status != "complete" {
		return fmt.Errorf("api-key login flow ended in status %q (error: %s)", state.Status, state.Error)
	}
	if err := c.verifyProvider(ctx, host, "stored"); err != nil {
		return fmt.Errorf("verify stored key: %w", err)
	}
	return nil
}

// waitForWeb polls the providers endpoint until it answers 200. Besides
// returning the provider list, the endpoint doubles as the web-ready probe: a
// freshly resumed actor takes a moment to serve.
func (c *httpActorAuthClient) waitForWeb(ctx context.Context, host string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		if _, err := c.getProviders(ctx, host); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for actor web to be ready: %w (last: %v)", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

// pollForRequestID polls the flow until its prompt.requestId appears.
func (c *httpActorAuthClient) pollForRequestID(ctx context.Context, host, flowID string) (*authPrompt, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var state authFlowState
		if err := c.doJSON(ctx, host, http.MethodGet, "/api/machines/local/auth/oauth/"+flowID, nil, &state); err != nil {
			return nil, err
		}
		if state.Prompt != nil {
			return state.Prompt, nil
		}
		if state.Status == "error" || state.Status == "cancelled" {
			return nil, fmt.Errorf("auth flow %q ended in status %q (error: %s)", flowID, state.Status, state.Error)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for api-key login prompt (status %q)", state.Status)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled while waiting for prompt: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// verifyProvider fetches the providers list and checks the deepseek entry's
// status.source. want=="" asserts it is no longer "stored".
func (c *httpActorAuthClient) verifyProvider(ctx context.Context, host, want string) error {
	providers, err := c.getProviders(ctx, host)
	if err != nil {
		return err
	}
	var found *authProvider
	for i := range providers {
		if providers[i].ID == "deepseek" {
			found = &providers[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("deepseek provider not present in auth providers list")
	}
	if want != "" && found.Status.Source != want {
		return fmt.Errorf("expected deepseek status.source=%q, got %q (configured=%v)", want, found.Status.Source, found.Status.Configured)
	}
	if want == "" && found.Status.Source == "stored" {
		return fmt.Errorf("deepseek still shows status.source=stored after clear")
	}
	return nil
}

func (c *httpActorAuthClient) getProviders(ctx context.Context, host string) ([]authProvider, error) {
	var resp struct {
		Providers []authProvider `json:"providers"`
	}
	if err := c.doJSON(ctx, host, http.MethodGet, "/api/machines/local/auth/providers?mode=login&authType=api_key", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Providers, nil
}

// doJSON performs one JSON request against the router with the per-actor Host
// header, decoding the response into out (if non-nil).
func (c *httpActorAuthClient) doJSON(ctx context.Context, host, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s returned %d: %s", method, path, resp.StatusCode, truncate(string(data), 300))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding %s response: %w", path, err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// authProvider mirrors pi-web's AuthProviderOption (a subset).
type authProvider struct {
	ID     string             `json:"id"`
	Name   string             `json:"name"`
	Status authProviderStatus `json:"status"`
}

type authProviderStatus struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source"`
}

// authPrompt mirrors the prompt block of pi-web's OAuthFlowState.
type authPrompt struct {
	RequestID string `json:"requestId"`
}

// authFlowState mirrors pi-web's OAuthFlowState (a subset).
type authFlowState struct {
	FlowID string      `json:"flowId"`
	Status string      `json:"status"`
	Prompt *authPrompt `json:"prompt"`
	Error  string      `json:"error"`
}
