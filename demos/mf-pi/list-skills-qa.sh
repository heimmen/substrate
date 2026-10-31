#!/usr/bin/env bash
# Thin wrapper: run ./list-skills.sh against the qa environment (58681).
#
# Namespace, atespace, ports and cookies come from env/qa.env via
# lib/env.sh; the base script only reads the exported MFPI_* variables.

set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=lib/env.sh
source ./lib/env.sh
mfpi_load_env qa
exec ./list-skills.sh "$@"
