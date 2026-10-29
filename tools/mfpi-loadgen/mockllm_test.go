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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestMock(t *testing.T, cfg mockConfig) *httptest.Server {
	t.Helper()
	srv := &mockServer{cfg: cfg}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestMockStreamsChatCompletion(t *testing.T) {
	ts := newTestMock(t, mockConfig{model: "mock-1", chunks: 3, ttft: 0, chunkDelay: 0})
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"mock-1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "data: [DONE]") {
		t.Errorf("missing [DONE] sentinel:\n%s", text)
	}
	if n := strings.Count(text, `"content":"token "`); n != 3 {
		t.Errorf("content chunks = %d, want 3\n%s", n, text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason stop:\n%s", text)
	}
}

func TestMockNonStreamCompletion(t *testing.T) {
	ts := newTestMock(t, mockConfig{model: "mock-1", chunks: 2})
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"mock-1","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content == "" {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestMockInjectedError(t *testing.T) {
	ts := newTestMock(t, mockConfig{model: "mock-1", chunks: 1, errorRate: 1})
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"mock-1","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestMockModels(t *testing.T) {
	ts := newTestMock(t, mockConfig{model: "mock-7"})
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.Contains(readAll(t, resp), `"id":"mock-7"`) {
		t.Error("model id missing from /v1/models")
	}
}

func TestMockSlowFirstTokenHonoursContext(t *testing.T) {
	ts := newTestMock(t, mockConfig{model: "mock-1", chunks: 100, ttft: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"mock-1","stream":true,"messages":[]}`))

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected the read to fail when the client context is cancelled")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("server did not stop promptly after cancellation: %s", elapsed)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
