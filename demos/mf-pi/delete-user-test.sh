#!/usr/bin/env bash
# Delete an mf-pi user (remove its actor) in the TEST environment (atespace
# mfpi-test). Thin wrapper around delete-user.sh, which honors the
# MFPI_ATESPACE env var.

set -euo pipefail
export MFPI_ATESPACE=mfpi-test
exec "$(dirname "$0")/delete-user.sh" "$@"
