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
# Deploy the mf-pi TEST environment (pi-web actors + the mfpi-admin
# user-management UI) against an Agent Substrate cluster.
#
# This is the test-environment counterpart of deploy.sh: it deploys into the
# ate-demo-mf-pi-test namespace using the mfpi-test atespace, so it never
# conflicts with the production environment (ate-demo-mf-pi / mfpi).
#
# Usage:
#   ./deploy-test.sh         # deploy (default)
#   ./deploy-test.sh deploy  # deploy
#
# All configuration is optional and falls back to sensible offline/kind
# defaults. Override any of them by exporting the variable first:
#   KO_DOCKER_REPO        (default localhost:5001)
#   BUCKET_NAME           (default ate-snapshots)
#   DEEPSEEK_API_KEY      (default: read from the mf-pi-provider-config secret)
#   MFPI_WORKER_REPLICAS  (default 2)
#   KO_DEFAULTBASEIMAGE   (default localhost:5001/distroless-static-debian13)
#
# The pi-web and pause workload images must be pushed (by digest) to
# ${KO_DOCKER_REPO}; the script resolves their digests before applying.

set -euo pipefail
cd "$(dirname "$0")"

NAMESPACE="ate-demo-mf-pi-test"
TEMPLATE="mf-pi-test.yaml.tmpl"

# --- Defaults ---------------------------------------------------------------
# Registry the cluster can pull from (kind/k3s local registry). Exported so
# the `ko` child process can read it.
export KO_DOCKER_REPO="${KO_DOCKER_REPO:-localhost:5001}"
# Logical snapshot bucket used by rustfs on kind/k3s.
: "${BUCKET_NAME:=ate-snapshots}"
# Provider credential: prefer an already-exported value; otherwise fall back
# to reading the live secret created by a previous deploy.
if [[ -z "${DEEPSEEK_API_KEY:-}" ]]; then
  DEEPSEEK_API_KEY="$(
    kubectl get secret mf-pi-provider-config -n "${NAMESPACE}" \
      -o jsonpath='{.data.DEEPSEEK_API_KEY}' 2>/dev/null | base64 -d
  )"
fi
: "${MFPI_WORKER_REPLICAS:=2}"
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
      -e "s|\${DEEPSEEK_API_KEY}|$(esc_repl "${DEEPSEEK_API_KEY:-placeholder}")|g" \
      -e "s|\${MF_PI_DIGEST}|$(esc_repl "${MF_PI_DIGEST}")|g" \
      -e "s|\${PAUSE_DIGEST}|$(esc_repl "${PAUSE_DIGEST}")|g" \
      -e "s|\${MFPI_WORKER_REPLICAS}|$(esc_repl "${MFPI_WORKER_REPLICAS:-2}")|g" \
      "${TEMPLATE}"
}

resolve_images() {
  local repo="${KO_DOCKER_REPO}"
  local piweb pause
  piweb="$(docker inspect "${repo}/pi-web:latest" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  pause="$(docker inspect "${repo}/pause:3.10.2" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  if [[ -z "${piweb}" || -z "${pause}" ]]; then
    echo "pi-web or pause image not found in ${repo}; push them first:" >&2
    echo "  cd /home/liuchong/git/pi-web && PI_WEB_IMAGE=pi-web:latest docker/scripts/build-image.sh" >&2
    echo "  docker tag pi-web:latest ${repo}/pi-web:latest && docker push ${repo}/pi-web:latest" >&2
    echo "  docker tag rancher/mirrored-pause:3.10.2 ${repo}/pause:3.10.2 && docker push ${repo}/pause:3.10.2" >&2
    return 1
  fi
  MF_PI_DIGEST="${piweb##*@}"
  PAUSE_DIGEST="${pause##*@}"
}

# ensure_at_recreate_if_changed <rendered-manifest> <namespace> <at-name>
# The ActorTemplate spec is immutable (pkg/api/v1alpha1/actortemplate_types.go:
# `self == oldSelf`), so `kubectl apply` cannot change it in place. When the
# rendered spec no longer matches the live object, delete the live CR so the
# following `ko apply` recreates it. Deleting the CR has no finalizer and does
# not touch real actors or their snapshots; the reconciler simply regenerates
# the golden base snapshot from the new spec. The comparison ignores live-only
# fields (e.g. the server-injected `sandboxClass` default), so a template with
# no effective change never triggers a recreate.
ensure_at_recreate_if_changed() {
  local manifest="$1" ns="$2" at="$3"
  local live_json
  if ! live_json="$(kubectl get actortemplate "${at}" -n "${ns}" -o json 2>/dev/null)"; then
    return 0   # first deploy: no live ActorTemplate to replace
  fi
  local live_file change
  live_file="$(mktemp)"
  printf '%s' "${live_json}" > "${live_file}"
  change="$(python3 - "${manifest}" "${live_file}" "${at}" <<'PYEOF'
import sys, json, yaml
manifest_path, live_path, name = sys.argv[1], sys.argv[2], sys.argv[3]
docs = [d for d in yaml.safe_load_all(open(manifest_path)) if d]
at = next((d for d in docs if d.get("kind") == "ActorTemplate"
           and d.get("metadata", {}).get("name") == name), None)
rendered = at["spec"] if at else None
live = json.load(open(live_path)).get("spec") or {}
def mismatch(r, l):
    # True when a field the template sets is absent or differs in the live
    # object. Live-only fields are ignored so a no-op deploy stays a no-op.
    if isinstance(r, dict):
        return any(k not in l or mismatch(v, l[k]) for k, v in r.items())
    if isinstance(r, list):
        return (not isinstance(l, list) or len(r) != len(l)
                or any(mismatch(a, b) for a, b in zip(r, l)))
    return r != l
print("1" if rendered is not None and mismatch(rendered, live) else "0")
PYEOF
)"
  rm -f "${live_file}"
  if [[ "${change}" == "1" ]]; then
    echo "  ActorTemplate '${at}' spec changed; replacing it (immutable spec)."
    echo "  This regenerates the golden base snapshot; existing actors and their snapshots are unaffected."
    kubectl delete actortemplate "${at}" -n "${ns}"
  fi
}

cmd_deploy() {
  for v in DEEPSEEK_API_KEY BUCKET_NAME KO_DOCKER_REPO; do
    if [[ -z "${!v:-}" ]]; then
      echo "warning: $v is empty (no exported value and no mf-pi-provider-config secret found)" >&2
    fi
  done
  if [[ -z "${DEEPSEEK_API_KEY:-}" ]]; then
    echo "warning: deploying with an empty DEEPSEEK_API_KEY; set it (or create the secret) for the app to authenticate." >&2
  fi

  if ! command -v ko >/dev/null 2>&1; then
    echo "ko is required to deploy (resolves ko:// image references)" >&2
    exit 1
  fi

  if ! resolve_images; then
    exit 1
  fi
  echo "  workload image digest: ${MF_PI_DIGEST}"
  echo "  pause image digest:    ${PAUSE_DIGEST}"
  echo "  worker replicas:       ${MFPI_WORKER_REPLICAS:-2}"

  local manifest
  manifest="$(mktemp)"
  render > "${manifest}"
  # Delete the immutable ActorTemplate first when its spec has changed; the
  # apply below then recreates it (see ensure_at_recreate_if_changed).
  ensure_at_recreate_if_changed "${manifest}" "${NAMESPACE}" mf-pi
  ko apply -f "${manifest}"
  rm -f "${manifest}"

  echo "Waiting for mfpi-admin to be ready..."
  kubectl rollout status deployment/mfpi-admin -n "${NAMESPACE}" --timeout=120s

  echo "mf-pi test environment deployed."
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
