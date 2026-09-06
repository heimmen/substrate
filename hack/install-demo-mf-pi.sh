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
# This is sourced as part of install-ate.sh. Do not run directly.
#
# PRODUCTION environment for the mf-pi demo: deploys into the ate-demo-mf-pi
# namespace using the mfpi atespace (see demos/mf-pi/mf-pi.yaml.tmpl).
# Mirror of demos/mf-pi/deploy.sh; keep the two in sync.

ATE_DEMOS+=(demo-mf-pi) # register demo-mf-pi

demo-mf-pi_cmdline() {
  case "${1}" in
    --deploy-demo-mf-pi) demo-mf-pi_deploy ;;
    --delete-demo-mf-pi) demo-mf-pi_delete ;;
    *)
      return 1
      ;;
  esac
  return 0
}

# Resolve the digest-pinned references for the pi-web workload, pause and
# minio images already pushed to ${KO_DOCKER_REPO} (localhost:5001 for kind).
# The workload images are shared with the test environment. A MinIO image
# missing from the repo is auto-localized from the local docker cache.
demo-mf-pi_images() {
  local repo="${KO_DOCKER_REPO}"
  local piweb pause minio
  piweb="$(docker inspect "${repo}/pi-web:latest" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  pause="$(docker inspect "${repo}/pause:3.10.2" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  if [[ -z "${piweb}" || -z "${pause}" ]]; then
    echo "pi-web or pause image not found in ${repo}; push them first:" >&2
    echo "  cd /home/liuchong/git/pi-web && PI_WEB_IMAGE=pi-web:latest docker/scripts/build-image.sh" >&2
    echo "  docker tag pi-web:latest ${repo}/pi-web:latest && docker push ${repo}/pi-web:latest" >&2
    echo "  docker tag rancher/mirrored-pause:3.10.2 ${repo}/pause:3.10.2 && docker push ${repo}/pause:3.10.2" >&2
    return 1
  fi
  # Extract the digest part (after '@') from the RepoDigests reference.
  MF_PI_IMAGE="${piweb}"
  PAUSE_IMAGE="${pause}"
  MF_PI_DIGEST="${piweb##*@}"
  PAUSE_DIGEST="${pause##*@}"
  # MinIO (per-user profile store): resolve from the repo, auto-localizing from
  # the local docker cache when it is not there yet.
  minio="$(docker inspect "${repo}/minio:latest" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  if [[ -z "${minio}" ]]; then
    local minio_image="${MINIO_IMAGE:-quay.io/minio/minio:RELEASE.2025-06-13T11-33-47Z}"
    if ! docker image inspect "${minio_image}" >/dev/null 2>&1; then
      echo "minio image ${minio_image} not found locally; load it first:" >&2
      echo "  docker pull ${minio_image}" >&2
      echo "  docker tag ${minio_image} ${repo}/minio:latest && docker push ${repo}/minio:latest" >&2
      return 1
    fi
    echo "  localizing ${minio_image} into ${repo} ..."
    docker tag "${minio_image}" "${repo}/minio:latest"
    docker push "${repo}/minio:latest" >/dev/null
    minio="$(docker inspect "${repo}/minio:latest" --format='{{index .RepoDigests 0}}')"
  fi
  MINIO_IMAGE="${repo}/minio@${minio##*@}"
  MINIO_DIGEST="${minio##*@}"
}

# Resolve the shared profile-sync token and MinIO root credentials in the same
# order as deploy.sh: an exported value, then the live Secret (kept stable so
# actors already running with the old value are not cut off), then a fresh
# random/default value (first deploy).
demo-mf-pi_sync_secrets() {
  if [[ -z "${MFPI_PROFILE_TOKEN:-}" ]]; then
    MFPI_PROFILE_TOKEN="$(run_kubectl get secret mfpi-profile-token -n ate-demo-mf-pi \
      -o jsonpath='{.data.token}' 2>/dev/null | base64 -d || true)"
  fi
  MFPI_PROFILE_TOKEN="${MFPI_PROFILE_TOKEN:-$(openssl rand -hex 32 2>/dev/null || tr -dc 'a-f0-9' </dev/urandom | head -c 64)}"
  if [[ -z "${MINIO_ROOT_USER:-}" ]]; then
    MINIO_ROOT_USER="$(run_kubectl get secret mfpi-minio-admin -n ate-demo-mf-pi \
      -o jsonpath='{.data.root-user}' 2>/dev/null | base64 -d || true)"
  fi
  MINIO_ROOT_USER="${MINIO_ROOT_USER:-minioadmin}"
  if [[ -z "${MINIO_ROOT_PASSWORD:-}" ]]; then
    MINIO_ROOT_PASSWORD="$(run_kubectl get secret mfpi-minio-admin -n ate-demo-mf-pi \
      -o jsonpath='{.data.root-password}' 2>/dev/null | base64 -d || true)"
  fi
  MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-$(openssl rand -hex 24 2>/dev/null || tr -dc 'a-f0-9' </dev/urandom | head -c 48)}"
}

# Render mf-pi.yaml.tmpl. Unset values fall back to placeholders so the output
# is always valid YAML (the delete path identifies resources by metadata alone).
demo-mf-pi_render() {
  sed -e "s|\${BUCKET_NAME}|${BUCKET_NAME:-placeholder}|g" \
      -e "s|\${DEEPSEEK_API_KEY}|${DEEPSEEK_API_KEY:-placeholder}|g" \
      -e "s|\${MF_PI_DIGEST}|${MF_PI_DIGEST:-placeholder}|g" \
      -e "s|\${PAUSE_DIGEST}|${PAUSE_DIGEST:-placeholder}|g" \
      -e "s|\${MFPI_WORKER_REPLICAS}|${MFPI_WORKER_REPLICAS:-4}|g" \
      -e "s|\${MFPI_PROFILE_TOKEN}|${MFPI_PROFILE_TOKEN:-placeholder}|g" \
      -e "s|\${MINIO_DIGEST}|${MINIO_DIGEST:-placeholder}|g" \
      -e "s|\${MINIO_ROOT_USER}|${MINIO_ROOT_USER:-minioadmin}|g" \
      -e "s|\${MINIO_ROOT_PASSWORD}|${MINIO_ROOT_PASSWORD:-minioadmin}|g" \
      demos/mf-pi/mf-pi.yaml.tmpl
}

demo-mf-pi_deploy() {
  log_step "demo-mf-pi_deploy"
  for v in DEEPSEEK_API_KEY BUCKET_NAME KO_DOCKER_REPO; do
    if [[ -z "${!v:-}" ]]; then
      echo "$v must be set" >&2
      return 1
    fi
  done
  if ! demo-mf-pi_images; then
    return 1
  fi
  log_step "  workload image: ${MF_PI_IMAGE}"
  log_step "  pause image: ${PAUSE_IMAGE}"
  log_step "  minio image: ${MINIO_IMAGE}"
  demo-mf-pi_sync_secrets

  # Number of physical workers; bounds max concurrently-active users.
  # Defaults to 4 when unset.
  MFPI_WORKER_REPLICAS="${MFPI_WORKER_REPLICAS:-4}"
  log_step "  worker replicas: ${MFPI_WORKER_REPLICAS}"

  # Render to a temp file first so the immutable ActorTemplate can be replaced
  # when its spec changed before ko apply (see ensure_at_recreate_if_changed).
  local manifest
  manifest="$(mktemp)"
  demo-mf-pi_render > "${manifest}"
  ensure_at_recreate_if_changed "${manifest}" ate-demo-mf-pi mf-pi
  run_ko apply -f "${manifest}"
  rm -f "${manifest}"

  # The user-management UI pod is part of the same template; wait for it so
  # the proxy at /usermanagement/ has an upstream by the time deploy returns.
  log_step "Waiting for mfpi-admin to be ready..."
  run_kubectl rollout status deployment/mfpi-admin -n ate-demo-mf-pi --timeout=120s
}

demo-mf-pi_delete() {
  log_step "demo-mf-pi_delete"
  delete_demo_actors ate-demo-mf-pi mf-pi
  demo-mf-pi_render | run_kubectl delete --ignore-not-found -f -
}

demo-mf-pi_usage() {
  echo ""
  echo "  Required env: DEEPSEEK_API_KEY, BUCKET_NAME, KO_DOCKER_REPO"
  echo "  Optional env: MFPI_WORKER_REPLICAS (default 4; max concurrently-active users), MFPI_PROFILE_TOKEN, MINIO_ROOT_USER, MINIO_ROOT_PASSWORD, MINIO_IMAGE"
  echo "  Deploys: pi-web actors + the mfpi-admin user-management UI + a per-user profile MinIO store"
  echo "  UI access: http://<hostname>:58681/usermanagement/ (via run-nginx.sh)"
  echo "  See demos/mf-pi/mfpi.md for the walkthrough."
}
