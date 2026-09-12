#!/usr/bin/env bash
# Start the two port-forwards the demo needs (router + admin UI), then run
# the mfpi-nginx container. Port-forwards already in use are skipped so the
# script is re-runnable without leaving duplicates behind.
#
#   58680 -> svc/atenet-router:80     (actor traffic)
#   58682 -> svc/mfpi-admin:8080      (user-management UI)

set -euo pipefail
cd "$(dirname "$0")"

# Start a kubectl port-forward in the background unless the local port is
# already taken (e.g. a previous run left one running).
port_forward() {
  local local_port="$1" namespace="$2" service="$3" remote_port="$4"
  if ss -ltn 2>/dev/null | grep -q "127.0.0.1:${local_port}[[:space:]]"; then
    echo "port ${local_port} already in use; skipping port-forward to ${namespace}/${service}"
    return
  fi
  echo "port-forward ${local_port} -> ${namespace}/${service}:${remote_port}"
  kubectl port-forward -n "${namespace}" "svc/${service}" "${local_port}:${remote_port}" &
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
