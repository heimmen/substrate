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
	"context"
	"encoding/json"
	"math/rand"
	"net/url"
	"regexp"
	"strings"
)

// idlePollEndpoints mirrors the polling mix observed in the production nginx
// access log (see docs/stress_test_plan.md §2.4): unread/notices/users dominate,
// status/config are less frequent.
var idlePollEndpoints = []string{
	"api/machines/local/status",
	"api/users",
	"api/users",
	"api/machines/local/sessions/unread",
	"api/machines/local/sessions/unread",
	"api/machines/local/notices",
	"api/machines/local/notices",
	"api/config",
	"api/pi-web/status",
}

// runIdleVU is Profile A: a browser sitting on the agent page with a live
// WebSocket and periodic polling.
func (r *runner) runIdleVU(ctx context.Context, idx int, c *userClient, rnd *rand.Rand) {
	assets := r.discoverAssets(ctx, c)
	startWS(ctx, c, c.userPath("api/machines/local/events"), r.stats, r.cfg.scenario)

	var sessionID string
	cycle := 0
	for ctx.Err() == nil {
		cycle++

		ep := idlePollEndpoints[rnd.Intn(len(idlePollEndpoints))]
		r.get(ctx, c, pollTag(ep), ep, "application/json")

		if sessionID == "" || cycle%20 == 0 {
			sessionID = r.openSession(ctx, c, rnd)
		}
		if sessionID != "" && cycle%3 == 0 {
			r.readSession(ctx, c, sessionID)
		}
		if len(assets) > 0 && cycle%6 == 0 {
			// Absolute path already including the /<user> prefix.
			r.getPath(ctx, c, "static", assets[rnd.Intn(len(assets))], "*/*")
		} else if len(assets) == 0 && cycle%10 == 0 {
			// The first SPA fetch may have raced a loading page; retry.
			assets = r.discoverAssets(ctx, c)
		}

		if !sleepCtx(ctx, r.think(rnd)) {
			return
		}
	}
}

func pollTag(endpoint string) string {
	switch {
	case strings.HasSuffix(endpoint, "/status"):
		return "poll.status"
	case strings.HasSuffix(endpoint, "/unread"):
		return "poll.unread"
	case strings.HasSuffix(endpoint, "/notices"):
		return "poll.notices"
	case strings.HasSuffix(endpoint, "/api/users"):
		return "poll.users"
	case strings.HasSuffix(endpoint, "/config"):
		return "poll.config"
	case strings.HasSuffix(endpoint, "/pi-web/status"):
		return "poll.piweb"
	default:
		return "poll.other"
	}
}

var assetRefRe = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// discoverAssets loads the SPA once and extracts same-origin asset URLs so the
// load loop fetches the same static files a browser would.
func (r *runner) discoverAssets(ctx context.Context, c *userClient) []string {
	body, code := r.get(ctx, c, "spa", "", "text/html")
	if code != 200 {
		return nil
	}
	// A navigation can be rewritten to the "agent is loading" page with HTTP
	// 200 (see docs/user_agent_api.md §3.3); it has no asset references.
	if strings.Contains(string(body), "mfpi-page=loading") {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range assetRefRe.FindAllStringSubmatch(string(body), -1) {
		u := strings.TrimSpace(m[1])
		if u == "" || strings.HasPrefix(u, "http") || strings.HasPrefix(u, "//") ||
			strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "#") {
			continue
		}
		if !strings.Contains(u, "/assets/") && !strings.Contains(u, "pi-web-plugins") {
			continue
		}
		u = strings.TrimPrefix(u, ".")
		if !strings.HasPrefix(u, "/") {
			u = "/" + u
		}
		// Only same-origin asset paths; the SPA is served under /<user>/.
		path := "/" + c.user.Name + u
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

// openSession opens (or reuses) a session in the configured cwd. Fresh actors
// have no sessions, so a 200 with a new id is the common path.
func (r *runner) openSession(ctx context.Context, c *userClient, rnd *rand.Rand) string {
	// Reuse an existing session most of the time: reopening a page does not
	// create a new session in a browser.
	path := "api/machines/local/sessions?cwd=" + url.QueryEscape(r.cfg.cwd)
	body, code := r.get(ctx, c, "session.list", path, "application/json")
	if code == 200 {
		var sessions []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &sessions) == nil && len(sessions) > 0 {
			return sessions[rnd.Intn(len(sessions))].ID
		}
	}

	create := map[string]string{"cwd": r.cfg.cwd}
	resp, code := r.postJSON(ctx, c, "session.create", "api/machines/local/sessions", create)
	if code == 404 {
		// Older/newer gateways may not expose the machines/local alias for
		// mutations; fall back to the canonical path.
		resp, code = r.postJSON(ctx, c, "session.create", "api/sessions", create)
	}
	if code != 200 {
		return ""
	}
	var s struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &s); err != nil || s.ID == "" {
		r.stats.RecordError("session.create: bad response")
		return ""
	}
	return s.ID
}

// readSession fetches the per-session payloads a browser requests when opening
// a session (status, messages, stream snapshot).
func (r *runner) readSession(ctx context.Context, c *userClient, id string) {
	q := "?cwd=" + url.QueryEscape(r.cfg.cwd)
	base := "api/machines/local/sessions/" + id
	r.get(ctx, c, "session.status", base+"/status"+q, "application/json")
	r.get(ctx, c, "session.messages", base+"/messages"+q, "application/json")
	r.get(ctx, c, "session.snapshot", base+"/stream-snapshot"+q, "application/json")
}

func (r *runner) sessionStatus(ctx context.Context, c *userClient, id string) (*sessionStatus, int) {
	q := "?cwd=" + url.QueryEscape(r.cfg.cwd)
	body, code := r.get(ctx, c, "session.status", "api/machines/local/sessions/"+id+"/status"+q, "application/json")
	if code != 200 {
		return nil, code
	}
	var st sessionStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, code
	}
	return &st, code
}

type sessionStatus struct {
	IsStreaming  bool `json:"isStreaming"`
	MessageCount int  `json:"messageCount"`
	PendingAsk   any  `json:"pendingAsk"`
	Tokens       struct {
		Input  int `json:"input"`
		Output int `json:"output"`
		Total  int `json:"total"`
	} `json:"tokens"`
}
