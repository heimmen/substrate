#!/usr/bin/env bash
# Delete an mf-pi user (remove its actor). The actor's session history
# (kept in its Full snapshot) is removed along with it. The actor must be
# suspended before it can be deleted: the API rejects deleting a running
# actor with FailedPrecondition. Suspend is idempotent, so it is safe to
# call whether the actor is RUNNING or already SUSPENDED.

set -euo pipefail
cd "$(dirname "$0")"

ATESPACE="${MFPI_ATESPACE:-mfpi}"

usage() {
  echo "Usage: $0 <username>" >&2
  exit 2
}

if [[ $# -ne 1 ]]; then
  usage
fi
username="$1"

# Idempotent: bail out cleanly if the actor doesn't exist.
if ! kubectl ate get actor "$username" -a "$ATESPACE" >/dev/null 2>&1; then
  echo "Actor '$username' does not exist in atespace '$ATESPACE'." >&2
  exit 1
fi

# Suspend first: deleting a RUNNING actor fails with FailedPrecondition.
kubectl ate suspend actor "$username" -a "$ATESPACE" >/dev/null

kubectl ate delete actor "$username" -a "$ATESPACE"
