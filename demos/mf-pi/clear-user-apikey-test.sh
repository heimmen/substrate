#!/usr/bin/env bash
# Clear the per-user DeepSeek API key in the TEST environment (namespace
# ate-demo-mf-pi-test, atespace mfpi-test). Thin wrapper around
# clear-user-apikey.sh, which honors the MFPI_NAMESPACE env var.

set -euo pipefail
export MFPI_NAMESPACE=ate-demo-mf-pi-test
exec "$(dirname "$0")/clear-user-apikey.sh" "$@"
