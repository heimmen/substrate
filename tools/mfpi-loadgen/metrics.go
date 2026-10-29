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
	"io"
	"strconv"
)

// writeMetrics emits a small Prometheus text exposition for the running load
// test. Histogram buckets are in seconds (Prometheus convention).
func writeMetrics(w io.Writer, s *Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Fprintln(w, "# HELP mfpi_loadgen_requests_total HTTP requests by endpoint and status.")
	fmt.Fprintln(w, "# TYPE mfpi_loadgen_requests_total counter")
	for k := range s.hists {
		for code, n := range s.statuses[k] {
			fmt.Fprintf(w, "mfpi_loadgen_requests_total{scenario=%q,endpoint=%q,code=%q} %d\n",
				k.scenario, k.endpoint, strconv.Itoa(code), n)
		}
		if n := s.transport[k]; n > 0 {
			fmt.Fprintf(w, "mfpi_loadgen_requests_total{scenario=%q,endpoint=%q,code=\"transport\"} %d\n",
				k.scenario, k.endpoint, n)
		}
	}

	fmt.Fprintln(w, "# HELP mfpi_loadgen_request_duration_seconds Request latency by endpoint.")
	fmt.Fprintln(w, "# TYPE mfpi_loadgen_request_duration_seconds histogram")
	for k, h := range s.hists {
		var cum int64
		for i, count := range h.counts {
			cum += count
			if i < len(latencyBuckets) {
				fmt.Fprintf(w, "mfpi_loadgen_request_duration_seconds_bucket{scenario=%q,endpoint=%q,le=%q} %d\n",
					k.scenario, k.endpoint, trimFloat(latencyBuckets[i]), cum)
			} else {
				fmt.Fprintf(w, "mfpi_loadgen_request_duration_seconds_bucket{scenario=%q,endpoint=%q,le=\"+Inf\"} %d\n",
					k.scenario, k.endpoint, cum)
			}
		}
		fmt.Fprintf(w, "mfpi_loadgen_request_duration_seconds_sum{scenario=%q,endpoint=%q} %g\n",
			k.scenario, k.endpoint, h.sum)
		fmt.Fprintf(w, "mfpi_loadgen_request_duration_seconds_count{scenario=%q,endpoint=%q} %d\n",
			k.scenario, k.endpoint, h.count)
	}

	fmt.Fprintln(w, "# HELP mfpi_loadgen_ws_events_total WebSocket events by type.")
	fmt.Fprintln(w, "# TYPE mfpi_loadgen_ws_events_total counter")
	fmt.Fprintf(w, "mfpi_loadgen_ws_events_total{scenario=%q,event=\"connect\"} %d\n", s.scenario, s.wsConnects)
	fmt.Fprintf(w, "mfpi_loadgen_ws_events_total{scenario=%q,event=\"reconnect\"} %d\n", s.scenario, s.wsRecon)
	fmt.Fprintf(w, "mfpi_loadgen_ws_events_total{scenario=%q,event=\"message\"} %d\n", s.scenario, s.wsMessages)
	fmt.Fprintf(w, "mfpi_loadgen_ws_events_total{scenario=%q,event=\"error\"} %d\n", s.scenario, s.wsErrors)

	fmt.Fprintln(w, "# HELP mfpi_loadgen_turns_total Agent turns by result.")
	fmt.Fprintln(w, "# TYPE mfpi_loadgen_turns_total counter")
	fmt.Fprintf(w, "mfpi_loadgen_turns_total{scenario=%q,result=\"ok\"} %d\n", s.scenario, s.turnOK)
	fmt.Fprintf(w, "mfpi_loadgen_turns_total{scenario=%q,result=\"failed\"} %d\n", s.scenario, s.turnFail)

	fmt.Fprintln(w, "# HELP mfpi_loadgen_turn_duration_seconds Agent turn end-to-end latency.")
	fmt.Fprintln(w, "# TYPE mfpi_loadgen_turn_duration_seconds histogram")
	var cum int64
	for i, count := range s.turns.counts {
		cum += count
		if i < len(latencyBuckets) {
			fmt.Fprintf(w, "mfpi_loadgen_turn_duration_seconds_bucket{scenario=%q,le=%q} %d\n",
				s.scenario, trimFloat(latencyBuckets[i]), cum)
		} else {
			fmt.Fprintf(w, "mfpi_loadgen_turn_duration_seconds_bucket{scenario=%q,le=\"+Inf\"} %d\n",
				s.scenario, cum)
		}
	}
	fmt.Fprintf(w, "mfpi_loadgen_turn_duration_seconds_sum{scenario=%q} %g\n", s.scenario, s.turns.sum)
	fmt.Fprintf(w, "mfpi_loadgen_turn_duration_seconds_count{scenario=%q} %d\n", s.scenario, s.turns.count)
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
