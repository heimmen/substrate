#!/usr/bin/env bash
# End-to-end test: mf-pi "Agent is loading" interstitial in the TEST
# environment (namespace ate-demo-mf-pi-test, atespace mfpi-test, entry port
# 59681).
#
# What it verifies
# ----------------
# Opening an agent whose actor is not RUNNING (e.g. suspended to save
# resources) must immediately show a friendly "Agent 正在载入" page that
# auto-retries, instead of a blank tab (the router blocks ~4s on resume and
# then answers 200, so without the gate there is no error to react to) or a
# bare 403/500/503.
#
#   1. Provision a test actor and give it a page password.
#   2. While the actor is SUSPENDED, GET /<user>/ with Accept: text/html must
#      return the interstitial page (marker meta tag) promptly, not the real
#      pi-web page. This is the behaviour the readiness gate adds.
#   3. The page must advertise an auto-refresh (meta refresh).
#   4. Re-requesting until the actor is RUNNING must eventually serve the real
#      pi-web UI ("PI WEB"), i.e. the interstitial is a transient state.
#   5. API/WebSocket clients must NOT receive the HTML page: an API request
#      with Accept: application/json while not ready must get a raw 503.
#   6. The internal gate/loading endpoints must not be reachable from outside.
#
# Requirements (live cluster)
# ---------------------------
#   * the mf-pi TEST deployment applied with the CURRENT admin (./deploy-test.sh)
#     — the gate is a new endpoint, so an old admin makes step 1 fail loudly,
#   * the test nginx proxy running the CURRENT nginx-test.conf
#     (./run-nginx-test.sh), reachable on the entry port,
#   * curl, jq, kubectl, kubectl-ate on PATH.
#
# Without a reachable cluster/entry port the script SKIPs (exit 0) with a clear
# message, so it is safe to run on a workstation without the demo.
#
# Override any of the following env vars to retarget:
#   MFPI_ATESPACE      (default mfpi-test)
#   MFPI_NAMESPACE     (default ate-demo-mf-pi-test)
#   MFPI_ENTRY_PORT    (default 59681; the test nginx entry)
#   MFPI_ADMIN_PORT    (default 59882; mfpi-admin port-forward)
#   TEST_USER          (default loadtest-<random>)
#   READY_TIMEOUT      (default 180; seconds to wait for the real page)

set -euo pipefail
cd "$(dirname "$0")"

# --- Configurable defaults ----------------------------------------------
ATESPACE="${MFPI_ATESPACE:-mfpi-test}"
NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi-test}"
ENTRY_PORT="${MFPI_ENTRY_PORT:-59681}"
ADMIN_PORT="${MFPI_ADMIN_PORT:-59882}"
TEST_USER="${TEST_USER:-loadtest-$(date +%s | tail -c 6)}"
READY_TIMEOUT="${READY_TIMEOUT:-180}"

# Stable marker embedded in the interstitial (see admin/loading.html). Its
# presence proves we got the mf-pi loading page and not pi-web's own HTML.
LOADING_MARKER='name="mfpi-page" content="loading"'
# Marker embedded in the real pi-web UI.
APP_MARKER='PI WEB'

if [[ -n "${KUBECTL_ATE_CMD:-}" ]]; then
  read -r -a KUBECTL_ATE_CMD <<<"${KUBECTL_ATE_CMD}"
elif command -v kubectl-ate >/dev/null 2>&1; then
  KUBECTL_ATE_CMD=(kubectl-ate)
else
  KUBECTL_ATE_CMD=(kubectl ate)
fi

PASS=0
FAIL=0
log() { printf '[test-loading-page] %s\n' "$*"; }
ok()  { PASS=$((PASS+1)); log "PASS: $*"; }
bad() { FAIL=$((FAIL+1)); log "FAIL: $*"; }

ENTRY="http://127.0.0.1:${ENTRY_PORT}"
ADMIN="http://127.0.0.1:${ADMIN_PORT}"

# --- Actor helpers -------------------------------------------------------
actor_status() {
  "${KUBECTL_ATE_CMD[@]}" get actor "$TEST_USER" -a "$ATESPACE" -o json 2>&1 \
    | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true
}

wait_for_status() { # <status> <timeout-seconds>
  local want="$1" timeout="$2" i=0
  while (( i < timeout )); do
    [[ "$(actor_status)" == "$want" ]] && return 0
    sleep 2; i=$((i+2))
  done
  return 1
}

suspend_actor() {
  "${KUBECTL_ATE_CMD[@]}" suspend actor "$TEST_USER" -a "$ATESPACE" >/dev/null 2>&1 || true
  wait_for_status "STATUS_SUSPENDED" 120
}

cleanup() {
  rm -f "${BODY:-}"
  # The actor may be mid-resume when we tear down (the gate wakes it in the
  # background), and the admin's delete suspends first, so retry until the
  # actor is really gone. DELETE also purges the sticky volume and clears the
  # password entry, so repeated runs stay clean.
  local i
  for i in 1 2 3 4 5; do
    if ! "${KUBECTL_ATE_CMD[@]}" get actor "$TEST_USER" -a "$ATESPACE" >/dev/null 2>&1; then
      return 0
    fi
    curl -sS -X DELETE "${ADMIN}/api/users/${TEST_USER}" >/dev/null 2>&1 || true
    sleep 2
  done
  # Fall back to kubectl-ate if the admin API could not remove it.
  "${KUBECTL_ATE_CMD[@]}" suspend actor "$TEST_USER" -a "$ATESPACE" >/dev/null 2>&1 || true
  "${KUBECTL_ATE_CMD[@]}" delete actor "$TEST_USER" -a "$ATESPACE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- Reachability gates --------------------------------------------------
if ! kubectl get ns "$NAMESPACE" >/dev/null 2>&1; then
  log "SKIP: namespace '${NAMESPACE}' not reachable (no live cluster?) — exiting 0"
  exit 0
fi

# The test nginx entry needs BOTH of its port-forwards: 59681 serves the
# browser-facing page, and its /_mfpi_gate + /_mfpi_loading upstreams go to the
# admin tunnel (59882). A dead tunnel makes the whole flow fail with a raw curl
# error, so bring the proxy up first (run-nginx-test.sh is idempotent and
# replaces stale tunnels). Set MFPI_NO_NGINX_REFRESH=1 to only check.
proxy_ready() {
  curl -s -o /dev/null -m 5 "${ENTRY}/" 2>/dev/null \
    && curl -s -o /dev/null -m 5 "${ADMIN}/_mfpi_loading" 2>/dev/null
}
if ! proxy_ready; then
  if [[ "${MFPI_NO_NGINX_REFRESH:-}" == "1" ]]; then
    log "SKIP: test proxy not reachable (${ENTRY} / ${ADMIN}) — run ./run-nginx-test.sh"
    exit 0
  fi
  log "test proxy not reachable (${ENTRY} / ${ADMIN}); running ./run-nginx-test.sh"
  timeout 180 ./run-nginx-test.sh >/dev/null 2>&1 || true
  sleep 1
fi
if ! proxy_ready; then
  log "SKIP: test proxy still not reachable (${ENTRY} / ${ADMIN}) — run ./run-nginx-test.sh"
  exit 0
fi

# The gate is a new admin endpoint. An old admin 404s it, which would make this
# test meaningless, so fail loudly with the fix instead of testing nothing.
gate_code="$(curl -s -o /dev/null -w '%{http_code}' -m 5 "${ADMIN}/_mfpi_gate" 2>/dev/null || echo 000)"
if [[ "$gate_code" == "404" ]]; then
  log "FATAL: ${ADMIN}/_mfpi_gate returned 404; the deployed mfpi-admin is stale."
  log "       Redeploy the test environment with the current source: ./deploy-test.sh"
  exit 1
fi
if [[ "$gate_code" == "000" ]]; then
  log "SKIP: mfpi-admin tunnel ${ADMIN} not answering — run ./run-nginx-test.sh"
  exit 0
fi

log "test user: ${TEST_USER} (atespace ${ATESPACE}, ns ${NAMESPACE})"

# --- 0. Provision the actor + password -----------------------------------
log "== 0. provision test actor and page password =="
if ! "${KUBECTL_ATE_CMD[@]}" get actor "$TEST_USER" -a "$ATESPACE" >/dev/null 2>&1; then
  "${KUBECTL_ATE_CMD[@]}" create actor "$TEST_USER" -a "$ATESPACE" \
    --template "ate-demo-mf-pi-test/mf-pi" >/dev/null 2>&1 || true
fi
PASSWORD="$(curl -sS -X POST "${ADMIN}/api/users/${TEST_USER}/password" 2>/dev/null | jq -r '.password // empty' || true)"
if [[ -z "$PASSWORD" ]]; then
  log "FATAL: could not obtain a page password for ${TEST_USER} from ${ADMIN}"
  bad "issue a page password"
  exit 1
fi
ok "issued a page password for ${TEST_USER}"

# Make sure it starts suspended (a fresh actor is).
if [[ "$(actor_status)" == "STATUS_RUNNING" ]]; then
  suspend_actor || true
fi

# --- 1. First navigation shows the interstitial, promptly ----------------
log "== 1. first navigation shows the loading page =="
BODY="$(mktemp)"
code_and_time="$(curl -s -o "$BODY" -w '%{http_code} %{time_total}' \
  -u "${TEST_USER}:${PASSWORD}" -H 'Accept: text/html,application/xhtml+xml' \
  "${ENTRY}/${TEST_USER}/" || echo '000 0')"
status="${code_and_time%% *}"; elapsed="${code_and_time##* }"
log "   status=${status} elapsed=${elapsed}s"
if [[ "$status" == "200" ]] && grep -qF "$LOADING_MARKER" "$BODY"; then
  ok "suspended actor shows the loading page on first navigation"
else
  bad "expected the loading page (marker '${LOADING_MARKER}'), got status=${status}: $(head -c 120 "$BODY")"
fi
# The gate answers immediately; without it the request blocks on the router's
# resume (measured ~4s) before returning the real page.
if awk -v t="$elapsed" 'BEGIN { exit !(t < 3.0) }'; then
  ok "loading page is served immediately (${elapsed}s < 3s)"
else
  bad "loading page was slow (${elapsed}s); the readiness gate may not be active"
fi
if grep -qF 'http-equiv="refresh"' "$BODY"; then
  ok "loading page auto-refreshes"
else
  bad "loading page does not advertise an auto-refresh"
fi

# --- 2. It is transient: the real UI appears once the actor is up --------
log "== 2. the interstitial gives way to the real UI =="
app_ok=0
deadline=$(( SECONDS + READY_TIMEOUT ))
while (( SECONDS < deadline )); do
  code="$(curl -s -o "$BODY" -w '%{http_code}' -u "${TEST_USER}:${PASSWORD}" \
    -H 'Accept: text/html' "${ENTRY}/${TEST_USER}/" || true)"
  if [[ "$code" == "200" ]] && grep -qF "$APP_MARKER" "$BODY"; then app_ok=1; break; fi
  # Nudge a resume in case the gate's background attempt failed (e.g. the pool
  # was momentarily full).
  "${KUBECTL_ATE_CMD[@]}" resume actor "$TEST_USER" -a "$ATESPACE" >/dev/null 2>&1 || true
  sleep 3
done
if (( app_ok )); then
  ok "real pi-web UI served after resume (actor $(actor_status))"
else
  bad "real UI never appeared within ${READY_TIMEOUT}s (actor $(actor_status))"
fi

# --- 3. API clients get a raw 503, never the HTML page -------------------
log "== 3. API clients do not receive the HTML loading page =="
if ! suspend_actor; then
  bad "could not suspend the actor for the API check"
else
  api_code="$(curl -s -o "$BODY" -w '%{http_code}' -u "${TEST_USER}:${PASSWORD}" \
    -H 'Accept: application/json' "${ENTRY}/${TEST_USER}/api/projects" || true)"
  if [[ "$api_code" == "503" ]] && ! grep -qF "$LOADING_MARKER" "$BODY"; then
    ok "API request while not ready got a raw 503 (no HTML page)"
  else
    bad "API request expected 503 without the loading page, got ${api_code}: $(head -c 120 "$BODY")"
  fi
fi

# --- 4. Internal endpoints are not externally reachable -----------------
log "== 4. internal gate/loading endpoints are not exposed =="
for p in "_mfpi_gate" "_mfpi_loading"; do
  c="$(curl -s -o /dev/null -w '%{http_code}' -H 'Accept: text/html' "${ENTRY}/${p}" || true)"
  if [[ "$c" == "404" ]]; then
    ok "/${p} is internal (404 from outside)"
  else
    bad "/${p} should be internal, got ${c}"
  fi
done

# --- Summary -------------------------------------------------------------
log "----------------------------------------"
log "PASS=${PASS} FAIL=${FAIL}"
if (( FAIL > 0 )); then
  log "RESULT: FAILED"
  exit 1
fi
log "RESULT: OK"
