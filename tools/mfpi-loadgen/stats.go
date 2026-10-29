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
	"fmt"
	"sort"
	"sync"
	"time"
)

// latencyBuckets are the upper bounds (seconds) of the latency histogram used
// both for percentile calculation and for the Prometheus exposition.
var latencyBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1,
	0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
}

type histogram struct {
	counts []int64 // len(latencyBuckets)+1; the last bucket is +Inf
	count  int64
	sum    float64
	min    float64
	max    float64
}

func newHistogram() *histogram {
	return &histogram{counts: make([]int64, len(latencyBuckets)+1)}
}

func (h *histogram) observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	v := d.Seconds()
	if h.count == 0 || v < h.min {
		h.min = v
	}
	if v > h.max {
		h.max = v
	}
	h.count++
	h.sum += v
	idx := sort.SearchFloat64s(latencyBuckets, v)
	h.counts[idx]++
}

// percentile returns the upper bound of the bucket containing the p-th value.
// Bucket-resolution is good enough for capacity decisions; use the raw report
// JSON if exact sample values are needed.
func (h *histogram) percentile(p float64) float64 {
	if h.count == 0 {
		return 0
	}
	target := p * float64(h.count)
	var cum int64
	for i, c := range h.counts {
		cum += c
		if float64(cum) >= target {
			if i < len(latencyBuckets) {
				return latencyBuckets[i]
			}
			return h.max
		}
	}
	return h.max
}

func (h *histogram) avg() float64 {
	if h.count == 0 {
		return 0
	}
	return h.sum / float64(h.count)
}

type endpointKey struct {
	scenario string
	endpoint string
}

// Stats is the thread-safe collector shared by all virtual users.
type Stats struct {
	mu         sync.Mutex
	scenario   string
	hists      map[endpointKey]*histogram
	statuses   map[endpointKey]map[int]int64
	transport  map[endpointKey]int64
	errors     map[string]int64
	turns      *histogram
	turnOK     int64
	turnFail   int64
	wsConnects int64
	wsRecon    int64
	wsMessages int64
	wsErrors   int64
}

func newStats(scenario string) *Stats {
	return &Stats{
		scenario:  scenario,
		hists:     make(map[endpointKey]*histogram),
		statuses:  make(map[endpointKey]map[int]int64),
		transport: make(map[endpointKey]int64),
		errors:    make(map[string]int64),
		turns:     newHistogram(),
	}
}

// Record stores one HTTP request outcome. A status of 0 means a transport-level
// failure (timeout, connection reset, ...); errMsg is recorded as a sample.
func (s *Stats) Record(scenario, endpoint string, d time.Duration, status int, errMsg string) {
	k := endpointKey{scenario, endpoint}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hists[k]
	if h == nil {
		h = newHistogram()
		s.hists[k] = h
	}
	h.observe(d)
	if status == 0 {
		s.transport[k]++
		if errMsg != "" {
			s.errors[errMsg]++
		}
		return
	}
	m := s.statuses[k]
	if m == nil {
		m = make(map[int]int64)
		s.statuses[k] = m
	}
	m[status]++
}

func (s *Stats) RecordTurn(d time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns.observe(d)
	if ok {
		s.turnOK++
	} else {
		s.turnFail++
	}
}

func (s *Stats) RecordWS(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch event {
	case "connect":
		s.wsConnects++
	case "reconnect":
		s.wsRecon++
	case "message":
		s.wsMessages++
	case "error":
		s.wsErrors++
	}
}

func (s *Stats) RecordError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors[msg]++
}

// EndpointReport is the JSON shape for one endpoint.
type EndpointReport struct {
	Count           int64            `json:"count"`
	Statuses        map[string]int64 `json:"statuses"`
	TransportErrors int64            `json:"transportErrors"`
	P50Ms           float64          `json:"p50Ms"`
	P95Ms           float64          `json:"p95Ms"`
	P99Ms           float64          `json:"p99Ms"`
	AvgMs           float64          `json:"avgMs"`
	MinMs           float64          `json:"minMs"`
	MaxMs           float64          `json:"maxMs"`
}

// WSReport summarizes WebSocket behavior.
type WSReport struct {
	Connects   int64 `json:"connects"`
	Reconnects int64 `json:"reconnects"`
	Messages   int64 `json:"messages"`
	Errors     int64 `json:"errors"`
}

// TurnReport summarizes agent turns.
type TurnReport struct {
	Success int64   `json:"success"`
	Failed  int64   `json:"failed"`
	P50Ms   float64 `json:"p50Ms"`
	P95Ms   float64 `json:"p95Ms"`
	P99Ms   float64 `json:"p99Ms"`
	AvgMs   float64 `json:"avgMs"`
}

// Report is the final JSON document written by "run".
type Report struct {
	Scenario        string                     `json:"scenario"`
	BaseURL         string                     `json:"baseUrl"`
	VUs             int                        `json:"vus"`
	StartedAt       time.Time                  `json:"startedAt"`
	DurationSeconds float64                    `json:"durationSeconds"`
	Endpoints       map[string]*EndpointReport `json:"endpoints"`
	WebSocket       WSReport                   `json:"websocket"`
	Turns           TurnReport                 `json:"turns"`
	ErrorSamples    map[string]int64           `json:"errorSamples,omitempty"`
}

// Snapshot builds a Report from the current counters.
func (s *Stats) Snapshot(baseURL string, vus int, started time.Time) *Report {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := &Report{
		Scenario:        s.scenario,
		BaseURL:         baseURL,
		VUs:             vus,
		StartedAt:       started,
		DurationSeconds: time.Since(started).Seconds(),
		Endpoints:       make(map[string]*EndpointReport),
		ErrorSamples:    make(map[string]int64, len(s.errors)),
	}
	for k, h := range s.hists {
		er := &EndpointReport{
			Count:           h.count,
			Statuses:        make(map[string]int64),
			TransportErrors: s.transport[k],
			P50Ms:           h.percentile(0.50) * 1000,
			P95Ms:           h.percentile(0.95) * 1000,
			P99Ms:           h.percentile(0.99) * 1000,
			AvgMs:           h.avg() * 1000,
			MinMs:           h.min * 1000,
			MaxMs:           h.max * 1000,
		}
		for code, n := range s.statuses[k] {
			er.Statuses[fmt.Sprintf("%d", code)] = n
		}
		r.Endpoints[k.endpoint] = er
	}
	for msg, n := range s.errors {
		r.ErrorSamples[msg] = n
	}
	r.WebSocket = WSReport{
		Connects:   s.wsConnects,
		Reconnects: s.wsRecon,
		Messages:   s.wsMessages,
		Errors:     s.wsErrors,
	}
	r.Turns = TurnReport{
		Success: s.turnOK,
		Failed:  s.turnFail,
		P50Ms:   s.turns.percentile(0.50) * 1000,
		P95Ms:   s.turns.percentile(0.95) * 1000,
		P99Ms:   s.turns.percentile(0.99) * 1000,
		AvgMs:   s.turns.avg() * 1000,
	}
	return r
}

func (r *Report) printSummary() {
	fmt.Printf("\n=== %s: %d VUs, %.0fs ===\n", r.Scenario, r.VUs, r.DurationSeconds)
	fmt.Printf("%-22s %9s %8s %8s %8s %8s %10s\n", "endpoint", "count", "p50ms", "p95ms", "p99ms", "maxms", "transport")
	names := make([]string, 0, len(r.Endpoints))
	for name := range r.Endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := r.Endpoints[name]
		fmt.Printf("%-22s %9d %8.1f %8.1f %8.1f %8.1f %10d\n",
			name, e.Count, e.P50Ms, e.P95Ms, e.P99Ms, e.MaxMs, e.TransportErrors)
	}
	if r.WebSocket.Connects > 0 || r.WebSocket.Errors > 0 {
		fmt.Printf("websocket: connects=%d reconnects=%d messages=%d errors=%d\n",
			r.WebSocket.Connects, r.WebSocket.Reconnects, r.WebSocket.Messages, r.WebSocket.Errors)
	}
	if r.Turns.Success+r.Turns.Failed > 0 {
		fmt.Printf("turns: ok=%d failed=%d p50=%.0fms p95=%.0fms p99=%.0fms\n",
			r.Turns.Success, r.Turns.Failed, r.Turns.P50Ms, r.Turns.P95Ms, r.Turns.P99Ms)
	}
	if len(r.ErrorSamples) > 0 {
		fmt.Println("errors:")
		for msg, n := range r.ErrorSamples {
			fmt.Printf("  %6d  %s\n", n, msg)
		}
	}
}
