#!/usr/bin/env bash
# Start the two port-forwards the demo needs (router + admin UI), then run
# the mfpi-nginx container. A local port that is already in use is reused if
# its tunnel is healthy, and replaced if the tunnel has gone stale (it still
# accepts TCP but never answers — this happens when a tunnel is left running
# across a router pod restart), so the script is re-runnable without leaving
# dead tunnels behind.
#
#   58680 -> svc/atenet-router:80     (actor traffic)
#   58682 -> svc/mfpi-admin:8080      (user-management UI)

set -euo pipefail
cd "$(dirname "$0")"

# Seconds to wait for an HTTP response before declaring an existing tunnel
# stale. Override for slow clusters: MFPI_PROBE_TIMEOUT=15 ./run-nginx.sh
PROBE_TIMEOUT="${MFPI_PROBE_TIMEOUT:-5}"

# True if something is listening on 127.0.0.1:<port>.
port_in_use() {
  ss -ltn 2>/dev/null | grep -q "127.0.0.1:${1}[[:space:]]"
}

# True if the local tunnel answers with any HTTP status line within
# PROBE_TIMEOUT seconds. A stale tunnel still accepts the TCP connection (the
# local kubectl process is alive) but resets or times out on real requests,
# which curl reports as status code 000.
tunnel_healthy() {
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' -m "$PROBE_TIMEOUT" "http://127.0.0.1:${1}/" 2>/dev/null || true)
  [ -n "$code" ] && [ "$code" != "000" ]
}

# Stop the kubectl port-forward process(es) listening on the given local port.
# Only processes whose command line contains "port-forward" are killed; if the
# port is held by anything else, abort instead of killing an unrelated process.
stop_port_forward() {
  local port="$1" pid cmdline kill_pids=""
  for pid in $(ss -tlnp "sport = :${port}" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u); do
    cmdline=$(tr '\0' ' ' <"/proc/${pid}/cmdline" 2>/dev/null || true)
    case "$cmdline" in
      *port-forward*) kill_pids="${kill_pids} ${pid}" ;;
    esac
  done
  if [ -z "$kill_pids" ]; then
    echo "error: port ${port} is in use by a non-port-forward process; free it and re-run" >&2
    exit 1
  fi
  # shellcheck disable=SC2086
  kill $kill_pids 2>/dev/null || true
  local i
  for i in $(seq 1 20); do
    port_in_use "$port" || return 0
    sleep 0.5
  done
  echo "error: port ${port} still in use after stopping the stale port-forward" >&2
  exit 1
}

# Start a kubectl port-forward in the background unless a healthy one is
# already listening on the local port; a stale one is detected and replaced.
port_forward() {
  local local_port="$1" namespace="$2" service="$3" remote_port="$4"
  if port_in_use "$local_port"; then
    if tunnel_healthy "$local_port"; then
      echo "port ${local_port} already in use and tunnel healthy; skipping port-forward to ${namespace}/${service}"
      return
    fi
    echo "port ${local_port} in use but tunnel is stale (no HTTP response within ${PROBE_TIMEOUT}s); replacing it"
    stop_port_forward "$local_port"
  fi
  echo "port-forward ${local_port} -> ${namespace}/${service}:${remote_port}"
  # Run with nohup + disown so the tunnel survives the script's exit (a plain
  # `&` job gets SIGHUP when the parent shell finishes, so the forwarded UI
  # port would be dead right after this script returns).
  nohup kubectl port-forward -n "${namespace}" "svc/${service}" "${local_port}:${remote_port}" \
    >/dev/null 2>&1 &
  disown
  # Give the tunnel a moment to establish before nginx starts proxying to it.
  sleep 1
}

port_forward 58680 ate-system atenet-router 80
port_forward 58682 ate-demo-mf-pi mfpi-admin 8080

# Fixed credentials for the /usermanagement/ Basic Auth. Override via env:
#   ADMIN_USER=... ADMIN_PASSWORD=... ./run-nginx.sh
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-mf@pass2026}"

# Generate an htpasswd file and bind-mount it over the baked-in default so the
# env-overridden credentials take effect. Use a stable path (must outlive the
# container) and make it world-readable (0644): the nginx worker runs as a
# non-root user and cannot read a mktemp-created 0600 file.
HTPASSWD_FILE="${TMPDIR:-/tmp}/mfpi-admin.htpasswd"
printf '%s:%s\n' "$ADMIN_USER" "$(openssl passwd -apr1 "$ADMIN_PASSWORD")" > "$HTPASSWD_FILE"
chmod 644 "$HTPASSWD_FILE"

docker rm -f mfpi-nginx 2>/dev/null || true
docker run -d -p 58681:58681 --name mfpi-nginx --network host \
  -v "$HTPASSWD_FILE:/etc/nginx/mfpi-admin.htpasswd:ro" \
  mfpi-nginx

echo "mfpi-nginx running."
echo "  users:          http://localhost:58681/<username>"
echo "  management UI:  http://localhost:58681/usermanagement/ (login: ${ADMIN_USER}/${ADMIN_PASSWORD})"
