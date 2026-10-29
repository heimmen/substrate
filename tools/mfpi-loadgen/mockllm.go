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
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

// mockConfig controls the fake model's timing and failure behaviour.
type mockConfig struct {
	addr       string
	model      string
	ttft       time.Duration
	chunkDelay time.Duration
	chunks     int
	errorRate  float64
	logBodies  bool
}

// mockServer implements just enough of the OpenAI chat-completions API for a
// pi-web provider: POST /v1/chat/completions (streaming and non-streaming),
// GET /v1/models and GET /healthz.
type mockServer struct {
	cfg mockConfig

	mu       sync.Mutex
	requests int64
}

func runMock(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve-mock", flag.ContinueOnError)
	cfg := mockConfig{}
	fs.StringVar(&cfg.addr, "addr", ":8080", "listen address")
	fs.StringVar(&cfg.model, "model", "mock-1", "model id to advertise")
	fs.DurationVar(&cfg.ttft, "ttft", 100*time.Millisecond, "delay before the first streamed token")
	fs.DurationVar(&cfg.chunkDelay, "chunk-delay", 10*time.Millisecond, "delay between streamed chunks")
	fs.IntVar(&cfg.chunks, "chunks", 32, "number of content chunks per response")
	fs.Float64Var(&cfg.errorRate, "error-rate", 0, "fraction of completion requests to fail with HTTP 500 (0-1)")
	fs.BoolVar(&cfg.logBodies, "log-bodies", false, "log request bodies (verbose)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.chunks < 0 {
		return fmt.Errorf("-chunks must be >= 0")
	}
	if cfg.errorRate < 0 || cfg.errorRate > 1 {
		return fmt.Errorf("-error-rate must be in [0,1]")
	}

	srv := &mockServer{cfg: cfg}
	httpSrv := &http.Server{Addr: cfg.addr, Handler: srv.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("mock llm listening on %s (model=%s ttft=%s chunk=%s chunks=%d error-rate=%.2f)",
		cfg.addr, cfg.model, cfg.ttft, cfg.chunkDelay, cfg.chunks, cfg.errorRate)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *mockServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "mfpi mock llm\n")
	})
	return mux
}

func (s *mockServer) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []any{map[string]any{
			"id":       s.cfg.model,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "mfpi-mock",
		}},
	})
}

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
}

func (s *mockServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.requests++
	n := s.requests
	s.mu.Unlock()

	if s.cfg.logBodies {
		log.Printf("[mock] request #%d model=%q stream=%v messages=%d body=%s", n, req.Model, req.Stream, len(req.Messages), truncate(string(body), 500))
	} else {
		log.Printf("[mock] request #%d model=%q stream=%v messages=%d", n, req.Model, req.Stream, len(req.Messages))
	}

	model := req.Model
	if model == "" {
		model = s.cfg.model
	}

	if s.cfg.errorRate > 0 && rand.Float64() < s.cfg.errorRate {
		writeJSONResponse(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": "mock llm: injected failure", "type": "server_error"},
		})
		return
	}

	if !req.Stream {
		writeJSONResponse(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": s.content()},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 16, "completion_tokens": s.cfg.chunks, "total_tokens": 16 + s.cfg.chunks},
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if !s.sleep(r.Context(), s.cfg.ttft) {
		return
	}
	s.writeChunk(w, flusher, model, map[string]any{"role": "assistant", "content": ""}, nil)
	for i := 0; i < s.cfg.chunks; i++ {
		if !s.sleep(r.Context(), s.cfg.chunkDelay) {
			return
		}
		s.writeChunk(w, flusher, model, map[string]any{"content": "token "}, nil)
	}
	// Final chunk carries finish_reason and usage (some clients only read usage
	// from the last chunk when stream_options.include_usage is set).
	s.writeChunkWithUsage(w, flusher, model)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *mockServer) content() string {
	out := make([]byte, 0, s.cfg.chunks*6)
	for i := 0; i < s.cfg.chunks; i++ {
		out = append(out, "token "...)
	}
	return string(out)
}

func (s *mockServer) writeChunk(w io.Writer, f http.Flusher, model string, delta map[string]any, finish any) {
	obj := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	b, _ := json.Marshal(obj)
	fmt.Fprintf(w, "data: %s\n\n", b)
	f.Flush()
}

func (s *mockServer) writeChunkWithUsage(w io.Writer, f http.Flusher, model string) {
	obj := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     16,
			"completion_tokens": s.cfg.chunks,
			"total_tokens":      16 + s.cfg.chunks,
		},
	}
	b, _ := json.Marshal(obj)
	fmt.Fprintf(w, "data: %s\n\n", b)
	f.Flush()
}

func (s *mockServer) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func writeJSONResponse(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
