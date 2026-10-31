#!/usr/bin/env bash
# Tear down what run-nginx.sh started for ONE environment: kill that
# environment's kubectl port-forwards and remove its gateway container.
#
#   ./stop-nginx.sh            # qa (default, historical 58681)
#   ./stop-nginx-test.sh       # test (59681)
#   ./stop-nginx-prod.sh       # prod (57681)
#   ./stop-nginx.sh qa|test|prod
#   MFPI_ENV=test ./stop-nginx.sh
#
# Safe to run repeatedly and when nothing is running (free ports / no container
# are reported and skipped). Only this environment's ports/container are
# touched, so stopping qa never disturbs prod or test.

set -euo pipefail
cd "$(dirname "$0")"

ENV_ARG=""
case "${1:-}" in
  prod|qa|test) ENV_ARG="$1"; shift ;;
esac
# shellcheck source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"
mfpi_load_env "${ENV_ARG}"

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

kill_port "${MFPI_ROUTER_PORT}"
kill_port "${MFPI_ADMIN_PORT}"

# Pre-split qa gateway (see run-nginx.sh).
if [[ "${MFPI_ENV_NAME}" == "qa" ]] &&
   docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx 'mfpi-nginx'; then
  echo "removing legacy mfpi-nginx container (pre-split qa gateway)"
  docker rm -f mfpi-nginx >/dev/null
fi

if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "${MFPI_NGINX_CONTAINER}"; then
  echo "removing ${MFPI_NGINX_CONTAINER} container (port ${MFPI_ENTRY_PORT})"
  docker rm -f "${MFPI_NGINX_CONTAINER}" >/dev/null
else
  echo "${MFPI_NGINX_CONTAINER} container not present; nothing to clean"
fi

echo "done. run-nginx.sh can now start ${MFPI_ENV_NAME} fresh."
