#!/usr/bin/env bash
# Install (upload) a managed "shared skill" into mfpi-admin by calling the
# REST API over a temporary kubectl port-forward.
#
# The admin is the authoritative source for skills that are distributed to
# every user (actor). It validates the package (must contain a SKILL.md, and
# reject ../ / absolute path entries), stores it under $SKILLS_DIR, and rewrites
# the manifest. See deploy_skill_to_actor.md for the full design.
#
# Usage: install-skill.sh <name> <file.tgz|file.zip|dir>
#   - A .tgz/.zip is uploaded as-is.
#   - A directory is packed into a .tgz on the fly (top-level dir preserved).
#   - <name> must match DNS-1123 and is the skill name stored by the admin.
#
# The response is printed as JSON; a non-2xx status exits non-zero. Honors the
# MFPI_NAMESPACE env var (default ate-demo-mf-pi; the mfpi-test atespace lives
# in ate-demo-mf-pi-test).

set -euo pipefail
cd "$(dirname "$0")"

NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi}"

DNS1123='^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'

usage() {
  echo "Usage: $0 <name> <file.tgz|file.zip|dir>" >&2
  echo "  <name> must match DNS-1123: ${DNS1123}" >&2
  echo "  <file.tgz|file.zip|dir> the skill package (must contain SKILL.md)" >&2
  exit 2
}

[[ $# -eq 2 ]] || usage
name="$1"
src="$2"

if ! [[ "$name" =~ $DNS1123 ]]; then
  echo "Invalid skill name '$name': must match ${DNS1123}" >&2
  exit 2
fi
if [[ ! -e "$src" ]]; then
  echo "No such file or directory: $src" >&2
  exit 2
fi

# Pack a directory into a tgz so only a single upload path is needed.
if [[ -d "$src" ]]; then
  tmp_tgz="$(mktemp --suffix=.tgz)"
  cleanup_src() { rm -f "$tmp_tgz"; }
  trap cleanup_src EXIT
  (
    cd "$(dirname "$src")" && tar -czf "$tmp_tgz" "$(basename "$src")"
  )
  upload="$tmp_tgz"
  upload_name="$(basename "$src").tgz"
else
  upload="$src"
  upload_name="$(basename "$src")"
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
  if [[ -d "$src" ]]; then
    rm -f "${tmp_tgz:-}"
  fi
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
    -X POST "http://127.0.0.1:${PORT}/api/skills" \
    -F "name=${name}" \
    -F "file=@${upload};filename=${upload_name}"
)" || { echo "request to mfpi-admin failed" >&2; exit 1; }

cat "$BODY_FILE"
echo
if [[ "$http_code" -lt 200 || "$http_code" -ge 300 ]]; then
  echo "HTTP ${http_code} (see JSON above)" >&2
  exit 1
fi
