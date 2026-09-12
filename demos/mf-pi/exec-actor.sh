#!/usr/bin/env bash

# exec-actor.sh — open a shell "inside" an mf-pi actor's pod.
#
# Substrate runs each actor inside a gVisor sandbox on a distroless worker pod.
# There is no exec endpoint (kubectl exec fails: the worker has no shell, and
# the sandbox runner exposes no `exec` subcommand), so we cannot get a live
# process shell into the running container.
#
# What we CAN do is open a shell on the kind node at the actor's persisted pod
# filesystem — the sticky volume that the control plane mounts at /data/pi-agent
# inside the actor. This is exactly the actor's data (chat sessions, auth.json,
# skills, …) and it is what survives a refresh-actor.sh redeploy. Use it to
# inspect or hand-edit the actor's on-disk state.
#
# Usage:
#   ./exec-actor.sh <actor> [--test] [--print]
#     <actor>   actor (user) name, e.g. tom2
#     --test    look in the mfpi-test atespace (default prefix: mfpi)
#     --print   print the resolved node path and exit (no interactive shell)
#
# Requires the kind node to be reachable as $NODE (default: kind-control-plane)
# via `docker exec`.

set -euo pipefail

ACTOR=""
TEST=0
PRINT=0
while [ $# -gt 0 ]; do
  case "$1" in
    --test) TEST=1 ;;
    --print) PRINT=1 ;;
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *)
      if [ -z "$ACTOR" ]; then ACTOR="$1"; else echo "unexpected arg: $1" >&2; exit 2; fi
      ;;
  esac
  shift
done

if [ -z "$ACTOR" ]; then
  echo "usage: $0 <actor> [--test] [--print]" >&2
  exit 2
fi

PREFIX="mfpi"
[ "$TEST" = 1 ] && PREFIX="mfpi-test"

NODE="${NODE:-kind-control-plane}"

# The sticky volume dir for a user is <prefix>-<actor>-userdata (e.g.
# mfpi-test-tom2-userdata). Fall back to any dir containing the actor name.
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
echo "PV dir: $VOL   (this is the actor's /data/pi-agent — what survives refresh)"

if [ "$PRINT" = 1 ]; then
  echo "$VOL"
  exit 0
fi

echo
echo "Entering a shell on the node at that directory."
echo "(Substrate workers are gVisor/distroless with no exec endpoint;"
echo " this is the persisted pod filesystem, not a live process shell.)"
echo "Type 'exit' to leave."
echo

docker exec -it "$NODE" sh -c "cd '$VOL' && exec sh"
