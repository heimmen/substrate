#!/usr/bin/env bash
# Release the worker of users that have been idle for a long time.
#
# "Release" is the snapshot-free path: delete the actor's worker pod; the
# syncer then moves the actor to STATUS_SUSPENDED (no snapshot, data stays on
# the sticky volume) and the worker pod comes back blank and free. The next
# request cold-boots the actor (~25-40s) behind the loading page.
#
# Idle time comes from the gateway access log: every authenticated request is
# logged as `IP - <user> [date] "..."`, which is a faithful "last input" signal.
# Last-seen timestamps are cached in ${MFPI_LOG_DIR:-/tmp/mfpi-logs}/idle-<env>.tsv
# so a container restart does not make everyone look idle.
#
# Usage:
#   ./idle-reaper.sh                     # dry run: list candidates (default)
#   ./idle-reaper.sh --apply             # actually release (up to --max)
#   ./idle-reaper.sh --idle 12h --max 5 --tiers small,mid --env qa
#   ./idle-reaper.sh --only-user alice --include-unknown --idle 0s --apply
#
# Defaults: idle 6h, max 5 per run, tiers small,mid, env qa, dry run.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -f "${SCRIPT_DIR}/lib/env.sh" ]]; then
  ROOT="${SCRIPT_DIR}"
else
  ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
fi
# shellcheck source=lib/env.sh
source "${ROOT}/lib/env.sh"

ENV_NAME="${MFPI_ENV:-qa}"
IDLE="6h"
MAX="5"
TIERS="small,mid"
APPLY=0
ONLY_USER=""
INCLUDE_UNKNOWN=0
LOOKBACK="168h"

usage() { sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

to_secs() {
  local v="$1"
  case "$v" in
    *d) echo $(( ${v%d} * 86400 )) ;;
    *h) echo $(( ${v%h} * 3600 )) ;;
    *m) echo $(( ${v%m} * 60 )) ;;
    *s) echo "${v%s}" ;;
    *)  echo "$v" ;;
  esac
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply)           APPLY=1 ;;
    --dry-run)         APPLY=0 ;;
    --idle)            IDLE="${2:?--idle needs a value}"; shift ;;
    --max)             MAX="${2:?--max needs a value}"; shift ;;
    --tiers)           TIERS="${2:?--tiers needs a value}"; shift ;;
    --env)             ENV_NAME="${2:?--env needs a value}"; shift ;;
    --lookback)        LOOKBACK="${2:?--lookback needs a value}"; shift ;;
    --only-user)       ONLY_USER="${2:?--only-user needs a value}"; shift ;;
    --include-unknown) INCLUDE_UNKNOWN=1 ;;
    -h|--help)         usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
  shift
done

mfpi_load_env "$ENV_NAME"
IDLE_SECS="$(to_secs "$IDLE")"
LOG_DIR="${MFPI_LOG_DIR:-/tmp/mfpi-logs}"
mkdir -p "$LOG_DIR"
STATE="${LOG_DIR}/idle-${MFPI_ENV}.tsv"
REAPER_LOG="${LOG_DIR}/idle-reaper-${MFPI_ENV}.log"

log() { printf '%s [%s] %s\n' "$(date -Is)" "$MFPI_ENV" "$*" | tee -a "$REAPER_LOG" >&2; }

declare -A LAST=()
if [[ -f "$STATE" ]]; then
  while IFS=$'\t' read -r u e; do
    [[ -n "${u:-}" && -n "${e:-}" ]] && LAST["$u"]="$e"
  done <"$STATE"
fi

# Merge the gateway's last-seen times (max) into the cache.
while IFS=$'\t' read -r u e; do
  [[ -z "${u:-}" || -z "${e:-}" ]] && continue
  if (( e > ${LAST[$u]:-0} )); then LAST["$u"]="$e"; fi
done < <(
  docker logs --since "$LOOKBACK" "$MFPI_NGINX_CONTAINER" 2>&1 |
    python3 -c '
import sys, re, calendar, time
last = {}
for line in sys.stdin:
    m = re.match(r"\S+ - (\S+) \[([^\]]+)\]", line)
    if not m or m.group(1) == "-":
        continue
    try:
        epoch = calendar.timegm(time.strptime(m.group(2).split()[0], "%d/%b/%Y:%H:%M:%S"))
    except ValueError:
        continue
    user = m.group(1)
    if epoch > last.get(user, 0):
        last[user] = epoch
for user, epoch in last.items():
    print("%s\t%d" % (user, epoch))
'
)

now="$(date +%s)"
declare -A SEEN=()
reaped=0
candidates=0

while IFS=$'\t' read -r user tmpl status podref; do
  [[ -z "${user:-}" ]] && continue
  SEEN["$user"]=1
  [[ "$status" == "STATUS_RUNNING" ]] || continue
  [[ -n "$ONLY_USER" && "$user" != "$ONLY_USER" ]] && continue

  tier="${tmpl##*-}"
  case ",${TIERS}," in
    *",${tier},"*) ;;
    *) continue ;;
  esac

  last="${LAST[$user]:-}"
  if [[ -z "$last" ]]; then
    if [[ "$INCLUDE_UNKNOWN" != "1" ]]; then
      # Never seen in the log/cache: be conservative and start the clock now.
      LAST["$user"]="$now"
      continue
    fi
    last=0
  fi

  idle_for=$((now - last))
  (( idle_for >= IDLE_SECS )) || continue

  candidates=$((candidates + 1))
  if (( last == 0 )); then
    human="unknown"
  else
    human="$((idle_for / 3600))h$(((idle_for % 3600) / 60))m"
  fi
  pod="${podref##*/}"
  if [[ "$APPLY" != "1" ]]; then
    printf 'would release %-32s idle=%-7s pod=%s\n' "$user" "$human" "${pod:-<none>}"
    continue
  fi
  [[ -z "$pod" ]] && continue

  log "releasing $user (idle ${human}, pod ${pod})"
  kubectl delete pod "$pod" -n "$MFPI_NAMESPACE" --wait=false >>"$REAPER_LOG" 2>&1 || true
  st=""
  for _ in $(seq 1 30); do
    st="$(kubectl ate get actor "$user" -a "$MFPI_ATESPACE" 2>/dev/null | awk 'NR==2{print $4}')"
    [[ "$st" == "STATUS_SUSPENDED" ]] && break
    sleep 2
  done
  if [[ "$st" == "STATUS_SUSPENDED" ]]; then
    log "released $user -> SUSPENDED (next access cold-boots)"
  else
    log "release of $user did not settle (status=${st:-?}); left for a human"
  fi
  reaped=$((reaped + 1))
  (( reaped >= MAX )) && break
done < <(kubectl ate get actor -a "$MFPI_ATESPACE" 2>/dev/null | awk 'NR>1 && NF>1 {print $2"\t"$3"\t"$4"\t"$5}')

# Persist last-seen for every user we saw, so unknown users keep aging.
{
  for u in "${!LAST[@]}"; do
    [[ -n "${SEEN[$u]:-}" || "$INCLUDE_UNKNOWN" == "1" ]] && printf '%s\t%s\n' "$u" "${LAST[$u]}"
  done
} >"$STATE"

if [[ "$APPLY" == "1" ]]; then
  log "released ${reaped} actor(s); idle>=${IDLE}, max ${MAX}, tiers ${TIERS}"
else
  echo "dry run: ${candidates} candidate(s) with idle >= ${IDLE} (tiers: ${TIERS}); use --apply to release"
fi
