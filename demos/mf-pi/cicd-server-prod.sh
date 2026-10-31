#!/usr/bin/env bash
# Thin wrapper: run the CI/CD page backend for the prod environment
# (port 57690, deploys via deploy-prod.sh).

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=prod
exec ./cicd-server.sh "$@"
