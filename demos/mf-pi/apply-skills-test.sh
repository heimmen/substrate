#!/usr/bin/env bash
# Kick the "立即应用" fan-out in the TEST environment (namespace
# ate-demo-mf-pi-test, atespace mfpi-test). Thin wrapper around
# apply-skills.sh, which honors the MFPI_NAMESPACE env var.

set -euo pipefail
export MFPI_NAMESPACE=ate-demo-mf-pi-test
exec "$(dirname "$0")/apply-skills.sh" "$@"
