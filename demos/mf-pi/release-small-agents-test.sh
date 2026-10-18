#!/usr/bin/env bash
# Release (temporarily) the resources of every SMALL-tier mf-pi agent in the
# TEST environment (atespace mfpi-test, entry port 59681): suspend each RUNNING
# small-tier actor to free its worker/Pod.
#
# Thin wrapper around release-small-agents.sh, which honors MFPI_ATESPACE.
# See release-small-agents.sh for details and options (e.g. --force).

set -euo pipefail
export MFPI_ATESPACE=mfpi-test
exec "$(dirname "$0")/release-small-agents.sh" "$@"
