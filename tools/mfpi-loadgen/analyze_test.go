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
	"strings"
	"testing"
)

const sampleLog = `127.0.0.1 - - [28/Oct/2026:10:00:00 +0000] "GET /alice/api/users HTTP/1.1" 200 123 "-" "ua"
127.0.0.1 - - [28/Oct/2026:10:00:30 +0000] "GET /bob/api/users HTTP/1.1" 200 123 "-" "ua"
127.0.0.1 - - [28/Oct/2026:10:01:00 +0000] "GET /alice/ HTTP/1.1" 503 123 "-" "ua"
127.0.0.1 - - [28/Oct/2026:10:01:05 +0000] "GET /usermanagement/api/users HTTP/1.1" 401 123 "-" "ua"
this line is not an access log line
`

func TestAnalyzeLog(t *testing.T) {
	rep, err := analyzeLog(strings.NewReader(sampleLog), 10)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Lines != 5 {
		t.Errorf("lines = %d, want 5", rep.Lines)
	}
	if rep.Parsed != 4 {
		t.Errorf("parsed = %d, want 4", rep.Parsed)
	}
	if rep.Status["200"] != 2 || rep.Status["503"] != 1 || rep.Status["401"] != 1 {
		t.Errorf("status = %v", rep.Status)
	}
	if rep.Minutes != 2 {
		t.Errorf("minutes = %d, want 2", rep.Minutes)
	}
	if rep.PeakPerMin != 2 {
		t.Errorf("peak = %d, want 2", rep.PeakPerMin)
	}
	if got := findPair(rep.Paths, "GET /{user}/api/users"); got != 2 {
		t.Errorf("top path = %v", rep.Paths)
	}
	if got := findPair(rep.Users, "alice"); got != 2 {
		t.Errorf("alice count = %d, want 2", got)
	}
	if got := findPair(rep.Users, "usermanagement"); got != 0 {
		t.Errorf("reserved prefix should not be counted as a user: %v", rep.Users)
	}
	if got := findPair(rep.Errors5xx, "GET /{user}/"); got != 1 {
		t.Errorf("5xx paths = %v", rep.Errors5xx)
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/":                          "/",
		"/alice":                     "/{user}",
		"/alice/":                    "/{user}/",
		"/alice/api/users":           "/{user}/api/users",
		"/alice/sessions/123/status": "/{user}/sessions/123/status",
		"/usermanagement/api/users":  "/usermanagement/api/users",
		"/_cicd_/state":              "/_cicd_/state",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func findPair(pairs []countPair, name string) int64 {
	for _, p := range pairs {
		if p.Name == name {
			return p.Count
		}
	}
	return 0
}
