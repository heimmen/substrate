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

# Resolve the digest-pinned references for the pi-web workload and pause
# images already pushed to ${KO_DOCKER_REPO} (localhost:5001 for kind).
demo-mf-pi_images() {
  local repo="${KO_DOCKER_REPO}"
  MF_PI_IMAGE="$(docker inspect "${repo}/pi-web:latest" --format='{{index .RepoDigests 0}}' 2>/dev/null)"
  PAUSE_IMAGE="$(docker inspect "${repo}/pause:3.10.2" --format='{{index .RepoDigests 0}}' 2>/dev/null)"
  if [[ -z "${MF_PI_IMAGE}" || -z "${PAUSE_IMAGE}" ]]; then
    echo "pi-web or pause image not found in ${repo}; push them first:" >&2
    echo "  cd /home/liuchong/git/pi-web && PI_WEB_IMAGE=pi-web:latest docker/scripts/build-image.sh" >&2
    echo "  docker tag pi-web:latest ${repo}/pi-web:latest && docker push ${repo}/pi-web:latest" >&2
    echo "  docker tag rancher/mirrored-pause:3.10.2 ${repo}/pause:3.10.2 && docker push ${repo}/pause:3.10.2" >&2
    return 1
  fi
  # Extract the digest part (after '@') from the RepoDigests reference.
  MF_PI_DIGEST="${MF_PI_IMAGE##*@}"
  PAUSE_DIGEST="${PAUSE_IMAGE##*@}"
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

  # Number of physical workers; bounds max concurrently-active users.
  # Defaults to 4 when unset.
  MFPI_WORKER_REPLICAS="${MFPI_WORKER_REPLICAS:-4}"
  log_step "  worker replicas: ${MFPI_WORKER_REPLICAS}"

  sed -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
      -e "s|\${DEEPSEEK_API_KEY}|${DEEPSEEK_API_KEY}|g" \
      -e "s|\${MF_PI_DIGEST}|${MF_PI_DIGEST}|g" \
      -e "s|\${PAUSE_DIGEST}|${PAUSE_DIGEST}|g" \
      -e "s|\${MFPI_WORKER_REPLICAS}|${MFPI_WORKER_REPLICAS}|g" \
      demos/mf-pi/mf-pi.yaml.tmpl \
    | run_ko apply -f -

  # The user-management UI pod is part of the same template; wait for it so
  # the proxy at /usermanagement/ has an upstream by the time deploy returns.
  log_step "Waiting for mfpi-admin to be ready..."
  run_kubectl rollout status deployment/mfpi-admin -n ate-demo-mf-pi --timeout=120s
}

demo-mf-pi_delete() {
  log_step "demo-mf-pi_delete"
  delete_demo_actors ate-demo-mf-pi mf-pi
  # Delete-time substitution doesn't need a real image — k8s identifies
  # resources by metadata, not container spec. Use placeholders so sed
  # produces valid YAML even when the env vars aren't set.
  sed -e "s|\${BUCKET_NAME}|${BUCKET_NAME:-placeholder}|g" \
      -e "s|\${DEEPSEEK_API_KEY}|placeholder|g" \
      -e "s|\${MF_PI_DIGEST}|placeholder|g" \
      -e "s|\${PAUSE_DIGEST}|placeholder|g" \
      -e "s|\${MFPI_WORKER_REPLICAS}|1|g" \
      demos/mf-pi/mf-pi.yaml.tmpl \
    | run_kubectl delete --ignore-not-found -f -
}

demo-mf-pi_usage() {
  echo ""
  echo "  Required env: DEEPSEEK_API_KEY, BUCKET_NAME, KO_DOCKER_REPO"
  echo "  Optional env: MFPI_WORKER_REPLICAS (default 4; max concurrently-active users)"
  echo "  Deploys: pi-web actors + the mfpi-admin user-management UI"
  echo "  UI access: http://<hostname>:58681/usermanagement/ (via run-nginx.sh)"
  echo "  See demos/mf-pi/mfpi.md for the walkthrough."
}
