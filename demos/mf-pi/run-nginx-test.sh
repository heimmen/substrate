#!/usr/bin/env bash
# Start the TEST environment gateway (entry 59681, container mfpi-nginx-test).
#
# Thin wrapper around run-nginx.sh; ports/conf/cookies come from env/test.env.

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=test
exec ./run-nginx.sh "$@"
