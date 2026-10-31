#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Shared implementation for the mf-pi demo installers. Sourced by install-ate.sh
# before the per-environment registration files; do not run directly.
#
# The three environments are defined in demos/mf-pi/env/<name>.env and rendered
# from the single parameterized demos/mf-pi/mf-pi.yaml.tmpl. Nothing here is
# environment-specific: callers pass prod|qa|test.

_MFPI_INSTALL_ROOT="${ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
# shellcheck source=../demos/mf-pi/lib/env.sh
source "${_MFPI_INSTALL_ROOT}/demos/mf-pi/lib/env.sh"

# Escape a value for the replacement part of `sed s|...|...|`.
_mfpi_esc() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//&/\\&}"
  printf '%s' "$s"
}

# Build the workload image from the miaofang-agent SOURCE so every deploy picks
# up the latest repo changes (build-image.sh --from-source). Set
# MFPI_SKIP_WORKLOAD_BUILD=1 to reuse an already-pushed image (offline/CI), and
# MFPI_WORKLOAD_SOURCE to point at a different checkout.
mfpi_install_build_workload_image() {
  local repo="${KO_DOCKER_REPO}"
  if [[ "${MFPI_SKIP_WORKLOAD_BUILD:-0}" == "1" ]]; then
    return 0
  fi
  local src="${MFPI_WORKLOAD_SOURCE:-/home/liuchong/git/miaofang-agent}"
  if [[ ! -d "${src}/docker" ]]; then
    echo "workload image source not found at '${src}'" >&2
    echo "  set MFPI_WORKLOAD_SOURCE to the miaofang-agent checkout, or" >&2
    echo "  set MFPI_SKIP_WORKLOAD_BUILD=1 to use the already-pushed mf-agent image" >&2
    return 1
  fi
  echo "building workload image from ${src} (latest source) ..."
  ( cd "${src}" && PI_WEB_IMAGE=mf-agent:latest docker/scripts/build-image.sh --from-source ) \
    || { echo "workload image build failed (from ${src})" >&2; return 1; }
  echo "pushing mf-agent:latest to ${repo} ..."
  docker tag mf-agent:latest "${repo}/mf-agent:latest"
  docker push "${repo}/mf-agent:latest"
}

# Resolve the digest-pinned mf-agent and pause image references. Both are shared
# by all environments (the same image serves prod/qa/test).
mfpi_install_images() {
  local repo="${KO_DOCKER_REPO}"
  local piweb pause
  mfpi_install_build_workload_image || return 1
  piweb="$(docker inspect "${repo}/mf-agent:latest" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  pause="$(docker inspect "${repo}/pause:3.10.2" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  if [[ -z "${piweb}" || -z "${pause}" ]]; then
    echo "mf-agent or pause image not present in ${repo} after build; push them first:" >&2
    echo "  cd /home/liuchong/git/miaofang-agent && PI_WEB_IMAGE=mf-agent:latest docker/scripts/build-image.sh --from-source" >&2
    echo "  docker tag mf-agent:latest ${repo}/mf-agent:latest && docker push ${repo}/mf-agent:latest" >&2
    echo "  docker tag rancher/mirrored-pause:3.10.2 ${repo}/pause:3.10.2 && docker push ${repo}/pause:3.10.2" >&2
    return 1
  fi
  MF_PI_IMAGE="${piweb}"
  PAUSE_IMAGE="${pause}"
  MF_PI_DIGEST="${piweb##*@}"
  PAUSE_DIGEST="${pause##*@}"
}

# Render mf-pi.yaml.tmpl for one environment. Unset values fall back to
# placeholders so the output is always valid YAML (the delete path identifies
# resources by metadata alone).
mfpi_install_render() {
  local env="$1"
  mfpi_load_env "${env}" || return 1
  sed -e "s|\${BUCKET_NAME}|$(_mfpi_esc "${BUCKET_NAME:-placeholder}")|g" \
      -e "s|\${DEEPSEEK_API_KEY}|$(_mfpi_esc "${DEEPSEEK_API_KEY:-placeholder}")|g" \
      -e "s|\${MF_PI_DIGEST}|$(_mfpi_esc "${MF_PI_DIGEST:-placeholder}")|g" \
      -e "s|\${PAUSE_DIGEST}|$(_mfpi_esc "${PAUSE_DIGEST:-placeholder}")|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_SMALL}|$(_mfpi_esc "${MFPI_WORKER_REPLICAS_SMALL:-24}")|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_MID}|$(_mfpi_esc "${MFPI_WORKER_REPLICAS_MID:-1}")|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_LARGE}|$(_mfpi_esc "${MFPI_WORKER_REPLICAS_LARGE:-1}")|g" \
      -e "s|\${MFPI_IDLE_TIMEOUT}|$(_mfpi_esc "${MFPI_IDLE_TIMEOUT:-90m}")|g" \
      -e "s|\${MFPI_AUTH_COOKIE_TTL}|$(_mfpi_esc "${MFPI_AUTH_COOKIE_TTL:-720m}")|g" \
      -e "s|\${MFPI_IDLE_TIERS}|$(_mfpi_esc "${MFPI_IDLE_TIERS:-small,mid}")|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_BASE}|$(_mfpi_esc "${MFPI_WORKER_REPLICAS_BASE:-${MFPI_WORKER_REPLICAS:-0}}")|g" \
      -e "s|\${MFPI_DEPLOY_REV}|$(_mfpi_esc "${MFPI_DEPLOY_REV:-0}")|g" \
      -e "s|\${MFPI_NAMESPACE}|$(_mfpi_esc "${MFPI_NAMESPACE}")|g" \
      -e "s|\${MFPI_ATESPACE}|$(_mfpi_esc "${MFPI_ATESPACE}")|g" \
      -e "s|\${MFPI_WORKLOAD_LABEL}|$(_mfpi_esc "${MFPI_WORKLOAD_LABEL}")|g" \
      -e "s|\${MFPI_AUTH_COOKIE}|$(_mfpi_esc "${MFPI_AUTH_COOKIE}")|g" \
      "${_MFPI_INSTALL_ROOT}/demos/mf-pi/mf-pi.yaml.tmpl"
}

# mfpi_install_deploy <prod|qa|test>
mfpi_install_deploy() {
  local env="${1:?environment required}"
  mfpi_load_env "${env}" || return 1
  log_step "mf-pi ${MFPI_ENV_NAME}: namespace=${MFPI_NAMESPACE} atespace=${MFPI_ATESPACE}"
  for v in DEEPSEEK_API_KEY BUCKET_NAME KO_DOCKER_REPO; do
    if [[ -z "${!v:-}" ]]; then
      echo "$v must be set" >&2
      return 1
    fi
  done
  if ! mfpi_install_images; then
    return 1
  fi
  log_step "  workload image: ${MF_PI_IMAGE}"
  log_step "  pause image: ${PAUSE_IMAGE}"

  MFPI_WORKER_REPLICAS_BASE="${MFPI_WORKER_REPLICAS_BASE:-${MFPI_WORKER_REPLICAS:-0}}"
  log_step "  worker replicas (base/small/mid/large): ${MFPI_WORKER_REPLICAS_BASE}/${MFPI_WORKER_REPLICAS_SMALL:-24}/${MFPI_WORKER_REPLICAS_MID:-1}/${MFPI_WORKER_REPLICAS_LARGE:-1}"

  # Render to a temp file first so the immutable ActorTemplate can be replaced
  # when its spec changed before ko apply.
  local manifest
  manifest="$(mktemp)"
  mfpi_install_render "${env}" > "${manifest}"
  ensure_at_recreate_if_changed "${manifest}" "${MFPI_NAMESPACE}" mf-pi
  run_ko apply -f "${manifest}"
  rm -f "${manifest}"

  log_step "Waiting for mfpi-admin to be ready..."
  run_kubectl rollout status deployment/mfpi-admin -n "${MFPI_NAMESPACE}" --timeout=120s
}

# mfpi_install_delete <prod|qa|test>
mfpi_install_delete() {
  local env="${1:?environment required}"
  mfpi_load_env "${env}" || return 1
  log_step "mf-pi ${MFPI_ENV_NAME}: deleting ${MFPI_NAMESPACE}"
  delete_demo_actors "${MFPI_NAMESPACE}" mf-pi
  mfpi_install_render "${env}" | run_kubectl delete --ignore-not-found -f -
}

# mfpi_install_usage <prod|qa|test>
mfpi_install_usage() {
  local env="${1:?environment required}"
  mfpi_load_env "${env}" >/dev/null 2>&1 || true
  echo ""
  echo "  mf-pi ${MFPI_ENV_NAME}: namespace=${MFPI_NAMESPACE}, atespace=${MFPI_ATESPACE}"
  echo "  Required env: DEEPSEEK_API_KEY, BUCKET_NAME, KO_DOCKER_REPO"
  echo "  Optional env: MFPI_WORKER_REPLICAS_SMALL/_MID/_LARGE (seats per tier;"
  echo "                defaults ${MFPI_WORKER_REPLICAS_SMALL:-24}/${MFPI_WORKER_REPLICAS_MID:-1}/${MFPI_WORKER_REPLICAS_LARGE:-1}); MFPI_WORKER_REPLICAS_BASE (default 0)"
  echo "  Deploys: mf-agent actors + the mfpi-admin user-management UI (user data persists on a sticky per-actor volume)"
  echo "  UI access: http://<hostname>:${MFPI_ENTRY_PORT}/usermanagement/ (via ${MFPI_RUN_NGINX_SCRIPT})"
  echo "  See demos/mf-pi/docs/mfpi.md for the walkthrough."
}
