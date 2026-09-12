#!/usr/bin/env bash
# Idempotently create a user (one mf-pi actor) in the TEST environment
# (atespace mfpi-test, template ate-demo-mf-pi-test/mf-pi). If the actor
# already exists, reuse it — its session history is preserved. This satisfies
# "no instance -> create; instance exists -> reuse".

set -euo pipefail
cd "$(dirname "$0")"

# atespace name; change if the test demo uses a different atespace.
ATESPACE="${MFPI_ATESPACE:-mfpi-test}"
TEMPLATE="ate-demo-mf-pi-test/mf-pi"

# Validate username against DNS-1123 (same rule as Substrate actor names).
DNS1123='^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'

usage() {
  echo "Usage: $0 <username>" >&2
  echo "  <username> must match DNS-1123: ${DNS1123}" >&2
  exit 2
}

if [[ $# -ne 1 ]]; then
  usage
fi
username="$1"

if ! [[ "$username" =~ $DNS1123 ]]; then
  echo "Invalid username '$username': must match ${DNS1123}" >&2
  exit 2
fi

# Ensure the atespace exists before creating actors in it.
if ! kubectl ate get atespace "$ATESPACE" >/dev/null 2>&1; then
  echo "Atespace '$ATESPACE' not found; creating it."
  kubectl ate create atespace "$ATESPACE"
fi

# Idempotent create-or-reuse.
if kubectl ate get actor "$username" -a "$ATESPACE" >/dev/null 2>&1; then
  echo "Actor '$username' already exists; reusing existing session (history preserved)."
else
  echo "Creating actor '$username' from template '$TEMPLATE'."
  kubectl ate create actor "$username" -a "$ATESPACE" --template "$TEMPLATE"
fi
