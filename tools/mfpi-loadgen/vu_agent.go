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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// agentPrompts are short on purpose: the model under test is the mock, and a
// real provider run (S8) should stay cheap.
var agentPrompts = []string{
	"Reply with the single word: ok",
	"Write one short sentence about load testing.",
	"List two colors, one per line.",
	"Say hello in three words.",
}

// runAgentVU is Profile B: a user who sends a prompt every turn-interval and
// waits for the agent to finish, on top of a live events WebSocket.
func (r *runner) runAgentVU(ctx context.Context, idx int, c *userClient, rnd *rand.Rand) {
	startWS(ctx, c, c.userPath("api/machines/local/events"), r.stats, r.cfg.scenario)

	sessionID := r.openSession(ctx, c, rnd)
	if sessionID == "" {
		log.Printf("vu %d (%s): could not open a session", idx, c.user.Name)
		return
	}
	if r.cfg.provider != "" {
		if err := r.selectModel(ctx, c, sessionID); err != nil {
			if !r.cfg.allowDefault {
				// Never silently fall back to a real provider model: that would
				// spend real budget and measure the wrong thing.
				log.Printf("vu %d (%s): %v; aborting this VU (pass -allow-default-model to override)",
					idx, c.user.Name, err)
				return
			}
			log.Printf("vu %d (%s): %v; CONTINUING WITH THE SESSION DEFAULT MODEL (-allow-default-model)",
				idx, c.user.Name, err)
		}
	}

	for ctx.Err() == nil {
		baseMsgs, baseTokens := 0, 0
		if st, code := r.sessionStatus(ctx, c, sessionID); code == 200 {
			baseMsgs = st.MessageCount
			baseTokens = st.Tokens.Total
		}

		body := map[string]any{
			"cwd":  r.cfg.cwd,
			"text": agentPrompts[rnd.Intn(len(agentPrompts))],
		}
		_, code := r.postJSON(ctx, c, "prompt", "api/machines/local/sessions/"+sessionID+"/prompt", body)
		if code != 200 {
			r.stats.RecordTurn(0, false)
		} else {
			r.waitTurn(ctx, c, sessionID, baseMsgs, baseTokens)
		}

		jitter := time.Duration(rnd.Int63n(int64(r.cfg.turnInterval/4) + 1))
		if !sleepCtx(ctx, r.cfg.turnInterval+jitter) {
			return
		}
	}
}

func (r *runner) setModel(ctx context.Context, c *userClient, sessionID string) int {
	body := map[string]string{
		"cwd":      r.cfg.cwd,
		"provider": r.cfg.provider,
		"modelId":  r.cfg.model,
	}
	_, code := r.postJSON(ctx, c, "model.set", "api/machines/local/sessions/"+sessionID+"/model", body)
	return code
}

// selectModel selects -provider/-model, retrying while the provider key is
// still being injected or the actor has not merged models.json yet. A custom
// provider only becomes selectable after mfpi-admin stores a key for it
// (docs/user_agent_api.md §4.6), which the reconcile loop may apply up to 30s
// after the actor starts.
func (r *runner) selectModel(ctx context.Context, c *userClient, sessionID string) error {
	deadline := time.Now().Add(r.cfg.modelTimeout)
	last := 0
	var lastInject time.Time
	for {
		// The provider only becomes selectable after (a) the actor's supervisor
		// merged models.json and (b) mfpi-admin stored a key for it. Both happen
		// asynchronously (up to ~60s after registration), so retry the injection
		// here instead of relying on the single startup attempt.
		if r.cfg.providerAPIKey != "" && time.Since(lastInject) > 15*time.Second {
			if err := r.ensureProviderKey(ctx, c.user.Name); err != nil {
				log.Printf("vu (%s): provider key not ready yet: %v", c.user.Name, err)
			}
			lastInject = time.Now()
		}
		last = r.setModel(ctx, c, sessionID)
		if last == 200 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("model %s/%s not selectable after %s (last status %d)",
				r.cfg.provider, r.cfg.model, r.cfg.modelTimeout, last)
		}
		if !sleepCtx(ctx, 3*time.Second) {
			return ctx.Err()
		}
	}
}

// ensureProviderKey stores -provider-api-key for user through mfpi-admin. It is
// idempotent and makes a custom provider selectable.
func (r *runner) ensureProviderKey(ctx context.Context, user string) error {
	base := strings.TrimRight(r.cfg.baseURL, "/")
	u := base + "/usermanagement/api/users/" + url.PathEscape(user) + "/apikey"
	payload, _ := json.Marshal(map[string]string{
		"apiKey":     r.cfg.providerAPIKey,
		"providerId": r.cfg.provider,
	})
	reqCtx, cancel := context.WithTimeout(ctx, r.cfg.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.SetBasicAuth(r.cfg.adminUser, r.cfg.adminPass)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("inject provider key: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// waitTurn polls the session status until the in-flight turn completes. It
// deliberately polls rather than reading the session WebSocket so the turn
// latency is measured independently of event-stream behaviour.
func (r *runner) waitTurn(ctx context.Context, c *userClient, sessionID string, baseCount, baseTokens int) {
	start := time.Now()
	deadline := start.Add(r.cfg.turnTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			r.stats.RecordTurn(time.Since(start), false)
			return
		}
		st, code := r.sessionStatus(ctx, c, sessionID)
		if code == 200 {
			if st.PendingAsk != nil {
				r.stats.RecordError("turn: agent is waiting for ask_user")
				r.stats.RecordTurn(time.Since(start), false)
				return
			}
			if !st.IsStreaming && st.MessageCount > baseCount && st.Tokens.Total > baseTokens {
				r.stats.RecordTurn(time.Since(start), true)
				return
			}
		}
		if !sleepCtx(ctx, 300*time.Millisecond) {
			r.stats.RecordTurn(time.Since(start), false)
			return
		}
	}
	r.stats.RecordTurn(time.Since(start), false)
}
