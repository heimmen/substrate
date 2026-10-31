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

# Deploy (or remove) the mf-pi mock LLM and register it as a provider.
#
# The mock is used by tools/mfpi-loadgen's "agent-turn" scenario so capacity
# tests do not spend real provider budget. It builds the image with ko from
# tools/mfpi-loadgen (subcommand "serve-mock"), deploys it into the demo
# namespace, and registers the endpoint through mfpi-admin so every actor's
# supervisor pulls it into /data/models.json within ~60s.
#
# Usage:
#   ./deploy-mock-llm.sh [--prod|--qa|--test] [--delete] [--no-register]
#
#   --prod          use production   (57681 / mfpi-prod / ate-demo-mf-pi-prod)
#   --qa            use qa           (58681 / mfpi / ate-demo-mf-pi)
#   --test          use test         (59681 / mfpi-test / ate-demo-mf-pi-test)
#   --delete        remove the Deployment/Service and the provider registration
#   --no-register   only deploy the workload; skip POST /api/providers
#
# Tunables (env):
#   KO_DOCKER_REPO   default localhost:5001 (the kind registry used by the demo)
#   ADMIN_USER       default admin
#   ADMIN_PASSWORD   default mf@pass2026
#   MOCK_TTFT        default 100ms   (time to first token)
#   MOCK_CHUNK_DELAY default 10ms
#   MOCK_CHUNKS      default 32
#   MOCK_ERROR_RATE  default 0
#   MOCK_PROVIDER_ID default mock
#   MOCK_MODEL_ID    default mock-1
#   MOCK_ENTRY       override the gateway URL (default http://127.0.0.1:<port>)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
cd "${REPO_ROOT}"

ENVIRONMENT="prod"
DELETE=0
REGISTER=1
for arg in "$@"; do
  case "${arg}" in
    --test) ENVIRONMENT="test" ;;
    --qa) ENVIRONMENT="qa" ;;
    --prod) ENVIRONMENT="prod" ;;
    --delete) DELETE=1 ;;
    --no-register) REGISTER=0 ;;
    -h|--help)
      sed -n '/^# Deploy (or remove) the mf-pi mock LLM/,/^set -euo/p' "${SCRIPT_DIR}/deploy-mock-llm.sh" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "unknown argument: ${arg}" >&2; exit 2 ;;
  esac
done

# Environment constants (namespace, workload label, entry port) come from the
# shared env files so the mock never lands in the wrong environment.
# shellcheck source=../lib/env.sh
source "${SCRIPT_DIR}/../lib/env.sh"
mfpi_load_env "${ENVIRONMENT}"
NAMESPACE="${MFPI_NAMESPACE}"
WORKLOAD_LABEL="${MFPI_WORKLOAD_LABEL}"
ENTRY="${MOCK_ENTRY:-http://127.0.0.1:${MFPI_ENTRY_PORT}}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-mf@pass2026}"
PROVIDER_ID="${MOCK_PROVIDER_ID:-mock}"
MODEL_ID="${MOCK_MODEL_ID:-mock-1}"
KO_DOCKER_REPO="${KO_DOCKER_REPO:-localhost:5001}"
# Same offline-friendly base image the demo's own deploy scripts use: gcr.io is
# not reachable from this host, but the mirror is already in the local registry.
KO_DEFAULTBASEIMAGE="${KO_DEFAULTBASEIMAGE:-localhost:5001/distroless-static-debian13}"
# The default Go build cache is stale/broken on some dev hosts (see the repo
# README); ko shells out to go, so give it a known-good cache unless the caller
# already provided one.
export GOCACHE="${GOCACHE:-/tmp/mfpi-gocache}"
export KO_DOCKER_REPO KO_DEFAULTBASEIMAGE

BASE_URL="http://mfpi-mock-llm.${NAMESPACE}.svc.cluster.local:8080/v1"
RENDERED="$(mktemp -t mfpi-mock-llm.XXXXXX.yaml)"
trap 'rm -f "${RENDERED}"' EXIT

export NAMESPACE WORKLOAD_LABEL
export MOCK_TTFT="${MOCK_TTFT:-100ms}"
export MOCK_CHUNK_DELAY="${MOCK_CHUNK_DELAY:-10ms}"
export MOCK_CHUNKS="${MOCK_CHUNKS:-32}"
export MOCK_ERROR_RATE="${MOCK_ERROR_RATE:-0}"
envsubst '${NAMESPACE} ${WORKLOAD_LABEL} ${MOCK_TTFT} ${MOCK_CHUNK_DELAY} ${MOCK_CHUNKS} ${MOCK_ERROR_RATE}' \
  < "${SCRIPT_DIR}/mock-llm.yaml.tmpl" > "${RENDERED}"

unregister_provider() {
  echo "removing provider ${PROVIDER_ID} from the management registry"
  code=$(curl -sS -o /tmp/mfpi-mock-llm-delete.out -w '%{http_code}' -m 20 \
    -u "${ADMIN_USER}:${ADMIN_PASSWORD}" -X DELETE \
    "${ENTRY}/usermanagement/api/providers/${PROVIDER_ID}" 2>/dev/null || true)
  case "${code}" in
    200|204|404) ;;
    *) echo "warning: DELETE provider returned ${code}: $(cat /tmp/mfpi-mock-llm-delete.out 2>/dev/null)" >&2 ;;
  esac
  rm -f /tmp/mfpi-mock-llm-delete.out
}

if [[ "${DELETE}" == "1" ]]; then
  if [[ "${REGISTER}" == "1" ]]; then
    unregister_provider
  fi
  echo "deleting mock LLM workload from ${NAMESPACE}"
  kubectl delete -f "${RENDERED}" --ignore-not-found=true
  echo "done. Note: actors that already merged ${PROVIDER_ID} into /data/models.json keep that entry."
  exit 0
fi

command -v ko >/dev/null 2>&1 || { echo "error: ko is required (hack/tools/ko or go install)" >&2; exit 1; }
command -v kubectl >/dev/null 2>&1 || { echo "error: kubectl is required" >&2; exit 1; }

echo "building and applying mock LLM (image ${KO_DOCKER_REPO}/.../mfpi-loadgen)"
ko apply -f "${RENDERED}"
kubectl rollout status deployment/mfpi-mock-llm -n "${NAMESPACE}" --timeout=180s

if [[ "${REGISTER}" == "1" ]]; then
  echo "registering provider ${PROVIDER_ID} -> ${BASE_URL}"
  payload=$(cat <<EOF
{"id":"${PROVIDER_ID}","name":"Mock LLM (load test)","baseUrl":"${BASE_URL}","models":[{"id":"${MODEL_ID}","name":"Mock 1","contextWindow":32768,"maxTokens":4096,"input":["text"]}]}
EOF
)
  code=$(curl -sS -o /tmp/mfpi-mock-llm-register.out -w '%{http_code}' -m 20 \
    -u "${ADMIN_USER}:${ADMIN_PASSWORD}" -X POST \
    -H 'Content-Type: application/json' -d "${payload}" \
    "${ENTRY}/usermanagement/api/providers" 2>/dev/null || true)
  if [[ "${code}" != "200" ]]; then
    echo "error: provider registration failed (HTTP ${code})" >&2
    cat /tmp/mfpi-mock-llm-register.out >&2 || true
    echo >&2
    echo "hint: is the gateway up? start it with ./run-nginx.sh (prod) or ./run-nginx-test.sh (test)" >&2
    exit 1
  fi
  rm -f /tmp/mfpi-mock-llm-register.out
  echo "registered. Actors merge models.json every ~60s."
fi

cat <<EOF

mock LLM ready in namespace ${NAMESPACE}:
  service:  http://mfpi-mock-llm.${NAMESPACE}.svc.cluster.local:8080
  provider: ${PROVIDER_ID}
  model:    ${MODEL_ID}

Run an agent-turn scenario (wait ~60s first so actors pick up models.json):

  GOCACHE=/tmp/mfpi-gocache go build -o /tmp/mfpi-loadgen ./tools/mfpi-loadgen
  /tmp/mfpi-loadgen run -base-url ${ENTRY} -creds <creds.json> \\
    -scenario agent-turn -vus 4 -duration 5m \\
    -provider ${PROVIDER_ID} -model ${MODEL_ID}

Remove it with: $0 --${ENVIRONMENT} --delete
EOF
