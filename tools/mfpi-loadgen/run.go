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
	"os"
	"strings"
	"sync"
	"time"
)

type loadConfig struct {
	baseURL        string
	credsPath      string
	scenario       string
	vus            int
	ramp           time.Duration
	duration       time.Duration
	thinkMin       time.Duration
	thinkMax       time.Duration
	auth           string
	cwd            string
	provider       string
	model          string
	providerAPIKey string
	modelTimeout   time.Duration
	allowDefault   bool
	turnInterval   time.Duration
	turnTimeout    time.Duration
	readyTimeout   time.Duration
	requestTimeout time.Duration
	reportPath     string
	metricsListen  string
	adminUser      string
	adminPass      string
	seed           int64
}

type runner struct {
	cfg     loadConfig
	creds   *CredsFile
	stats   *Stats
	wg      sync.WaitGroup
	started time.Time
}

func runLoad(ctx context.Context, args []string) error {
	cfg := loadConfig{}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&cfg.baseURL, "base-url", envDefault("MFPI_BASE", "http://127.0.0.1:58681"), "gateway base URL (e.g. http://127.0.0.1:59681 for the test env)")
	fs.StringVar(&cfg.credsPath, "creds", "creds.json", "credentials JSON produced by the users subcommand")
	fs.StringVar(&cfg.scenario, "scenario", "idle-browser", "scenario: idle-browser | agent-turn | admin")
	fs.IntVar(&cfg.vus, "vus", 1, "number of virtual users (one actor is used per VU, cycling through creds)")
	fs.DurationVar(&cfg.ramp, "ramp", 0, "time over which to start all VUs (0 = all at once)")
	fs.DurationVar(&cfg.duration, "duration", 5*time.Minute, "load duration after the ramp")
	fs.StringVar(&cfg.auth, "auth", "cookie", "auth mode: cookie (Basic once, then cookie) | basic (Basic every request)")
	fs.StringVar(&cfg.cwd, "cwd", "/data/home", "session working directory inside the actor (must exist)")
	fs.StringVar(&cfg.provider, "provider", "", "agent-turn: provider id to select (e.g. mock)")
	fs.StringVar(&cfg.model, "model", "", "agent-turn: model id to select (e.g. mock-1)")
	fs.StringVar(&cfg.providerAPIKey, "provider-api-key", "", "agent-turn: key to inject for -provider through mfpi-admin (default sk-mock when provider is mock)")
	fs.DurationVar(&cfg.modelTimeout, "model-timeout", 120*time.Second, "agent-turn: how long to wait for the provider key/model to become available")
	fs.BoolVar(&cfg.allowDefault, "allow-default-model", false, "agent-turn: continue with the session default model if -provider/-model cannot be selected (may incur real provider cost)")
	fs.DurationVar(&cfg.turnInterval, "turn-interval", 60*time.Second, "agent-turn: delay between prompts")
	fs.DurationVar(&cfg.turnTimeout, "turn-timeout", 120*time.Second, "agent-turn: max wait for one turn to finish")
	fs.DurationVar(&cfg.readyTimeout, "ready-timeout", 180*time.Second, "max wait for a user's actor to become reachable")
	fs.DurationVar(&cfg.requestTimeout, "request-timeout", 30*time.Second, "per-request timeout")
	fs.StringVar(&cfg.reportPath, "report", "", "JSON report path (default report-<scenario>-<timestamp>.json)")
	fs.StringVar(&cfg.metricsListen, "metrics-listen", "", "if set, expose Prometheus metrics on this address (e.g. 127.0.0.1:9091)")
	fs.StringVar(&cfg.adminUser, "admin-user", envDefault("MFPI_ADMIN_USER", "admin"), "admin username for the admin scenario")
	fs.StringVar(&cfg.adminPass, "admin-pass", envDefault("MFPI_ADMIN_PASSWORD", "mf@pass2026"), "admin password for the admin scenario")
	fs.Int64Var(&cfg.seed, "seed", 0, "random seed (0 = time-based)")

	think := fs.String("think", "3s-10s", `think time between requests, "min-max" (e.g. 3s-10s) or a single duration`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := parseThinkRange(*think, &cfg.thinkMin, &cfg.thinkMax); err != nil {
		return err
	}
	switch cfg.scenario {
	case "idle-browser", "agent-turn", "admin":
	default:
		return fmt.Errorf("unknown scenario %q (want idle-browser, agent-turn or admin)", cfg.scenario)
	}
	if cfg.vus < 1 {
		return fmt.Errorf("-vus must be >= 1")
	}
	switch strings.ToLower(cfg.auth) {
	case "cookie", "basic", "mixed":
	default:
		return fmt.Errorf("unknown -auth %q (want cookie, basic or mixed)", cfg.auth)
	}
	if cfg.scenario == "agent-turn" && (cfg.provider == "") != (cfg.model == "") {
		return fmt.Errorf("-provider and -model must be set together")
	}
	if cfg.scenario == "agent-turn" && cfg.provider != "" && cfg.providerAPIKey == "" && cfg.provider == "mock" {
		cfg.providerAPIKey = "sk-mock"
	}
	if cfg.seed == 0 {
		cfg.seed = time.Now().UnixNano()
	}

	r := &runner{cfg: cfg, stats: newStats(cfg.scenario), started: time.Now()}

	if cfg.scenario != "admin" {
		creds, err := loadCreds(cfg.credsPath)
		if err != nil {
			return err
		}
		r.creds = creds
	}

	if cfg.metricsListen != "" {
		go func() {
			log.Printf("metrics: http://%s/metrics", cfg.metricsListen)
			mux := http.NewServeMux()
			mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
				writeMetrics(w, r.stats)
			})
			if err := http.ListenAndServe(cfg.metricsListen, mux); err != nil {
				log.Printf("metrics server: %v", err)
			}
		}()
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	log.Printf("scenario=%s vus=%d ramp=%s duration=%s auth=%s target=%s",
		cfg.scenario, cfg.vus, cfg.ramp, cfg.duration, cfg.auth, cfg.baseURL)

	rt := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        4096,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   false,
	}

	// readyCh receives one signal per VU once its actor is reachable (or its
	// bootstrap failed). The measurement window starts only after every VU is
	// ready, so "-duration 10m" means 10 minutes at the target concurrency even
	// when actors have to cold-start first.
	readyCh := make(chan struct{}, cfg.vus)
	for i := 0; i < cfg.vus; i++ {
		r.wg.Add(1)
		go r.runVU(runCtx, i, rt, readyCh)
		if cfg.ramp > 0 && i < cfg.vus-1 {
			if !sleepCtx(runCtx, cfg.ramp/time.Duration(cfg.vus)) {
				break
			}
		}
	}
	ready := 0
	readyTimer := time.NewTimer(cfg.ramp + cfg.readyTimeout + 30*time.Second)
	defer readyTimer.Stop()
waitReady:
	for ready < cfg.vus {
		select {
		case <-readyCh:
			ready++
		case <-readyTimer.C:
			log.Printf("readiness wait timed out after %s; starting the measurement window with %d/%d VUs ready",
				cfg.ramp+cfg.readyTimeout+30*time.Second, ready, cfg.vus)
			break waitReady
		case <-runCtx.Done():
			break waitReady
		}
	}
	log.Printf("all VUs ready (%d/%d); holding for %s", ready, cfg.vus, cfg.duration)
	go func() {
		select {
		case <-time.After(cfg.duration):
			cancel()
		case <-runCtx.Done():
		}
	}()
	r.wg.Wait()

	report := r.stats.Snapshot(cfg.baseURL, cfg.vus, r.started)
	report.printSummary()

	path := cfg.reportPath
	if path == "" {
		path = fmt.Sprintf("report-%s-%s.json", cfg.scenario, time.Now().Format("20060102-150405"))
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	log.Printf("report written to %s", path)
	return nil
}

func (r *runner) runVU(ctx context.Context, idx int, rt http.RoundTripper, readyCh chan<- struct{}) {
	defer r.wg.Done()

	if r.cfg.scenario == "admin" {
		signalReady(readyCh)
		r.runAdminVU(ctx, idx, rt)
		return
	}

	cred := r.creds.Users[idx%len(r.creds.Users)]
	mode := authModeFor(r.cfg.auth, idx)
	c, err := newUserClient(r.cfg.baseURL, cred, mode, rt)
	if err != nil {
		r.stats.RecordError(fmt.Sprintf("vu %d: %v", idx, err))
		return
	}

	t0 := time.Now()
	code, dur, err := c.bootstrap(ctx, r.cfg.readyTimeout)
	r.stats.Record(r.cfg.scenario, "bootstrap", dur, code, errString(err))
	signalReady(readyCh)
	if err != nil {
		log.Printf("vu %d (%s): %v", idx, cred.Name, err)
		return
	}
	log.Printf("vu %d (%s) ready in %s", idx, cred.Name, time.Since(t0).Round(time.Millisecond))

	if r.cfg.scenario == "agent-turn" && r.cfg.provider != "" && r.cfg.providerAPIKey != "" {
		if err := r.ensureProviderKey(ctx, cred.Name); err != nil {
			// A stored key is replayed by mfpi-admin's reconcile loop within
			// ~30s; selectModel retries, so this is not fatal on its own.
			log.Printf("vu %d (%s): inject provider key: %v", idx, cred.Name, err)
		}
	}

	rnd := rand.New(rand.NewSource(r.cfg.seed + int64(idx)*7919))
	switch r.cfg.scenario {
	case "idle-browser":
		r.runIdleVU(ctx, idx, c, rnd)
	case "agent-turn":
		r.runAgentVU(ctx, idx, c, rnd)
	}
}

// signalReady reports a VU as ready without ever blocking (the channel is
// buffered with the VU count; a duplicate or late signal is dropped).
func signalReady(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func authModeFor(mode string, idx int) authMode {
	switch strings.ToLower(mode) {
	case "basic":
		return authBasic
	case "mixed":
		if idx%2 == 0 {
			return authBasic
		}
		return authCookie
	default:
		return authCookie
	}
}

func (r *runner) think(rnd *rand.Rand) time.Duration {
	if r.cfg.thinkMax <= r.cfg.thinkMin {
		return r.cfg.thinkMin
	}
	span := r.cfg.thinkMax - r.cfg.thinkMin
	// ±30% jitter around the sampled point would make the range exceed its
	// bounds; sample uniformly instead and keep the configured window exact.
	return r.cfg.thinkMin + time.Duration(rnd.Int63n(int64(span)+1))
}

// runAdminVU loads the management plane (ate-api-server ListActors through
// mfpi-admin), which is the control-plane hot path exercised by every request
// via the readiness gate.
func (r *runner) runAdminVU(ctx context.Context, idx int, rt http.RoundTripper) {
	hc := &http.Client{Transport: rt}
	base := strings.TrimRight(r.cfg.baseURL, "/")
	rnd := rand.New(rand.NewSource(r.cfg.seed + int64(idx)*7919))
	endpoints := []string{
		"/usermanagement/api/users",
		"/usermanagement/api/users",
		"/usermanagement/healthz",
	}
	for ctx.Err() == nil {
		path := endpoints[rnd.Intn(len(endpoints))]
		reqCtx, cancel := context.WithTimeout(ctx, r.cfg.requestTimeout)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, base+path, nil)
		req.SetBasicAuth(r.cfg.adminUser, r.cfg.adminPass)
		req.Header.Set("Accept", "application/json")
		start := time.Now()
		resp, err := hc.Do(req)
		dur := time.Since(start)
		cancel()
		tag := "admin.users"
		if strings.HasSuffix(path, "/healthz") {
			tag = "admin.healthz"
		}
		if err != nil {
			r.stats.Record("admin", tag, dur, 0, errString(err))
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			r.stats.Record("admin", tag, dur, resp.StatusCode, "")
		}
		if !sleepCtx(ctx, r.think(rnd)) {
			return
		}
	}
}

func parseThinkRange(s string, min, max *time.Duration) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("-think must not be empty")
	}
	parts := strings.SplitN(s, "-", 2)
	lo, err := time.ParseDuration(strings.TrimSpace(parts[0]))
	if err != nil {
		return fmt.Errorf("invalid -think %q: %w", s, err)
	}
	if len(parts) == 1 {
		*min, *max = lo, lo
		return nil
	}
	hi, err := time.ParseDuration(strings.TrimSpace(parts[1]))
	if err != nil {
		return fmt.Errorf("invalid -think %q: %w", s, err)
	}
	if lo < 0 || hi < lo {
		return fmt.Errorf("invalid -think %q: want 0 <= min <= max", s)
	}
	*min, *max = lo, hi
	return nil
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
