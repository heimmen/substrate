#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Deploy the mf-cc TEST environment (mf-cc actors + the mfcc-admin
# user-management UI) against an Agent Substrate cluster.
#
# This is the test-environment counterpart of deploy.sh: it deploys into the
# ate-demo-mf-cc-test namespace using the mfcc-test atespace, so it never
# conflicts with the production environment (ate-demo-mf-cc / mfcc).
#
# Usage:
#   ./deploy-test.sh         # deploy (default)
#   ./deploy-test.sh deploy  # deploy
#
# All configuration is optional and falls back to sensible offline/kind
# defaults. Override any of them by exporting the variable first:
#   KO_DOCKER_REPO        (default localhost:5001)
#   BUCKET_NAME           (default ate-snapshots)
#   ANTHROPIC_AUTH_TOKEN  (default: read from the mf-cc-provider-config secret)
#   ANTHROPIC_BASE_URL    (default https://api.deepseek.com/anthropic)
#   ANTHROPIC_MODEL       (default deepseek-v4-flash)
#   MFCC_WORKER_REPLICAS  (default 2)
#   KO_DEFAULTBASEIMAGE   (default localhost:5001/distroless-static-debian13)
#
# The mf-cc and pause workload images must already be pushed (by digest) to
# ${KO_DOCKER_REPO}; the script resolves their digests before applying.

set -euo pipefail
cd "$(dirname "$0")"

NAMESPACE="ate-demo-mf-cc-test"
TEMPLATE="mf-cc-test.yaml.tmpl"

# --- Defaults ---------------------------------------------------------------
# Registry the cluster can pull from (kind/k3s local registry). Exported so
# the `ko` child process can read it.
export KO_DOCKER_REPO="${KO_DOCKER_REPO:-localhost:5001}"
# Logical snapshot bucket used by rustfs on kind/k3s.
: "${BUCKET_NAME:=ate-snapshots}"
# Provider credentials: prefer an already-exported value; otherwise fall back
# to reading the live secret created by a previous deploy.
if [[ -z "${ANTHROPIC_AUTH_TOKEN:-}" ]]; then
  ANTHROPIC_AUTH_TOKEN="$(
    kubectl get secret mf-cc-provider-config -n "${NAMESPACE}" \
      -o jsonpath='{.data.ANTHROPIC_AUTH_TOKEN}' 2>/dev/null | base64 -d
  )"
fi
: "${ANTHROPIC_BASE_URL:=https://api.deepseek.com/anthropic}"
: "${ANTHROPIC_MODEL:=deepseek-v4-flash}"
: "${MFCC_WORKER_REPLICAS:=2}"
# ko builds ateom-gvisor (and other Go images) on this offline-friendly base.
export KO_DEFAULTBASEIMAGE="${KO_DEFAULTBASEIMAGE:-localhost:5001/distroless-static-debian13}"

# Escape a string for safe use in the replacement part of `sed s|...|...|`.
esc_repl() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//&/\\&}"
  printf '%s' "$s"
}

render() {
  sed -e "s|\${BUCKET_NAME}|$(esc_repl "${BUCKET_NAME:-placeholder}")|g" \
      -e "s|\${ANTHROPIC_AUTH_TOKEN}|$(esc_repl "${ANTHROPIC_AUTH_TOKEN:-placeholder}")|g" \
      -e "s|\${ANTHROPIC_BASE_URL}|$(esc_repl "${ANTHROPIC_BASE_URL:-placeholder}")|g" \
      -e "s|\${ANTHROPIC_MODEL}|$(esc_repl "${ANTHROPIC_MODEL:-placeholder}")|g" \
      -e "s|\${MFCC_DIGEST}|$(esc_repl "${MFCC_DIGEST}")|g" \
      -e "s|\${PAUSE_DIGEST}|$(esc_repl "${PAUSE_DIGEST}")|g" \
      -e "s|\${MFCC_WORKER_REPLICAS}|$(esc_repl "${MFCC_WORKER_REPLICAS:-2}")|g" \
      "${TEMPLATE}"
}

resolve_images() {
  local repo="${KO_DOCKER_REPO}"
  local mfcc pause
  mfcc="$(docker inspect "${repo}/mf-cc:latest" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  pause="$(docker inspect "${repo}/pause:3.10.2" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  if [[ -z "${mfcc}" || -z "${pause}" ]]; then
    echo "mf-cc or pause image not found in ${repo}; push them first:" >&2
    echo "  docker tag mf-cc:latest ${repo}/mf-cc:latest && docker push ${repo}/mf-cc:latest" >&2
    echo "  docker tag rancher/mirrored-pause:3.10.2 ${repo}/pause:3.10.2 && docker push ${repo}/pause:3.10.2" >&2
    return 1
  fi
  MFCC_DIGEST="${mfcc##*@}"
  PAUSE_DIGEST="${pause##*@}"
}

cmd_deploy() {
  for v in ANTHROPIC_AUTH_TOKEN BUCKET_NAME KO_DOCKER_REPO; do
    if [[ -z "${!v:-}" ]]; then
      echo "warning: $v is empty (no exported value and no mf-cc-provider-config secret found)" >&2
    fi
  done
  if [[ -z "${ANTHROPIC_AUTH_TOKEN:-}" ]]; then
    echo "warning: deploying with an empty ANTHROPIC_AUTH_TOKEN; set it (or create the secret) for the app to authenticate." >&2
  fi

  if ! command -v ko >/dev/null 2>&1; then
    echo "ko is required to deploy (resolves ko:// image references)" >&2
    exit 1
  fi

  if ! resolve_images; then
    exit 1
  fi
  echo "  workload image digest: ${MFCC_DIGEST}"
  echo "  pause image digest:    ${PAUSE_DIGEST}"
  echo "  worker replicas:       ${MFCC_WORKER_REPLICAS:-2}"

  render | ko apply -f -

  echo "Waiting for mfcc-admin to be ready..."
  kubectl rollout status deployment/mfcc-admin -n "${NAMESPACE}" --timeout=120s

  echo "mf-cc test environment deployed."
  echo "  management UI: run ./run-nginx-test.sh, then open http://localhost:59881/usermanagement/"
}

usage() {
  echo "Usage: $0 [deploy]" >&2
  exit 2
}

case "${1:-}" in
  ""|deploy) cmd_deploy ;;
  *)         usage ;;
esac
