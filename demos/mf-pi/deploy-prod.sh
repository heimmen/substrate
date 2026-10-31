#!/usr/bin/env bash
# Deploy the mf-pi PRODUCTION environment (entry 57681, namespace ate-demo-mf-pi-prod).
#
# Thin wrapper around ./deploy.sh. The environment (namespace, atespace,
# workload label, ports, gateway conf) comes from env/prod.env via lib/env.sh.

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=prod
exec ./deploy.sh "$@"
