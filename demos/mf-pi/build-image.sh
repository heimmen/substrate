#!/usr/bin/env bash
# Build the mfpi-nginx multi-user reverse proxy image.
#
# Uses the default (BuildKit) builder. If that fails — e.g. because BuildKit's
# activity-tracking wants to write somewhere the current environment does not
# permit (a read-only HOME, a sandbox, an unwritable ~/.docker/buildx/... ) —
# fall back to the legacy builder (DOCKER_BUILDKIT=0), which does not need that
# tracker and produces the same image from the same Dockerfile.

set -euo pipefail
cd "$(dirname "$0")"

if docker build -t mfpi-nginx .; then
  exit 0
fi

echo "default (BuildKit) build failed; retrying with the legacy builder (DOCKER_BUILDKIT=0)..." >&2
DOCKER_BUILDKIT=0 docker build -t mfpi-nginx .
