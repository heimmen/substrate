#!/usr/bin/env bash
# Deploy the mf-pi demo (pi-web actors + the mfpi-admin user-management UI)
# against an Agent Substrate cluster.
#
# Usage:
#   ./deploy.sh            # deploy (default)
#   ./deploy.sh deploy     # deploy
#
# All configuration is optional and falls back to sensible offline/kind
# defaults. Override any of them by exporting the variable first:
#   KO_DOCKER_REPO        (default localhost:5001)
#   BUCKET_NAME           (default ate-snapshots)
#   DEEPSEEK_API_KEY      (default: read from the mf-pi-provider-config secret)
#   MFPI_WORKER_REPLICAS  (default 16)
#   KO_DEFAULTBASEIMAGE   (default localhost:5001/distroless-static-debian13)
#   MFPI_SKIP_REFRESH     (set 1 to skip refreshing existing users after an
#                          ActorTemplate spec change; default is to refresh)
#
# The pi-web and pause workload images must be pushed (by digest) to
# ${KO_DOCKER_REPO}; the script resolves their digests before applying.

set -euo pipefail
cd "$(dirname "$0")"

NAMESPACE="ate-demo-mf-pi"
TEMPLATE="mf-pi.yaml.tmpl"
# Names of the ActorTemplates whose spec changed during this deploy (see
# ensure_at_recreate_if_changed). Existing users on a changed template are
# refreshed after the apply (see refresh_existing_users).
CHANGED_ATS=()

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
: "${MFPI_WORKER_REPLICAS:=16}"
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
      -e "s|\${MFPI_WORKER_REPLICAS}|$(esc_repl "${MFPI_WORKER_REPLICAS:-16}")|g" \
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
    CHANGED_ATS+=("${at}")
    echo "  ActorTemplate '${at}' spec changed; replacing it (immutable spec)."
    echo "  This regenerates the golden base snapshot; existing actors and their snapshots are unaffected."
    kubectl delete actortemplate "${at}" -n "${ns}"
  fi
}

# refresh_existing_users rolls an ActorTemplate spec change out to live users.
#
# The ActorTemplate spec is immutable: when deploy.sh replaced a changed CR
# above, actors created from the OLD spec keep running unchanged. External
# volumes declared by the new spec are never provisioned for them (volume
# provisioning only happens on actor creation), so the next resume of such an
# actor fails with "volume <name> not found for actor <user>" and wedges it in
# STATUS_RESUMING. Refreshing each affected user (suspend -> delete ->
# recreate -> resume, via refresh-actor.sh) re-provisions volumes from the
# current spec; the sticky userdata volume keeps user data and passwords
# intact. Only users whose template changed are refreshed, and only when a
# template actually changed (a no-op deploy never interrupts users). Skipped
# entirely with MFPI_SKIP_REFRESH=1. Refresh failures are reported but do not
# fail the deploy.
refresh_existing_users() {
  if [[ "${MFPI_SKIP_REFRESH:-0}" == "1" ]]; then
    echo "  MFPI_SKIP_REFRESH=1; skipping existing-user refresh."
    return 0
  fi
  if (( ${#CHANGED_ATS[@]} == 0 )); then
    return 0   # no template changed: existing users are unaffected
  fi
  if ! command -v jq >/dev/null 2>&1; then
    echo "WARNING: jq is required to refresh existing users but was not found;" >&2
    echo "  users on changed template(s) [${CHANGED_ATS[*]}] were NOT refreshed." >&2
    echo "  Install jq and run: ./refresh-actor.sh -y <user>..." >&2
    return 0
  fi
  local actors_json users=() line u ns tpl at
  if ! actors_json="$(kubectl ate get actor -a "${MFPI_ATESPACE:-mfpi}" -o json 2>&1)"; then
    echo "WARNING: could not list actors in atespace '${MFPI_ATESPACE:-mfpi}': ${actors_json}" >&2
    echo "  users on changed template(s) [${CHANGED_ATS[*]}] were NOT refreshed." >&2
    return 0
  fi
  while IFS=$'\t' read -r u ns tpl; do
    [[ -z "${u}" ]] && continue
    for at in "${CHANGED_ATS[@]}"; do
      if [[ "${tpl}" == "${at}" && "${ns}" == "${NAMESPACE}" ]]; then
        users+=("${u}")
        break
      fi
    done
  done < <(printf '%s' "${actors_json}" |
             jq -r '.actors[] | [.metadata.name,
                                (.actorTemplateNamespace // ""),
                                (.actorTemplateName // "")] | @tsv')
  if (( ${#users[@]} == 0 )); then
    echo "  no existing users on changed template(s) [${CHANGED_ATS[*]}]; nothing to refresh."
    return 0
  fi
  echo "  refreshing existing users on changed template(s) [${CHANGED_ATS[*]}]:"
  echo "    ${users[*]}"
  echo "  (user data on the sticky userdata volume survives; set MFPI_SKIP_REFRESH=1 to skip)"
  # deploy.sh is non-interactive, so confirm on the caller's behalf (-y) and
  # skip the proxy probe (the port-forward usually is not up during deploy).
  if ! MFPI_SKIP_PROXY_CHECK=1 ./refresh-actor.sh -y "${users[@]}"; then
    echo "WARNING: some users failed to refresh (see [refresh-actor] output above)." >&2
    echo "  A user wedged in STATUS_RESUMING cannot be suspended or deleted via the" >&2
    echo "  CLI; recover it manually, then re-run ./refresh-actor.sh -y <user>:" >&2
    echo "    kubectl -n ${NAMESPACE} scale workerpool mf-pi-workerpool --replicas=0" >&2
    echo "    # wait for the actor to report STATUS_SUSPENDED, then:" >&2
    echo "    kubectl -n ${NAMESPACE} scale workerpool mf-pi-workerpool --replicas=${MFPI_WORKER_REPLICAS:-16}" >&2
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
  echo "  worker replicas:       ${MFPI_WORKER_REPLICAS:-16}"

  local manifest
  manifest="$(mktemp)"
  render > "${manifest}"
  # ActorTemplate specs are immutable, so delete any base/per-tier template
  # whose spec changed before applying (see ensure_at_recreate_if_changed).
  # Base mf-pi (legacy/default) plus one template per resource tier.
  for at in mf-pi mf-pi-small mf-pi-mid mf-pi-large; do
    ensure_at_recreate_if_changed "${manifest}" "${NAMESPACE}" "${at}"
  done
  ko apply -f "${manifest}"
  rm -f "${manifest}"

  echo "Waiting for mfpi-admin to be ready..."
  kubectl rollout status deployment/mfpi-admin -n "${NAMESPACE}" --timeout=120s

  refresh_existing_users

  echo "mf-pi demo deployed."
  echo "  management UI: run ./run-nginx.sh, then open http://localhost:58681/usermanagement/"
}

usage() {
  echo "Usage: $0 [deploy]" >&2
  exit 2
}

case "${1:-}" in
  ""|deploy) cmd_deploy ;;
  *)         usage ;;
esac
