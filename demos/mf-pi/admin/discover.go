// Provider model discovery for the mf-pi admin server.
//
// The usermanagement page's endpoint form offers a 「获取可用模型」 button: it
// asks the endpoint the operator just typed which models it serves, so adding a
// provider is one pass instead of save-then-return, and model ids come from the
// endpoint rather than from memory. The probe runs here rather than in the
// browser because the endpoint is a cluster-external URL and the console is a
// plain page with no CORS arrangement with it.
//
// This mirrors the tolerant reading pi-web's own discovery uses: an OpenAI-style
// `data` array or an Anthropic-style `models` object, with the field aliases
// gateways actually publish. A protocol this build cannot list is refused rather
// than guessed at — reporting an authentication failure as "no models" would be
// worse than saying so — and every failure keeps hand entry available.
//
// The probe target is an operator-supplied URL, so the request is bounded (one
// page, a byte ceiling, a timeout) and only http/https is accepted. The route
// sits behind the console's Basic auth: like every other admin endpoint it is
// operator-only and must not be exposed to end users.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// discoverMaxBytes refuses a reply larger than this. The ceiling holds on
	// the bytes actually read, not on what the server claims, and overflow
	// rejects instead of truncating: a truncated listing is not parseable.
	discoverMaxBytes = 4 << 20
	// discoverTimeout bounds one probe end to end.
	discoverTimeout = 20 * time.Second
	// anthropicVersion is the API version Anthropic's listing endpoint wants.
	anthropicVersion = "2023-06-01"
	// anthropicModelPage is the largest page that endpoint serves in one reply;
	// discovery reads one page and does not follow pagination.
	anthropicModelPage = 1000
)

// listableProtocols are the pi-ai protocols whose model listing this build can
// read. Anything else is refused by name rather than probed with the wrong
// shape.
var listableProtocols = map[string]bool{
	"openai-completions": true,
	"openai-responses":   true,
	"anthropic-messages": true,
}

// discoveredModel is one model an endpoint reported. Capacities are omitted
// when the endpoint did not publish them.
type discoveredModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	MaxTokens     int    `json:"maxTokens,omitempty"`
}

// listingURL joins the endpoint base with the protocol's listing path. The base
// is treated as a prefix rather than resolved as a URL, so a deployment path
// such as https://gateway.example/openai/v1 keeps its segments. OpenAI
// protocols list at {base}/models; Anthropic lists at the root's /v1/models,
// where the root is the base without trailing slashes and without one trailing
// /v1 segment, because gateways publish both spellings of the same root.
func listingURL(baseURL, api string) string {
	base := strings.TrimRight(baseURL, "/")
	if api == "anthropic-messages" {
		base = strings.TrimSuffix(base, "/v1")
		return base + "/v1/models?limit=" + fmt.Sprint(anthropicModelPage)
	}
	return base + "/models"
}

// handleDiscoverProviderModels answers POST /api/providers/discover.
func (s *server) handleDiscoverProviderModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req struct {
		BaseURL string `json:"baseUrl"`
		API     string `json:"api"`
		APIKey  string `json:"apiKey"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "baseUrl 不能为空"})
		return
	}
	if u, err := url.Parse(baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("baseUrl %q 非法：需要 http:// 或 https:// 开头的完整地址", baseURL)})
		return
	}
	api := strings.TrimSpace(req.API)
	if api == "" {
		api = registryAPIDefault
	}
	if !listableProtocols[api] {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("API 协议 %q 不支持探测模型列表；请手工填写模型", api),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	defer cancel()
	models, err := probeModelListing(ctx, baseURL, api, strings.TrimSpace(req.APIKey))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// probeModelListing performs the bounded GET and parses the reply. Every error
// it returns is already phrased for the console.
func probeModelListing(ctx context.Context, baseURL, api, apiKey string) ([]discoveredModel, error) {
	url := listingURL(baseURL, api)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("无法连接 %s", url)
	}
	req.Header.Set("accept", "application/json")
	if api == "anthropic-messages" {
		req.Header.Set("anthropic-version", anthropicVersion)
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
		}
	} else if apiKey != "" {
		req.Header.Set("authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法连接 %s", url)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%s 返回 %d；请检查 API Key", url, resp.StatusCode)
		}
		return nil, fmt.Errorf("%s 返回 %d", url, resp.StatusCode)
	}

	// Read one byte past the ceiling so an oversized reply is detectable
	// without buffering it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, discoverMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("无法连接 %s", url)
	}
	if len(body) > discoverMaxBytes {
		return nil, fmt.Errorf("%s 的响应超过 4MiB，已拒绝", url)
	}
	models, err := readModelListing(body)
	if err != nil {
		return nil, fmt.Errorf("%s %s", url, err.Error())
	}
	return models, nil
}

// readModelListing tolerates the two listing shapes gateways publish: an OpenAI
// `data` array, or an Anthropic `models` object keyed by model id. A present but
// empty listing is "this endpoint serves nothing this shape can name", which the
// console reports as such — distinct from a reply that carries neither key,
// which is a parse failure worth naming.
func readModelListing(body []byte) ([]discoveredModel, error) {
	const noListing = "的模型列表既没有 data 数组也没有 models 对象；请手工填写模型"

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, errors.New(noListing)
	}

	if raw, ok := envelope["data"]; ok {
		var rows []map[string]any
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, errors.New(noListing)
		}
		models := make([]discoveredModel, 0, len(rows))
		for _, row := range rows {
			if model, ok := discoveredFromRow(row, ""); ok {
				models = append(models, model)
			}
		}
		return models, nil
	}

	if raw, ok := envelope["models"]; ok {
		var entries map[string]map[string]any
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, errors.New(noListing)
		}
		models := make([]discoveredModel, 0, len(entries))
		for key, row := range entries {
			if model, ok := discoveredFromRow(row, key); ok {
				models = append(models, model)
			}
		}
		return models, nil
	}

	return nil, errors.New(noListing)
}

// discoveredFromRow maps one listing entry, naming the model by its id or — for
// the Anthropic shape, whose entries may omit one — by the key it was filed
// under. Rows with no usable id are skipped rather than named "".
func discoveredFromRow(row map[string]any, key string) (discoveredModel, bool) {
	id := firstLabel(row["id"], key)
	if id == "" {
		return discoveredModel{}, false
	}
	return discoveredModel{
		ID:            id,
		Name:          firstLabel(row["name"], row["display_name"], row["displayName"], id),
		ContextWindow: contextWindowOf(row),
		MaxTokens:     maxTokensOf(row),
	}, true
}

// firstLabel returns the first non-empty string among the candidates, or "".
func firstLabel(candidates ...any) string {
	for _, candidate := range candidates {
		if s, ok := candidate.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// contextWindowOf reads the context window from the flat names gateways use and
// from the nested `limit.context` enriched directories publish.
func contextWindowOf(row map[string]any) int {
	return firstPositive(
		row["contextWindow"], row["context_window"], row["context_length"], row["max_input_tokens"],
		nested(row, "limit", "context"),
	)
}

// maxTokensOf reads the output cap from its flat names and from the nested
// shapes that carry it. It is deliberately separate from contextWindowOf: the
// two live under different nested keys, and reading one as the other would
// report a context window as an output cap.
func maxTokensOf(row map[string]any) int {
	return firstPositive(
		row["maxOutputTokens"], row["max_output_tokens"], row["maxTokens"], row["max_tokens"],
		nested(row, "limit", "output"),
		nested(row, "top_provider", "max_completion_tokens"),
	)
}

// nested reads one field of a nested object, or nil when the parent is absent or
// not an object.
func nested(row map[string]any, parent, key string) any {
	child, ok := row[parent].(map[string]any)
	if !ok {
		return nil
	}
	return child[key]
}

// firstPositive returns the first candidate that is a positive integer, or 0.
func firstPositive(candidates ...any) int {
	for _, candidate := range candidates {
		if n, ok := positiveNumber(candidate); ok {
			return n
		}
	}
	return 0
}

// positiveNumber accepts a JSON number that is a positive integer.
func positiveNumber(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f <= 0 || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}
