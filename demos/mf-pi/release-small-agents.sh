#!/usr/bin/env bash
# Release (temporarily) the worker/Pod resources of every SMALL-tier mf-pi
# agent in a given environment by suspending it.
#
# Suspending an actor frees its worker Pod (the resources it was holding) while
# keeping the actor's Full snapshot and sticky user volume, so nothing is lost:
# the actor lazily recovers on its next access — same as the idle auto-suspend
# feature (admin/idle.go).
#
# "Small tier" = actors whose ActorTemplate is the base/default template
# (e.g. `mf-pi`, which is the default-small tier) or the `-small` tier template
# (e.g. `mf-pi-small`). See demos/mf-pi/admin/tier.go for the mapping.
#
# Two environment-specific entry points (mirroring list-users.sh /
# list-users-test.sh):
#   ./release-small-agents.sh         # PRODUCTION (atespace mfpi, entry 58681)
#   ./release-small-agents-test.sh    # TEST (atespace mfpi-test, entry 59681)
# You can also pass `--prod` / `--test`, or set MFPI_ATESPACE explicitly
# (e.g. `MFPI_ATESPACE=mfpi-test ./release-small-agents.sh`).
#
# Behavior:
#   * By default it suspends every small-tier agent that is RUNNING (only
#     RUNNING ones hold resources) and reports the final state.
#   * An agent that is still RUNNING after a suspend has an active
#     session/connection and auto-resumes; it cannot be held suspended without
#     interrupting that live session, so it is *reported* rather than force-killed.
#   * Pass --force to keep re-issuing suspend for agents that re-resume (best
#     effort; still cannot beat an actively-stuffed session, just tries harder).
#
# Requirements: a reachable cluster + `kubectl` with the ate plugin (`kubectl-ate`).

set -euo pipefail
cd "$(dirname "$0")"

# --- kubectl-ate resolution ---------------------------------------------
if [[ -n "${KUBECTL_ATE:-}" ]]; then
  read -r -a KUBECTL_ATE_CMD <<<"${KUBECTL_ATE}"
elif command -v kubectl-ate >/dev/null 2>&1; then
  KUBECTL_ATE_CMD=(kubectl-ate)
else
  KUBECTL_ATE_CMD=(kubectl ate)
fi

# --- Args / environment target -------------------------------------------
FORCE=0
DRY=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --force)   FORCE=1 ;;
    --dry-run|-n) DRY=1 ;;
    --prod|-p) export MFPI_ATESPACE="${MFPI_ATESPACE:-mfpi}" ;;
    --test|-t) export MFPI_ATESPACE="${MFPI_ATESPACE:-mfpi-test}" ;;
    -h|--help) grep '^#' "$0" | sed 's/^#\{1,2\} //' | sed '/^$/q'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

ATESPACE="${MFPI_ATESPACE:-mfpi}"
# Base template (default-small tier). Per-tier templates are "<base>-<tier>".
BASE_TEMPLATE="${MFPI_BASE_TEMPLATE:-mf-pi}"
# How long (s) to wait for a suspend to take before reporting it as "active".
SUSPEND_WAIT="${MFPI_SUSPEND_WAIT:-30}"
# How many extra --force re-suspend rounds to attempt.
FORCE_ROUNDS="${MFPI_FORCE_ROUNDS:-2}"

log() { printf '[release-small-agents] %s\n' "$*"; }

# actor_field <name> <json-field> -> prints the field (or empty)
actor_field() {
  "${KUBECTL_ATE_CMD[@]}" get actor "$1" -a "$ATESPACE" -o json 2>/dev/null \
    | python3 -c "import sys,json; d=json.load(sys.stdin); a=(d.get('actors') or [d])[0]; print(a.get('$2',''))"
}

# is_small_tier <template-ref> : template ref like ate-demo-mf-pi/mf-pi-small
is_small_tier() {
  local ref="$1" base dsb
  base="${ref##*/}"                       # basename of the template name
  if [[ "$base" == "$BASE_TEMPLATE" ]]; then return 0; fi        # base = default small
  dsb="${BASE_TEMPLATE}-small"
  [[ "$base" == "$dsb" ]] && return 0                             # explicit small tier
  return 1
}

# --- Discover small-tier RUNNING agents --------------------------------
if ! "${KUBECTL_ATE_CMD[@]}" get actors -a "$ATESPACE" >/dev/null 2>&1; then
  log "SKIP: atespace '$ATESPACE' not reachable (no live cluster?) — exiting 0"
  exit 0
fi

mapfile -t TARGETS < <(
  "${KUBECTL_ATE_CMD[@]}" get actors -a "$ATESPACE" -o json 2>/dev/null \
  | python3 -c "
import sys, json
d = json.load(sys.stdin)
rows = d.get('actors') or [d]
for a in rows:
    tpl = a.get('actorTemplateName','')
    if a.get('status') == 'STATUS_RUNNING' and tpl:
        print(a.get('metadata',{}).get('name',''), tpl)
"
)

log "environment atespace=${ATESPACE} base_template=${BASE_TEMPLATE} force=${FORCE}"

SMALL_RUNNING=()
while read -r name tpl; do
  [[ -z "$name" ]] && continue
  if is_small_tier "$tpl"; then
    SMALL_RUNNING+=("$name")
  fi
done < <(printf '%s\n' "${TARGETS[@]}")

if [[ ${#SMALL_RUNNING[@]} -eq 0 ]]; then
  log "no small-tier RUNNING agents in '${ATESPACE}' — nothing to release."
  exit 0
fi
log "releasing resources for ${#SMALL_RUNNING[@]} small-tier RUNNING agent(s): ${SMALL_RUNNING[*]}"

# --- Suspend each (issue once) ------------------------------------------
if [[ "$DRY" == "1" ]]; then
  log "--dry-run: would suspend these ${#SMALL_RUNNING[@]} small-tier RUNNING agent(s) in '${ATESPACE}': ${SMALL_RUNNING[*]}"
  exit 0
fi
for u in "${SMALL_RUNNING[@]}"; do
  log "  suspending ${u} to release its worker..."
  "${KUBECTL_ATE_CMD[@]}" suspend actor "$u" -a "$ATESPACE" >/dev/null 2>&1 || true
done

# --- Wait for each to reach SUSPENDED ------------------------------------
declare -A STILL_RUNNING=()
for u in "${SMALL_RUNNING[@]}"; do
  local_st=""
  for (( i=0; i<SUSPEND_WAIT/2; i++ )); do
    local_st="$(actor_field "$u" status)"
    [[ "$local_st" == "STATUS_SUSPENDED" ]] && break
    sleep 2
  done
  if [[ "$local_st" == "STATUS_SUSPENDED" ]]; then
    log "  OK   ${u} -> SUSPENDED (worker released)"
  else
    log "  ACTIVE ${u} -> still STARTING/RUNNING (active session auto-resumes)"
    STILL_RUNNING["$u"]=1
  fi
done

# --- Optional --force re-suspend rounds ----------------------------------
if [[ "$FORCE" == "1" && ${#STILL_RUNNING[@]} -gt 0 ]]; then
  for (( r=1; r<=FORCE_ROUNDS; r++ )); do
    [[ ${#STILL_RUNNING[@]} -eq 0 ]] && break
    log "  --force round ${r}: re-suspending ${!STILL_RUNNING[*]}"
    for u in "${!STILL_RUNNING[@]}"; do
      "${KUBECTL_ATE_CMD[@]}" suspend actor "$u" -a "$ATESPACE" >/dev/null 2>&1 || true
    done
    sleep 3
    for u in "${!STILL_RUNNING[@]}"; do
      st="$(actor_field "$u" status)"
      if [[ "$st" == "STATUS_SUSPENDED" ]]; then
        log "  OK   ${u} -> SUSPENDED (worker released)"
        unset 'STILL_RUNNING[$u]'
      fi
    done
  done
fi

# --- Summary -------------------------------------------------------------
REL=$(( ${#SMALL_RUNNING[@]} - ${#STILL_RUNNING[@]} ))
log "----------------------------------------"
log "released: ${REL}/${#SMALL_RUNNING[@]} small-tier agent(s)"
if [[ ${#STILL_RUNNING[@]} -gt 0 ]]; then
  log "still RUNNING (active, not force-released): ${!STILL_RUNNING[*]}"
  log "  -> these have live sessions; suspend them after their session ends, or use --force."
fi
