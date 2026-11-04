#!/usr/bin/env bash
# ensure-no-golden.sh — keep the mf-pi ActorTemplates free of a golden base
# snapshot.
#
# WHY
#   The ActorTemplate controller creates a temporary "golden" actor, runs it
#   once and stores a Full snapshot (status.goldenSnapshot). That snapshot
#   records the 9p mount tree of the GOLDEN actor's own external volume. Every
#   mf-pi actor has its OWN per-user `userdata` volume, so restoring the
#   golden snapshot against it either fails outright (runsc: `failed to walk
#   "skills" ... no such file or directory`, actor stuck in STATUS_RESUMING)
#   or restores a mount tree that does not match the user's data. See
#   docs/persistence_gaps.md for the full analysis.
#
#   This script is the demo-side workaround: after the controller finishes
#   generating the snapshot (phase=Ready), clear status.goldenSnapshot. The
#   controller treats phase=Ready as a no-op, so it never regenerates it and
#   every resume boots from scratch (Run) instead of restoring the golden
#   snapshot (Restore). The cost is a cold boot on the first resume of an
#   actor (~25-40s, covered by the "Agent 正在载入" loading page); later
#   suspend/resume cycles still use the actor's own (healthy) snapshot.
#
#   deploy.sh / deploy-test.sh call this automatically after applying the
#   manifest. It is idempotent and safe to run repeatedly. Do NOT run it if
#   you deliberately want the platform's (broken for per-user volumes) golden
#   fast-start; deploy.sh supports MFPI_KEEP_GOLDEN=1 for that.
#
# Usage:
#   ./ensure-no-golden.sh            # production namespace (ate-demo-mf-pi)
#   ./ensure-no-golden.sh --test     # test namespace (ate-demo-mf-pi-test)
#   ./ensure-no-golden.sh --dry-run  # print what would be cleared
#
# Environment overrides:
#   MFPI_NAMESPACE        (default ate-demo-mf-pi / ate-demo-mf-pi-test)
#   MFPI_TEMPLATES        (default "mf-pi mf-pi-small mf-pi-mid mf-pi-large")
#   MFPI_GOLDEN_TIMEOUT   seconds to wait for phase=Ready (default 180)
#
# Exit status: 0 when every template ends with an empty goldenSnapshot;
# 1 when the namespace is missing or a status patch failed.

set -uo pipefail
cd "$(dirname "$0")"

TEST=0
DRY_RUN=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --test)    TEST=1 ;;
    --dry-run) DRY_RUN=1 ;;
    -h|--help) sed -n '2,39p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

if (( TEST )); then
  NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi-test}"
  ENV_LABEL="test"
else
  NAMESPACE="${MFPI_NAMESPACE:-ate-demo-mf-pi}"
  ENV_LABEL="production"
fi
TEMPLATES_RAW="${MFPI_TEMPLATES:-mf-pi mf-pi-small mf-pi-mid mf-pi-large}"
TIMEOUT="${MFPI_GOLDEN_TIMEOUT:-180}"

read -r -a TEMPLATES <<<"${TEMPLATES_RAW}"

FAIL=0
log() { printf '[ensure-no-golden] %s\n' "$*" >&2; }

if ! command -v kubectl >/dev/null 2>&1; then
  echo "kubectl not found on PATH" >&2
  exit 1
fi
if ! kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1; then
  echo "namespace '${NAMESPACE}' not found (deploy the demo first)" >&2
  exit 1
fi

golden_of() {
  kubectl get actortemplate "$1" -n "${NAMESPACE}" -o jsonpath='{.status.goldenSnapshot}' 2>/dev/null || true
}
phase_of() {
  kubectl get actortemplate "$1" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true
}

# Wait for the controller to finish its golden-snapshot pass, so the patch is
# not raced by a snapshot that appears a moment later. A template that is
# already Ready (golden already cleared, or never generated) returns at once;
# an empty phase means the controller has not reconciled the CR yet, so keep
# polling.
wait_ready() {
  local t="$1" i=0 phase
  while (( i < TIMEOUT )); do
    phase="$(phase_of "${t}")"
    if [[ "${phase}" == "Ready" ]]; then
      return 0
    fi
    sleep 2
    i=$((i+2))
  done
  return 1
}

clear_one() {
  local t="$1" golden phase
  golden="$(golden_of "${t}")"
  if [[ -z "${golden}" ]]; then
    phase="$(phase_of "${t}")"
    log "  ${t}: golden snapshot already empty (phase=${phase:-?}) — nothing to do"
    return 0
  fi
  if (( DRY_RUN )); then
    log "  [dry-run] would clear golden snapshot on ${t} (${golden})"
    return 0
  fi
  if ! kubectl patch actortemplate "${t}" -n "${NAMESPACE}" --subresource=status \
       --type=merge -p '{"status":{"goldenSnapshot":""}}' >/dev/null 2>&1; then
    log "  ${t}: FAILED to clear golden snapshot" >&2
    return 1
  fi
  golden="$(golden_of "${t}")"
  if [[ -n "${golden}" ]]; then
    log "  ${t}: golden snapshot still set after patch (${golden})" >&2
    return 1
  fi
  log "  ${t}: cleared golden snapshot -> resumes boot from scratch"
  return 0
}

log "namespace=${NAMESPACE} (${ENV_LABEL}) templates='${TEMPLATES_RAW}'"
for t in "${TEMPLATES[@]}"; do
  if ! kubectl get actortemplate "${t}" -n "${NAMESPACE}" >/dev/null 2>&1; then
    log "  ${t}: not found in ${NAMESPACE} — skipping"
    continue
  fi
  if ! wait_ready "${t}"; then
    log "  ${t}: phase did not reach Ready within ${TIMEOUT}s; clearing whatever is set"
  fi
  if ! clear_one "${t}"; then
    FAIL=$((FAIL+1))
  fi
done

if (( FAIL > 0 )); then
  log "RESULT: FAIL (${FAIL} template(s) could not be cleared)"
  exit 1
fi
log "RESULT: PASS (no golden base snapshot in ${NAMESPACE})"
