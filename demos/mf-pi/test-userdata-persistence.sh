#!/usr/bin/env bash
# End-to-end test: mf-pi TEST deployment persists user data across an actor
# refresh (delete + recreate).
#
# What it verifies
# ----------------
# The mf-pi test ActorTemplate mounts a sticky userdata ExternalVolumeTemplate
# at /data/pi-agent and (since the PI_WEB_DATA_DIR=/data/pi-agent fix) pi-web
# stores its real data there. The sticky volume plugin keeps the backing
# directory on actor delete (DeleteVolume is a no-op) and re-attaches the same
# directory on recreate (volumeID is actor-stable), so user data survives a
# delete+recreate redeploy (image refresh).
#
# The test asserts BOTH axes of recovery after a refresh:
#   * Data recovery: a sentinel written to /data/pi-agent (via the pi-web
#     workspace file API, which resolves to a path on the PV) is still readable
#     after suspend + delete + recreate + resume.
#   * The /data directory itself: a best-effort in-actor probe (runsc exec on
#     the kind node) checks that the live, refreshed actor still mounts
#     /data/pi-agent and that the recovered sentinel is present there. This
#     guards the regression "after a refresh there is no /data dir in the
#     actor": the persisted store is the PV mounted at /data/pi-agent, so the
#     directory must survive. (For manual inspection see exec-actor.sh, which
#     drops a shell on the node at the actor's persisted /data/pi-agent.)
#
# This script:
#   1. creates a test user (which creates + resumes the actor and returns a
#      one-time access password),
#   2. writes a sentinel file into the actor's PV via the pi-web workspace
#      file API (the workspace path is an absolute path under /data/pi-agent,
#      so it lands on the PV),
#   3. REFRESHES the actor: kubectl ate suspend actor + delete actor +
#      create actor (the password and PV are preserved),
#   4. reads the sentinel back and asserts it is unchanged -> user data
#      persisted across the refresh.
#
# Requirements (live cluster)
# ---------------------------
#   * a kind/k3s cluster running Agent Substrate with the rebuilt images,
#   * the mf-pi TEST deployment applied (./deploy-test.sh),
#   * the nginx proxy up on the test entry port (./run-nginx-test.sh; default
#     localhost:59681),
#   * kubectl, kubectl-ate, curl, jq on PATH.
#
# Without a reachable cluster/proxy the script SKIPs (exit 0) with a clear
# message, so it is safe to run in CI / on a workstation without the demo.
#
# Override any of the following env vars to retarget:
#   MFPI_ATESPACE        (default mfpi-test)
#   MFPI_TEMPLATE        (default ate-demo-mf-pi-test/mf-pi)
#   MFPI_NAMESPACE       (default ate-demo-mf-pi-test)
#   MFPI_PROXY           (default localhost:59681)
#   ADMIN_USER / ADMIN_PASSWORD  (default admin / mf@pass2026)
#   TEST_USER            (default persist-test-<random>)

set -euo pipefail
cd "$(dirname "$0")"

# --- Configurable defaults ----------------------------------------------
ATESPACE="${MFPI_ATESPACE:-mfpi-test}"
TEMPLATE="${MFPI_TEMPLATE:-ate-demo-mf-pi-test/mf-pi}"
NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi-test}"
PROXY="${MFPI_PROXY:-localhost:59681}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-mf@pass2026}"
# Resolve the kubectl-ate invocation once. It may be installed either as a
# standalone plugin binary (kubectl-ate, preferred: a single token) or only
# reachable via `kubectl ate`. Store it as an array so a multi-word command is
# never quoted into one bogus executable name ("kubectl ate: command not found").
if [[ -n "${KUBECTL_ATE:-}" ]]; then
  read -r -a KUBECTL_ATE_CMD <<<"${KUBECTL_ATE_CMD[@]}"
elif command -v kubectl-ate >/dev/null 2>&1; then
  KUBECTL_ATE_CMD=(kubectl-ate)
else
  KUBECTL_ATE_CMD=(kubectl ate)
fi
TEST_USER="${TEST_USER:-persist-test-$(date +%s | tail -c 6)}"
PROJ_PATH="/data/pi-agent/persist-test"
MARKER="persistence-marker.txt"
SENTINEL="mf-pi-pv-persistence-check-$(date +%s)-$$"

COOKIE_JAR="$(mktemp)"

# Remove a test user (and its actor). If the actor is RUNNING, suspend it first
# and WAIT until it is fully SUSPENDED before issuing the admin DELETE: the
# admin DELETE tears down the actor's volume, and racing it against an in-flight
# suspend wedges the actor in STATUS_SUSPENDING forever (the suspend workflow's
# DetachVolume step then fails with "volume not found", its FinalizeSuspended
# step never runs, and the worker slot leaks). Observed 2026-09-11 with
# mock-vol-17/persist-test-46541. All steps are best-effort.
remove_test_user() {
  local raw status
  raw="$("${KUBECTL_ATE_CMD[@]}" get actor "$1" -a "${ATESPACE}" -o json 2>&1 || true)"
  status="$(printf '%s' "${raw}" | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true)"
  if [[ "$status" == "STATUS_RUNNING" ]]; then
    "${KUBECTL_ATE_CMD[@]}" suspend actor "$1" -a "${ATESPACE}" >/dev/null 2>&1 || true
    local i=0
    while (( i < 120 )); do
      raw="$("${KUBECTL_ATE_CMD[@]}" get actor "$1" -a "${ATESPACE}" -o json 2>&1 || true)"
      status="$(printf '%s' "${raw}" | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true)"
      [[ "$status" == "STATUS_SUSPENDED" ]] && break
      sleep 2
      i=$((i+2))
    done
  fi
  if ! admin_api DELETE "/api/users/$1" >/dev/null 2>&1; then
    log "WARNING: admin DELETE of '$1' failed (actor may be wedged in SUSPENDING; free its worker pod or ignore)"
  fi
}

cleanup() {
  # Best-effort: remove the test user (and its actor) so interrupted/failed
  # runs never leak actors and exhaust the test WorkerPool's worker slots.
  if [[ -n "${TEST_USER:-}" ]]; then
    remove_test_user "${TEST_USER}"
  fi
  rm -f "${COOKIE_JAR}"
}
trap cleanup EXIT

PASS=0
FAIL=0

log()  { printf '[test-userdata-persistence] %s\n' "$*"; }
ok()   { PASS=$((PASS+1)); log "PASS: $*"; }
bad()  { FAIL=$((FAIL+1)); log "FAIL: $*"; }

admin_api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -u "${ADMIN_USER}:${ADMIN_PASSWORD}"
              -H "Content-Type: application/json"
              "http://${PROXY}/usermanagement${path}")
  case "$method" in
    POST|PUT|DELETE) args=(-X "$method" "${args[@]}") ;;
  esac
  if [[ -n "$body" ]]; then args+=(--data "$body"); fi
  curl "${args[@]}"
}

piweb_api() {
  local method="$1" path="$2" body="${3:-}" ct="${4:-application/json}"
  local args=(-sS -u "${TEST_USER}:${TEST_PASSWORD}"
              -b "${COOKIE_JAR}" -H "Content-Type: ${ct}"
              "http://${PROXY}/${TEST_USER}${path}")
  case "$method" in
    POST|PUT|DELETE) args=(-X "$method" "${args[@]}") ;;
  esac
  if [[ -n "$body" ]]; then args+=(--data "$body"); fi
  curl "${args[@]}"
}

ensure_cookie() {
  curl -sS -o /dev/null -c "${COOKIE_JAR}" "http://${PROXY}/${TEST_USER}"
}

# The gateway/router can take several seconds to register a freshly (re)created
# actor and pi-web inside it may still be starting, so pi-web calls issued right
# after STATUS_RUNNING can return a 503 "upstream connect error" from the
# atenet-router (verified: a new actor answers 503 for ~6s, then 200). Wait
# until pi-web answers with valid JSON before proceeding.
wait_for_piweb() {
  local i=0 timeout=120 resp
  while (( i < timeout )); do
    resp="$(piweb_api GET /api/projects 2>/dev/null || true)"
    if printf '%s' "${resp}" | jq -e . >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
    i=$((i+2))
  done
  log "pi-web did not become reachable after ${timeout}s (last response: ${resp})"
  return 1
}

# Remove any previously-created test users (same naming prefix) so repeated runs
# do not accumulate suspended actors. Best-effort; ignores API errors.
cleanup_leftover_test_users() {
  local names
  names="$(admin_api GET /api/users 2>/dev/null \
            | jq -r '.users[]?.name // empty' 2>/dev/null \
            | grep -E '^persist-test-' || true)"
  for n in $names; do
    [[ "$n" == "$TEST_USER" ]] && continue
    log "cleaning up leftover test user '$n'"
    remove_test_user "$n"
  done
}

wait_for_running() {
  local i=0 timeout=300
  while (( i < timeout )); do
    # kubectl-ate writes "Error: ..." to STDOUT (not stderr) when the actor is
    # not (yet) visible, e.g. right after creation. Capture stdout+stderr and let
    # jq fail softly so a transient race is treated as "not running yet" instead
    # of aborting the whole script under `set -e`.
    local raw status
    raw="$("${KUBECTL_ATE_CMD[@]}" get actor "${TEST_USER}" -a "${ATESPACE}" -o json 2>&1 || true)"
    status="$(printf '%s' "${raw}" | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true)"
    if [[ "$status" == "STATUS_RUNNING" ]]; then
      return 0
    fi
    # `kubectl ate create actor` (and a possibly-failed admin auto-resume) leaves
    # the actor non-running until something resumes it. Nudge a resume once after
    # a short grace period, and again later, to cover transient scheduling races.
    # Resume is a no-op on an already-running actor.
    if (( i == 10 || i == 60 )); then
      "${KUBECTL_ATE_CMD[@]}" resume actor "${TEST_USER}" -a "${ATESPACE}" >/dev/null 2>&1 || true
    fi
    sleep 2
    i=$((i+2))
  done
  # Surface the real status/conditions so a failure is diagnosable.
  log "actor '${TEST_USER}' status diagnostics:"
  local diag
  diag="$("${KUBECTL_ATE_CMD[@]}" get actor "${TEST_USER}" -a "${ATESPACE}" -o json 2>&1 || true)"
  printf '%s\n' "${diag}" | jq . 2>/dev/null || printf '%s\n' "${diag}"
  log "HINT: if the actor is stuck non-running, the test WorkerPool likely has no free worker slot (default MFPI_WORKER_REPLICAS=2). Free a worker (suspend/delete another test actor) or re-deploy with a higher MFPI_WORKER_REPLICAS."
  return 1
}

# Suspend is asynchronous (SUSPENDING -> SUSPENDED); `delete actor` refuses
# until the actor is fully SUSPENDED, so wait for it.
wait_for_suspended() {
  local i=0 timeout=120 raw status
  while (( i < timeout )); do
    raw="$("${KUBECTL_ATE_CMD[@]}" get actor "${TEST_USER}" -a "${ATESPACE}" -o json 2>&1 || true)"
    status="$(printf '%s' "${raw}" | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true)"
    if [[ "$status" == "STATUS_SUSPENDED" ]]; then
      return 0
    fi
    sleep 2
    i=$((i+2))
  done
  log "actor '${TEST_USER}' did not reach STATUS_SUSPENDED after ${timeout}s (last status: ${status})"
  return 1
}

# Best-effort in-actor /data verification. Substrate runs each actor inside a
# gVisor sandbox on a distroless worker pod, so there is no ssh/kubectl exec
# into it. But runsc (the gVisor runtime) is present on the kind node and can
# `exec` directly into the running "pi-web" container. We use that to confirm
# the refreshed actor still mounts /data/pi-agent (the sticky PV) and that the
# recovered sentinel is present there — directly guarding the "no /data dir
# after refresh" regression. Skips (no-op) when docker / the kind node / runsc
# are not reachable (e.g. CI without the local kind cluster). A transient or
# unreachable runsc exec is logged as a WARN and does NOT fail the suite — the
# authoritative recovery check is the API-level sentinel read-back above.
verify_in_actor_data_dir() {
  # NOTE: the caller runs under `set -euo pipefail`. Every command substitution
  # here must be guarded (`cmd || true`) or wrapped in `if ! var=$(...)`: an
  # unguarded substitution that exits non-zero would abort the whole suite
  # before our WARN/skip handling runs. This function must NEVER change the
  # test's exit status — it is purely supplementary to the API-level check.
  local NODE="${NODE:-kind-control-plane}"
  local REXEC_TIMEOUT=20
  command -v docker >/dev/null 2>&1 || { log "WARN in-actor /data check skipped: docker not on PATH"; return 0; }
  if ! docker exec "$NODE" true >/dev/null 2>&1; then
    log "WARN in-actor /data check skipped: node ${NODE} unreachable"
    return 0
  fi
  local uid
  uid="$("${KUBECTL_ATE_CMD[@]}" get actor "${TEST_USER}" -a "${ATESPACE}" -o json 2>/dev/null | jq -r '.actors[0].metadata.uid // empty' || true)"
  [[ -z "$uid" ]] && { log "WARN in-actor /data check skipped: no actor uid"; return 0; }
  local state="/var/lib/ateom-gvisor/actors/${uid}/runsc-state"
  local runsc
  runsc="$(docker exec "$NODE" sh -c 'ls /var/lib/ateom-gvisor/static-files/runsc-* 2>/dev/null | head -1' || true)"
  [[ -z "$runsc" ]] && { log "WARN in-actor /data check skipped: runsc not found on node"; return 0; }

  local data_ls
  # `runsc exec` prints a "waiting on pid N: sandbox is not running" notice to
  # stderr and exits non-zero even when it successfully reads the (paused)
  # container rootfs, so we must NOT treat a non-zero exit as failure. Capture
  # combined output (`|| true` keeps `set -e` happy), strip the notice line, and
  # decide from the directory entries themselves.
  data_ls="$(timeout "${REXEC_TIMEOUT}" docker exec "$NODE" "$runsc" --root "$state" exec pi-web ls /data 2>&1 || true)"
  local entries
  entries="$(printf '%s\n' "$data_ls" | grep -vE '^(waiting on|$)' || true)"
  if printf '%s\n' "$entries" | grep -qx 'pi-agent'; then
    ok "live actor exposes /data/pi-agent after refresh (PV mounted)"
  elif [[ -n "$entries" ]]; then
    bad "live actor /data lacks pi-agent after refresh (ls /data: ${entries})"
  else
    log "WARN in-actor /data check skipped: runsc exec inconclusive (output: ${data_ls:-<none>})"
  fi

  local marker
  marker="$(timeout "${REXEC_TIMEOUT}" docker exec "$NODE" "$runsc" --root "$state" exec pi-web cat "${PROJ_PATH}/${MARKER}" 2>&1 || true)"
  if printf '%s\n' "$marker" | grep -qF "${SENTINEL}"; then
    ok "live actor /data/pi-agent contains the recovered sentinel after refresh"
  else
    log "WARN in-actor sentinel not readable via runsc (got: ${marker:-<none>}) — relying on API-level recovery check"
  fi
}

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

log "Target: atespace=${ATESPACE} template=${TEMPLATE} proxy=${PROXY} user=${TEST_USER}"

# Drop any leftover test users from earlier runs so they don't pile up.
cleanup_leftover_test_users

CREATE_RESP="$(admin_api POST /api/users "{\"name\":\"${TEST_USER}\"}")"
TEST_PASSWORD="$(printf '%s' "${CREATE_RESP}" | jq -r '.password // empty')"
if [[ -z "${TEST_PASSWORD}" ]]; then
  log "user '${TEST_USER}' already exists; resetting its password"
  RESET_RESP="$(admin_api POST "/api/users/${TEST_USER}/password")"
  TEST_PASSWORD="$(printf '%s' "${RESET_RESP}" | jq -r '.password // empty')"
fi
if [[ -z "${TEST_PASSWORD}" ]]; then
  bad "could not obtain a test-user password (admin response: ${CREATE_RESP})"
  exit 1
fi
log "test user created; waiting for actor to be RUNNING"
if ! wait_for_running; then
  bad "actor '${TEST_USER}' did not reach STATUS_RUNNING"
  exit 1
fi
ensure_cookie
wait_for_piweb

PROJ_RESP="$(piweb_api POST /api/projects "{\"path\":\"${PROJ_PATH}\",\"create\":true}")"
PROJ_ID="$(printf '%s' "${PROJ_RESP}" | jq -r '.id // empty' 2>/dev/null || true)"
if [[ -z "${PROJ_ID}" ]]; then
  bad "failed to create pi-web project (response: ${PROJ_RESP})"
  exit 1
fi
WS_RESP="$(piweb_api GET "/api/projects/${PROJ_ID}/workspaces")"
# GET .../workspaces returns {"workspaces":[...]} (older builds return a bare
# array), so accept both shapes.
ws_id_from_list() {
  printf '%s' "$1" | jq -r '.workspaces[0].id // .[0].id // empty' 2>/dev/null || true
}

WS_ID="$(ws_id_from_list "${WS_RESP}")"
if [[ -z "${WS_ID}" ]]; then
  bad "failed to list pi-web workspaces (response: ${WS_RESP})"
  exit 1
fi
log "project=${PROJ_ID} workspace=${WS_ID}; writing sentinel to ${PROJ_PATH}/${MARKER}"

WRITE_RESP="$(piweb_api PUT "/api/projects/${PROJ_ID}/workspaces/${WS_ID}/file?path=${MARKER}&overwrite=true" "${SENTINEL}" "text/plain")"
if ! printf '%s' "${WRITE_RESP}" | jq -e '.path // .ok // false' >/dev/null 2>&1; then
  bad "failed to write sentinel file (response: ${WRITE_RESP})"
  exit 1
fi

READ_BEFORE="$(piweb_api GET "/api/projects/${PROJ_ID}/workspaces/${WS_ID}/file?path=${MARKER}" | jq -r '.content // empty' 2>/dev/null || true)"
if [[ "${READ_BEFORE}" == "${SENTINEL}" ]]; then
  ok "sentinel written and readable (pre-refresh)"
else
  bad "sentinel read-back mismatch before refresh (got: ${READ_BEFORE})"
  exit 1
fi

log "refreshing actor '${TEST_USER}' (suspend + delete + recreate + resume)"
# `delete actor` refuses while the actor is RUNNING, so suspend and wait first.
"${KUBECTL_ATE_CMD[@]}" suspend actor "${TEST_USER}" -a "${ATESPACE}" >/dev/null 2>&1 || true
if ! wait_for_suspended; then
  bad "actor '${TEST_USER}' did not reach STATUS_SUSPENDED before delete"
  exit 1
fi
"${KUBECTL_ATE_CMD[@]}" delete actor "${TEST_USER}" -a "${ATESPACE}" >/dev/null
"${KUBECTL_ATE_CMD[@]}" create actor "${TEST_USER}" -a "${ATESPACE}" --template "${TEMPLATE}" >/dev/null
# `create actor` does NOT auto-resume; resume explicitly so the new instance boots.
"${KUBECTL_ATE_CMD[@]}" resume actor "${TEST_USER}" -a "${ATESPACE}" >/dev/null 2>&1 || true
if ! wait_for_running; then
  bad "actor '${TEST_USER}' did not reach STATUS_RUNNING after recreate"
  exit 1
fi
ensure_cookie
wait_for_piweb

PROJ_LIST="$(piweb_api GET /api/projects)"
PROJ_ID="$(printf '%s' "${PROJ_LIST}" | jq -r --arg p "${PROJ_PATH}" '.[] | select(.path==$p) | .id // empty' 2>/dev/null | head -n1 || true)"
if [[ -z "${PROJ_ID}" ]]; then
  bad "project at ${PROJ_PATH} not found after refresh (user data may have been lost)"
  exit 1
fi
WS_ID="$(ws_id_from_list "$(piweb_api GET "/api/projects/${PROJ_ID}/workspaces")")"
READ_AFTER="$(piweb_api GET "/api/projects/${PROJ_ID}/workspaces/${WS_ID}/file?path=${MARKER}" | jq -r '.content // empty' 2>/dev/null || true)"
if [[ "${READ_AFTER}" == "${SENTINEL}" ]]; then
  ok "sentinel survived actor refresh (user data persisted in PV)"
else
  bad "sentinel LOST after refresh (got: ${READ_AFTER})"
fi

# Best-effort: confirm the live, refreshed actor still mounts /data/pi-agent and
# holds the recovered sentinel there (guards the "no /data dir after refresh"
# regression). Skips gracefully when a kind node / runsc is not reachable. The
# `|| true` keeps this supplementary probe from ever affecting the suite exit
# status under `set -e`.
verify_in_actor_data_dir || true

log "cleaning up test user '${TEST_USER}'"
remove_test_user "${TEST_USER}"

if (( FAIL == 0 )); then
  log "RESULT: PASS (${PASS} passed)"
  exit 0
else
  log "RESULT: FAIL (${PASS} passed, ${FAIL} failed)"
  exit 1
fi
