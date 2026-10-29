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
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// authMode controls how a virtual user authenticates.
type authMode int

const (
	// authBasic sends HTTP Basic on every request. This is the expensive path
	// (PBKDF2-100k in mfpi-admin) and is what a naive script would do.
	authBasic authMode = iota
	// authCookie sends Basic on the first request and then relies on the
	// mfpi_auth cookie, like a real browser. A 401 re-arms Basic.
	authCookie
)

func (m authMode) String() string {
	if m == authBasic {
		return "basic"
	}
	return "cookie"
}

// userClient is one virtual user's HTTP state. Each userClient has its own
// cookie jar; the underlying transport is shared by all users.
type userClient struct {
	base string
	user UserCred
	mode authMode
	hc   *http.Client

	mu        sync.Mutex
	sendBasic bool
}

func newUserClient(base string, cred UserCred, mode authMode, rt http.RoundTripper) (*userClient, error) {
	base = strings.TrimRight(base, "/")
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("invalid base URL %q: %w", base, err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookie jar: %w", err)
	}
	return &userClient{
		base:      base,
		user:      cred,
		mode:      mode,
		sendBasic: true,
		hc: &http.Client{
			Transport: rt,
			Jar:       jar,
			// We set per-request contexts; leave the client timeout unset so
			// long reads (WS upgrade excluded) are bounded by the context.
		},
	}, nil
}

func (c *userClient) basicEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendBasic
}

func (c *userClient) setBasic(v bool) {
	c.mu.Lock()
	c.sendBasic = v
	c.mu.Unlock()
}

// noteStatus updates the auth state after a response.
//
// In cookie mode, Basic stays enabled until a response proves authentication
// worked. A 403 from the readiness gate means "authenticated but actor not
// running" (nginx drops the Set-Cookie in that case), so it must NOT switch us
// to cookie-only. A 401 means the cookie was missing/expired: re-arm Basic.
func (c *userClient) noteStatus(code int) {
	if c.mode != authCookie {
		return
	}
	switch {
	case code == http.StatusUnauthorized:
		c.setBasic(true)
	case code >= 200 && code < 400:
		c.setBasic(false)
	}
}

func (c *userClient) userPath(rest string) string {
	rest = strings.TrimPrefix(rest, "/")
	return "/" + c.user.Name + "/" + rest
}

// do performs one HTTP request. status 0 + err means a transport error.
func (c *userClient) do(ctx context.Context, method, path string, body []byte, accept, contentType string, timeout time.Duration) ([]byte, int, time.Duration, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, c.base+path, rdr)
	if err != nil {
		return nil, 0, 0, err
	}
	if c.basicEnabled() {
		req.SetBasicAuth(c.user.Name, c.user.Password)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "mfpi-loadgen/1.0")

	start := time.Now()
	resp, err := c.hc.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return nil, 0, elapsed, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	c.noteStatus(resp.StatusCode)
	if readErr != nil {
		return data, resp.StatusCode, elapsed, readErr
	}
	return data, resp.StatusCode, elapsed, nil
}

// dialWS opens a WebSocket for path, copying the client's current credentials
// and cookies.
func (c *userClient) dialWS(ctx context.Context, path string) (*websocket.Conn, error) {
	base := c.base
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return nil, fmt.Errorf("unsupported base URL %q", c.base)
	}
	raw := base + path
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	if c.basicEnabled() {
		token := base64.StdEncoding.EncodeToString([]byte(c.user.Name + ":" + c.user.Password))
		h.Set("Authorization", "Basic "+token)
	}
	bu, err := url.Parse(c.base)
	if err != nil {
		return nil, err
	}
	for _, ck := range c.hc.Jar.Cookies(bu) {
		h.Add("Cookie", ck.Name+"="+ck.Value)
	}
	d := &websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := d.DialContext(ctx, u.String(), h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		return nil, fmt.Errorf("ws dial %s (http %d): %w", path, code, err)
	}
	return conn, nil
}

// bootstrap waits until the actor's pi-web is actually serving API traffic.
//
// It must NOT use a browser navigation (Accept: text/html) as the readiness
// probe: the gateway rewrites a not-ready actor to a 200 loading page for
// navigations (docs/user_agent_api.md §3.3), so a 200 would be a false
// positive. API requests get the raw 503 instead.
//
// The first successful request establishes the mfpi_auth cookie in cookie mode.
// The returned duration is the TOTAL readiness wait (including retries and a
// cold resume), which is what the "bootstrap" row in the report should show.
func (c *userClient) bootstrap(ctx context.Context, readyTimeout time.Duration) (int, time.Duration, error) {
	start := time.Now()
	deadline := start.Add(readyTimeout)
	backoff := time.Second
	lastCode := 0
	probe := c.userPath("api/machines/local/status")
	for {
		_, code, _, err := c.do(ctx, http.MethodGet, probe, nil, "application/json", "", 30*time.Second)
		if err == nil {
			lastCode = code
			if code == http.StatusOK {
				return code, time.Since(start), nil
			}
			if code != http.StatusServiceUnavailable && code != http.StatusForbidden {
				if code == http.StatusUnauthorized {
					return code, time.Since(start), fmt.Errorf("bootstrap: unauthorized (check the user password)")
				}
				if code == http.StatusNotFound {
					// Older builds may not expose the machines/local alias.
					probe = c.userPath("api/config")
				} else {
					return code, time.Since(start), fmt.Errorf("bootstrap: unexpected status %d", code)
				}
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return lastCode, 0, fmt.Errorf("bootstrap: actor not ready after %s: %w", readyTimeout, err)
			}
			return lastCode, 0, fmt.Errorf("bootstrap: actor not ready after %s (last status %d)", readyTimeout, lastCode)
		}
		select {
		case <-ctx.Done():
			return lastCode, 0, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}
