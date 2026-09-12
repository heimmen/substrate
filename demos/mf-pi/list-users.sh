#!/usr/bin/env bash
# List all mf-pi users (actors in the mfpi atespace).

set -euo pipefail
cd "$(dirname "$0")"

ATESPACE="${MFPI_ATESPACE:-mfpi}"

kubectl ate get actors -a "$ATESPACE"
