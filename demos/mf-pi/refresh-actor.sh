#!/usr/bin/env bash
# Refresh one or more PRODUCTION mf-pi user actors in place:
#   suspend -> wait SUSPENDED -> delete -> recreate -> resume -> wait RUNNING
#   -> wait for the proxy route to come back.
#
# This simulates an image-refresh / redeploy (delete + recreate) so you can
# roll a new pi-web/pause image or template change out to live users. The
# userdata ExternalVolume is mount-type "sticky" (see internal/volume/sticky):
# DeleteVolume keeps the backing directory on disk and a recreate of the same
# actor re-attaches the same directory, so whatever the user wrote under
# /data/pi-agent survives the refresh. The actor is re-attached to the SAME
# user/volume, so they can log back in with their existing password and their
# projects / saved files / session are intact.
#
# PRODUCTION SAFETY:
#   * A refresh interrupts the user's running instance. Their data is
#     preserved, but any in-flight work that was not persisted to the sticky
#     volume is lost. Don't run this on a live user casually.
#   * By default the script prompts for confirmation before touching anything
#     (once, for the whole batch). Run non-interactively with -y/--yes or
#     MFPI_FORCE=1.
#   * Pass --dry-run to print the exact plan (suspend/delete/create/resume per
#     user) and exit without doing anything.
#   * The recreate uses the actor's CURRENT template (auto-detected from the
#     live actor, e.g. ate-demo-mf-pi/mf-pi-mid) so a tiered user keeps its
#     resource tier. Override with MFPI_TEMPLATE if needed.
#
# Usage:
#   ./refresh-actor.sh <username> [<username> ...]
#   ./refresh-actor.sh -y alice bob
#   ./refresh-actor.sh --dry-run alice
#   MFPI_ATESPACE=mfpi MFPI_NAMESPACE=ate-demo-mf-pi \
#     MFPI_PROXY=localhost:58681 ./refresh-actor.sh alice
#
# Requires a reachable cluster and the production mf-pi deployment applied
# (./deploy.sh). The nginx proxy (./run-nginx.sh) is only used for a
# best-effort routing check at the end and is not required for the refresh
# itself.

set -uo pipefail
cd "$(dirname "$0")"

# --- Configurable defaults (PRODUCTION) ----------------------------------
ATESPACE="${MFPI_ATESPACE:-mfpi}"
NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi}"
PROXY="${MFPI_PROXY:-localhost:58681}"
# Default fallback template; normally auto-detected from the live actor.
TEMPLATE_DEFAULT="${MFPI_TEMPLATE:-ate-demo-mf-pi/mf-pi}"
# Bypass the initial proxy reachability probe (e.g. you run this somewhere
# without the local proxy port-forward up).
SKIP_PROXY_CHECK="${MFPI_SKIP_PROXY_CHECK:-0}"

# --- Flags -----------------------------------------------------------------
ASSUME_YES=0
DRY_RUN=0
USERS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    -y|--yes)   ASSUME_YES=1 ;;
    --dry-run)  DRY_RUN=1 ;;
    -h|--help)
      grep '^#' "$0" | sed 's/^#\{1,2\} //' | sed '/^$/q'
      exit 0 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *)  USERS+=("$1") ;;
  esac
  shift
done

if [[ ${#USERS[@]} -eq 0 ]]; then
  echo "usage: $0 [--yes] [--dry-run] <username> [<username> ...]" >&2
  exit 2
fi

# --- kubectl-ate resolution ----------------------------------------------
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
log()  { printf '[refresh-actor] %s\n' "$*" >&2; }
ok()   { PASS=$((PASS+1)); log "PASS: $*"; }
bad()  { FAIL=$((FAIL+1)); log "FAIL: $*"; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "required command '$1' not found on PATH" >&2; exit 1; }
}
need_cmd kubectl
need_cmd "${KUBECTL_ATE_CMD[0]}"
need_cmd curl
need_cmd jq

if ! kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1; then
  echo "namespace '${NAMESPACE}' not found (is the production mf-pi deployed? run ./deploy.sh)" >&2
  exit 1
fi

# --- Pre-flight banner + confirmation ------------------------------------
log "==================================================================="
log " PRODUCTION actor refresh"
log "   atespace = ${ATESPACE}"
log "   namespace = ${NAMESPACE}"
log "   proxy    = ${PROXY} (best-effort routing check only)"
log "   users    = ${USERS[*]}"
log " This suspends, deletes, recreates and resumes each actor above."
log " User data on the sticky PV is preserved; the running instance is"
log " interrupted during the window between suspend and resume."
log "==================================================================="

if (( DRY_RUN )); then
  log "DRY-RUN: no changes will be made."
elif (( ! ASSUME_YES )); then
  # Require explicit confirmation when stdin is a TTY; otherwise refuse unless
  # -y / MFPI_FORCE=1 is given (never auto-destroy prod from a pipe).
  if [[ -t 0 ]]; then
    printf 'Type "yes" to proceed with refreshing %d user(s): ' "${#USERS[@]}" >&2
    read -r REPLY
    [[ "${REPLY,,}" == "yes" ]] || { log "aborted by user."; exit 1; }
  else
    echo "refusing to refresh production actors without confirmation:" >&2
    echo "  pass -y/--yes (or set MFPI_FORCE=1) for non-interactive runs." >&2
    exit 1
  fi
fi

# --- Wait helpers ---------------------------------------------------------
get_status() {
  local raw st
  raw="$("${KUBECTL_ATE_CMD[@]}" get actor "$1" -a "${ATESPACE}" -o json 2>&1 || true)"
  st="$(printf '%s' "${raw}" | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true)"
  printf '%s' "${st}"
}

get_template() {
  # Returns "<namespace>/<name>" of the actor's current template, or empty.
  # The live actor JSON carries the template ref as top-level fields
  # (actorTemplateName / actorTemplateNamespace); keep the dotted fallbacks
  # for older output shapes.
  local raw ns nm
  raw="$("${KUBECTL_ATE_CMD[@]}" get actor "$1" -a "${ATESPACE}" -o json 2>&1 || true)"
  ns="$(printf '%s' "${raw}" | jq -r '.actors[0].actorTemplateNamespace // .actor_template.namespace // empty' 2>/dev/null || true)"
  nm="$(printf '%s' "${raw}" | jq -r '.actors[0].actorTemplateName // .actor_template.name // empty' 2>/dev/null || true)"
  if [[ -n "${ns}" && -n "${nm}" ]]; then
    printf '%s/%s' "${ns}" "${nm}"
  fi
}

wait_for_status() {
  # wait_for_status <user> <target-status> <timeout-seconds> [resume-nudge]
  local user="$1" target="$2" timeout="$3" nudge="${4:-0}" i=0 st
  while (( i < timeout )); do
    st="$(get_status "${user}")"
    if [[ "$st" == "${target}" ]]; then return 0; fi
    # Optionally nudge resume at i==10 and i==60 to cover scheduling races.
    if (( nudge )) && { [[ $i -eq 10 ]] || [[ $i -eq 60 ]]; }; then
      "${KUBECTL_ATE_CMD[@]}" resume actor "${user}" -a "${ATESPACE}" >/dev/null 2>&1 || true
    fi
    sleep 2
    i=$((i+2))
  done
  log "  actor '${user}' did not reach ${target} after ${timeout}s (last status: ${st:-unknown})"
  return 1
}

# Best-effort: wait until the proxy can route to the refreshed actor. The proxy
# entry `/<user>` returns 302 when the actor is RUNNING and routed; 503 while it
# is still coming up. Requires no password. Non-fatal.
wait_for_proxy() {
  local user="$1" i=0 timeout=120 code
  while (( i < timeout )); do
    code="$(curl -sS -o /dev/null -w '%{http_code}' -m 3 "http://${PROXY}/${user}" 2>/dev/null)"
    if [[ "$code" == "200" || "$code" == "302" ]]; then return 0; fi
    sleep 2
    i=$((i+2))
  done
  return 1
}

# --- Optional proxy reachability probe (non-fatal) ------------------------
if (( ! SKIP_PROXY_CHECK )); then
  if curl -sS -o /dev/null -m 5 "http://${PROXY}/usermanagement/healthz" 2>/dev/null; then
    log "proxy reachable at http://${PROXY}"
  else
    log "WARN: proxy not reachable at http://${PROXY} (proxy check skipped; refresh continues)"
  fi
fi

# --- Refresh each user ----------------------------------------------------
refresh_one() {
  local user="$1"
  local tpl status

  status="$(get_status "${user}")"
  if [[ -z "${status}" ]]; then
    bad "actor '${user}' not found in atespace '${ATESPACE}' (skip)"
    return
  fi
  log "actor '${user}' current status: ${status}"

  # Detect the actor's real template so a tiered user keeps its tier.
  tpl="$(get_template "${user}")"
  if [[ -z "${tpl}" ]]; then
    tpl="${TEMPLATE_DEFAULT}"
    log "  could not detect actor template; falling back to ${tpl}"
  else
    log "  using actor template ${tpl}"
  fi

  if (( DRY_RUN )); then
    log "  [dry-run] would: suspend -> delete -> create --template ${tpl} -> resume '${user}'"
    ok "[dry-run] plan printed for '${user}'"
    return
  fi

  # delete actor refuses while RUNNING, so suspend and wait for SUSPENDED first.
  "${KUBECTL_ATE_CMD[@]}" suspend actor "${user}" -a "${ATESPACE}" >/dev/null 2>&1 || true
  if ! wait_for_status "${user}" "STATUS_SUSPENDED" 120; then
    bad "actor '${user}' did not reach STATUS_SUSPENDED before delete (skip)"
    return
  fi

  "${KUBECTL_ATE_CMD[@]}" delete actor "${user}" -a "${ATESPACE}" >/dev/null
  "${KUBECTL_ATE_CMD[@]}" create actor "${user}" -a "${ATESPACE}" --template "${tpl}" >/dev/null
  # create actor does NOT auto-resume; resume explicitly so the new instance boots.
  "${KUBECTL_ATE_CMD[@]}" resume actor "${user}" -a "${ATESPACE}" >/dev/null 2>&1 || true

  if ! wait_for_status "${user}" "STATUS_RUNNING" 300 1; then
    bad "actor '${user}' did not reach STATUS_RUNNING after recreate"
    return
  fi

  if wait_for_proxy "${user}"; then
    log "  proxy reachable at http://${PROXY}/${user}"
  else
    log "  WARN: proxy not reachable at http://${PROXY}/${user} within timeout (actor is RUNNING; may still be warming up)"
  fi

  ok "actor '${user}' refreshed and back to STATUS_RUNNING (user data on the sticky PV should be intact)"
}

for u in "${USERS[@]}"; do
  log "-------------------------------------------------------------------"
  refresh_one "${u}"
done

log "-------------------------------------------------------------------"
if (( FAIL == 0 )); then
  log "RESULT: PASS (${PASS} passed) — affected users can log in with their existing password to verify data persistence"
  exit 0
else
  log "RESULT: FAIL (${PASS} passed, ${FAIL} failed)"
  exit 1
fi
