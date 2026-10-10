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

import "strings"

// providerOption is one selectable provider in the usermanagement UI.
type providerOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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
