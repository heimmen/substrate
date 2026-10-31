#!/usr/bin/env bash
# Thin wrapper: run the CI/CD page backend for the qa environment
# (port 58690, deploys via deploy-qa.sh).

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=qa
exec ./cicd-server.sh "$@"
