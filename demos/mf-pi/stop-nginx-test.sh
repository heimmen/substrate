#!/usr/bin/env bash
# Stop the TEST environment gateway and its port-forwards (59681/59880/59882).
#
# Thin wrapper around stop-nginx.sh; only this environment's ports and
# container are touched.

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=test
exec ./stop-nginx.sh "$@"
