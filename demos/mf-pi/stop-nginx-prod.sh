#!/usr/bin/env bash
# Stop the PRODUCTION gateway and its port-forwards (57681/57680/57682).
#
# Thin wrapper around ./stop-nginx.sh. The environment (namespace, atespace,
# workload label, ports, gateway conf) comes from env/prod.env via lib/env.sh.

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=prod
exec ./stop-nginx.sh "$@"
