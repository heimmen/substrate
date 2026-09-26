#!/usr/bin/env bash
# Install (upload) a managed shared skill in the TEST environment (namespace
# ate-demo-mf-pi-test, atespace mfpi-test). Thin wrapper around
# install-skill.sh, which honors the MFPI_NAMESPACE env var.

set -euo pipefail
export MFPI_NAMESPACE=ate-demo-mf-pi-test
exec "$(dirname "$0")/install-skill.sh" "$@"
