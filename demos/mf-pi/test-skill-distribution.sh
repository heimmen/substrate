#!/usr/bin/env bash
# End-to-end test: mf-pi "管理员统一安装 Skill" (admin-managed shared skills)
# distribution in the TEST environment (namespace ate-demo-mf-pi-test, atespace
# mfpi-test).
#
# What it verifies
# ----------------
# The admin is the authoritative source for skills distributed to every user
# (actor). Each actor runs a 10s background pull loop that diffs the admin's
# manifest against local state and incrementally installs/updates/removes
# managed skills under /data/pi-agent/skills. See deploy_skill_to_actor.md.
#
# This script asserts the full lifecycle, mirroring the manual E2E that was
# done during #12:
#   1. Install (upload) a new skill -> admin parses the tgz and returns a
#      manifest with a sha256.
#   2. Auto incremental pull -> a running test actor picks the skill up within
#      ~10s (its background pull loop downloads, verifies and unpacks it to
#      /data/pi-agent/skills/<name>).
#   3. Incremental update (v2) -> after re-uploading changed content, the actor
#      detects the sha256 change and atomically overwrites the installed copy.
#   4. Unload + isolation boundary -> a user's own, never-managed custom skill
#      is NOT removed; deleting the managed skill removes only the managed dir
#      and cleans .mfpi-managed-skills.json.
#   5. Apply (immediate fan-out reload) -> admin asks every RUNNING actor to
#      reload its open pi-web sessions so new skills take effect now; asserts
#      the response reports all online actors reloaded.
#
# Requirements (live cluster)
# ---------------------------
#   * a kind/k3s cluster running Agent Substrate with the rebuilt images,
#   * the mf-pi TEST deployment applied (./deploy-test.sh),
#   * the admin + actor PVCs healthy (mfpi-shared-skills, per-user userdata),
#   * kubectl, kubectl-ate, curl, jq, tar on PATH,
#   * the mfpi-admin service reachable via kubectl port-forward (no proxy needed
#     for the admin API; actor verification uses the sticky PV on the node).
#
# Without a reachable cluster the script SKIPs (exit 0) with a clear message,
# so it is safe to run in CI / on a workstation without the demo.
#
# Override any of the following env vars to retarget:
#   MFPI_ATESPACE      (default mfpi-test)
#   MFPI_NAMESPACE     (default ate-demo-mf-pi-test)
#   NODE               (default kind-control-plane; used for in-node PV checks)
#   TEST_USER          (default skilltest-<random>; an actor is created for it)
#   ADMIN_USER / ADMIN_PASSWORD  (not used here; admin API is unauthenticated)

set -euo pipefail
cd "$(dirname "$0")"

# --- Configurable defaults ----------------------------------------------
ATESPACE="${MFPI_ATESPACE:-mfpi-test}"
NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi-test}"
NODE="${NODE:-kind-control-plane}"
TEST_USER="${TEST_USER:-skilltest-$(date +%s | tail -c 6)}"
SKILL_NAME="hello-skill"
SKILLS_DIR="/data/pi-agent/skills"
MANAGED_STATE="/data/pi-agent/.mfpi-managed-skills.json"

# Resolve the kubectl-ate invocation once (single token preferred):
if [[ -n "${KUBECTL_ATE_CMD:-}" ]]; then
  read -r -a KUBECTL_ATE_CMD <<<"${KUBECTL_ATE_CMD}"
elif command -v kubectl-ate >/dev/null 2>&1; then
  KUBECTL_ATE_CMD=(kubectl-ate)
else
  KUBECTL_ATE_CMD=(kubectl ate)
fi

PASS=0
FAIL=0

log() { printf '[test-skill-distribution] %s\n' "$*"; }
ok()  { PASS=$((PASS+1)); log "PASS: $*"; }
bad() { FAIL=$((FAIL+1)); log "FAIL: $*"; }

# --- Admin REST client (via kubectl port-forward) ------------------------
PORT=""
PF_PID=""
admin_pf_up() {
  local p i
  for p in $(seq 18282 18382); do
    if ! ss -ltn 2>/dev/null | grep -q "127.0.0.1:${p}[[:space:]]"; then
      PORT="$p"; break
    fi
  done
  [[ -n "$PORT" ]] || { log "no free local port in 18282-18382"; return 1; }
  kubectl port-forward -n "$NAMESPACE" "svc/mfpi-admin" "${PORT}:8080" >/dev/null 2>&1 &
  PF_PID=$!
  for i in $(seq 1 50); do
    if curl -sS -o /dev/null "http://127.0.0.1:${PORT}/api/skills" 2>/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}
admin_pf_down() { [[ -n "$PF_PID" ]] && kill "$PF_PID" 2>/dev/null || true; PF_PID=""; }

admin_upload() { # <name> <file.tgz>
  curl -sS -X POST "http://127.0.0.1:${PORT}/api/skills" \
    -F "name=$1" -F "file=@$2;filename=$(basename "$2")"
}
admin_list() { curl -sS "http://127.0.0.1:${PORT}/api/skills"; }
admin_delete() { curl -sS -X DELETE "http://127.0.0.1:${PORT}/api/skills/$1"; }
admin_apply() { curl -sS -X POST "http://127.0.0.1:${PORT}/api/skills/apply" -H 'Content-Type: application/json' --data '{}'; }

# --- Actor state helpers -------------------------------------------------
actor_status() {
  "${KUBECTL_ATE_CMD[@]}" get actor "$TEST_USER" -a "$ATESPACE" -o json 2>&1 \
    | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true
}
actor_uid() {
  "${KUBECTL_ATE_CMD[@]}" get actor "$TEST_USER" -a "$ATESPACE" -o json 2>&1 \
    | jq -r '.actors[0].metadata.uid // empty' 2>/dev/null || true
}

# The actor's sticky volume dir on the kind node == its /data/pi-agent
# (CONTENTS only, no nested pi-agent/). Keyed by volume name.
actor_pv() {
  docker exec "$NODE" sh -c \
    "ls -d /var/lib/ateom-gvisor/stickyvolumes/*${ATESPACE}-${TEST_USER}-userdata 2>/dev/null \
     || ls -d /var/lib/ateom-gvisor/stickyvolumes/*${TEST_USER}* 2>/dev/null" | head -1 || true
}

wait_for_running() {
  local i=0 timeout=300 status
  while (( i < timeout )); do
    status="$(actor_status)"
    if [[ "$status" == "STATUS_RUNNING" ]]; then return 0; fi
    if (( i == 10 || i == 60 )); then
      "${KUBECTL_ATE_CMD[@]}" resume actor "$TEST_USER" -a "$ATESPACE" >/dev/null 2>&1 || true
    fi
    sleep 2; i=$((i+2))
  done
  log "actor '${TEST_USER}' did not reach STATUS_RUNNING (last: ${status})"
  return 1
}

# Poll the actor's PV on the node until a predicate succeeds. The pull loop
# runs every ~10s, so give it several cycles.
wait_for_pv() { # <description> <relative-path-under-pv> <timeout>
  # Waits until <pv>/<relative-path> exists (as a file or directory).
  local desc="$1" rel="$2" i=0 timeout="${3:-60}" pv
  pv="$(actor_pv)"
  [[ -n "$pv" ]] || { log "no PV found for '${TEST_USER}' on ${NODE}"; return 1; }
  while (( i < timeout )); do
    if docker exec "$NODE" sh -c "test -e '$pv/$rel'" 2>/dev/null; then
      return 0
    fi
    sleep 3; i=$((i+3))
  done
  log "timeout waiting for: $desc (pattern: $rel)"
  return 1
}

read_pv_file() { # <path-relative-to-pv-or-container-data-dir>
  local pv="$(actor_pv)"
  local rel="${1#/data/pi-agent/}"
  rel="${rel#/}"
  [[ -n "$pv" ]] || return 1
  docker exec "$NODE" cat "$pv/$rel" 2>/dev/null || true
}

# --- Package helpers -----------------------------------------------------
make_skill_tgz() { # <dir> -> echoes path to a tgz (top-level dir preserved)
  local dir="$1" out
  out="$(mktemp --suffix=.tgz)"
  ( cd "$(dirname "$dir")" && tar -czf "$out" "$(basename "$dir")" )
  echo "$out"
}

# --- Cleanup -------------------------------------------------------------
remove_actor() {
  local status
  status="$(actor_status)"
  if [[ "$status" == "STATUS_RUNNING" ]]; then
    "${KUBECTL_ATE_CMD[@]}" suspend actor "$TEST_USER" -a "$ATESPACE" >/dev/null 2>&1 || true
    local i=0
    while (( i < 120 )); do
      [[ "$(actor_status)" == "STATUS_SUSPENDED" ]] && break
      sleep 2; i=$((i+2))
    done
  fi
  "${KUBECTL_ATE_CMD[@]}" delete actor "$TEST_USER" -a "$ATESPACE" >/dev/null 2>&1 || true
}

cleanup() {
  # Best-effort: remove the admin skill + test actor + temp files so repeated
  # runs are clean. Delete the skill BEFORE tearing down the port-forward
  # (admin_delete needs it), and only if we own the skill (created it here).
  if [[ -n "${PORT:-}" ]]; then admin_delete "$SKILL_NAME" >/dev/null 2>&1 || true; fi
  admin_pf_down
  if [[ -n "${TEST_USER:-}" ]]; then remove_actor || true; fi
  rm -rf "${WORKDIR:-}"
  rm -f "${TMPDIR:-/tmp}"/mfpi-skill-e2e-*.tgz 2>/dev/null || true
}
WORKDIR="$(mktemp -d)"
trap cleanup EXIT

# --- Cluster reachability gate ------------------------------------------
if ! kubectl get ns "$NAMESPACE" >/dev/null 2>&1; then
  log "SKIP: namespace '${NAMESPACE}' not reachable (no live cluster?) — exiting 0"
  exit 0
fi
if ! admin_pf_up; then
  log "SKIP: mfpi-admin not reachable via port-forward — is deploy-test.sh applied?"
  admin_pf_down
  exit 0
fi
log "admin reachable via port-forward ${PORT}; test user: ${TEST_USER}"

# --- 0. Provision a test actor ------------------------------------------
log "== 0. provision test actor =="
if ! "${KUBECTL_ATE_CMD[@]}" get actor "$TEST_USER" -a "$ATESPACE" >/dev/null 2>&1; then
  "${KUBECTL_ATE_CMD[@]}" create actor "$TEST_USER" -a "$ATESPACE" \
    --template "ate-demo-mf-pi-test/mf-pi" >/dev/null 2>&1 || true
fi
if ! wait_for_running; then
  log "FATAL: could not bring test actor up; aborting"
  bad "provision test actor running"
  exit 1
fi
ok "test actor ${TEST_USER} is STATUS_RUNNING"

# --- 1. Install (upload) a new skill -------------------------------------
log "== 1. install a new skill =="
SKILL_DIR="$WORKDIR/$SKILL_NAME"
mkdir -p "$SKILL_DIR"
cat > "$SKILL_DIR/SKILL.md" <<EOF
---
name: $SKILL_NAME
version: "1.0.0"
---
# $SKILL_NAME
E2E hello skill v1.
EOF
echo "console.log('hello v1');" > "$SKILL_DIR/tool.js"

TGZ1="$(make_skill_tgz "$SKILL_DIR")"
RESP="$(admin_upload "$SKILL_NAME" "$TGZ1")"
# admin returns: {"message":"...","skill":{"name":...,"sha256":...}} on install
#            or: {"message":"...","skills":[...]} on list
if printf '%s' "$RESP" | jq -e '(.skill.name // .skills[]?.name) == "'"$SKILL_NAME"'"' >/dev/null 2>&1; then
  ok "admin accepted skill '${SKILL_NAME}' upload"
else
  bad "admin accepts skill upload: $RESP"
fi
SHA1="$(printf '%s' "$RESP" | jq -r '(.skill.sha256) // (.skills[]? | select(.name=="'"$SKILL_NAME"'") | .sha256) // empty' 2>/dev/null)"
[[ -n "$SHA1" ]] && ok "admin generated sha256 for skill: ${SHA1:0:12}…" || bad "admin returned sha256 in manifest"
LIST_SHA="$(admin_list | jq -r '.skills[]? | select(.name=="'"$SKILL_NAME"'") | .sha256 // empty' 2>/dev/null)"
[[ -n "$LIST_SHA" && "$LIST_SHA" == "$SHA1" ]] && ok "admin list reflects uploaded sha256" || bad "admin '/api/skills' list consistent with upload"

# --- 2. Actor auto incremental pull --------------------------------------
log "== 2. actor auto pulls the new skill (10s loop) =="
if wait_for_pv "skill '${SKILL_NAME}' installed" "skills/${SKILL_NAME}"; then
  ok "actor installed '${SKILL_NAME}' under ${SKILLS_DIR}"
else
  bad "actor installed '${SKILL_NAME}' under ${SKILLS_DIR} (pull loop did not pick it up)"
fi
CONTENT="$(read_pv_file "skills/${SKILL_NAME}/tool.js")"
case "$CONTENT" in
  *"hello v1"*) ok "installed skill content matches uploaded v1" ;;
  *) bad "installed v1 content mismatch: '${CONTENT}'" ;;
esac
# The managed-state write (writeState) happens after renameSync completes,
# which is what makes the skill dir appear. Add a small grace period to let
# the atomic rename of the state file propagate to the host filesystem view.
sleep 3
MANAGED_OK=0
for _i in $(seq 1 5); do
  _state="$(read_pv_file "$MANAGED_STATE")"
  if printf '%s' "$_state" | grep -q "$SKILL_NAME"; then
    MANAGED_OK=1; break
  fi
  sleep 2
done
if [[ "$MANAGED_OK" -eq 1 ]]; then
  ok "actor recorded '${SKILL_NAME}' in managed state"
else
  bad "actor recorded '${SKILL_NAME}' in managed state (state: $(read_pv_file "$MANAGED_STATE"))"
fi

# --- 3. Incremental update (v2) ------------------------------------------
log "== 3. incremental update to v2 =="
cat > "$SKILL_DIR/tool.js" <<EOF
console.log('hello v2');
EOF
TGZ2="$(make_skill_tgz "$SKILL_DIR")"
RESP="$(admin_upload "$SKILL_NAME" "$TGZ2")"
SHA2="$(printf '%s' "$RESP" | jq -r '(.skill.sha256) // (.skills[]? | select(.name=="'"$SKILL_NAME"'") | .sha256) // empty' 2>/dev/null)"
[[ -n "$SHA2" && "$SHA2" != "$SHA1" ]] && ok "re-upload produced a new sha256 (${SHA2:0:12}…) != ${SHA1:0:12}…" \
  || bad "re-upload did not change sha256 (got: ${SHA2:-none})"
# Wait for the actor to pull v2: poll the file content directly (the file
# already exists from v1, so we cannot use wait_for_pv which checks existence).
V2_FOUND=0
for _i in $(seq 1 20); do
  _c="$(read_pv_file "skills/${SKILL_NAME}/tool.js")"
  if [[ "$_c" == *"hello v2"* ]]; then V2_FOUND=1; break; fi
  sleep 3
done
if [[ "$V2_FOUND" -eq 1 ]]; then
  ok "actor atomically updated to v2 content"
else
  CONTENT="$(read_pv_file "skills/${SKILL_NAME}/tool.js")"
  bad "actor did not fetch v2 update (content: '${CONTENT}')"
fi

# --- 4. Unload + isolation boundary --------------------------------------
log "== 4. unload + isolation boundary =="
# 4a. create a user's own, never-managed custom skill inside the actor's dir
CUSTOM_SKILL="$SKILLS_DIR/my-custom-skill"
pv="$(actor_pv)"
if [[ -n "$pv" ]]; then
  docker exec "$NODE" sh -c "mkdir -p '$pv/skills/my-custom-skill' && echo \"private\" > '$pv/skills/my-custom-skill/note.txt'" || true
fi
# 4b. delete the managed skill
RESP="$(admin_delete "$SKILL_NAME")"
if printf '%s' "$RESP" | jq -e . >/dev/null 2>&1; then
  ok "admin deleted '${SKILL_NAME}'"
else
  bad "admin delete returned: ${RESP}"
fi
# The actor's pull loop deletes the managed dir after the admin removes it, so
# wait for the managed dir to actually disappear (the key assertion), then
# verify the custom skill survived alongside.
wait_for_missing() { # <description> <relative-path> <timeout>
  local desc="$1" rel="$2" i=0 timeout="${3:-60}" pv
  pv="$(actor_pv)"
  [[ -n "$pv" ]] || { log "no PV found for '${TEST_USER}' on ${NODE}"; return 1; }
  while (( i < timeout )); do
    if ! docker exec "$NODE" sh -c "test -e '$pv/$rel'" 2>/dev/null; then
      return 0
    fi
    sleep 3; i=$((i+3))
  done
  log "timeout waiting for removal: $desc ($rel still present)"
  return 1
}
if wait_for_missing "managed '${SKILL_NAME}' removed by actor" "skills/${SKILL_NAME}" 60; then
  ok "managed skill '${SKILL_NAME}' dir removed from actor"
  if docker exec "$NODE" sh -c "test -f '$pv/skills/my-custom-skill/note.txt'" 2>/dev/null; then
    ok "user's custom skill 'my-custom-skill' preserved (isolation boundary)"
  else
    bad "user's custom skill was wrongly touched"
  fi
  if ! read_pv_file "$MANAGED_STATE" 2>/dev/null | grep -q "$SKILL_NAME"; then
    ok "managed state no longer lists '${SKILL_NAME}'"
  else
    bad "managed state still lists '${SKILL_NAME}'"
  fi
else
  bad "actor did not remove managed skill within timeout"
fi

# --- 5. Apply (immediate fan-out reload) ---------------------------------
log "== 5. apply (immediate fan-out reload) =="
RESP="$(admin_apply)"
ONLINE="$(printf '%s' "$RESP" | jq -r '.online // empty' 2>/dev/null || true)"
RELOADED="$(printf '%s' "$RESP" | jq -r '.reloaded // empty' 2>/dev/null || true)"
FAILED="$(printf '%s' "$RESP" | jq -r '.failed // empty' 2>/dev/null || true)"
if [[ -n "$ONLINE" && -n "$RELOADED" ]]; then
  ok "apply fan-out returned online=${ONLINE} reloaded=${RELOADED} failed=${FAILED:-0}"
  if [[ "${FAILED:-0}" -eq 0 ]]; then
    ok "apply fan-out completed without failures"
  else
    bad "apply fan-out had failures (see JSON: ${RESP})"
  fi
else
  bad "apply fan-out failed (got: ${RESP})"
fi

# --- Summary -------------------------------------------------------------
echo
echo "=================================================="
echo " skill-distribution E2E: ${PASS} passed, ${FAIL} failed"
echo "=================================================="
[[ "$FAIL" -eq 0 ]]
