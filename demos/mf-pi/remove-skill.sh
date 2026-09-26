#!/usr/bin/env bash
# Uninstall (remove) a managed "shared skill" from mfpi-admin by calling the
# REST API over a temporary kubectl port-forward.
#
# This only deletes the admin's managed copy and notifies actors to remove it;
# a user's own, never-managed, same-named skill is left untouched. See
# deploy_skill_to_actor.md for the full design.
#
# The response is printed as JSON; a non-2xx status exits non-zero. Honors the
# MFPI_NAMESPACE env var (default ate-demo-mf-pi).

set -euo pipefail
cd "$(dirname "$0")"

NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi}"

DNS1123='^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'

usage() {
  echo "Usage: $0 <name>" >&2
  echo "  <name> must match DNS-1123: ${DNS1123}" >&2
  exit 2
}

[[ $# -eq 1 ]] || usage
name="$1"

if ! [[ "$name" =~ $DNS1123 ]]; then
  echo "Invalid skill name '$name': must match ${DNS1123}" >&2
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
  if curl -sS -o /dev/null "http://127.0.0.1:${PORT}/api/skills" 2>/dev/null; then
    break
  fi
  sleep 0.2
done

http_code="$(
  curl -sS -o "$BODY_FILE" -w '%{http_code}' \
    -X DELETE "http://127.0.0.1:${PORT}/api/skills/${name}"
)" || { echo "request to mfpi-admin failed" >&2; exit 1; }

cat "$BODY_FILE"
echo
if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
  echo "HTTP ${http_code} (see JSON above)" >&2
  exit 1
fi
