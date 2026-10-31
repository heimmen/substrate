#!/usr/bin/env bash
# Stop the QA gateway and its port-forwards (58681/58680/58682).
#
# Thin wrapper around ./stop-nginx.sh. The environment (namespace, atespace,
# workload label, ports, gateway conf) comes from env/qa.env via lib/env.sh.

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=qa
exec ./stop-nginx.sh "$@"
