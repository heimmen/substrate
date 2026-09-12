#!/usr/bin/env bash
# remove-pv.sh — permanently remove a user's persistent volume (sticky
# userdata directory).
#
# Deleting a user/actor does NOT reclaim storage: the sticky volume plugin's
# DeleteVolume keeps the backing directory on purpose, so a delete+recreate
# REFRESH (refresh-actor.sh) re-attaches the same data. Only this script (or
# the admin's user-delete flow, which calls the same control-plane RPC)
# actually removes the PV and everything the user stored in it:
# chat sessions, auth.json (API keys), skills, settings, ...
#
# Usage:
#   ./remove-pv.sh <user> [--test] [--print]
#     <user>   actor (user) name, e.g. tom3
#     --test   operate in the mfpi-test atespace (default: mfpi)
#     --print  print the node path that would be removed and exit
#
# Requires kubectl-ate; node verification additionally needs docker access
# to $NODE (default: kind-control-plane) and is skipped when unavailable.

set -euo pipefail

USER_NAME=""
TEST=0
PRINT=0
while [ $# -gt 0 ]; do
  case "$1" in
    --test) TEST=1 ;;
    --print) PRINT=1 ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *)
      if [ -z "$USER_NAME" ]; then USER_NAME="$1"; else echo "unexpected arg: $1" >&2; exit 2; fi
      ;;
  esac
  shift
done

if [ -z "$USER_NAME" ]; then
  echo "usage: $0 <user> [--test] [--print]" >&2
  exit 2
fi

ATESPACE="mfpi"
TEMPLATE="ate-demo-mf-pi/mf-pi"
[ "$TEST" = 1 ] && ATESPACE="mfpi-test" && TEMPLATE="ate-demo-mf-pi-test/mf-pi"

# kubectl-ate resolution (same convention as the other mf-pi scripts).
if [[ -n "${KUBECTL_ATE:-}" ]]; then
  read -r -a KUBECTL_ATE_CMD <<<"${KUBECTL_ATE}"
elif command -v kubectl-ate >/dev/null 2>&1; then
  KUBECTL_ATE_CMD=(kubectl-ate)
else
  KUBECTL_ATE_CMD=(kubectl ate)
fi

need_cmd() { command -v "$1" >/dev/null 2>&1 || { echo "SKIP: '$1' not found on PATH" >&2; exit 0; }; }
need_cmd "${KUBECTL_ATE_CMD[0]}"

# Best-effort node-side check of the sticky volume dir before and after.
vol_dir() {
  local prefix="mfpi"
  [ "$TEST" = 1 ] && prefix="mfpi-test"
  NODE="${NODE:-kind-control-plane}"
  command -v docker >/dev/null 2>&1 || return 0
  docker exec "$NODE" true >/dev/null 2>&1 || return 0
  docker exec "$NODE" sh -c "ls -d /var/lib/ateom-gvisor/stickyvolumes/${prefix}-${USER_NAME}-userdata 2>/dev/null | head -1" || true
}

BEFORE="$(vol_dir)"
echo "User     : ${USER_NAME}  (atespace: ${ATESPACE}, template: ${TEMPLATE})"
if [ -n "$BEFORE" ]; then
  echo "PV dir   : ${BEFORE}"
else
  echo "PV dir   : (not found on node '${NODE:-kind-control-plane}'; purging anyway)"
fi

if [ "$PRINT" = 1 ]; then
  [ -n "$BEFORE" ] && echo "$BEFORE"
  exit 0
fi

# The actor must not be RUNNING; purge refuses that server-side. Suspend
# best-effort to make the common "remove a live user" flow work.
"${KUBECTL_ATE_CMD[@]}" suspend actor "${USER_NAME}" -a "${ATESPACE}" >/dev/null 2>&1 || true

if ! "${KUBECTL_ATE_CMD[@]}" purge volumes "${USER_NAME}" -a "${ATESPACE}" --template "${TEMPLATE}"; then
  echo "purge failed (is the actor still RUNNING? is --template correct?)" >&2
  exit 1
fi

AFTER="$(vol_dir)"
if [ -n "$BEFORE" ]; then
  if [ -z "$AFTER" ]; then
    echo "verified: ${BEFORE} removed from node '${NODE:-kind-control-plane}'"
  else
    echo "WARNING: ${AFTER} still present on node '${NODE:-kind-control-plane}' after purge" >&2
    exit 1
  fi
else
  echo "purge command succeeded (no node-side verification available)"
fi
