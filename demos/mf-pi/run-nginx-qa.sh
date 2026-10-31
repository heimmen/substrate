#!/usr/bin/env bash
# Start the QA gateway (entry 58681, container mfpi-nginx-qa).
#
# Thin wrapper around ./run-nginx.sh. The environment (namespace, atespace,
# workload label, ports, gateway conf) comes from env/qa.env via lib/env.sh.

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=qa
exec ./run-nginx.sh "$@"
