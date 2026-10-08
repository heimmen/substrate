// "Agent is loading" interstitial page for the mf-pi admin server.
//
// When a user opens an actor that is not ready yet — SUSPENDED/RESUMING while
// the router wakes it, or RUNNING with its web server still booting — the
// request fails with 502/503/504 at the router. mfpi-nginx intercepts those
// codes for browser navigations (Accept: text/html) and proxies them here, so
// instead of a bare 503 the user gets a friendly page that says the agent is
// loading, shows live status, and auto-retries. API and WebSocket clients are
// deliberately NOT intercepted (see nginx.conf): they keep the raw 503 so the
// SPA's own retry logic is unaffected.
//
// The page is rendered from the live actor status, so the message matches what
// is actually happening (waking a suspended actor vs. waiting for the web
// server to come up vs. a genuine problem such as a missing user).
package main

import (
	"context"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// loadingPageTemplate is parsed from the embedded loading.html.
var loadingPageTemplate = template.Must(template.ParseFS(staticFS, "loading.html"))

// loadingRetryAfter is advertised to the browser (and used by the page's own
// meta refresh) between retries.
const loadingRetryAfter = 3

// loadingPageData drives loading.html.
type loadingPageData struct {
	Username    string
	Title       string
	Message     string
	AutoRefresh bool
	Error       bool
	RetryURL    string
}

// handleLoadingPage renders the interstitial shown while a user's actor wakes
// up. It resolves the target user the same way handleAuth does (URL path or
// mfpi_user cookie) and looks up the actor's live status to tailor the message.
// Status lookup failures are non-fatal: the page still renders with a generic
// "loading" message and keeps retrying.
func (s *server) handleLoadingPage(w http.ResponseWriter, r *http.Request) {
	username := targetUser(r)
	// The path-derived user is already DNS-1123 by construction, but the
	// bare-origin fallback takes the user from the mfpi_user cookie, which the
	// client controls. Reject anything that is not a valid actor name so we
	// neither query ateapi with garbage nor render a bogus user.
	if username != "" && !dns1123Re.MatchString(username) {
		username = ""
	}

	data := loadingPageData{
		Username:    username,
		Title:       "Agent 正在载入…",
		Message:     "正在准备你的 Agent，请稍候。",
		AutoRefresh: true,
	}
	// Prefer the exact original URL (nginx passes it in X-Original-URI); fall
	// back to the user's canonical entry point. The nginx internal redirect
	// keeps the address bar unchanged, so the meta refresh reloads the
	// original URL regardless.
	if orig := r.Header.Get("X-Original-URI"); strings.HasPrefix(orig, "/") {
		data.RetryURL = orig
	} else if username != "" {
		data.RetryURL = "/" + username + "/"
	} else {
		data.RetryURL = "/"
	}

	if username != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		actor, err := s.client.GetActor(ctx, &ateapipb.GetActorRequest{
			Actor: &ateapipb.ObjectRef{Atespace: s.atespace, Name: username},
		})
		switch {
		case err == nil:
			data.Title, data.Message = loadingCopyForStatus(actor.GetStatus())
		case status.Code(err) == codes.NotFound:
			data.Title = "用户不存在"
			data.Message = "未找到该用户，可能已被删除。"
			data.AutoRefresh = false
			data.Error = true
		default:
			// Could not read status (ateapi hiccup): keep the generic loading
			// copy and let the page retry.
			data.Message = "正在准备你的 Agent，请稍候。"
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if data.AutoRefresh {
		w.Header().Set("Retry-After", strconv.Itoa(loadingRetryAfter))
	}
	w.WriteHeader(http.StatusOK)
	_ = loadingPageTemplate.Execute(w, data)
}

// loadingCopyForStatus maps an actor status to the interstitial's title and
// body. Unknown/transitional states fall back to the generic loading copy.
func loadingCopyForStatus(st ateapipb.Actor_Status) (title, message string) {
	switch st {
	case ateapipb.Actor_STATUS_RESUMING:
		return "正在唤醒 Agent…", "Agent 之前处于挂起状态以节省资源，正在从快照恢复，请稍候。"
	case ateapipb.Actor_STATUS_SUSPENDED:
		return "正在唤醒 Agent…", "Agent 处于挂起状态，正在申请资源并恢复会话。"
	case ateapipb.Actor_STATUS_RUNNING:
		return "Agent 正在启动…", "Agent 已就绪，Web 服务正在启动，马上就好。"
	case ateapipb.Actor_STATUS_SUSPENDING, ateapipb.Actor_STATUS_PAUSING, ateapipb.Actor_STATUS_PAUSED:
		return "Agent 正在切换状态…", "Agent 正在完成上一次的挂起/恢复操作，请稍候。"
	case ateapipb.Actor_STATUS_CRASHED:
		return "Agent 正在重启…", "Agent 刚刚异常退出，正在尝试恢复，请稍候。"
	default:
		return "Agent 正在载入…", "正在准备你的 Agent，请稍候。"
	}
}
