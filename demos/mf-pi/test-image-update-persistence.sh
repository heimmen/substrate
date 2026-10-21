#!/usr/bin/env bash
# End-to-end test: mf-pi user-installed skills/files survive an ACTOR IMAGE
# UPDATE (deploy -> ActorTemplate spec change -> refresh existing users).
#
# Why this test exists
# --------------------
# A plain suspend/resume keeps the sandbox rootfs, so rootfs-only data looks
# persistent; an image update cannot reuse that snapshot (image digest and
# ActorTemplate spec changed) and the only durable surface is the sticky
# `userdata` volume at /data/pi-agent. On top of that, the platform's golden
# base snapshot is not restorable against a per-user external volume, so the
# demo must clear it (see docs/persistence_gaps.md and ensure-no-golden.sh).
#
# This script exercises that whole path:
#   1. creates a test user, opens three pi-web projects that live on the PV:
#        /data/pi-agent/skills/<skill>          (user-installed skill)
#        /data/pi-agent/home/.pi/packages/<pkg> (user-installed Pi package,
#                                                i.e. $HOME/.pi/packages)
#        /data/pi-agent/projects/<proj>         (uploaded project file)
#      and writes a sentinel into each,
#   2. runs a REAL redeploy that changes the ActorTemplate spec
#        MFPI_DEPLOY_REV=$(date +%s) ./deploy-test.sh
#      which deletes+recreates the templates and refreshes every affected
#      user (the image-update code path),
#   3. asserts that every sentinel is still readable byte-for-byte, that the
#      actor is RUNNING (not wedged in STATUS_RESUMING), that no golden base
#      snapshot is left on the templates, and that a BRAND-NEW user (empty
#      volume) can resume successfully afterwards.
#
# Requirements (live cluster)
#   * a kind/k3s cluster with Agent Substrate deployed, the pi-web/pause images
#     pushed to ${KO_DOCKER_REPO}, and `ko` + `docker` on PATH (the redeploy
#     re-applies the manifest),
#   * the mf-pi TEST deployment applied (./deploy-test.sh) and the nginx proxy
#     up on the test entry port (./run-nginx-test.sh, default localhost:59681).
#
# Without the prerequisites the script SKIPs (exit 0) with a clear message.
#
# Usage:
#   ./test-image-update-persistence.sh           # test environment (default)
#   ./test-image-update-persistence.sh --prod    # production environment
#
# Override any of the following env vars:
#   MFPI_ATESPACE / MFPI_NAMESPACE / MFPI_PROXY / MFPI_TEMPLATE
#   MFPI_DEPLOY_SCRIPT   (default ./deploy-test.sh, or ./deploy.sh with --prod)
#   ADMIN_USER / ADMIN_PASSWORD  (default admin / mf@pass2026)
#   TEST_USER            (default imgupd-test-<random>)
#   NEW_USER             (default imgupd-new-<random>)

set -euo pipefail
cd "$(dirname "$0")"

# --- Configurable defaults ----------------------------------------------
PROD=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --prod) PROD=1 ;;
    -h|--help) sed -n '2,48p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
  shift
done

if (( PROD )); then
  ATESPACE="${MFPI_ATESPACE:-mfpi}"
  NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi}"
  PROXY="${MFPI_PROXY:-localhost:58681}"
  TEMPLATE="${MFPI_TEMPLATE:-ate-demo-mf-pi/mf-pi-small}"
  DEPLOY_SCRIPT="${MFPI_DEPLOY_SCRIPT:-./deploy.sh}"
  ENV_LABEL="production"
else
  ATESPACE="${MFPI_ATESPACE:-mfpi-test}"
  NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi-test}"
  PROXY="${MFPI_PROXY:-localhost:59681}"
  TEMPLATE="${MFPI_TEMPLATE:-ate-demo-mf-pi-test/mf-pi-small}"
  DEPLOY_SCRIPT="${MFPI_DEPLOY_SCRIPT:-./deploy-test.sh}"
  ENV_LABEL="test"
fi
# NOTE: the test user is created through the admin API (POST /api/users), so the
# actor's template is decided by the admin's tier mapping (DEFAULT_TIER=small ->
# mf-pi-<tier>), not by TEMPLATE. TEMPLATE is only what we report, so it is
# defaulted to the small tier to match what real users get.
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-mf@pass2026}"
STAMP="$(date +%s)"
TEST_USER="${TEST_USER:-imgupd-test-$((STAMP % 100000))}"
NEW_USER="${NEW_USER:-imgupd-new-$((STAMP % 100000))}"

SKILL_PATH="/data/pi-agent/skills/imgupd-skill"
PKG_PATH="/data/pi-agent/home/.pi/packages/imgupd-pkg"
PROJ_PATH="/data/pi-agent/projects/imgupd-proj"
SENTINEL="mf-pi-imgupd-$(date +%s)-$$"

COOKIE_JAR="$(mktemp)"

# Resolve a single-word kubectl-ate plugin binary when possible (same
# convention as the other mf-pi scripts).
if [[ -n "${KUBECTL_ATE:-}" ]]; then
  read -r -a KUBECTL_ATE_CMD <<<"${KUBECTL_ATE}"
elif command -v kubectl-ate >/dev/null 2>&1; then
  KUBECTL_ATE_CMD=(kubectl-ate)
else
  KUBECTL_ATE_CMD=(kubectl ate)
fi

PASS=0
FAIL=0
log()  { printf '[test-image-update-persistence] %s\n' "$*"; }
ok()   { PASS=$((PASS+1)); log "PASS: $*"; }
bad()  { FAIL=$((FAIL+1)); log "FAIL: $*"; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || { log "SKIP: '$1' not found on PATH"; exit 0; }; }
need_cmd kubectl
need_cmd "${KUBECTL_ATE_CMD[0]}"
need_cmd curl
need_cmd jq

if ! command -v ko >/dev/null 2>&1; then
  log "SKIP: 'ko' not found on PATH (needed to re-apply the manifest for the image update)"
  exit 0
fi
if ! command -v docker >/dev/null 2>&1; then
  log "SKIP: 'docker' not found on PATH (needed to resolve the pinned image digests)"
  exit 0
fi
if [[ ! -x "${DEPLOY_SCRIPT}" ]]; then
  log "SKIP: deploy script '${DEPLOY_SCRIPT}' not found"
  exit 0
fi
if ! kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1; then
  log "SKIP: namespace '${NAMESPACE}' not found (deploy the mf-pi ${ENV_LABEL} environment first: ${DEPLOY_SCRIPT})"
  exit 0
fi
if ! curl -sS -o /dev/null -m 5 -u "${ADMIN_USER}:${ADMIN_PASSWORD}" \
     "http://${PROXY}/usermanagement/healthz"; then
  log "SKIP: mf-pi proxy not reachable at http://${PROXY} (run ./run-nginx.sh or ./run-nginx-test.sh)"
  exit 0
fi

# --- Admin / pi-web helpers ---------------------------------------------
admin_api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -u "${ADMIN_USER}:${ADMIN_PASSWORD}"
              -H "Content-Type: application/json"
              "http://${PROXY}/usermanagement${path}")
  case "$method" in
    POST|PUT|DELETE) args=(-X "$method" "${args[@]}") ;;
  esac
  if [[ -n "${body}" ]]; then args+=(--data "${body}"); fi
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
  if [[ -n "${body}" ]]; then args+=(--data "${body}"); fi
  curl "${args[@]}"
}

ensure_cookie() {
  curl -sS -o /dev/null -c "${COOKIE_JAR}" "http://${PROXY}/${TEST_USER}"
}

actor_status() {
  local raw
  raw="$("${KUBECTL_ATE_CMD[@]}" get actor "$1" -a "${ATESPACE}" -o json 2>&1 || true)"
  printf '%s' "${raw}" | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true
}

wait_for_status() { # wait_for_status <user> <target> <timeout-sec> [nudge]
  local u="$1" target="$2" timeout="$3" nudge="${4:-0}" i=0 st
  while (( i < timeout )); do
    st="$(actor_status "${u}")"
    if [[ "${st}" == "${target}" ]]; then return 0; fi
    if (( nudge )) && { [[ $i -eq 10 ]] || [[ $i -eq 60 ]]; }; then
      # --boot: never restore the (demo-cleared) golden base snapshot.
      "${KUBECTL_ATE_CMD[@]}" resume actor "${u}" -a "${ATESPACE}" --boot >/dev/null 2>&1 || true
    fi
    sleep 2
    i=$((i+2))
  done
  log "actor '${u}' did not reach ${target} after ${timeout}s (last status: ${st:-unknown})"
  return 1
}

wait_for_piweb() { # wait_for_piweb <user> <password>
  local u="$1" pw="$2" i=0 timeout=180 resp
  while (( i < timeout )); do
    resp="$(curl -sS -u "${u}:${pw}" "http://${PROXY}/${u}/api/projects" 2>/dev/null || true)"
    if printf '%s' "${resp}" | jq -e . >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
    i=$((i+2))
  done
  log "pi-web for '${u}' did not become reachable after ${timeout}s (last: ${resp:-<none>})"
  return 1
}

# Create (or open) a pi-web project rooted at an absolute PV path, write the
# sentinel into its first workspace, and echo the project id. Uses the real
# workspace file API, exactly like a user uploading files.
project_write() { # project_write <abs-path> <filename> <content>
  local path="$1" name="$2" content="$3" resp pid ws wsid
  resp="$(piweb_api POST /api/projects "{\"path\":\"${path}\",\"create\":true}")"
  pid="$(printf '%s' "${resp}" | jq -r '.id // empty' 2>/dev/null || true)"
  if [[ -z "${pid}" ]]; then
    pid="$(piweb_api GET /api/projects \
      | jq -r --arg p "${path}" '.[] | select(.path==$p) | .id // empty' 2>/dev/null | head -n1 || true)"
  fi
  if [[ -z "${pid}" ]]; then
    printf 'could not open project %s (response: %s)\n' "${path}" "${resp}" >&2
    return 1
  fi
  ws="$(piweb_api GET "/api/projects/${pid}/workspaces")"
  wsid="$(printf '%s' "${ws}" | jq -r '.workspaces[0].id // .[0].id // empty' 2>/dev/null || true)"
  if [[ -z "${wsid}" ]]; then
    printf 'could not resolve a workspace for project %s (response: %s)\n' "${path}" "${ws}" >&2
    return 1
  fi
  resp="$(piweb_api PUT "/api/projects/${pid}/workspaces/${wsid}/file?path=${name}&overwrite=true" "${content}" "text/plain")"
  if ! printf '%s' "${resp}" | jq -e '.path // .ok // false' >/dev/null 2>&1; then
    printf 'could not write %s under %s (response: %s)\n' "${name}" "${path}" "${resp}" >&2
    return 1
  fi
  printf '%s' "${pid}"
}

project_read() { # project_read <abs-path> <filename> -> file content
  local path="$1" name="$2" pid ws wsid
  pid="$(piweb_api GET /api/projects \
      | jq -r --arg p "${path}" '.[] | select(.path==$p) | .id // empty' 2>/dev/null | head -n1 || true)"
  if [[ -z "${pid}" ]]; then
    printf 'project %s not found after the image update\n' "${path}" >&2
    return 1
  fi
  ws="$(piweb_api GET "/api/projects/${pid}/workspaces")"
  wsid="$(printf '%s' "${ws}" | jq -r '.workspaces[0].id // .[0].id // empty' 2>/dev/null || true)"
  if [[ -z "${wsid}" ]]; then
    printf 'no workspace for project %s after the image update (response: %s)\n' "${path}" "${ws}" >&2
    return 1
  fi
  piweb_api GET "/api/projects/${pid}/workspaces/${wsid}/file?path=${name}" \
    | jq -r '.content // empty' 2>/dev/null || true
}

remove_user() { # best-effort admin delete (purges the user's PV)
  local u="$1" st i=0
  st="$(actor_status "${u}")"
  if [[ "${st}" == "STATUS_RUNNING" ]]; then
    "${KUBECTL_ATE_CMD[@]}" suspend actor "${u}" -a "${ATESPACE}" >/dev/null 2>&1 || true
    while (( i < 120 )); do
      st="$(actor_status "${u}")"
      [[ "${st}" == "STATUS_SUSPENDED" ]] && break
      sleep 2
      i=$((i+2))
    done
  fi
  admin_api DELETE "/api/users/${u}" >/dev/null 2>&1 || \
    log "WARNING: admin DELETE of '${u}' failed (leftover actor/PV may remain)"
}

cleanup() {
  if [[ -n "${TEST_USER:-}" ]]; then remove_user "${TEST_USER}"; fi
  if [[ -n "${NEW_USER:-}" ]]; then remove_user "${NEW_USER}"; fi
  rm -f "${COOKIE_JAR}"
}
trap cleanup EXIT

# Remove leftover users from earlier interrupted runs (same prefix) so the
# image-update refresh does not consume the (small) test worker pool.
cleanup_leftover_test_users() {
  local names n
  names="$(admin_api GET /api/users 2>/dev/null \
            | jq -r '.users[]?.name // empty' 2>/dev/null \
            | grep -E '^imgupd-' || true)"
  while IFS= read -r n; do
    [[ -z "${n}" ]] && continue
    log "cleaning up leftover test user '${n}'"
    remove_user "${n}"
  done < <(printf '%s\n' "${names}")
}

cleanup_leftover_test_users
# The redeploy refreshes EVERY user on a changed template, and the test pool
# has only a couple of worker slots; unrelated users can exhaust it.
OTHERS="$(admin_api GET /api/users 2>/dev/null \
            | jq -r '.users[]?.name // empty' 2>/dev/null \
            | grep -vE '^imgupd-' | wc -l | tr -d ' ' || true)"
if [[ -n "${OTHERS}" && "${OTHERS}" -gt 0 ]]; then
  log "WARNING: ${OTHERS} unrelated user(s) exist; the redeploy refreshes them too and may exhaust the worker pool"
fi

# --- 1. test user + sentinels -------------------------------------------
log "Target: atespace=${ATESPACE} namespace=${NAMESPACE} template=${TEMPLATE} proxy=${PROXY} user=${TEST_USER}"

CREATE_RESP="$(admin_api POST /api/users "{\"name\":\"${TEST_USER}\"}")"
TEST_PASSWORD="$(printf '%s' "${CREATE_RESP}" | jq -r '.password // empty')"
if [[ -z "${TEST_PASSWORD}" ]]; then
  log "user '${TEST_USER}' already exists; resetting its password"
  TEST_PASSWORD="$(admin_api POST "/api/users/${TEST_USER}/password" | jq -r '.password // empty')"
fi
if [[ -z "${TEST_PASSWORD}" ]]; then
  bad "could not obtain a test-user password (admin response: ${CREATE_RESP})"
  exit 1
fi
log "test user created; waiting for the actor to be RUNNING"
if ! wait_for_status "${TEST_USER}" "STATUS_RUNNING" 300 1; then
  bad "actor '${TEST_USER}' did not reach STATUS_RUNNING"
  exit 1
fi
ensure_cookie
if ! wait_for_piweb "${TEST_USER}" "${TEST_PASSWORD}"; then
  bad "pi-web did not become reachable before writing the sentinels"
  exit 1
fi

if ! SKILL_PID="$(project_write "${SKILL_PATH}" "SKILL.md" "${SENTINEL}-skill")"; then
  bad "could not write the skill sentinel under ${SKILL_PATH}"
  exit 1
fi
if ! PKG_PID="$(project_write "${PKG_PATH}" "package.json" "${SENTINEL}-pkg")"; then
  bad "could not write the Pi-package sentinel under ${PKG_PATH}"
  exit 1
fi
if ! PROJ_PID="$(project_write "${PROJ_PATH}" "note.txt" "${SENTINEL}-proj")"; then
  bad "could not write the project sentinel under ${PROJ_PATH}"
  exit 1
fi
ok "sentinels written under /data/pi-agent (skill=${SKILL_PID}, pkg=${PKG_PID}, project=${PROJ_PID})"

# --- 2. image update: real redeploy with a changed ActorTemplate spec ----
log "forcing an image-update redeploy: MFPI_DEPLOY_REV=${STAMP} ${DEPLOY_SCRIPT}"
if ! MFPI_DEPLOY_REV="${STAMP}" "${DEPLOY_SCRIPT}"; then
  bad "redeploy failed (${DEPLOY_SCRIPT}); see its output above"
  exit 1
fi
ok "redeploy completed (ActorTemplate spec changed -> users refreshed)"

# --- 3. assertions ------------------------------------------------------
if ! wait_for_status "${TEST_USER}" "STATUS_RUNNING" 300 1; then
  bad "actor '${TEST_USER}' did not reach STATUS_RUNNING after the image update (wedged in STATUS_RESUMING?)"
  exit 1
fi
ok "actor is RUNNING after the image update (no STATUS_RESUMING wedge)"

ensure_cookie
if ! wait_for_piweb "${TEST_USER}" "${TEST_PASSWORD}"; then
  bad "pi-web did not come back after the image update"
  exit 1
fi
ok "pi-web is serving again after the image update"

# No golden base snapshot may be left behind: it is not restorable against the
# per-user volume (docs/persistence_gaps.md).
GOLDEN_LEFT=0
for at in mf-pi mf-pi-small mf-pi-mid mf-pi-large; do
  g="$(kubectl get actortemplate "${at}" -n "${NAMESPACE}" -o jsonpath='{.status.goldenSnapshot}' 2>/dev/null || true)"
  if [[ -n "${g}" ]]; then
    bad "ActorTemplate ${at} still has a golden base snapshot (${g})"
    GOLDEN_LEFT=$((GOLDEN_LEFT+1))
  fi
done
if (( GOLDEN_LEFT == 0 )); then
  ok "no golden base snapshot on any mf-pi ActorTemplate"
fi

check_survived() { # check_survived <label> <path> <file> <want>
  local label="$1" path="$2" file="$3" want="$4" got
  got="$(project_read "${path}" "${file}" || true)"
  if [[ "${got}" == "${want}" ]]; then
    ok "${label} survived the image update"
  else
    bad "${label} LOST after the image update (want '${want}', got '${got:-<empty>}')"
  fi
}
check_survived "user-installed skill (${SKILL_PATH})" "${SKILL_PATH}" "SKILL.md" "${SENTINEL}-skill"
check_survived "user-installed Pi package (${PKG_PATH})" "${PKG_PATH}" "package.json" "${SENTINEL}-pkg"
check_survived "uploaded project file (${PROJ_PATH})" "${PROJ_PATH}" "note.txt" "${SENTINEL}-proj"

# A brand-new user (empty volume) must also be able to start: this is the
# regression that a leftover golden base snapshot caused.
log "creating a brand-new user to prove empty volumes resume cleanly"
NEW_RESP="$(admin_api POST /api/users "{\"name\":\"${NEW_USER}\"}")"
NEW_PASSWORD="$(printf '%s' "${NEW_RESP}" | jq -r '.password // empty')"
if [[ -z "${NEW_PASSWORD}" ]]; then
  bad "could not create the new user (admin response: ${NEW_RESP})"
else
  if wait_for_status "${NEW_USER}" "STATUS_RUNNING" 300 1; then
    ok "brand-new user resumed successfully after the image update"
    if wait_for_piweb "${NEW_USER}" "${NEW_PASSWORD}"; then
      ok "brand-new user's pi-web is serving"
    else
      bad "brand-new user's pi-web did not become reachable"
    fi
  else
    bad "brand-new user did not reach STATUS_RUNNING (empty-volume golden-restore regression?)"
  fi
fi

# --- 4. result ----------------------------------------------------------
if (( FAIL == 0 )); then
  log "RESULT: PASS (${PASS} passed)"
  exit 0
fi
log "RESULT: FAIL (${PASS} passed, ${FAIL} failed)"
exit 1
