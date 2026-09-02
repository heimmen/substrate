#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Start the two port-forwards the TEST environment needs (router + admin UI),
# then run the mfpi-nginx-test container. Port-forwards already in use are
# skipped so the script is re-runnable without leaving duplicates behind.
#
# This is the test-environment counterpart of run-nginx.sh: it uses distinct
# local ports (59880/59882), the ate-demo-mf-pi-test namespace, a distinct
# container name (mfpi-nginx-test), a distinct htpasswd path and bind-mounts
# nginx-test.conf over the image's baked-in conf. The production environment
# (58680/58682, mfpi-nginx, nginx.conf) is left untouched.
#
#   59880 -> svc/atenet-router:80     (actor traffic)
#   59882 -> svc/mfpi-admin:8080      (user-management UI, test ns)

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

port_forward 59880 ate-system atenet-router 80
port_forward 59882 ate-demo-mf-pi-test mfpi-admin 8080

# Fixed credentials for the /usermanagement/ Basic Auth. Override via env:
#   ADMIN_USER=... ADMIN_PASSWORD=... ./run-nginx-test.sh
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-mf@pass2026}"

# Generate an htpasswd file and bind-mount it over the baked-in default so the
# env-overridden credentials take effect. Use a stable path (must outlive the
# container) and make it world-readable (0644): the nginx worker runs as a
# non-root user and cannot read a mktemp-created 0600 file. The path is
# distinct from the prod script's so the two never clobber each other.
HTPASSWD_FILE="${TMPDIR:-/tmp}/mfpi-admin-test.htpasswd"
printf '%s:%s\n' "$ADMIN_USER" "$(openssl passwd -apr1 "$ADMIN_PASSWORD")" > "$HTPASSWD_FILE"
chmod 644 "$HTPASSWD_FILE"

docker rm -f mfpi-nginx-test 2>/dev/null || true
docker run -d -p 59881:59881 --name mfpi-nginx-test --network host \
  -v "$HTPASSWD_FILE:/etc/nginx/mfpi-admin.htpasswd:ro" \
  -v "$PWD/nginx-test.conf:/etc/nginx/conf.d/default.conf:ro" \
  mfpi-nginx

echo "mfpi-nginx-test running."
echo "  users:          http://localhost:59881/<username>"
echo "  management UI:  http://localhost:59881/usermanagement/ (login: ${ADMIN_USER}/${ADMIN_PASSWORD})"
