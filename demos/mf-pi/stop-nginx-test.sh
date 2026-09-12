#!/usr/bin/env bash
# Tear down what run-nginx-test.sh started: kill the kubectl port-forwards
# holding the TEST environment ports and remove the mfpi-nginx-test container.
# Safe to run repeatedly and when nothing is running (free ports / no container
# are reported and skipped). This clears a stale "port already in use" so
# run-nginx-test.sh can be re-run cleanly.
#
#   59880 -> svc/atenet-router:80     (actor traffic)
#   59882 -> svc/mfpi-admin:8080      (user-management UI, test ns)
#   59681 -> mfpi-nginx-test container (docker, --network host)

set -euo pipefail
cd "$(dirname "$0")"

# kill_port finds and kills the process listening on the given TCP port.
# Prefers lsof, falls back to fuser; a free port is not an error.
kill_port() {
  local port="$1" pids
  if command -v lsof >/dev/null 2>&1; then
    pids="$(lsof -ti tcp:"${port}" 2>/dev/null || true)"
  elif command -v fuser >/dev/null 2>&1; then
    pids="$(fuser "${port}/tcp" 2>/dev/null || true)"
  else
    echo "warning: neither lsof nor fuser available; cannot find PIDs on port ${port}"
    return
  fi
  if [[ -n "$pids" ]]; then
    # shellcheck disable=SC2086
    echo "killing PID(s) on port ${port}: ${pids}"
    kill ${pids} 2>/dev/null || true
    # Give the port a moment to be released.
    sleep 1
  else
    echo "port ${port} free; nothing to clean"
  fi
}

kill_port 59880
kill_port 59882
kill_port 59681

if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx 'mfpi-nginx-test'; then
  echo "removing mfpi-nginx-test container (port 59681)"
  docker rm -f mfpi-nginx-test >/dev/null
else
  echo "mfpi-nginx-test container not present; nothing to clean"
fi

echo "done. run-nginx-test.sh can now start fresh."
