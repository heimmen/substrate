#!/usr/bin/env bash
# List all mf-pi users in the TEST environment (atespace mfpi-test).
# Thin wrapper around list-users.sh, which honors the MFPI_ATESPACE env var.

set -euo pipefail
export MFPI_ATESPACE=mfpi-test
exec "$(dirname "$0")/list-users.sh" "$@"
