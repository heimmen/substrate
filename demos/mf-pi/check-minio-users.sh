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
# Inspect the mf-pi per-user MinIO object store: every user bucket, the stored
# profile.tar.gz object, and what that archive actually holds (the actor tag,
# the DeepSeek key, and the skill/session/settings content). Cross-references
# the live actors so a user who has never synced, or a bucket retained from a
# deleted user (by design), shows up clearly.
#
# Runs mc inside the mfpi-minio pod (it bundles /usr/bin/mc); no port-forward
# or external S3 client needed.
#
# Usage:
#   ./check-minio-users.sh [--test] [--keys] [user ...]
#     --test   check the test environment (ate-demo-mf-pi-test / mfpi-test)
#     --keys   print stored DeepSeek keys in full (default: masked)
#     user     only inspect these buckets (default: all)

set -euo pipefail
cd "$(dirname "$0")"

NS="ate-demo-mf-pi"
ATESPACE="mfpi"
SHOW_KEYS=0
FILTER=()

usage() {
  echo "Usage: $0 [--test] [--keys] [user ...]" >&2
  exit "${1:-2}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -test|--test) NS="ate-demo-mf-pi-test"; ATESPACE="mfpi-test"; shift ;;
    --keys) SHOW_KEYS=1; shift ;;
    -h|--help) usage 0 ;;
    -*) echo "unknown option: $1" >&2; usage 2 ;;
    *) FILTER+=("$1"); shift ;;
  esac
done

# Root credentials for the demo's mfpi-minio (single central admin credential).
ROOT_USER="$(kubectl get secret mfpi-minio-admin -n "${NS}" -o jsonpath='{.data.root-user}' 2>/dev/null | base64 -d || true)"
ROOT_PASS="$(kubectl get secret mfpi-minio-admin -n "${NS}" -o jsonpath='{.data.root-password}' 2>/dev/null | base64 -d || true)"
if [[ -z "${ROOT_USER}" || -z "${ROOT_PASS}" ]]; then
  echo "error: no mfpi-minio-admin secret in namespace ${NS}; is the demo deployed there?" >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# minio_sh runs a shell script inside the minio pod with the root creds as
# $1/$2; any extra args are forwarded as $3, $4, ... inside the remote shell.
minio_sh() {
  local script="$1"
  shift
  kubectl exec -n "${NS}" deploy/mfpi-minio -- sh -c "${script}" _ "${ROOT_USER}" "${ROOT_PASS}" "$@"
}

# Inventory: recursive object listing plus the top-level bucket names (the
# latter catches empty buckets, which exist as soon as a user is created).
minio_sh 'mc alias set m http://127.0.0.1:9000 "$1" "$2" >/dev/null 2>&1 && mc ls --recursive --json m' > "${TMP}/objects.json"
minio_sh 'mc alias set m http://127.0.0.1:9000 "$1" "$2" >/dev/null 2>&1 && mc ls m' > "${TMP}/buckets.txt"

# Live actor names in this environment's atespace (best-effort cross-reference).
LIVE=()
if command -v jq >/dev/null 2>&1; then
  while IFS= read -r a; do
    [[ -n "${a}" ]] && LIVE+=("${a}")
  done < <(kubectl ate get actors -a "${ATESPACE}" -o json 2>/dev/null | jq -r '.actors[].metadata.name' 2>/dev/null || true)
fi
is_live() {
  local b="$1" a
  for a in "${LIVE[@]}"; do
    [[ "${a}" == "${b}" ]] && return 0
  done
  return 1
}

# Build a per-bucket table: bucket, profile size, profile mtime, extras count.
# "Extras" are any objects that are not profile.tar.gz (should never happen).
python3 - "${TMP}/objects.json" "${TMP}/buckets.txt" "${TMP}/buckets.tsv" <<'PYEOF'
import json, os, sys
objpath, dirpath, outpath = sys.argv[1], sys.argv[2], sys.argv[3]
profiles, extras = {}, {}
for line in open(objpath):
    line = line.strip()
    if not line:
        continue
    d = json.loads(line)
    key = d.get("key", "")
    bucket, _, name = key.partition("/")
    if not name:
        continue
    rec = {"size": d.get("size", 0), "mtime": d.get("lastModified", ""), "etag": d.get("etag", "")}
    if name == "profile.tar.gz":
        profiles[bucket] = rec
    else:
        extras.setdefault(bucket, []).append(name)
buckets = set(profiles)
for line in open(dirpath):
    name = line.rstrip("\n").rsplit(None, 1)[-1]
    if name.endswith("/"):
        buckets.add(name[:-1])
with open(outpath, "w") as out:
    for b in sorted(buckets):
        p = profiles.get(b)
        size = p["size"] if p else ""
        mtime = p["mtime"] if p else ""
        extra = len(extras.get(b, []))
        out.write("%s\t%s\t%s\t%d\n" % (b, size, mtime, extra))
PYEOF

mask_key() {
  local k="$1"
  if [[ -z "${k}" ]]; then
    echo "unset"
  elif [[ ${SHOW_KEYS} -eq 1 ]]; then
    echo "${k}"
  else
    echo "${k:0:6}…${k: -4}"
  fi
}

fetch_profile() { # bucket -> tar.gz path (or empty)
  local b="$1"
  minio_sh 'mc alias set m http://127.0.0.1:9000 "$1" "$2" >/dev/null 2>&1 && mc cat "m/$3/profile.tar.gz"' "${b}" > "${TMP}/${b}.tgz"
  echo "${TMP}/${b}.tgz"
}

human_size() {
  local n="$1"
  python3 - "$n" <<'PYEOF'
import sys
n = int(sys.argv[1])
for unit in ("B", "KB", "MB", "GB"):
    if n < 1024 or unit == "GB":
        print("%.0f %s" % (n, unit) if unit == "B" else "%.1f %s" % (n, unit))
        break
    n /= 1024.0
PYEOF
}

echo "mf-pi per-user MinIO store: namespace ${NS}, atespace ${ATESPACE}"
echo ""
live_missing=()
seen=()
while IFS=$'\t' read -r bucket size mtime extra; do
  [[ -n "${bucket}" ]] || continue
  # Honor the user filter.
  if ((${#FILTER[@]} > 0)); then
    ok=0
    for f in "${FILTER[@]}"; do
      [[ "${f}" == "${bucket}" ]] && ok=1
    done
    ((ok)) || continue
  fi
  seen+=("${bucket}")
  live=-
  is_live "${bucket}" && live=live
  if [[ -n "${size}" ]]; then
    printf '%-22s %-5s profile.tar.gz  %10s  synced %s' "${bucket}" "${live}" "$(human_size "${size}")" "${mtime}"
    ((extra > 0)) && printf '  (+%d stray object(s))' "${extra}"
    echo ""
    tgz="$(fetch_profile "${bucket}")"
    if [[ ! -s "${tgz}" ]]; then
      printf '      warning: profile.tar.gz could not be pulled from the pod\n'
      continue
    fi
    tag="$(tar xzOf "${tgz}" ./.mfpi-profile-actor 2>/dev/null || true)"
    ak="$(tar xzOf "${tgz}" ./auth.json 2>/dev/null | python3 -c 'import sys, json
try:
    print((json.load(sys.stdin).get("deepseek") or {}).get("key", ""))
except Exception:
    print("")' || true)"
    skills="$(tar tzf "${tgz}" 2>/dev/null | grep '^\./skills/' | grep -cv '/$' || true)"
    sessions="$(tar tzf "${tgz}" 2>/dev/null | grep '^\./sessions/' | grep -cv '/$' || true)"
    pres() { tar tzf "${tgz}" 2>/dev/null | grep -q "^\./$1$" && echo "yes" || echo "no"; }
    printf '      tag .mfpi-profile-actor : %s\n' "${tag:-<absent>}"
    printf '      deepseek key           : %s (in auth.json)\n' "$(mask_key "${ak}")"
    printf '      content                : skills %s files | sessions %s files | settings.json %s | models-store.json %s\n' \
      "${skills}" "${sessions}" "$(pres settings.json)" "$(pres models-store.json)"
  else
    printf '%-22s %-5s bucket exists but NO stored profile (created but never synced yet)\n' "${bucket}" "${live}"
    if is_live "${bucket}"; then
      printf '      user is live; the first sync push has not happened or the actor was suspended before it ran\n'
    fi
  fi
done < "${TMP}/buckets.tsv"

# Sanity notes.
if ((${#FILTER[@]} == 0)); then
  echo ""
  if ((${#LIVE[@]} > 0)); then
    for a in "${LIVE[@]}"; do
      found=0
      for s in "${seen[@]}"; do
        [[ "${s}" == "${a}" ]] && found=1
      done
      ((found)) || live_missing+=("${a}")
    done
    if ((${#live_missing[@]} > 0)); then
      echo "live actors with NO bucket (never created / never synced): ${live_missing[*]}"
    fi
  fi
  for b in "${seen[@]}"; do
    is_live "${b}" || echo "bucket '${b}' has no live actor (retained after a delete, or the golden boot bucket)"
  done
fi
