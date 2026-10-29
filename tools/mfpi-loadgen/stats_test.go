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
	"testing"
	"time"
)

func TestHistogramPercentiles(t *testing.T) {
	h := newHistogram()
	for i := 0; i < 100; i++ {
		h.observe(time.Duration(i) * time.Millisecond)
	}
	if h.count != 100 {
		t.Fatalf("count = %d, want 100", h.count)
	}
	// Bucket resolution: p50 falls in [0.025, 0.05) because bucket 0.05 is the
	// first upper bound >= 50ms.
	if got := h.percentile(0.50); got != 0.05 {
		t.Errorf("p50 = %v, want 0.05s (bucket upper bound)", got)
	}
	if got := h.percentile(0.99); got != 0.1 {
		t.Errorf("p99 = %v, want 0.1s", got)
	}
	if got := h.max; got != 0.099 {
		t.Errorf("max = %v, want 0.099s", got)
	}
}

func TestHistogramEmpty(t *testing.T) {
	h := newHistogram()
	if got := h.percentile(0.95); got != 0 {
		t.Errorf("empty p95 = %v, want 0", got)
	}
	if got := h.avg(); got != 0 {
		t.Errorf("empty avg = %v, want 0", got)
	}
}

func TestStatsSnapshot(t *testing.T) {
	s := newStats("idle-browser")
	s.Record("idle-browser", "poll.users", 10*time.Millisecond, 200, "")
	s.Record("idle-browser", "poll.users", 20*time.Millisecond, 200, "")
	s.Record("idle-browser", "poll.users", 30*time.Millisecond, 503, "")
	s.Record("idle-browser", "spa", 5*time.Millisecond, 0, "connection reset")
	s.RecordWS("connect")
	s.RecordWS("message")
	s.RecordTurn(1500*time.Millisecond, true)
	s.RecordTurn(0, false)

	r := s.Snapshot("http://example", 2, time.Now().Add(-time.Second))
	if r.Scenario != "idle-browser" || r.VUs != 2 {
		t.Fatalf("unexpected report header: %+v", r)
	}
	users := r.Endpoints["poll.users"]
	if users == nil || users.Count != 3 {
		t.Fatalf("poll.users = %+v, want count 3", users)
	}
	if users.Statuses["200"] != 2 || users.Statuses["503"] != 1 {
		t.Errorf("status counts = %v", users.Statuses)
	}
	if r.Endpoints["spa"].TransportErrors != 1 {
		t.Errorf("spa transport errors = %d, want 1", r.Endpoints["spa"].TransportErrors)
	}
	if r.WebSocket.Connects != 1 || r.WebSocket.Messages != 1 {
		t.Errorf("ws = %+v", r.WebSocket)
	}
	if r.Turns.Success != 1 || r.Turns.Failed != 1 {
		t.Errorf("turns = %+v", r.Turns)
	}
	if r.ErrorSamples["connection reset"] != 1 {
		t.Errorf("error samples = %v", r.ErrorSamples)
	}
}
