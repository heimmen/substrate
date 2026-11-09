#!/usr/bin/env bash
# One-table overview of every mf-pi environment (prod / qa / test).
#
# Usage:
#   ./status-all.sh             # summary: users, gateway, workers, stale actors
#   ./status-all.sh --detail    # + one line per user
#   ./status-all.sh --fix       # repair unhealthy envs once (watchdog --once)
#   ./status-all.sh --watch 5   # refresh every 5 seconds
#
# "stale" = actor is STATUS_RUNNING but its worker container restarted
# (RESTARTS>0). That is the OOM case that strands a user on the loading page;
# watchdog.sh recovers these, and this column shows whether any are left.
#
# Exit code: 0 when every gateway is OK and no stale actor is found, else 1.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# In the demo checkout lib/ sits next to this script; the sop/ copy finds it in
# the parent directory.
if [[ -f "${SCRIPT_DIR}/lib/env.sh" ]]; then
  ROOT="${SCRIPT_DIR}"
else
  ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
fi
if [[ ! -f "${ROOT}/lib/env.sh" ]]; then
  echo "error: lib/env.sh not found next to or above $0" >&2
  exit 2
fi

DETAIL=0
FIX=0
WATCH=""

usage() {
  sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --detail) DETAIL=1 ;;
    --fix)    FIX=1 ;;
    --watch)  WATCH="${2:-5}"; shift ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
  shift
done

# shellcheck source=lib/env.sh
source "${ROOT}/lib/env.sh"

http_code() { curl -s -o /dev/null -w '%{http_code}' -m 3 "$1" 2>/dev/null || true; }

# actor_probe <user>: router status for the actor, bypassing nginx auth.
actor_probe() {
  curl -s -o /dev/null -w '%{http_code}' -m 4 \
    -H "Host: ${1}.${MFPI_ATESPACE}.actors.resources.substrate.ate.dev" \
    "http://127.0.0.1:${MFPI_ROUTER_PORT}/" 2>/dev/null || true
}

print_header() {
  printf '%-5s %-6s %6s %6s %6s %-4s %9s %6s\n' \
    ENV ENTRY USERS RUN SUSP GW WORKERS STALE
}

report_env() {
  local env="$1"
  mfpi_load_env "$env"

  local container=down
  if docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$MFPI_NGINX_CONTAINER"; then
    container=up
  fi
  local entry router admin
  entry="$(http_code "http://127.0.0.1:${MFPI_ENTRY_PORT}/")"
  router="$(http_code "http://127.0.0.1:${MFPI_ROUTER_PORT}/")"
  admin="$(http_code "http://127.0.0.1:${MFPI_ADMIN_PORT}/healthz")"
  local gw=ok
  if [[ "$container" != up || "$entry" == "000" || "$router" == "000" || "$admin" != "200" ]]; then
    gw=DOWN
  fi

  # Worker replicas: mf-pi-wp-* plus the legacy mf-pi-workerpool.
  local ready desired
  read -r ready desired < <(
    kubectl get deploy -n "$MFPI_NAMESPACE" --no-headers \
      -o custom-columns='NAME:.metadata.name,R:.status.readyReplicas,D:.spec.replicas' 2>/dev/null \
      | awk '$1 ~ /^mf-pi-(wp-|workerpool)/ {r += $2; d += $3} END {print r+0, d+0}'
  )

  local -a users=() statuses=() pods=()
  local name status podref
  while read -r name status podref; do
    [[ -z "$name" ]] && continue
    users+=("$name"); statuses+=("$status"); pods+=("${podref##*/}")
  done < <(kubectl ate get actor -a "$MFPI_ATESPACE" 2>/dev/null | awk 'NR>1 && NF>1 {print $2, $4, $5}')

  local running=0 suspended=0 other=0
  local -A restarts=()
  while read -r p r; do
    [[ -n "$p" && -n "$r" ]] && restarts["$p"]="$r"
  done < <(kubectl get pods -n "$MFPI_NAMESPACE" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[0].restartCount}{"\n"}{end}' 2>/dev/null || true)

  local stale=0 i
  local -a probes=()
  for i in "${!users[@]}"; do
    case "${statuses[$i]}" in
      STATUS_RUNNING) running=$((running + 1)) ;;
      STATUS_SUSPENDED) suspended=$((suspended + 1)) ;;
      *) other=$((other + 1)) ;;
    esac
    probes[$i]="-"
    if [[ "${statuses[$i]}" == STATUS_RUNNING ]]; then
      local rc="${restarts[${pods[$i]}]:-0}"
      if [[ "${rc:-0}" != "0" ]]; then
        # A restart count may predate the current actor (the pod was restarted
        # while free, then served a new actor). Only a failed probe means the
        # actor is actually stranded.
        probes[$i]="$(actor_probe "${users[$i]}")"
        [[ "${probes[$i]}" != "200" ]] && stale=$((stale + 1))
      fi
    fi
  done

  printf '%-5s %-6s %6s %6s %6s %-4s %9s %6s\n' \
    "$env" "$MFPI_ENTRY_PORT" "${#users[@]}" "$running" "$suspended" "$gw" "${ready}/${desired}" "$stale"

  if [[ "$DETAIL" == "1" ]]; then
    for i in "${!users[@]}"; do
      local rc="${restarts[${pods[$i]}]:-0}" mark=" "
      [[ "${probes[$i]:--}" != "-" && "${probes[$i]}" != "200" ]] && mark="!"
      printf '  %s %-36s %-18s pod=%-38s restarts=%s probe=%s\n' \
        "$mark" "${users[$i]}" "${statuses[$i]}" "${pods[$i]:-<none>}" "${rc:-0}" "${probes[$i]:--}"
    done
  fi

  [[ "$gw" == "ok" && "$stale" -eq 0 ]]
}

run_once() {
  if ! kubectl get --raw=/readyz --request-timeout=3s >/dev/null 2>&1; then
    echo "error: Kubernetes API 不可达（整个集群挂了？）" >&2
    echo "  1) docker ps -a --filter name=kind-control-plane" >&2
    echo "  2) docker start kind-control-plane" >&2
    echo "  3) 等 kubectl get nodes 变 Ready 后重跑" >&2
    return 2
  fi

  print_header
  local bad=0 env
  BAD_ENVS=()
  while read -r env; do
    # Subshell: each env export must not leak MFPI_PROXY/MFPI_ADMIN into the next.
    if ! ( report_env "$env" ); then
      bad=1
      BAD_ENVS+=("$env")
    fi
  done < <(mfpi_envs)

  if [[ "$FIX" == "1" && ${#BAD_ENVS[@]} -gt 0 ]]; then
    for env in "${BAD_ENVS[@]}"; do
      echo "--- fixing $env (watchdog --once) ---"
      "${ROOT}/watchdog.sh" --once --env "$env" || echo "watchdog failed for $env (see $LOG_DIR/watchdog-$env.log)"
    done
    echo "--- after fix ---"
    print_header
    bad=0
    for env in "${BAD_ENVS[@]}"; do
      ( report_env "$env" ) || bad=1
    done
  fi
  return "$bad"
}

if [[ -z "$WATCH" ]]; then
  run_once
else
  while true; do
    printf '\n=== %s ===\n' "$(date '+%F %T')"
    run_once || true
    sleep "$WATCH"
  done
fi
