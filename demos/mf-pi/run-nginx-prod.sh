#!/usr/bin/env bash
# Start the PRODUCTION gateway (entry 57681, container mfpi-nginx-prod).
#
# Thin wrapper around ./run-nginx.sh. The environment (namespace, atespace,
# workload label, ports, gateway conf) comes from env/prod.env via lib/env.sh.

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=prod
exec ./run-nginx.sh "$@"
