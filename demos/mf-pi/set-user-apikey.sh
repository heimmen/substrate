#!/usr/bin/env bash
# Set (or overwrite) the per-user provider API key for an mf-pi user by
# calling the mfpi-admin REST API (svc/mfpi-admin:8080) over a temporary
# kubectl port-forward.
#
# The provider defaults to DeepSeek; pass a third argument to store a key for
# another OpenAI-compatible provider (openai, openrouter, moonshotai, zai,
# groq, ...). One provider is kept per user: storing a different one switches,
# logging the previous provider out of the actor first. A configured
# provider's models then show up in that user's pi-web model picker.
#
# The admin server first persists the key to the mfpi-user-provider-keys
# Secret, resumes the user's actor if it is suspended, then drives the key
# into the actor's pi-web auth store (so it outranks the provider's env var,
# e.g. DEEPSEEK_API_KEY). See docs/injectDeepsseekKey.md for the full design.
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
  echo "Usage: $0 <username> <api-key> [provider-id]" >&2
  echo "  <username> must match DNS-1123: ${DNS1123}" >&2
  echo "  [provider-id] defaults to deepseek; GET /api/providers on the admin" >&2
  echo "                lists the provider ids this deployment accepts." >&2
  exit 2
}

[[ $# -ge 2 && $# -le 3 ]] || usage
username="$1"
api_key="$2"
provider_id="${3:-deepseek}"

if ! [[ "$username" =~ $DNS1123 ]]; then
  echo "Invalid username '$username': must match ${DNS1123}" >&2
  exit 2
fi
if [[ -z "$api_key" || "$api_key" == *'"'* || "$api_key" == *'\'* ]]; then
  echo "Invalid API key: must be non-empty and contain no double quotes or backslashes" >&2
  exit 2
fi
# Provider ids are pi-web's own ids; keep them to a conservative charset so the
# JSON payload needs no escaping.
if [[ ! "$provider_id" =~ ^[a-z0-9][a-z0-9._-]*$ ]]; then
  echo "Invalid provider id '$provider_id': expected a lowercase id like deepseek or openai" >&2
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

payload="{\"apiKey\":\"${api_key}\",\"providerId\":\"${provider_id}\"}"
http_code="$(
  curl -sS -o "$BODY_FILE" -w '%{http_code}' \
    -X POST "http://127.0.0.1:${PORT}/api/users/${username}/apikey" \
    -H 'Content-Type: application/json' \
    --data "$payload"
)" || { echo "request to mfpi-admin failed" >&2; exit 1; }

cat "$BODY_FILE"
echo
if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
  echo "HTTP ${http_code} (see JSON above)" >&2
  exit 1
fi
