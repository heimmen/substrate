#!/usr/bin/env bash
# Build the mfpi-nginx multi-user reverse proxy image.

set -euo pipefail
cd "$(dirname "$0")"
docker build -t mfpi-nginx .
