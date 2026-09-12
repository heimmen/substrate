#!/usr/bin/env bash

# exec-actor.sh — open a shell "inside" an mf-pi actor's pod.
#
# Substrate runs each actor inside a gVisor sandbox on a distroless worker pod.
# There is no exec endpoint (kubectl exec fails: the worker has no shell, and
# the sandbox runner exposes no `exec` subcommand), so we cannot get a live
# process shell into the running container.
#
# What we CAN do is open a shell on the kind node over the actor's persisted
# filesystem — the sticky volume that the control plane mounts at
# /data/pi-agent inside the actor. This is exactly the actor's data (chat
# sessions, auth.json, skills, …) and it is what survives a refresh-actor.sh
# redeploy. Use it to inspect or hand-edit the actor's on-disk state.
#
# IMPORTANT: the sticky volume directory IS /data/pi-agent. The PV holds the
# CONTENTS of that path (auth.json, sessions/, …) directly — there is no
# nested pi-agent/ folder inside the PV, and the node path contains no /data
# prefix. To make that explicit, the shell by default starts in a small view
# that mirrors the actor layout:
#
#   data/pi-agent/...   -> the sticky PV (what the actor sees at /data/pi-agent)
#
# The other /data dirs inside the actor (config, home, npm-cache, pi-web) are
# ephemeral sandbox overlay and are not visible from the node.
#
# Usage:
#   ./exec-actor.sh <actor> [--test] [--print] [--raw]
#     <actor>   actor (user) name, e.g. tom3
#     --test    look in the mfpi-test atespace (default prefix: mfpi)
#     --print   print the resolved node PV path and exit (no interactive shell)
#     --raw     cd straight into the PV dir instead of the actor-style view
#
# Requires the kind node to be reachable as $NODE (default: kind-control-plane)
# via `docker exec`.

set -euo pipefail

ACTOR=""
TEST=0
PRINT=0
RAW=0
while [ $# -gt 0 ]; do
  case "$1" in
    --test) TEST=1 ;;
    --print) PRINT=1 ;;
    --raw) RAW=1 ;;
    -h|--help) sed -n '2,45p' "$0"; exit 0 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *)
      if [ -z "$ACTOR" ]; then ACTOR="$1"; else echo "unexpected arg: $1" >&2; exit 2; fi
      ;;
  esac
  shift
done

if [ -z "$ACTOR" ]; then
  echo "usage: $0 <actor> [--test] [--print] [--raw]" >&2
  exit 2
fi

PREFIX="mfpi"
[ "$TEST" = 1 ] && PREFIX="mfpi-test"

NODE="${NODE:-kind-control-plane}"

# The sticky volume dir for a user is <prefix>-<actor>-userdata (e.g.
# mfpi-test-tom3-userdata). Fall back to any dir containing the actor name.
VOL=$(docker exec "$NODE" sh -c "ls -d /var/lib/ateom-gvisor/stickyvolumes/*${PREFIX}-${ACTOR}-userdata 2>/dev/null | head -1" || true)
if [ -z "$VOL" ]; then
  VOL=$(docker exec "$NODE" sh -c "ls -d /var/lib/ateom-gvisor/stickyvolumes/*${ACTOR}* 2>/dev/null | head -1" || true)
fi

if [ -z "$VOL" ]; then
  echo "no sticky volume found for actor '$ACTOR' (prefix=$PREFIX) on node $NODE" >&2
  echo "existing volumes:" >&2
  docker exec "$NODE" sh -c "ls -1 /var/lib/ateom-gvisor/stickyvolumes 2>/dev/null" >&2 || true
  exit 1
fi

echo "Actor : $ACTOR  (atespace prefix: $PREFIX)"
echo "Node  : $NODE"
echo "PV dir: $VOL"
echo "        (= the actor's /data/pi-agent — this is what survives refresh)"

if [ "$PRINT" = 1 ]; then
  echo "$VOL"
  exit 0
fi

# Actor-style view: a directory laid out like the actor's paths, so you can
# `cd data/pi-agent` exactly as you would inside the actor. The PV dir itself
# holds the /data/pi-agent CONTENTS (no nested pi-agent/), which is confusing
# to browse directly — hence the view. Keyed by volume name (actor-stable).
VIEW="/var/lib/ateom-gvisor/shellviews/$(basename "$VOL")"
if [ "$RAW" != 1 ]; then
  docker exec "$NODE" sh -c "mkdir -p '$VIEW/data' && ln -sfn '$VOL' '$VIEW/data/pi-agent'" || true
fi

echo
if [ "$RAW" = 1 ]; then
  echo "Entering a shell at the PV directory (--raw)."
else
  echo "Entering a shell in the actor-style view:"
  echo "  $VIEW/data/pi-agent/  ->  the PV  (= actor /data/pi-agent)"
  echo "Example: cd data/pi-agent && ls    # auth.json, sessions/, skills/, ..."
fi
echo "(Substrate workers are gVisor/distroless with no exec endpoint;"
echo " this is the persisted pod filesystem, not a live process shell.)"
echo "Type 'exit' to leave."
echo

if [ "$RAW" = 1 ]; then
  docker exec -it "$NODE" sh -c "cd '$VOL' && exec sh"
else
  docker exec -it "$NODE" sh -c "cd '$VIEW' && exec sh"
fi
