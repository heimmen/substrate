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
#   MFPI_WORKER_REPLICAS_BASE   (default 0; legacy base pool)
#   MFPI_WORKER_REPLICAS_SMALL  (default 48)
#   MFPI_WORKER_REPLICAS_MID    (default 1)
#   MFPI_WORKER_REPLICAS_LARGE  (default 1)
#   KO_DEFAULTBASEIMAGE   (default localhost:5001/distroless-static-debian13)
#   MFPI_SKIP_REFRESH     (set 1 to skip refreshing existing users after an
#                          ActorTemplate spec change; default is to refresh)
#   MFPI_DEPLOY_REV       (default 0) rendered into PI_WEB_DEPLOY_REV; bump it
#                          to force a real ActorTemplate change/roll-out (used
#                          by test-image-update-persistence.sh)
#   MFPI_KEEP_GOLDEN      (set 1 to keep the controller's golden base snapshot;
#                          not recommended: it is not restorable against the
#                          per-user volumes — docs/persistence_gaps.md)
#   MFPI_ALLOW_MULTI_NODE (set 1 to allow a cluster with more than one node;
#                          sticky userdata volumes are node-local)
#   MFPI_SKIP_NGINX      (set 1 to skip rebuilding/recreating the mfpi-nginx
#                          gateway on 58681 after the cluster deploy)
#
# The pi-web and pause workload images must be pushed (by digest) to
# ${KO_DOCKER_REPO}; the script resolves their digests before applying.
#
# Note: deploy.sh ONLY deploys the cluster-side resources (ActorTemplates +
# mfpi-admin). The mfpi-nginx reverse proxy (listening on 58681) is a plain
# host Docker container with --network host, managed by build-image.sh /
# run-nginx.sh — NOT a cluster resource. To keep the gateway in sync with
# nginx.conf, cmd_deploy calls refresh_nginx_gateway() at the end (set
# MFPI_SKIP_NGINX=1 to opt out).

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
# Base (legacy) tier: unused by the demo, so it defaults to 0. The old
# MFPI_WORKER_REPLICAS name is still honoured as an alias.
: "${MFPI_WORKER_REPLICAS_BASE:=${MFPI_WORKER_REPLICAS:-0}}"
# The small tier is the default tier for mf-pi users and is sized separately
# (more workers, smaller requests) so it can pack many concurrent users onto
# one node. See the small WorkerPool in mf-pi.yaml.tmpl.
: "${MFPI_WORKER_REPLICAS_SMALL:=48}"
# mid/large reserve a whole core (or two) per worker, so they default to a
# single seat each: both tiers share the node with the small pool, and the old
# shared MFPI_WORKER_REPLICAS default of 16 would have asked for 48 cores.
: "${MFPI_WORKER_REPLICAS_MID:=1}"
: "${MFPI_WORKER_REPLICAS_LARGE:=1}"
# Idle auto-suspend knobs. Unset keeps the defaults; MFPI_IDLE_TIERS=off
# disables the sweep. An *empty* value is normalised to "off" on purpose: the
# container cannot tell an empty env var from an unset one, so an empty value
# would silently fall back to the default tiers instead of pausing.
MFPI_IDLE_TIMEOUT="${MFPI_IDLE_TIMEOUT-90m}"
MFPI_IDLE_TIERS="${MFPI_IDLE_TIERS-small,mid}"
if [ -z "${MFPI_IDLE_TIERS}" ]; then MFPI_IDLE_TIERS=off; fi
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
      -e "s|\${MFPI_WORKER_REPLICAS_SMALL}|$(esc_repl "${MFPI_WORKER_REPLICAS_SMALL:-48}")|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_MID}|$(esc_repl "${MFPI_WORKER_REPLICAS_MID:-1}")|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_LARGE}|$(esc_repl "${MFPI_WORKER_REPLICAS_LARGE:-1}")|g" \

      -e "s|\${MFPI_IDLE_TIMEOUT}|$(esc_repl "${MFPI_IDLE_TIMEOUT}")|g" \

      -e "s|\${MFPI_IDLE_TIERS}|$(esc_repl "${MFPI_IDLE_TIERS}")|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_BASE}|$(esc_repl "${MFPI_WORKER_REPLICAS_BASE:-0}")|g" \
      -e "s|\${MFPI_DEPLOY_REV}|$(esc_repl "${MFPI_DEPLOY_REV:-0}")|g" \
      "${TEMPLATE}"
}

# Build the mf-pi workload image from the miaofang-agent SOURCE so every deploy
# picks up the latest repo changes (build-image.sh --from-source packs the repo's
# own src/ instead of the released npm package). Set MFPI_SKIP_WORKLOAD_BUILD=1
# to reuse an already-pushed image (offline/CI), and MFPI_WORKLOAD_SOURCE to point
# at a different checkout. The result is tagged mf-agent:latest and pushed so
# resolve_images below can pin its digest.
build_workload_image() {
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

resolve_images() {
  local repo="${KO_DOCKER_REPO}"
  local piweb pause
  build_workload_image || return 1
  piweb="$(docker inspect "${repo}/mf-agent:latest" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  pause="$(docker inspect "${repo}/pause:3.10.2" --format='{{index .RepoDigests 0}}' 2>/dev/null || true)"
  if [[ -z "${piweb}" || -z "${pause}" ]]; then
    echo "mf-agent or pause image not present in ${repo} after build; push them first:" >&2
    echo "  cd /home/liuchong/git/miaofang-agent && PI_WEB_IMAGE=mf-agent:latest docker/scripts/build-image.sh --from-source" >&2
    echo "  docker tag mf-agent:latest ${repo}/mf-agent:latest && docker push ${repo}/mf-agent:latest" >&2
    echo "  docker tag rancher/mirrored-pause:3.10.2 ${repo}/pause:3.10.2 && docker push ${repo}/pause:3.10.2" >&2
    return 1
  fi
  MF_PI_DIGEST="${piweb##*@}"
  PAUSE_DIGEST="${pause##*@}"
}

# check_cluster_nodes refuses to deploy onto a multi-node cluster by default.
# mf-pi user data lives on node-local sticky volumes
# (/var/lib/ateom-gvisor/stickyvolumes, no CSI/PV, no volume affinity), so an
# actor resumed on another node silently gets an empty /data/pi-agent (and its
# snapshot restore fails). See docs/persistence_gaps.md.
check_cluster_nodes() {
  if [[ "${MFPI_ALLOW_MULTI_NODE:-0}" == "1" ]]; then
    return 0
  fi
  local count
  count="$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  if [[ -n "${count}" && "${count}" -gt 1 ]]; then
    echo "ERROR: this cluster has ${count} nodes, but mf-pi user data lives on" >&2
    echo "  node-local sticky volumes (no CSI, no volume affinity). An actor" >&2
    echo "  resumed on another node loses its /data/pi-agent contents." >&2
    echo "  See docs/persistence_gaps.md. Refusing to deploy." >&2
    echo "  Set MFPI_ALLOW_MULTI_NODE=1 to override (accepting the risk)." >&2
    return 1
  fi
  return 0
}

# ensure_no_golden clears the controller-generated golden base snapshot on the
# mf-pi ActorTemplates. The golden snapshot records the mount tree of the
# GOLDEN actor's own external volume and is therefore not restorable against a
# per-user volume (runsc: `failed to walk "skills" ... no such file`), so the
# demo boots actors from scratch instead. Idempotent; see
# ./ensure-no-golden.sh and docs/persistence_gaps.md.
ensure_no_golden() {
  if [[ "${MFPI_KEEP_GOLDEN:-0}" == "1" ]]; then
    echo "  MFPI_KEEP_GOLDEN=1; leaving the golden base snapshot untouched."
    return 0
  fi
  if ! MFPI_NAMESPACE="${NAMESPACE}" ./ensure-no-golden.sh; then
    echo "WARNING: could not clear every golden base snapshot (see above)." >&2
    echo "  An actor may wedge in STATUS_RESUMING on restore; run" >&2
    echo "  ./recover-actors.sh -y to recover it." >&2
  fi
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
    echo "  This regenerates the golden base snapshot (cleared again below: it is"
    echo "  not restorable against per-user volumes); existing actors and their"
    echo "  own snapshots are unaffected."
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
    echo "    kubectl -n ${NAMESPACE} scale workerpool mf-pi-workerpool --replicas=${MFPI_WORKER_REPLICAS_BASE:-0}" >&2
  fi
}

# refresh_nginx_gateway rebuilds the mfpi-nginx image and recreates the
# 58681 gateway container so a running proxy picks up the latest nginx.conf.
#
# Unlike the cluster resources above, mfpi-nginx is a plain host Docker
# container (--network host) managed by build-image.sh / run-nginx.sh — it is
# NOT part of the manifest applied by ko, so a redeploy would otherwise leave
# a stale proxy running. This function rebuilds the image from nginx.conf and
# recreates the container in place, preserving an existing htpasswd bind mount
# and leaving the port-forward tunnels (58680/58682) untouched. It is skipped
# entirely with MFPI_SKIP_NGINX=1 (e.g. CI where nginx isn't managed here).
refresh_nginx_gateway() {
  if [[ "${MFPI_SKIP_NGINX:-0}" == "1" ]]; then
    echo "MFPI_SKIP_NGINX=1; skipping the mfpi-nginx gateway refresh."
    return 0
  fi
  if ! command -v docker >/dev/null 2>&1; then
    echo "WARNING: docker not found; skipping mfpi-nginx gateway refresh." >&2
    echo "  The 58681 proxy still runs its old config; rebuild/recreate it" >&2
    echo "  separately: ./build-image.sh && docker rm -f mfpi-nginx && ./run-nginx.sh" >&2
    return 0
  fi

  echo "Refreshing mfpi-nginx gateway (rebuild image + recreate 58681 container)..."
  if ! ./build-image.sh; then
    echo "WARNING: mfpi-nginx image build failed; leaving the running proxy untouched." >&2
    return 0
  fi

  # Recreate the container with the same runtime shape as run-nginx.sh, but
  # preserve an existing htpasswd bind mount (so the admin password set by the
  # last run-nginx.sh is not reset) and never touch the healthy port-forwards.
  local bind_specs=() host_network=
  # Use --type container: a bare `docker inspect mfpi-nginx` also matches the
  # IMAGE named mfpi-nginx, so it returns 0 (success) even when the container
  # is absent, which would wrongly take the "reuse existing" branch and drop
  # the bind/network flags.
  if docker inspect --type container mfpi-nginx >/dev/null 2>&1; then
    # Reuse the running container's binds (e.g. /tmp/mfpi-admin.htpasswd ->
    # /etc/nginx/mfpi-admin.htpasswd:ro) and its host-network mode.
    mapfile -t bind_specs < <(docker inspect --type container mfpi-nginx \
      --format '{{range .HostConfig.Binds}}{{.}}{{"\n"}}{{end}}' 2>/dev/null)
    host_network="$(docker inspect --type container mfpi-nginx --format '{{.HostConfig.NetworkMode}}' 2>/dev/null || true)"
    docker rm -f mfpi-nginx >/dev/null 2>&1 || true
  else
    # No existing container: run with the same default credentials run-nginx.sh
    # would use (admin / mf@pass2026), generated into a stable path.
    local htpasswd="${TMPDIR:-/tmp}/mfpi-admin.htpasswd"
    if [[ ! -f "${htpasswd}" ]]; then
      printf '%s:%s\n' "${ADMIN_USER:-admin}" \
        "$(openssl passwd -apr1 "${ADMIN_PASSWORD:-mf@pass2026}")" > "${htpasswd}"
      chmod 644 "${htpasswd}"
    fi
    bind_specs=("${htpasswd}:/etc/nginx/mfpi-admin.htpasswd:ro")
    host_network="host"
  fi

  local args=(-d --name mfpi-nginx)
  # Each bind spec needs its own -v flag.
  local spec
  for spec in "${bind_specs[@]}"; do
    [[ -n "${spec}" ]] && args+=("-v" "${spec}")
  done
  if [[ "${host_network}" == "host" ]]; then
    # --network host ignores -p; keep -p only for the non-host fallback.
    args+=("--network" "host")
  else
    args+=("-p" "58681:58681")
  fi
  args+=("mfpi-nginx:latest")

  docker run "${args[@]}"
  echo "mfpi-nginx gateway refreshed:"
  echo "  proxy:      http://localhost:58681/<username>"
  echo "  management: http://localhost:58681/usermanagement/"
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

  if ! check_cluster_nodes; then
    exit 1
  fi

  if ! resolve_images; then
    exit 1
  fi
  echo "  workload image digest: ${MF_PI_DIGEST}"
  echo "  pause image digest:    ${PAUSE_DIGEST}"
  echo "  workers (base/small/mid/large): ${MFPI_WORKER_REPLICAS_BASE:-0}/${MFPI_WORKER_REPLICAS_SMALL:-48}/${MFPI_WORKER_REPLICAS_MID:-1}/${MFPI_WORKER_REPLICAS_LARGE:-1}"

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

  # Clear the freshly regenerated golden base snapshot before touching users:
  # it is not restorable against a per-user volume, so a refreshed actor must
  # boot from scratch instead (docs/persistence_gaps.md).
  ensure_no_golden

  echo "Waiting for mfpi-admin to be ready..."
  kubectl rollout status deployment/mfpi-admin -n "${NAMESPACE}" --timeout=120s

  refresh_existing_users

  # Second pass: on a slow cluster the controller may only reach phase=Ready
  # (and publish the golden snapshot) after the first pass timed out. This is a
  # cheap no-op when every template's goldenSnapshot is already empty.
  ensure_no_golden

  # Rebuild the mfpi-nginx image and recreate the 58681 gateway from the
  # current nginx.conf (the proxy is a host container, not a cluster resource,
  # so it is not refreshed by the ko apply above). Set MFPI_SKIP_NGINX=1 to skip.
  refresh_nginx_gateway

  echo "mf-pi demo deployed."
  echo "  management UI: http://localhost:58681/usermanagement/ (see run-nginx.sh for credentials)"
  echo "  gateway refreshed from nginx.conf on 58681 (unless MFPI_SKIP_NGINX=1)"
}

usage() {
  echo "Usage: $0 [deploy]" >&2
  exit 2
}

case "${1:-}" in
  ""|deploy) cmd_deploy ;;
  *)         usage ;;
esac
