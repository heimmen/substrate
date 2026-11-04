#!/usr/bin/env bash
# Health watchdog for one mf-pi environment.
#
# Repairs the two failure modes that used to require manual work:
#
#   1. gateway -> router/admin port-forward died. The gateway then returns
#      502/503 for every user. Re-running run-nginx.sh rebuilds the tunnels
#      (idempotent: healthy tunnels are reused).
#
#   2. a worker container restarted (OOM/crash) in place. ateom comes back
#      blank but the actor stays STATUS_RUNNING on that worker, so the router
#      returns 503 and the browser sits on the loading page forever. The fix is
#      to delete the bad worker pod, wait for the syncer to release the actor
#      (STATUS_SUSPENDED), then resume it (cold boot, data on the volume is
#      kept).
#
# Both actions are idempotent and safe to run from cron/systemd every minute.
#
# Usage:
#   ./watchdog.sh [--once] [--interval N] [--no-actors] [--env prod|qa|test]
#
# Defaults: one pass, actors enabled, env from $MFPI_ENV (qa).
# Logs go to ${MFPI_LOG_DIR:-/tmp/mfpi-logs}/watchdog-<env>.log.

set -euo pipefail
cd "$(dirname "$0")"

ENV_NAME="${MFPI_ENV:-qa}"
INTERVAL=""
CHECK_ACTORS=1
MAX_FIX="${MFPI_WATCHDOG_MAX_FIX:-5}"

usage() {
  sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --once)       INTERVAL="" ;;
    --interval)   INTERVAL="${2:?--interval needs a value}"; shift ;;
    --no-actors)  CHECK_ACTORS=0 ;;
    --env)        ENV_NAME="${2:?--env needs a value}"; shift ;;
    -h|--help)    usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
  shift
done

# Two modes:
#  - in the demo checkout: load env/<name>.env via lib/env.sh;
#  - standalone (offline installer / copied bundle): take MFPI_* from the
#    environment, so the caller sets ATESPACE/NAMESPACE/ports/container name.
if [[ -f ./lib/env.sh ]]; then
  # shellcheck source=lib/env.sh
  source ./lib/env.sh
  mfpi_load_env "$ENV_NAME"
else
  MFPI_ENV="$ENV_NAME"
  : "${MFPI_ATESPACE:?standalone mode needs MFPI_ATESPACE}"     "${MFPI_NAMESPACE:?standalone mode needs MFPI_NAMESPACE}"     "${MFPI_ROUTER_PORT:?standalone mode needs MFPI_ROUTER_PORT}"     "${MFPI_ADMIN_PORT:?standalone mode needs MFPI_ADMIN_PORT}"     "${MFPI_NGINX_CONTAINER:?standalone mode needs MFPI_NGINX_CONTAINER}"
fi

LOG_DIR="${MFPI_LOG_DIR:-/tmp/mfpi-logs}"
mkdir -p "$LOG_DIR"
LOG="${LOG_DIR}/watchdog-${MFPI_ENV}.log"

log() { printf '%s [%s] %s\n' "$(date -Is)" "$MFPI_ENV" "$*" | tee -a "$LOG" >&2; }

# Any HTTP status line within 5s counts as "alive"; curl prints 000 on
# connection refused/timeout.
http_code() {
  curl -s -o /dev/null -w '%{http_code}' -m 5 "$1" 2>/dev/null || true
}

router_alive() { [ "$(http_code "http://127.0.0.1:${MFPI_ROUTER_PORT}/")" != "000" ]; }
admin_alive()  { [ "$(http_code "http://127.0.0.1:${MFPI_ADMIN_PORT}/healthz")" = "200" ]; }
nginx_running() { docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$1"; }

restart_nginx() {
  local router admin
  router="$(http_code "http://127.0.0.1:${MFPI_ROUTER_PORT}/")"
  admin="$(http_code "http://127.0.0.1:${MFPI_ADMIN_PORT}/healthz")"
  if [ "$router" != "000" ] && [ "$admin" = "200" ] && nginx_running "$MFPI_NGINX_CONTAINER"; then
    return 1
  fi
  log "gateway unhealthy (router=$router admin=$admin container=$(nginx_running "$MFPI_NGINX_CONTAINER" && echo up || echo down)); re-running run-nginx.sh"
  if ./run-nginx.sh "$MFPI_ENV" >>"$LOG" 2>&1; then
    log "gateway repaired (router=$(http_code "http://127.0.0.1:${MFPI_ROUTER_PORT}/") admin=$(http_code "http://127.0.0.1:${MFPI_ADMIN_PORT}/healthz"))"
  else
    log "run-nginx.sh failed; see $LOG"
  fi
  return 0
}

# actor_code prints the router's HTTP status for <user> bypassing authentication.
actor_code() {
  local user="$1"
  curl -s -o /dev/null -w '%{http_code}' -m 5 \
    -H "Host: ${user}.${MFPI_ATESPACE}.actors.resources.substrate.ate.dev" \
    "http://127.0.0.1:${MFPI_ROUTER_PORT}/" 2>/dev/null || true
}

# recover_actor deletes the bad worker pod, waits for the actor to be released
# (STATUS_SUSPENDED) and resumes it. Data lives on the sticky volume; resume
# cold-boots in ~25-40s.
recover_actor() {
  local user="$1" pod="$2"
  log "actor $user is RUNNING on a restarted worker ($pod); recovering"
  kubectl delete pod "$pod" -n "$MFPI_NAMESPACE" --wait=false >>"$LOG" 2>&1 || true
  local st=""
  for _ in $(seq 1 30); do
    st="$(kubectl ate get actor "$user" -a "$MFPI_ATESPACE" 2>/dev/null | awk 'NR==2{print $4}')"
    [ "$st" = "STATUS_SUSPENDED" ] && break
    sleep 2
  done
  if [ "$st" != "STATUS_SUSPENDED" ]; then
    log "actor $user did not reach SUSPENDED (got ${st:-?}); leaving it for a human"
    return 1
  fi
  if kubectl ate resume actor "$user" -a "$MFPI_ATESPACE" >>"$LOG" 2>&1; then
    log "actor $user resumed (cold boot); probe=$(actor_code "$user")"
  else
    log "resume failed for $user; see $LOG"
  fi
}

check_actors() {
  local fixed=0
  while read -r user status podref; do
    [ -z "${user:-}" ] && continue
    [ "$status" = "STATUS_RUNNING" ] || continue
    local pod="${podref##*/}"
    [ -z "$pod" ] && continue

    local restarts
    restarts="$(kubectl get pod "$pod" -n "$MFPI_NAMESPACE" \
      -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || true)"
    # A blank worker has zero actor logs; restarts>0 is the cheap signal.
    if [ -z "${restarts:-}" ] || [ "$restarts" = "0" ]; then
      continue
    fi
    if [ "$(actor_code "$user")" = "200" ]; then
      continue
    fi
    recover_actor "$user" "$pod" || true
    fixed=$((fixed + 1))
    [ "$fixed" -ge "$MAX_FIX" ] && { log "hit MAX_FIX=$MAX_FIX, stopping this pass"; break; }
  done < <(kubectl ate get actor -a "$MFPI_ATESPACE" 2>/dev/null | awk 'NR>1 && NF>1 {print $2, $4, $5}')
  return 0
}

run_once() {
  restart_nginx || true
  if [ "$CHECK_ACTORS" = "1" ] && router_alive; then
    check_actors
  fi
}

if [ -z "$INTERVAL" ]; then
  run_once
else
  run_once
  while sleep "$INTERVAL"; do run_once; done
fi
