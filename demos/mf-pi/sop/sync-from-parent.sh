#!/usr/bin/env bash
# Re-copy the SOP helper scripts from demos/mf-pi (the originals).
#
# The scripts in this directory are copies so the SOP can be handed out on its
# own. Run this after changing any of the originals to keep the copies in sync.

set -euo pipefail
cd "$(dirname "$0")"

FILES=(
  watchdog.sh
  install-watchdog.sh
  check_agent_access.sh
  refresh-actor.sh
  recover-actors.sh
  ensure-no-golden.sh
  exec-actor.sh
  list-pvs.sh
  create-user.sh
  list-users.sh
  delete-user.sh
)

for f in "${FILES[@]}"; do
  cp "../$f" "./$f"
  chmod +x "./$f"
done

echo "synced ${#FILES[@]} scripts from .."
