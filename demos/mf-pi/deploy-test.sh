#!/usr/bin/env bash
# Deploy the mf-pi TEST environment (entry 59681, namespace ate-demo-mf-pi-test).
#
# Thin wrapper around deploy.sh. Namespace, atespace, workload label, worker
# counts and gateway ports all come from env/test.env via lib/env.sh, so this
# environment is fully isolated from qa and prod.

set -euo pipefail
cd "$(dirname "$0")"
export MFPI_ENV=test
exec ./deploy.sh "$@"
