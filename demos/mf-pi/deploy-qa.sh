#!/usr/bin/env bash
# Deploy the mf-pi QA environment (entry 58681, namespace ate-demo-mf-pi).
#
# Thin wrapper around ./deploy.sh. The environment (namespace, atespace,
# workload label, ports, gateway conf) comes from env/qa.env via lib/env.sh.

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=qa
exec ./deploy.sh "$@"
