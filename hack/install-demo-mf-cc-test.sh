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
# TEST environment for the mf-cc demo: deploys into the ate-demo-mf-cc-test
# namespace using the mfcc-test atespace (see demos/mf-cc/mf-cc-test.yaml.tmpl),
# so it never conflicts with the production demo-mf-cc.

ATE_DEMOS+=(demo-mf-cc-test) # register demo-mf-cc-test

demo-mf-cc-test_cmdline() {
  case "${1}" in
    --deploy-demo-mf-cc-test) demo-mf-cc-test_deploy ;;
    --delete-demo-mf-cc-test) demo-mf-cc-test_delete ;;
    *)
      return 1
      ;;
  esac
  return 0
}

# Resolve the digest-pinned references for the mf-cc workload and pause
# images already pushed to ${KO_DOCKER_REPO} (localhost:5001 for kind). The
# workload images are shared with the production demo.
demo-mf-cc-test_images() {
  local repo="${KO_DOCKER_REPO}"
  MFCC_IMAGE="$(docker inspect "${repo}/mf-cc:latest" --format='{{index .RepoDigests 0}}' 2>/dev/null)"
  PAUSE_IMAGE="$(docker inspect "${repo}/pause:3.10.2" --format='{{index .RepoDigests 0}}' 2>/dev/null)"
  if [[ -z "${MFCC_IMAGE}" || -z "${PAUSE_IMAGE}" ]]; then
    echo "mf-cc or pause image not found in ${repo}; push them first:" >&2
    echo "  docker tag mf-cc:latest ${repo}/mf-cc:latest && docker push ${repo}/mf-cc:latest" >&2
    echo "  docker tag rancher/mirrored-pause:3.10.2 ${repo}/pause:3.10.2 && docker push ${repo}/pause:3.10.2" >&2
    return 1
  fi
  # Extract the digest part (after '@') from the RepoDigests reference.
  MFCC_DIGEST="${MFCC_IMAGE##*@}"
  PAUSE_DIGEST="${PAUSE_IMAGE##*@}"
}

demo-mf-cc-test_deploy() {
  log_step "demo-mf-cc-test_deploy"
  for v in ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_MODEL BUCKET_NAME KO_DOCKER_REPO; do
    if [[ -z "${!v:-}" ]]; then
      echo "$v must be set" >&2
      return 1
    fi
  done
  if ! demo-mf-cc-test_images; then
    return 1
  fi
  log_step "  workload image: ${MFCC_IMAGE}"
  log_step "  pause image: ${PAUSE_IMAGE}"

  # Number of physical workers; bounds max concurrently-active users.
  # Defaults to 2 for the test environment.
  MFCC_WORKER_REPLICAS="${MFCC_WORKER_REPLICAS:-2}"
  log_step "  worker replicas: ${MFCC_WORKER_REPLICAS}"

  sed -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
      -e "s|\${ANTHROPIC_AUTH_TOKEN}|${ANTHROPIC_AUTH_TOKEN}|g" \
      -e "s|\${ANTHROPIC_BASE_URL}|${ANTHROPIC_BASE_URL}|g" \
      -e "s|\${ANTHROPIC_MODEL}|${ANTHROPIC_MODEL}|g" \
      -e "s|\${MFCC_DIGEST}|${MFCC_DIGEST}|g" \
      -e "s|\${PAUSE_DIGEST}|${PAUSE_DIGEST}|g" \
      -e "s|\${MFCC_WORKER_REPLICAS}|${MFCC_WORKER_REPLICAS}|g" \
      demos/mf-cc/mf-cc-test.yaml.tmpl \
    | run_ko apply -f -

  # The user-management UI pod is part of the same template; wait for it so
  # the proxy at /usermanagement/ has an upstream by the time deploy returns.
  log_step "Waiting for mfcc-admin to be ready..."
  run_kubectl rollout status deployment/mfcc-admin -n ate-demo-mf-cc-test --timeout=120s
}

demo-mf-cc-test_delete() {
  log_step "demo-mf-cc-test_delete"
  delete_demo_actors ate-demo-mf-cc-test mf-cc
  # Delete-time substitution doesn't need a real image — k8s identifies
  # resources by metadata, not container spec. Use placeholders so sed
  # produces valid YAML even when the env vars aren't set.
  sed -e "s|\${BUCKET_NAME}|${BUCKET_NAME:-placeholder}|g" \
      -e "s|\${ANTHROPIC_AUTH_TOKEN}|placeholder|g" \
      -e "s|\${ANTHROPIC_BASE_URL}|placeholder|g" \
      -e "s|\${ANTHROPIC_MODEL}|placeholder|g" \
      -e "s|\${MFCC_DIGEST}|placeholder|g" \
      -e "s|\${PAUSE_DIGEST}|placeholder|g" \
      -e "s|\${MFCC_WORKER_REPLICAS}|1|g" \
      demos/mf-cc/mf-cc-test.yaml.tmpl \
    | run_kubectl delete --ignore-not-found -f -
}

demo-mf-cc-test_usage() {
  echo ""
  echo "  Required env: ANTHROPIC_AUTH_TOKEN, ANTHROPIC_BASE_URL, ANTHROPIC_MODEL, BUCKET_NAME, KO_DOCKER_REPO"
  echo "  Optional env: MFCC_WORKER_REPLICAS (default 2; max concurrently-active users)"
  echo "  Deploys: mf-cc test actors + the mfcc-admin user-management UI (namespace ate-demo-mf-cc-test, atespace mfcc-test)"
  echo "  UI access: http://<hostname>:59881/usermanagement/ (via run-nginx-test.sh)"
}
