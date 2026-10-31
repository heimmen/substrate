#!/usr/bin/env bash
# Thin wrapper: run the CI/CD page backend for the test environment
# (port 59690, deploys via deploy-test.sh).

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=test
exec ./cicd-server.sh "$@"
