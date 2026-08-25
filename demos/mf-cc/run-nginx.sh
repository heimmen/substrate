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
# Start the two port-forwards the demo needs (router + admin UI), then run
# the mfcc-nginx container. Port-forwards already in use are skipped so the
# script is re-runnable without leaving duplicates behind.
#
#   58880 -> svc/atenet-router:80     (actor traffic)
#   58882 -> svc/mfcc-admin:8080      (user-management UI)

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

port_forward 58880 ate-system atenet-router 80
port_forward 58882 ate-demo-mf-cc mfcc-admin 8080

docker rm -f mfcc-nginx 2>/dev/null || true
docker run -d -p 58881:58881 --name mfcc-nginx --network host mfcc-nginx

echo "mfcc-nginx running."
echo "  users:          http://localhost:58881/<username>"
echo "  management UI:  http://localhost:58881/usermanagement/"
