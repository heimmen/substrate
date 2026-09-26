#!/usr/bin/env bash
# List managed shared skills in the TEST environment (namespace
# ate-demo-mf-pi-test, atespace mfpi-test). Thin wrapper around
# list-skills.sh, which honors the MFPI_NAMESPACE env var.

set -euo pipefail
export MFPI_NAMESPACE=ate-demo-mf-pi-test
exec "$(dirname "$0")/list-skills.sh" "$@"
