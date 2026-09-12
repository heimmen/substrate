#!/usr/bin/env bash
# list-pvs.sh — list the persistent volumes (sticky userdata dirs) backing the
# mf-pi actors, so you can check what data each user has stored.
#
# Each mf-pi actor's userdata external volume is a sticky volume: the control
# plane keeps a per-actor directory on the kind node under
# /var/lib/ateom-gvisor/stickyvolumes/<prefix>-<actor>-userdata and mounts it
# at /data/pi-agent inside the actor. That directory holds everything pi-web
# persists for the user: auth.json (provider credentials), projects.json,
# sessions/ (chat history), skills/, settings.json, ...
#
# Usage:
#   ./list-pvs.sh [--test] [--all] [--detail] [--show-auth] [user ...]
#     --test       look in the mfpi-test atespace (default: mfpi)
#     --all        include every sticky volume (not just <prefix>-* names)
#     --detail     show a recursive (depth-2) file listing per volume
#     --show-auth  print each volume's auth.json (contains API keys — careful)
#     user ...     only show these actor (user) names
#
# Output is a summary table (user, actor status, size, mtimes, file count)
# plus the top-level entries of each volume, so you can see at a glance what
# data each user has accumulated. For a live shell in one volume use
# ./exec-actor.sh <actor> --test.

set -euo pipefail

TEST=0
ALL=0
DETAIL=0
SHOW_AUTH=0
USERS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --test) TEST=1 ;;
    --all) ALL=1 ;;
    --detail) DETAIL=1 ;;
    --show-auth) SHOW_AUTH=1 ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *) USERS+=("$1") ;;
  esac
  shift
done

PREFIX="mfpi"
ATESPACE="mfpi"
if [ "$TEST" = 1 ]; then
  PREFIX="mfpi-test"
  ATESPACE="mfpi-test"
fi

NODE="${NODE:-kind-control-plane}"
STICKY_DIR=/var/lib/ateom-gvisor/stickyvolumes

# kubectl-ate resolution (same convention as the other mf-pi scripts).
if [[ -n "${KUBECTL_ATE:-}" ]]; then
  read -r -a KUBECTL_ATE_CMD <<<"${KUBECTL_ATE}"
elif command -v kubectl-ate >/dev/null 2>&1; then
  KUBECTL_ATE_CMD=(kubectl-ate)
else
  KUBECTL_ATE_CMD=(kubectl ate)
fi

if ! command -v docker >/dev/null 2>&1 || ! docker exec "$NODE" true >/dev/null 2>&1; then
  echo "cannot reach node '$NODE' via docker exec (set NODE=... or use a reachable kind node)" >&2
  exit 1
fi

# Enumerate the sticky volume dirs on the node.
mapfile -t VOLS < <(docker exec "$NODE" sh -c "ls -1 '$STICKY_DIR' 2>/dev/null" || true)
if [ "${#VOLS[@]}" -eq 0 ]; then
  echo "no sticky volumes found under $STICKY_DIR on node $NODE" >&2
  exit 0
fi

# Actor status via kubectl-ate (best-effort: "- " when unknown).
actor_status() {
  local raw st
  raw="$("${KUBECTL_ATE_CMD[@]}" get actor "$1" -a "${ATESPACE}" -o json 2>&1 || true)"
  st="$(printf '%s' "$raw" | jq -r '.actors[0].status // .status // empty' 2>/dev/null || true)"
  printf '%s' "${st:-unknown}"
}

echo "mf-pi persistent volumes (node: $NODE, atespace: $ATESPACE)"
echo "path: $STICKY_DIR/<volume>  (mounted at /data/pi-agent inside the actor)"
echo

# Column headers.
printf '%-24s %-16s %8s %25s %6s\n' "USER" "ACTOR STATUS" "SIZE" "MODIFIED" "FILES"
printf '%-24s %-16s %8s %25s %6s\n' "----" "------------" "----" "--------" "-----"

count=0
for vol in "${VOLS[@]}"; do
  # Skip non-user volumes unless --all. The user volume names are
  # <prefix>-<actor>-userdata; anything else (e.g. mock-vol-N) is shown only
  # with --all.
  if [ "$ALL" != 1 ] && [[ ! "$vol" == "${PREFIX}-"* ]]; then
    continue
  fi
  # Derive the actor name: strip <prefix>- and the trailing -userdata.
  user="${vol#"$PREFIX"-}"
  user="${user%-userdata}"
  if [ -z "$user" ] || [ "$user" = "$vol" ]; then
    # Name didn't match the expected shape; only surface it under --all.
    [ "$ALL" = 1 ] || continue
    user="$vol"
  fi
  # Optional user filter.
  if [ "${#USERS[@]}" -gt 0 ]; then
    keep=0
    for u in "${USERS[@]}"; do
      [ "$u" = "$user" ] && keep=1
    done
    [ "$keep" = 1 ] || continue
  fi

  # One node round-trip for size / mtime / file count.
  meta="$(docker exec "$NODE" sh -c "du -sh '$STICKY_DIR/$vol' 2>/dev/null | cut -f1; stat -c %y '$STICKY_DIR/$vol' 2>/dev/null | cut -c1-19; find '$STICKY_DIR/$vol' -type f 2>/dev/null | wc -l" || true)"
  size="$(printf '%s' "$meta" | sed -n 1p)"
  mtime="$(printf '%s' "$meta" | sed -n 2p)"
  files="$(printf '%s' "$meta" | sed -n 3p)"

  status="$(actor_status "$user")"

  printf '%-24s %-16s %8s %25s %6s\n' "$user" "$status" "${size:-?}" "${mtime:-?}" "${files:-?}"

  # Top-level entries of the volume (the data the user has stored).
  entries="$(docker exec "$NODE" sh -c "ls -1 '$STICKY_DIR/$vol' 2>/dev/null" || true)"
  if [ -n "$entries" ]; then
    # Compress into a single line: "auth.json, projects.json, sessions, ..."
    # (paste -d ', ' would ROTATE the two separator chars; use ',' then widen.)
    flat="$(printf '%s\n' "$entries" | paste -sd, - | sed 's/,/, /g')"
    printf '    contents: %s\n' "$flat"
  fi

  if [ "$DETAIL" = 1 ]; then
    findout="$(docker exec "$NODE" sh -c "find '$STICKY_DIR/$vol' -mindepth 1 -maxdepth 2 2>/dev/null | sed 's#^$STICKY_DIR/$vol##' | sort | head -60" || true)"
    if [ -n "$findout" ]; then
      printf '%s\n' "$findout" | sed 's/^/      /'
    fi
  fi

  if [ "$SHOW_AUTH" = 1 ]; then
    auth="$(docker exec "$NODE" sh -c "cat '$STICKY_DIR/$vol/auth.json' 2>/dev/null" || true)"
    if [ -n "$auth" ]; then
      printf '    auth.json: %s\n' "$auth"
    else
      printf '    auth.json: <absent or unreadable>\n'
    fi
  fi

  count=$((count + 1))
done

echo
if [ "$count" -eq 0 ]; then
  echo "no matching user volumes (prefix: $PREFIX; add --all to see everything)"
else
  echo "$count volume(s) listed."
fi
