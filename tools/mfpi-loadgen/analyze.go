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
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// combinedLogRe matches the nginx "combined" access-log format. The second
// field is the authenticated user ($remote_user), which is "-" for the
// per-user paths (they authenticate via the auth subrequest).
var combinedLogRe = regexp.MustCompile(`^(\S+) - (\S+) \[([^\]]+)\] "(\S+) ([^ ]+) [^"]*" (\d{3}) \S+`)

type countPair struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

type logReport struct {
	Lines        int              `json:"lines"`
	Parsed       int              `json:"parsed"`
	FirstTS      string           `json:"firstTs,omitempty"`
	LastTS       string           `json:"lastTs,omitempty"`
	Minutes      int              `json:"minutes"`
	AvgPerMinute float64          `json:"avgPerMinute"`
	PeakPerMin   int64            `json:"peakPerMinute"`
	Status       map[string]int64 `json:"status"`
	Users        []countPair      `json:"topUsers"`
	Paths        []countPair      `json:"topPaths"`
	Errors5xx    []countPair      `json:"top5xxPaths"`
	PeakMinutes  []countPair      `json:"peakMinutes"`
}

// runAnalyzeLog summarizes an nginx access log so the load model can be
// calibrated against real traffic (see docs/stress_test_plan.md §2.4).
func runAnalyzeLog(args []string) error {
	fs := flag.NewFlagSet("analyze-log", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit JSON instead of a text summary")
	topN := fs.Int("top", 15, "number of rows in each top-N table")
	file := fs.String("file", "", "access log path (default: stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var rd io.Reader = os.Stdin
	if *file != "" {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		rd = f
	}

	rep, err := analyzeLog(rd, *topN)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printLogReport(rep)
	return nil
}

func analyzeLog(rd io.Reader, topN int) (*logReport, error) {
	rep := &logReport{Status: map[string]int64{}}
	users := map[string]int64{}
	paths := map[string]int64{}
	errPaths := map[string]int64{}
	perMin := map[string]int64{}

	var first, last time.Time
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		rep.Lines++
		m := combinedLogRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		rep.Parsed++
		tsRaw, method, path, code := m[3], m[4], m[5], m[6]
		rep.Status[code]++

		ts, err := time.Parse("02/Jan/2006:15:04:05 -0700", tsRaw)
		if err == nil {
			if first.IsZero() || ts.Before(first) {
				first = ts
			}
			if last.IsZero() || ts.After(last) {
				last = ts
			}
			perMin[ts.Format("2006-01-02T15:04")]++
		}

		p := path
		if i := strings.IndexByte(p, '?'); i >= 0 {
			p = p[:i]
		}
		p = normalizePath(p)
		paths[method+" "+p]++

		if u := firstSegment(path); u != "" {
			users[u]++
		}
		if strings.HasPrefix(code, "5") {
			errPaths[method+" "+p]++
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	if !first.IsZero() {
		rep.FirstTS = first.Format(time.RFC3339)
		rep.LastTS = last.Format(time.RFC3339)
	}
	rep.Minutes = len(perMin)
	var totalMin int64
	for _, n := range perMin {
		totalMin += n
		if n > rep.PeakPerMin {
			rep.PeakPerMin = n
		}
	}
	if rep.Minutes > 0 {
		rep.AvgPerMinute = float64(totalMin) / float64(rep.Minutes)
	}
	rep.Users = topCounts(users, topN)
	rep.Paths = topCounts(paths, topN)
	rep.Errors5xx = topCounts(errPaths, topN)
	rep.PeakMinutes = topCounts(perMin, 10)
	return rep, nil
}

var reservedFirstSeg = map[string]bool{
	"usermanagement": true,
	"_cicd_":         true,
	"_mfpi_auth":     true,
	"_mfpi_gate":     true,
	"_mfpi_loading":  true,
}

// normalizePath replaces the leading username with {user} so that per-user
// paths aggregate.
func normalizePath(p string) string {
	if !strings.HasPrefix(p, "/") || p == "/" {
		return p
	}
	rest := strings.TrimPrefix(p, "/")
	seg := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		seg = rest[:i]
	}
	if seg == "" || reservedFirstSeg[seg] {
		return p
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return "/{user}/" + rest[i+1:]
	}
	return "/{user}"
}

func firstSegment(p string) string {
	rest := strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" || reservedFirstSeg[rest] {
		return ""
	}
	return rest
}

func topCounts(m map[string]int64, n int) []countPair {
	out := make([]countPair, 0, len(m))
	for k, v := range m {
		out = append(out, countPair{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

func printLogReport(r *logReport) {
	fmt.Printf("lines=%d parsed=%d\n", r.Lines, r.Parsed)
	if r.FirstTS != "" {
		fmt.Printf("range=%s .. %s  minutes=%d  avg=%.1f req/min  peak=%d req/min\n",
			r.FirstTS, r.LastTS, r.Minutes, r.AvgPerMinute, r.PeakPerMin)
	}
	fmt.Println("\nstatus:")
	for _, k := range []string{"200", "302", "304", "400", "401", "403", "404", "499", "500", "502", "503", "504", "101"} {
		if n := r.Status[k]; n > 0 {
			fmt.Printf("  %s  %d\n", k, n)
		}
	}
	printPairs("top users", r.Users)
	printPairs("top paths", r.Paths)
	if len(r.Errors5xx) > 0 {
		printPairs("top 5xx paths", r.Errors5xx)
	}
	printPairs("peak minutes", r.PeakMinutes)
}

func printPairs(title string, pairs []countPair) {
	if len(pairs) == 0 {
		return
	}
	fmt.Printf("\n%s:\n", title)
	for _, p := range pairs {
		fmt.Printf("  %8d  %s\n", p.Count, p.Name)
	}
}
