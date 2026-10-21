// Batch credential endpoints: apply one provider API key to many users in a
// single action (POST /api/apikeys) and clear it for many users in one action
// (POST /api/apikeys/clear).
//
// The per-user endpoints in main.go remain the primitive: this file only adds
// a fan-out over them. Two properties matter:
//
//   - The credential store is written once (SetMany/DeleteMany), so a batch
//     never leaves a half-written Secret.
//   - Only RUNNING actors are touched immediately. Suspended users get the key
//     persisted and pick it up when they resume (injectStoredKey on the resume
//     path, plus the background reconciler), so one batch does not wake the
//     whole fleet.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

const (
	// batchMaxUsers caps one request so a typo cannot fan out unboundedly.
	batchMaxUsers = 500
	// batchConcurrency is how many actors are driven in parallel. Actor calls
	// are I/O bound but each one drives pi-web's interactive login flow, so a
	// small pool is deliberate.
	batchConcurrency = 4
	// batchApplyTimeout bounds one user's actor calls.
	batchApplyTimeout = 20 * time.Second
	// batchOverallTimeout bounds the whole request. Keys are already persisted
	// when this expires, so retrying is safe and idempotent.
	batchOverallTimeout = 2 * time.Minute
)

// batchAPIKeyRequest is the body shared by both batch endpoints. Omitting
// `users` (null or empty) targets every user in the atespace.
type batchAPIKeyRequest struct {
	APIKey     string   `json:"apiKey"`
	ProviderID string   `json:"providerId"`
	Users      []string `json:"users"`
}

// batchFailure is one user the batch could not apply.
type batchFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// batchTarget is one resolved user: its status and the credential it had
// before this batch (needed to log out of a previous provider on a switch).
type batchTarget struct {
	name        string
	status      ateapipb.Actor_Status
	previous    providerKey
	hadPrevious bool
}

type batchSetResponse struct {
	ProviderID string         `json:"providerId"`
	Total      int            `json:"total"`
	OK         []string       `json:"ok"`
	Pending    []string       `json:"pending"`
	Failed     []batchFailure `json:"failed"`
	Message    string         `json:"message"`
}

type batchClearResponse struct {
	Total   int            `json:"total"`
	Cleared []string       `json:"cleared"`
	Pending []string       `json:"pending"`
	Failed  []batchFailure `json:"failed"`
	Message string         `json:"message"`
}

// handleBatchAPIKeys serves POST /api/apikeys and POST /api/apikeys/clear.
func (s *server) handleBatchAPIKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	clear := strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/clear")

	var req batchAPIKeyRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
		return
	}
	names, err := normalizeBatchNames(req.Users)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), batchOverallTimeout)
	defer cancel()

	targets, failed, err := s.resolveBatchTargets(ctx, names)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "列出用户失败: " + err.Error()})
		return
	}
	if clear {
		s.batchClearAPIKeys(ctx, w, targets, failed)
		return
	}
	s.batchSetAPIKeys(ctx, w, targets, failed, req)
}

// normalizeBatchNames trims, de-duplicates and validates the explicit target
// list. A nil/empty input means "all users" and is returned as nil.
func normalizeBatchNames(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > batchMaxUsers {
		return nil, fmt.Errorf("一次最多操作 %d 个用户", batchMaxUsers)
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		if !dns1123Re.MatchString(name) {
			return nil, fmt.Errorf("非法用户名 %q：必须匹配 DNS-1123", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("users 中没有有效用户名")
	}
	return out, nil
}

// resolveBatchTargets expands the requested names into concrete targets.
// Unknown names in an explicit list become failures (the rest still proceed);
// an empty list means every actor in the atespace, sorted by name.
func (s *server) resolveBatchTargets(ctx context.Context, names []string) ([]batchTarget, []batchFailure, error) {
	actors, err := s.listActors(ctx)
	if err != nil {
		return nil, nil, err
	}
	statusByName := make(map[string]ateapipb.Actor_Status, len(actors))
	for _, a := range actors {
		statusByName[a.GetMetadata().GetName()] = a.GetStatus()
	}

	failed := make([]batchFailure, 0)
	var wanted []string
	if len(names) == 0 {
		wanted = make([]string, 0, len(statusByName))
		for name := range statusByName {
			wanted = append(wanted, name)
		}
		sort.Strings(wanted)
	} else {
		for _, name := range names {
			if _, ok := statusByName[name]; !ok {
				failed = append(failed, batchFailure{Name: name, Error: "用户不存在"})
				continue
			}
			wanted = append(wanted, name)
		}
	}

	targets := make([]batchTarget, 0, len(wanted))
	for _, name := range wanted {
		prev, had := s.keys.Get(name)
		targets = append(targets, batchTarget{
			name:        name,
			status:      statusByName[name],
			previous:    prev,
			hadPrevious: had,
		})
	}
	return targets, failed, nil
}

// batchSetAPIKeys persists one key for every target, then injects it into the
// reachable (RUNNING) actors. Store failures abort the whole batch; per-user
// injection failures are reported but never roll back the stored credential.
func (s *server) batchSetAPIKeys(ctx context.Context, w http.ResponseWriter, targets []batchTarget, failed []batchFailure, req batchAPIKeyRequest) {
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "apiKey 不能为空"})
		return
	}
	providerID := strings.TrimSpace(req.ProviderID)
	if providerID == "" {
		providerID = defaultProviderID
	}
	provider, ok := lookupProvider(s.providerOptions(), providerID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "不支持的 provider: " + providerID + "（可选: " + s.providerIDList() + "）",
		})
		return
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, batchSetResponse{
			ProviderID: providerID,
			Total:      len(failed),
			OK:         []string{},
			Pending:    []string{},
			Failed:     failed,
			Message:    "没有可应用的用户",
		})
		return
	}

	batch := make(map[string]providerKey, len(targets))
	for _, t := range targets {
		batch[t.name] = providerKey{ProviderID: providerID, APIKey: key}
	}
	if err := s.keys.SetMany(batch); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存 Key 失败: " + err.Error()})
		return
	}

	okNames, pending, applyFailed := s.applyBatchKeys(ctx, targets, providerID, key)
	allFailed := append(failed, applyFailed...)
	sort.Slice(allFailed, func(i, j int) bool { return allFailed[i].Name < allFailed[j].Name })

	writeJSON(w, http.StatusOK, batchSetResponse{
		ProviderID: providerID,
		Total:      len(targets) + len(failed),
		OK:         okNames,
		Pending:    pending,
		Failed:     allFailed,
		Message: fmt.Sprintf("已保存 %d 个用户的 %s Key：%d 个运行中已注入，%d 个将在恢复后自动生效，%d 个失败",
			len(targets), provider.Name, len(okNames), len(pending), len(allFailed)),
	})
}

// applyBatchKeys drives the key into every RUNNING target with a bounded
// worker pool. Suspended targets are reported as pending: the reconciler (and
// the resume path) injects the stored key once the actor runs.
func (s *server) applyBatchKeys(ctx context.Context, targets []batchTarget, providerID, key string) (ok, pending []string, failed []batchFailure) {
	ok = []string{}
	pending = []string{}
	failed = []batchFailure{}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, batchConcurrency)

	for _, t := range targets {
		t := t
		if t.status != ateapipb.Actor_STATUS_RUNNING {
			pending = append(pending, t.name)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				failed = append(failed, batchFailure{Name: t.name, Error: "超时，可重试"})
				mu.Unlock()
				return
			}
			defer func() { <-sem }()

			callCtx, cancel := context.WithTimeout(ctx, batchApplyTimeout)
			defer cancel()
			if err := s.applyOneKey(callCtx, t, providerID, key); err != nil {
				mu.Lock()
				failed = append(failed, batchFailure{Name: t.name, Error: err.Error()})
				mu.Unlock()
				return
			}
			mu.Lock()
			ok = append(ok, t.name)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Strings(ok)
	sort.Strings(pending)
	return ok, pending, failed
}

// applyOneKey logs the actor out of a previous provider (when the provider is
// changing) and then stores the new key. The old-provider logout is best
// effort: the new key is still injected, and the stale login is reported.
func (s *server) applyOneKey(ctx context.Context, t batchTarget, providerID, apiKey string) error {
	host := actorHostname(t.name, s.atespace)
	if t.hadPrevious && t.previous.ProviderID != providerID {
		if err := s.actors.clearPersonalKey(ctx, host, t.previous.ProviderID); err != nil {
			if setErr := s.actors.setPersonalKey(ctx, host, providerID, apiKey); setErr != nil {
				return fmt.Errorf("退出旧 provider %s 失败，且注入新 Key 失败: %v；%v", t.previous.ProviderID, err, setErr)
			}
			return fmt.Errorf("新 Key 已注入，但退出旧 provider %s 失败: %v", t.previous.ProviderID, err)
		}
	}
	if err := s.actors.setPersonalKey(ctx, host, providerID, apiKey); err != nil {
		return fmt.Errorf("注入失败: %w", err)
	}
	return nil
}

// batchClearAPIKeys deletes the stored credentials in one Secret write, then
// logs out of the provider on every reachable RUNNING actor. Suspended users
// have nothing to log out of, so they are reported as pending.
func (s *server) batchClearAPIKeys(ctx context.Context, w http.ResponseWriter, targets []batchTarget, failed []batchFailure) {
	storedNames := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.hadPrevious {
			storedNames = append(storedNames, t.name)
		}
	}
	if err := s.keys.DeleteMany(storedNames); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "清除存储的 Key 失败: " + err.Error()})
		return
	}

	cleared, pending, applyFailed := s.clearBatchKeys(ctx, targets)
	allFailed := append(failed, applyFailed...)
	sort.Slice(allFailed, func(i, j int) bool { return allFailed[i].Name < allFailed[j].Name })

	writeJSON(w, http.StatusOK, batchClearResponse{
		Total:   len(targets) + len(failed),
		Cleared: cleared,
		Pending: pending,
		Failed:  allFailed,
		Message: fmt.Sprintf("已处理 %d 个用户：%d 个已清除，%d 个挂起（恢复后无凭据），%d 个失败",
			len(targets), len(cleared), len(pending), len(allFailed)),
	})
}

func (s *server) clearBatchKeys(ctx context.Context, targets []batchTarget) (cleared, pending []string, failed []batchFailure) {
	cleared = []string{}
	pending = []string{}
	failed = []batchFailure{}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, batchConcurrency)

	for _, t := range targets {
		t := t
		if !t.hadPrevious {
			// Nothing stored: the clear is already a no-op success.
			cleared = append(cleared, t.name)
			continue
		}
		if t.status != ateapipb.Actor_STATUS_RUNNING {
			pending = append(pending, t.name)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				failed = append(failed, batchFailure{Name: t.name, Error: "超时，可重试"})
				mu.Unlock()
				return
			}
			defer func() { <-sem }()

			callCtx, cancel := context.WithTimeout(ctx, batchApplyTimeout)
			defer cancel()
			host := actorHostname(t.name, s.atespace)
			if err := s.actors.clearPersonalKey(callCtx, host, t.previous.ProviderID); err != nil {
				mu.Lock()
				failed = append(failed, batchFailure{Name: t.name, Error: "已删除存储的 Key，但退出登录失败: " + err.Error()})
				mu.Unlock()
				return
			}
			mu.Lock()
			cleared = append(cleared, t.name)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Strings(cleared)
	sort.Strings(pending)
	return cleared, pending, failed
}
