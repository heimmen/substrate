// Curated list of providers the usermanagement UI can store a key for.
//
// pi-web (through its SDK) ships a registry of ~40 providers. Almost all of
// them are OpenAI-API-compatible and expose the same interactive api-key login,
// so any of them works with the plumbing in apikey.go — the admin only needs
// the provider id. We deliberately offer a short, curated list instead of all
// of them: the raw registry includes providers that need extra credential
// fields (Bedrock, Vertex, Cloudflare AI Gateway) or that are better used
// through OAuth.
//
// The list is overridable per environment with KEY_PROVIDERS, a
// comma-separated spec whose entries are either "id" or "id=Display Name",
// e.g.:
//
//	KEY_PROVIDERS="deepseek=DeepSeek,openai=OpenAI,zai=Z.AI (GLM)"
//
// A blank or unparsable spec falls back to defaultProviderOptions.
package main

import (
	"sort"
	"strings"
)

// providerOption is one selectable provider in the usermanagement UI.
type providerOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Custom marks providers that come from the admin-managed registry
	// (arbitrary OpenAI-compatible endpoints) rather than pi-web's built-in
	// registry.
	Custom bool `json:"custom,omitempty"`
	// BaseURL, API and Models describe a custom endpoint so the usermanagement
	// UI can list it without a second request. They are empty for built-ins.
	BaseURL string          `json:"baseUrl,omitempty"`
	API     string          `json:"api,omitempty"`
	Models  []registryModel `json:"models,omitempty"`
	// Pinned reports that the endpoint's model list is a closed set.
	Pinned bool `json:"pinned,omitempty"`
}

// defaultProviderID is the provider assumed when a request omits providerId and
// the one attributed to keys stored before multi-provider support.
const defaultProviderID = "deepseek"

// defaultProviderOptions is the curated provider list. Every entry is an
// OpenAI-API-compatible provider whose api-key login asks only for the key
// itself. Keep IDs in sync with the SDK registry (pi-ai providers/all.js).
var defaultProviderOptions = []providerOption{
	{ID: "deepseek", Name: "DeepSeek"},
	{ID: "openai", Name: "OpenAI"},
	{ID: "openrouter", Name: "OpenRouter"},
	{ID: "moonshotai", Name: "Moonshot AI (Kimi)"},
	{ID: "zai", Name: "Z.AI (GLM)"},
	{ID: "qwen-token-plan", Name: "Qwen (通义千问)"},
	{ID: "minimax", Name: "MiniMax"},
	{ID: "groq", Name: "Groq"},
	{ID: "mistral", Name: "Mistral"},
	{ID: "xai", Name: "xAI (Grok)"},
}

// parseProviderOptions parses a KEY_PROVIDERS spec ("id" or "id=Name" entries
// separated by commas). Blank entries are skipped; a spec with no usable entry
// yields fallback unchanged.
func parseProviderOptions(spec string, fallback []providerOption) []providerOption {
	var out []providerOption
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, name, found := strings.Cut(entry, "=")
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !found || strings.TrimSpace(name) == "" {
			name = id
		} else {
			name = strings.TrimSpace(name)
		}
		out = append(out, providerOption{ID: id, Name: name})
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// defaultProviderSpec renders defaultProviderOptions as the KEY_PROVIDERS
// fallback value, so an operator can copy the effective default from the
// manifests or the admin logs.
func defaultProviderSpec() string {
	parts := make([]string, 0, len(defaultProviderOptions))
	for _, p := range defaultProviderOptions {
		parts = append(parts, p.ID+"="+p.Name)
	}
	return strings.Join(parts, ",")
}

// builtinProviderIndex indexes the built-in curated providers by id, so a
// custom registry entry can be rejected when it would shadow one.
func builtinProviderIndex() map[string]providerOption {
	m := make(map[string]providerOption, len(defaultProviderOptions))
	for _, p := range defaultProviderOptions {
		m[p.ID] = p
	}
	return m
}

// providerOptions merges the built-in curated list with the admin-managed
// registry, which is what both the UI dropdown and providerId validation use.
// Registry entries are flagged Custom and ordered by id after the built-ins.
func (s *server) providerOptions() []providerOption {
	out := make([]providerOption, 0, len(s.providers)+4)
	out = append(out, s.providers...)
	if s.providerRegistry == nil {
		return out
	}
	reg := s.providerRegistry.list()
	ids := make([]string, 0, len(reg))
	for id := range reg {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, s.providerOptionFor(id, reg[id]))
	}
	return out
}

// providerOptionFor renders one registry entry as a UI option, including the
// endpoint details the usermanagement UI lists.
func (s *server) providerOptionFor(id string, p registryProvider) providerOption {
	name := p.Name
	if name == "" {
		name = id
	}
	return providerOption{ID: id, Name: name, Custom: true, BaseURL: p.BaseURL, API: p.API, Models: p.Models, Pinned: p.Pinned}
}

// lookupProvider returns the curated option for id, if it is selectable.
func lookupProvider(options []providerOption, id string) (providerOption, bool) {
	for _, p := range options {
		if p.ID == id {
			return p, true
		}
	}
	return providerOption{}, false
}

// providerLabel returns id's display name from the curated list, falling back
// to the id itself (e.g. for a key stored by an older configuration).
func providerLabel(options []providerOption, id string) string {
	if p, ok := lookupProvider(options, id); ok {
		return p.Name
	}
	return id
}
