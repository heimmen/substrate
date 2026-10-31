#!/usr/bin/env bash
# Thin wrapper: run ./refresh-actor.sh against the prod environment (57681).
#
# Namespace, atespace, ports and cookies come from env/prod.env via
# lib/env.sh; the base script only reads the exported MFPI_* variables.

set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=lib/env.sh
source ./lib/env.sh
mfpi_load_env prod
exec ./refresh-actor.sh "$@"
