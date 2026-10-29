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
	"net/http"
)

// getPath performs a GET against an absolute path (already including the
// /<user> prefix) and records the result.
func (r *runner) getPath(ctx context.Context, c *userClient, tag, path, accept string) ([]byte, int) {
	body, code, dur, err := c.do(ctx, http.MethodGet, path, nil, accept, "", r.cfg.requestTimeout)
	r.stats.Record(r.cfg.scenario, tag, dur, code, errString(err))
	return body, code
}

// get performs a GET on a user-relative path ("api/...").
func (r *runner) get(ctx context.Context, c *userClient, tag, rest, accept string) ([]byte, int) {
	return r.getPath(ctx, c, tag, c.userPath(rest), accept)
}

// postJSON performs a JSON POST on a user-relative path and records the result.
func (r *runner) postJSON(ctx context.Context, c *userClient, tag, rest string, v any) ([]byte, int) {
	payload, err := json.Marshal(v)
	if err != nil {
		r.stats.RecordError("marshal: " + err.Error())
		return nil, 0
	}
	body, code, dur, err := c.do(ctx, http.MethodPost, c.userPath(rest), payload, "application/json", "application/json", r.cfg.requestTimeout)
	r.stats.Record(r.cfg.scenario, tag, dur, code, errString(err))
	return body, code
}
