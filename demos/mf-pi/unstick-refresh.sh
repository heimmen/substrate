#!/usr/bin/env bash
# Batch unstick + refresh all mf-pi user actors stuck in STATUS_SUSPENDING.
#
# Root cause: after the cluster outage, every gVisor sandbox (runsc) process
# died while worker pods stayed "Running" (ateom alive, sandbox stopped), so
# checkpoint always fails ("cannot checkpoint container in state stopped") and
# suspend never reaches STATUS_SUSPENDED. Deleting the bound worker pod makes
# the WorkerPoolSyncer release the actor back to STATUS_SUSPENDED; the new
# worker is healthy and brings up a fresh sandbox. User data lives on the
# sticky volume (keyed by stable actor-volume name), which DeleteVolume keeps
# and a recreate re-attaches, so nothing is lost.
#
# Per user:
#   delete worker pod -> wait STATUS_SUSPENDED
#   delete actor       (sticky data kept on disk)
#   create actor       (same template)
#   resume actor       -> wait STATUS_RUNNING
#
# Usage: ./unstick-refresh.sh <username> [<username> ...]

set -uo pipefail
cd "$(dirname "$0")"

ATESPACE="${MFPI_ATESPACE:-mfpi}"
NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi}"
TEMPLATE_DEFAULT="${MFPI_TEMPLATE:-ate-demo-mf-pi/mf-pi}"

log()  { printf '[unstick-refresh] %s\n' "$*" >&2; }
ok()   { log "OK:   $*"; }
bad()  { log "FAIL: $*"; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || { echo "required command '$1' not found on PATH" >&2; exit 1; }; }
need_cmd kubectl
need_cmd python3

PASS=0; FAIL=0

actor_field() { # actor_field <user> <field>
  kubectl ate get actor "$1" -a "${ATESPACE}" -o json 2>/dev/null \
    | python3 -c "import sys,json; d=json.load(sys.stdin); a=d['actors'][0] if 'actors' in d else d; print(a.get('$2',''))"
}

wait_status() { # wait_status <user> <target> <timeout_s>
  local user="$1" target="$2" timeout="$3" i=0 st
  while (( i < timeout )); do
    st="$(actor_field "$user" status)"
    [[ "$st" == "$target" ]] && return 0
    sleep 2; i=$((i+2))
  done
  log "  actor '${user}' did not reach ${target} after ${timeout}s (last: ${st:-unknown})"
  return 1
}

for user in "$@"; do
  log "=== ${user} ==="
  st="$(actor_field "$user" status)"
  pod="$(actor_field "$user" ateomPodName)"
  log "  status=${st} worker=${pod:-<none>}"

  # 1. If still bound to a worker, delete it to unstick (only if != SUSPENDED).
  if [[ -n "$pod" && "$st" != "STATUS_SUSPENDED" ]]; then
    log "  deleting worker ${pod} to unstick ${user}..."
    kubectl delete pod -n "$NAMESPACE" "$pod" --wait=false >/dev/null 2>&1
  fi

  if ! wait_status "$user" "STATUS_SUSPENDED" 120; then
    bad "  ${user} still not SUSPENDED (skip)"
    FAIL=$((FAIL+1)); continue
  fi
  ok "  ${user} -> STATUS_SUSPENDED"

  tpl="$(actor_field "$user" actorTemplateName)"
  tplns="$(actor_field "$user" actorTemplateNamespace)"
  if [[ -n "$tpl" && -n "$tplns" ]]; then
    TEMPLATE="${tplns}/${tpl}"
  else
    TEMPLATE="$TEMPLATE_DEFAULT"
  fi
  log "  using template ${TEMPLATE}"

  kubectl ate delete actor "$user" -a "${ATESPACE}" >/dev/null 2>&1
  if ! kubectl ate create actor "$user" -a "${ATESPACE}" --template "$TEMPLATE" >/dev/null 2>&1; then
    bad "  create ${user} failed"
    FAIL=$((FAIL+1)); continue
  fi
  ok "  ${user} recreated (sticky data preserved)"

  kubectl ate resume actor "$user" -a "${ATESPACE}" >/dev/null 2>&1
  if wait_status "$user" "STATUS_RUNNING" 180; then
    ok "  ${user} -> STATUS_RUNNING"
    PASS=$((PASS+1))
  else
    bad "  ${user} did not reach RUNNING after resume"
    FAIL=$((FAIL+1))
  fi
done

log "RESULT: ${PASS} passed, ${FAIL} failed"
exit $(( FAIL == 0 ? 0 : 1 ))
