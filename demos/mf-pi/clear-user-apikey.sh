#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Clear the per-user DeepSeek API key for an mf-pi user by calling the
# mfpi-admin REST API (svc/mfpi-admin:8080) over a temporary kubectl
# port-forward.
#
# The admin server first drives the actor's pi-web auth logout (so the agent
# falls back to the DEEPSEEK_API_KEY env), then removes the persisted key from
# the mfpi-user-provider-keys Secret. Idempotent: clearing a user with no key
# succeeds. See injectDeepsseekKey.md for the full design.
#
# The response is printed as JSON; a non-2xx status exits non-zero. Honors the
# MFPI_NAMESPACE env var (default ate-demo-mf-pi; the mfpi-admin in the
# mfpi-test atespace lives in ate-demo-mf-pi-test).

set -euo pipefail
cd "$(dirname "$0")"

NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi}"

# Validate the username against DNS-1123 (same rule as Substrate actor names).
DNS1123='^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'

usage() {
  echo "Usage: $0 <username>" >&2
  echo "  <username> must match DNS-1123: ${DNS1123}" >&2
  exit 2
}

[[ $# -eq 1 ]] || usage
username="$1"

if ! [[ "$username" =~ $DNS1123 ]]; then
  echo "Invalid username '$username': must match ${DNS1123}" >&2
  exit 2
fi

# Find a free local port for the temporary port-forward.
free_port() {
  local p
  for p in $(seq 18082 18182); do
    if ! ss -ltn 2>/dev/null | grep -q "127.0.0.1:${p}[[:space:]]"; then
      echo "$p"
      return 0
    fi
  done
  return 1
}
PORT="$(free_port)" || { echo "no free local port in 18082-18182" >&2; exit 1; }

PF_PID=""
BODY_FILE="$(mktemp)"
cleanup() {
  [[ -n "$PF_PID" ]] && kill "$PF_PID" 2>/dev/null || true
  rm -f "$BODY_FILE"
}
trap cleanup EXIT

echo "port-forward ${PORT} -> ${NAMESPACE}/svc/mfpi-admin:8080"
kubectl port-forward -n "$NAMESPACE" "svc/mfpi-admin" "${PORT}:8080" &
PF_PID=$!

# Give the tunnel a moment to accept connections before the request.
for _ in $(seq 1 50); do
  if curl -sS -o /dev/null "http://127.0.0.1:${PORT}/api/users" 2>/dev/null; then
    break
  fi
  sleep 0.2
done

http_code="$(
  curl -sS -o "$BODY_FILE" -w '%{http_code}' \
    -X DELETE "http://127.0.0.1:${PORT}/api/users/${username}/apikey"
)" || { echo "request to mfpi-admin failed" >&2; exit 1; }

cat "$BODY_FILE"
echo
if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
  echo "HTTP ${http_code} (see JSON above)" >&2
  exit 1
fi
