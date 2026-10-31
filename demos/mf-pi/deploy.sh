#!/usr/bin/env bash
# Deploy one mf-pi environment (pi-web actors + the mfpi-admin user-management
# UI) against an Agent Substrate cluster.
#
# Three isolated environments are supported; the environment decides the
# namespace, atespace, workload label and gateway ports (env/<name>.env):
#
#   qa    58681   ate-demo-mf-pi      / mfpi        (default)
#   prod  57681   ate-demo-mf-pi-prod / mfpi-prod
#   test  59681   ate-demo-mf-pi-test / mfpi-test
#
# Usage:
#   ./deploy.sh            # deploy qa (historical 58681 behaviour)
#   ./deploy.sh prod       # deploy production (new 57681)
#   ./deploy.sh test       # deploy test
#   ./deploy.sh prod deploy
#   MFPI_ENV=qa ./deploy.sh
#   ./deploy-qa.sh         # thin wrapper
#
# All configuration is optional and falls back to sensible offline/kind
# defaults. Override any of them by exporting the variable first:
#   KO_DOCKER_REPO        (default localhost:5001)
#   BUCKET_NAME           (default ate-snapshots)
#   DEEPSEEK_API_KEY      (default: read from the mf-pi-provider-config secret)
#   MFPI_WORKER_REPLICAS_BASE   (default 0; legacy base pool)
#   MFPI_WORKER_REPLICAS_SMALL  (default 24 for qa/prod, 8 for test)
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
#   MFPI_DRY_RUN         (set 1 to render the manifest and stop before applying;
#                          resolves images but does not touch the cluster)
#   MFPI_SKIP_NGINX      (set 1 to skip recreating this environment's gateway
#                          container and port-forwards after the deploy)
#   MFPI_SKIP_GIT_PULL   (set 1 to leave the miaofang-agent checkout untouched;
#                          default updates it to its upstream on every deploy,
#                          independent of MFPI_SKIP_WORKLOAD_BUILD)
#   MFPI_WORKLOAD_SOURCE (checkout to build from; default
#                          /home/liuchong/git/miaofang-agent)
#   MFPI_WORKLOAD_REMOTE / MFPI_WORKLOAD_BRANCH
#                        (what to pull; default: origin + the checkout's upstream,
#                          falling back to main)
#
# The pi-web and pause workload images must be pushed (by digest) to
# ${KO_DOCKER_REPO}; the script resolves their digests before applying.
#
# Note: this script ONLY deploys the cluster-side resources (ActorTemplates +
# mfpi-admin) into the selected environment. Its gateway is a plain host Docker
# container with --network host, managed by build-image.sh and the environment's
# run/stop scripts — NOT a cluster resource. cmd_deploy calls
# refresh_nginx_gateway() at the end (set MFPI_SKIP_NGINX=1 to opt out).

set -euo pipefail
cd "$(dirname "$0")"

# Environment selection: ./deploy.sh [prod|qa|test] [deploy]. Each environment
# owns its own namespace, atespace, workload label and gateway ports (see
# env/<name>.env); nothing is shared, so deploys cannot interfere.
ENV_ARG=""
case "${1:-}" in
  prod|qa|test) ENV_ARG="$1"; shift ;;
esac
# shellcheck source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"
mfpi_load_env "${ENV_ARG}"

NAMESPACE="${MFPI_NAMESPACE}"
TEMPLATE="mf-pi.yaml.tmpl"

# Any unexpected failure should be visible on the CI/CD page instead of a bare
# non-zero exit (the bash -e default prints nothing for a failing assignment).
trap 'rc=$?; echo "ERROR: deploy.sh [${MFPI_ENV_NAME:-?}] failed at line ${LINENO}: ${BASH_COMMAND} (exit ${rc})" >&2' ERR
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
  # A brand-new environment has no secret yet; do not let a failed lookup abort
  # the whole deploy (set -e + pipefail used to exit here with no message).
  DEEPSEEK_API_KEY="$(
    kubectl get secret mf-pi-provider-config -n "${NAMESPACE}" \
      -o jsonpath='{.data.DEEPSEEK_API_KEY}' 2>/dev/null | base64 -d 2>/dev/null || true
  )"
fi
# Base (legacy) tier: unused by the demo, so it defaults to 0. The old
# MFPI_WORKER_REPLICAS name is still honoured as an alias.
: "${MFPI_WORKER_REPLICAS_BASE:=${MFPI_WORKER_REPLICAS:-0}}"
# The small tier is the default tier for mf-pi users and is sized separately
# (more workers, smaller requests) so it can pack many concurrent users onto
# one node. See the small WorkerPool in mf-pi.yaml.tmpl.
: "${MFPI_WORKER_REPLICAS_SMALL:=24}"
# mid/large reserve a whole core (or two) per worker, so they default to a
# single seat each: both tiers share the node with the small pool, and the old
# shared MFPI_WORKER_REPLICAS default of 16 would have asked for 48 cores.
: "${MFPI_WORKER_REPLICAS_MID:=1}"
: "${MFPI_WORKER_REPLICAS_LARGE:=1}"
# Idle auto-suspend knobs. Unset keeps the defaults; MFPI_IDLE_TIERS=off
# disables the sweep. An *empty* value is normalised to "off" on purpose: the
# container cannot tell an empty env var from an unset one, so an empty value
# would silently fall back to the default tiers instead of pausing.
# How long the agent session cookie keeps a browser signed in (default 12h).
# 0 disables the cookie; the browser's own Basic-Auth cache then decides again.
MFPI_AUTH_COOKIE_TTL="${MFPI_AUTH_COOKIE_TTL-720m}"
MFPI_IDLE_TIMEOUT="${MFPI_IDLE_TIMEOUT-90m}"
# Default OFF: resume from an actor snapshot is currently broken against the
# per-actor external volume (see the snapshotsConfig note in the templates),
# so suspending an idle actor strands the user on the next visit.
MFPI_IDLE_TIERS="${MFPI_IDLE_TIERS-off}"
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
      -e "s|\${MFPI_WORKER_REPLICAS_SMALL}|$(esc_repl "${MFPI_WORKER_REPLICAS_SMALL:-24}")|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_MID}|$(esc_repl "${MFPI_WORKER_REPLICAS_MID:-1}")|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_LARGE}|$(esc_repl "${MFPI_WORKER_REPLICAS_LARGE:-1}")|g" \
      -e "s|\${MFPI_IDLE_TIMEOUT}|$(esc_repl "${MFPI_IDLE_TIMEOUT}")|g" \
      -e "s|\${MFPI_AUTH_COOKIE_TTL}|$(esc_repl "${MFPI_AUTH_COOKIE_TTL}")|g" \
      -e "s|\${MFPI_IDLE_TIERS}|$(esc_repl "${MFPI_IDLE_TIERS}")|g" \
      -e "s|\${MFPI_NAMESPACE}|$(esc_repl "${MFPI_NAMESPACE}")|g" \
      -e "s|\${MFPI_ATESPACE}|$(esc_repl "${MFPI_ATESPACE}")|g" \
      -e "s|\${MFPI_WORKLOAD_LABEL}|$(esc_repl "${MFPI_WORKLOAD_LABEL}")|g" \
      -e "s|\${MFPI_AUTH_COOKIE}|$(esc_repl "${MFPI_AUTH_COOKIE}")|g" \
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
# pull_workload_source updates the miaofang-agent checkout to its upstream before
# anything is built. It is a separate step on purpose: the pull used to live inside
# build_workload_image(), so MFPI_SKIP_WORKLOAD_BUILD=1 returned before reaching it
# and a deploy silently left the checkout stale — which looks exactly like "deploy
# does not pull latest".
#
# Knobs:
#   MFPI_SKIP_GIT_PULL=1     do not touch the checkout (offline / pinned build)
#   MFPI_WORKLOAD_SOURCE     checkout path (default /home/liuchong/git/miaofang-agent)
#   MFPI_WORKLOAD_REMOTE     remote to pull from (default origin)
#   MFPI_WORKLOAD_BRANCH     branch to pull (default: the checkout's upstream, else main)
pull_workload_source() {
  local src="${MFPI_WORKLOAD_SOURCE:-/home/liuchong/git/miaofang-agent}"
  if [[ "${MFPI_SKIP_GIT_PULL:-0}" == "1" ]]; then
    echo "MFPI_SKIP_GIT_PULL=1: not updating ${src}; using whatever is checked out."
    return 0
  fi
  if [[ ! -d "${src}" ]] || ! git -C "${src}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "note: ${src} is not a git checkout; using it as-is."
    return 0
  fi
  local remote="${MFPI_WORKLOAD_REMOTE:-origin}"
  local branch="${MFPI_WORKLOAD_BRANCH:-}"
  if [[ -z "${branch}" ]]; then
    branch="$(git -C "${src}" rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null || true)"
    branch="${branch##*/}"
    [[ -z "${branch}" ]] && branch="main"
  fi
  echo "updating ${src} to ${remote}/${branch} ..."
  if ! git -C "${src}" fetch --prune "${remote}" "${branch}"; then
    echo "ERROR: could not fetch ${remote}/${branch} for '${src}'; refusing to build a" >&2
    echo "  possibly-stale workload image. Fix the checkout, or set MFPI_SKIP_GIT_PULL=1" >&2
    echo "  to build the currently checked-out code anyway." >&2
    return 1
  fi
  if ! git -C "${src}" merge --ff-only "${remote}/${branch}"; then
    echo "ERROR: '${src}' cannot fast-forward to ${remote}/${branch} (local commits or" >&2
    echo "  diverged history). Merge/rebase it yourself, or set MFPI_SKIP_GIT_PULL=1 to" >&2
    echo "  build the currently checked-out code." >&2
    return 1
  fi
  echo "  ${src} now at $(git -C "${src}" rev-parse --short HEAD) $(git -C "${src}" log -1 --format='%s')"
}

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
  # The checkout is updated by pull_workload_source() (called from
  # resolve_images), so it happens even when this build is skipped.
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
  pull_workload_source || return 1
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
# actor resumed on another node silently gets an empty /data (and its
# snapshot restore fails). See docs/persistence_gaps.md.
check_cluster_nodes() {
  if [[ "${MFPI_ALLOW_MULTI_NODE:-0}" == "1" ]]; then
    return 0
  fi
  local count
  count="$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ' || true)"
  if [[ -n "${count}" && "${count}" -gt 1 ]]; then
    echo "ERROR: this cluster has ${count} nodes, but mf-pi user data lives on" >&2
    echo "  node-local sticky volumes (no CSI, no volume affinity). An actor" >&2
    echo "  resumed on another node loses its /data contents." >&2
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
  if ! actors_json="$(kubectl ate get actor -a "${MFPI_ATESPACE}" -o json 2>&1)"; then
    echo "WARNING: could not list actors in atespace '${MFPI_ATESPACE}': ${actors_json}" >&2
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

# refresh_nginx_gateway resets this environment's host gateway via the
# environment's stop/run scripts, bringing the port-forwards into a known-good
# state after a deploy.
#
# The gateway is a plain host Docker container (--network host), NOT part of
# the manifest applied by ko. The image is environment-agnostic: each run script
# bind-mounts env/<env>.env's nginx conf and htpasswd, so there is no image to
# rebuild when a conf changes — recreating the container is enough.
# Skipped with MFPI_SKIP_NGINX=1 (e.g. CI where the gateway isn't managed here).
refresh_nginx_gateway() {
  if [[ "${MFPI_SKIP_NGINX:-0}" == "1" ]]; then
    echo "MFPI_SKIP_NGINX=1; skipping the ${MFPI_NGINX_CONTAINER} gateway refresh."
    return 0
  fi
  if ! command -v docker >/dev/null 2>&1; then
    echo "WARNING: docker not found; skipping ${MFPI_NGINX_CONTAINER} gateway refresh." >&2
    echo "  set it up manually: ./${MFPI_STOP_NGINX_SCRIPT} && ./${MFPI_RUN_NGINX_SCRIPT}" >&2
    return 0
  fi

  echo "Resetting the ${MFPI_ENV_NAME} gateway (${MFPI_NGINX_CONTAINER}, ports ${MFPI_ROUTER_PORT}/${MFPI_ADMIN_PORT})..."
  ./"${MFPI_STOP_NGINX_SCRIPT}"
  ./"${MFPI_RUN_NGINX_SCRIPT}"
  echo "${MFPI_NGINX_CONTAINER} gateway refreshed:"
  echo "  proxy:      http://localhost:${MFPI_ENTRY_PORT}/<username>"
  echo "  management: http://localhost:${MFPI_ENTRY_PORT}/usermanagement/"
}

cmd_deploy() {
  echo "environment: $(mfpi_env_summary)"
  # If the target small pool is smaller than the number of existing actors, a
  # deploy will scale it down and some users may get "no free workers" when
  # they resume. Warn instead of surprising the operator.
  local existing_actors
  existing_actors="$(kubectl ate get actor -a "${MFPI_ATESPACE}" 2>/dev/null | awk 'NR>1 && NF>1' | wc -l | tr -d ' ' || true)"
  if [[ -n "${existing_actors}" && "${existing_actors}" -gt 0 &&
        "${MFPI_WORKER_REPLICAS_SMALL:-0}" -lt "${existing_actors}" ]]; then
    echo "WARNING: atespace ${MFPI_ATESPACE} has ${existing_actors} actors but" >&2
    echo "  MFPI_WORKER_REPLICAS_SMALL=${MFPI_WORKER_REPLICAS_SMALL} — after this deploy" >&2
    echo "  only that many can be active at once. Override with e.g." >&2
    echo "  MFPI_WORKER_REPLICAS_SMALL=${existing_actors} ./deploy.sh" >&2
  fi
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
  echo "  workers (base/small/mid/large): ${MFPI_WORKER_REPLICAS_BASE:-0}/${MFPI_WORKER_REPLICAS_SMALL:-24}/${MFPI_WORKER_REPLICAS_MID:-1}/${MFPI_WORKER_REPLICAS_LARGE:-1}"

  local manifest
  manifest="$(mktemp)"
  render > "${manifest}"
  if [[ "${MFPI_DRY_RUN:-0}" == "1" ]]; then
    echo "MFPI_DRY_RUN=1: rendered ${MFPI_ENV_NAME} manifest at ${manifest}; not applying."
    echo "  namespace=${NAMESPACE} atespace=${MFPI_ATESPACE} gateway=${MFPI_NGINX_CONTAINER}"
    return 0
  fi
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

  # Recreate this environment's gateway container and port-forwards (the proxy
  # is a host container, not a cluster resource, so it is not refreshed by the
  # ko apply above). Set MFPI_SKIP_NGINX=1 to skip.
  refresh_nginx_gateway

  echo "mf-pi ${MFPI_ENV_NAME} environment deployed."
  echo "  management UI: http://localhost:${MFPI_ENTRY_PORT}/usermanagement/ (see ${MFPI_RUN_NGINX_SCRIPT} for credentials)"
  echo "  gateway refreshed from ${MFPI_NGINX_CONF} on ${MFPI_ENTRY_PORT} (unless MFPI_SKIP_NGINX=1)"
}

usage() {
  echo "Usage: $0 [prod|qa|test] [deploy]" >&2
  echo "  MFPI_ENV also selects the environment (default: qa)." >&2
  echo "  Wrappers: ./deploy-prod.sh ./deploy-qa.sh ./deploy-test.sh" >&2
  exit 2
}

case "${1:-}" in
  ""|deploy) cmd_deploy ;;
  *)         usage ;;
esac
