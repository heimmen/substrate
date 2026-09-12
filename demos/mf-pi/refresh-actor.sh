#!/usr/bin/env bash
# Refresh a single mf-pi actor in place: suspend -> delete -> recreate ->
# resume. This simulates an image-refresh / redeploy (delete+recreate) so you
# can manually verify that user data persisted on the sticky PV survives.
#
# The userdata external volume is mount-type "sticky" (see
# internal/volume/sticky): DeleteVolume keeps the backing directory on disk and
# a recreate of the same actor re-attaches the same directory, so whatever the
# user wrote under /data/pi-agent is still there after the refresh.
#
# After the refresh the actor is RUNNING again and re-attached to the SAME
# user/volume, so you can log in with the user's existing password and check
# that their data (projects, saved files, session, ...) is intact.
#
# Usage:
#   ./refresh-actor.sh <username>
#   MFPI_ATESPACE=mfpi-test MFPI_TEMPLATE=ate-demo-mf-pi-test/mf-pi \
#     MFPI_NAMESPACE=ate-demo-mf-pi-test MFPI_PROXY=localhost:59681 \
#     ./refresh-actor.sh alice2
#
# Requires a reachable cluster/proxy and the mf-pi deployment applied
# (./deploy-test.sh) plus the nginx proxy up (./run-nginx-test.sh).

set -euo pipefail
cd "$(dirname "$0")"

# --- Configurable defaults ----------------------------------------------
ATESPACE="${MFPI_ATESPACE:-mfpi-test}"
TEMPLATE="${MFPI_TEMPLATE:-ate-demo-mf-pi-test/mf-pi}"
NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi-test}"
PROXY="${MFPI_PROXY:-localhost:59681}"

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <username>" >&2
  exit 2
fi
USER="$1"

# --- kubectl-ate resolution ---------------------------------------------
# Resolve to a single-word plugin binary when possible so the two-word
# `kubectl ate` form is never quoted into one bogus executable name.
if [[ -n "${KUBECTL_ATE:-}" ]]; then
  read -r -a KUBECTL_ATE_CMD <<<"${KUBECTL_ATE}"
elif command -v kubectl-ate >/dev/null 2>&1; then
  KUBECTL_ATE_CMD=(kubectl-ate)
else
  KUBECTL_ATE_CMD=(kubectl ate)
fi

PASS=0
FAIL=0
log()  { printf '[refresh-actor] %s\n' "$*"; }
ok()   { PASS=$((PASS+1)); log "PASS: $*"; }
bad()  { FAIL=$((FAIL+1)); log "FAIL: $*"; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || { log "SKIP: '$1' not found on PATH"; exit 0; }; }
need_cmd kubectl
need_cmd "${KUBECTL_ATE_CMD[0]}"
need_cmd curl
need_cmd jq

if ! kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1; then
  log "SKIP: namespace '${NAMESPACE}' not found (deploy the test environment first: ./deploy-test.sh)"
  exit 0
fi
if ! curl -sS -o /dev/null -m 5 "http://${PROXY}/usermanagement/healthz"; then
  log "SKIP: mf-pi test nginx proxy not reachable at http://${PROXY} (run ./run-nginx-test.sh)"
  exit 0
fi

log "Target: atespace=${ATESPACE} template=${TEMPLATE} proxy=${PROXY} user=${USER}"

# --- Actor existence + current status -----------------------------------
raw="$("${KUBECTL_ATE_CMD[@]}" get actor "${USER}" -a "${ATESPACE}" -o json 2>&1 || true)"
status="$(printf '%s' "${raw}" | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true)"
if [[ -z "${status}" ]]; then
  bad "actor '${USER}' not found in atespace '${ATESPACE}' (create it first, e.g. via the admin API)"
  exit 1
fi
log "actor '${USER}' current status: ${status}"

# --- Wait helpers -------------------------------------------------------
wait_for_suspended() {
  local i=0 timeout=120 raw st
  while (( i < timeout )); do
    raw="$("${KUBECTL_ATE_CMD[@]}" get actor "${USER}" -a "${ATESPACE}" -o json 2>&1 || true)"
    st="$(printf '%s' "${raw}" | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true)"
    if [[ "$st" == "STATUS_SUSPENDED" ]]; then
      return 0
    fi
    sleep 2
    i=$((i+2))
  done
  log "actor '${USER}' did not reach STATUS_SUSPENDED after ${timeout}s (last status: ${st:-unknown})"
  return 1
}

wait_for_running() {
  local i=0 timeout=300 raw st
  while (( i < timeout )); do
    raw="$("${KUBECTL_ATE_CMD[@]}" get actor "${USER}" -a "${ATESPACE}" -o json 2>&1 || true)"
    st="$(printf '%s' "${raw}" | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true)"
    if [[ "$st" == "STATUS_RUNNING" ]]; then
      return 0
    fi
    # `kubectl ate create actor` does not auto-resume; nudge once after a short
    # grace period and again later to cover transient scheduling races.
    if (( i == 10 || i == 60 )); then
      "${KUBECTL_ATE_CMD[@]}" resume actor "${USER}" -a "${ATESPACE}" >/dev/null 2>&1 || true
    fi
    sleep 2
    i=$((i+2))
  done
  log "actor '${USER}' did not reach STATUS_RUNNING after ${timeout}s (last status: ${st:-unknown})"
  return 1
}

# Best-effort: wait until the proxy can route to the refreshed actor. The proxy
# entry `/<user>` returns 302 when the actor is RUNNING and routed; 503 while it
# is still coming up. Requires no password. Non-fatal.
wait_for_proxy() {
  local i=0 timeout=120 code
  while (( i < timeout )); do
    code="$(curl -sS -o /dev/null -w '%{http_code}' -m 3 "http://${PROXY}/${USER}" 2>/dev/null)"
    if [[ "$code" == "200" || "$code" == "302" ]]; then
      return 0
    fi
    sleep 2
    i=$((i+2))
  done
  return 1
}

# --- Refresh ------------------------------------------------------------
log "refreshing actor '${USER}' (suspend -> delete -> recreate -> resume)"

# delete actor refuses while RUNNING, so suspend and wait for SUSPENDED first.
"${KUBECTL_ATE_CMD[@]}" suspend actor "${USER}" -a "${ATESPACE}" >/dev/null 2>&1 || true
if ! wait_for_suspended; then
  bad "actor '${USER}' did not reach STATUS_SUSPENDED before delete"
  exit 1
fi

"${KUBECTL_ATE_CMD[@]}" delete actor "${USER}" -a "${ATESPACE}" >/dev/null
"${KUBECTL_ATE_CMD[@]}" create actor "${USER}" -a "${ATESPACE}" --template "${TEMPLATE}" >/dev/null
# create actor does NOT auto-resume; resume explicitly so the new instance boots.
"${KUBECTL_ATE_CMD[@]}" resume actor "${USER}" -a "${ATESPACE}" >/dev/null 2>&1 || true

if ! wait_for_running; then
  bad "actor '${USER}' did not reach STATUS_RUNNING after recreate"
  exit 1
fi

if wait_for_proxy; then
  log "proxy reachable at http://${PROXY}/${USER}"
else
  log "WARN: proxy not reachable at http://${PROXY}/${USER} within timeout (actor is RUNNING; it may still be warming up)"
fi

ok "actor '${USER}' refreshed and back to STATUS_RUNNING (user data on the sticky PV should be intact)"

if (( FAIL == 0 )); then
  log "RESULT: PASS (${PASS} passed) — log in as '${USER}' with the existing password to verify data persistence"
  exit 0
else
  log "RESULT: FAIL (${PASS} passed, ${FAIL} failed)"
  exit 1
fi
