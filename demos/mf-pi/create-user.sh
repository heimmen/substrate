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
# Idempotently create a user (one mf-pi actor). If the actor already exists,
# reuse it — its session history lives in the actor's snapshot (Full snapshot
# of process memory + filesystem) and is preserved. This satisfies
# "no instance -> create; instance exists -> reuse".

set -euo pipefail
cd "$(dirname "$0")"

# atespace name; change if the demo uses a different atespace.
ATESPACE="${MFPI_ATESPACE:-mfpi}"
TEMPLATE="ate-demo-mf-pi/mf-pi"

# Validate username against DNS-1123 (same rule as Substrate actor names).
DNS1123='^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'

usage() {
  echo "Usage: $0 <username>" >&2
  echo "  <username> must match DNS-1123: ${DNS1123}" >&2
  exit 2
}

if [[ $# -ne 1 ]]; then
  usage
fi
username="$1"

if ! [[ "$username" =~ $DNS1123 ]]; then
  echo "Invalid username '$username': must match ${DNS1123}" >&2
  exit 2
fi

# Ensure the atespace exists before creating actors in it.
if ! kubectl ate get atespace "$ATESPACE" >/dev/null 2>&1; then
  echo "Atespace '$ATESPACE' not found; creating it."
  kubectl ate create atespace "$ATESPACE"
fi

# Idempotent create-or-reuse.
if kubectl ate get actor "$username" -a "$ATESPACE" >/dev/null 2>&1; then
  echo "Actor '$username' already exists; reusing existing session (history preserved)."
else
  echo "Creating actor '$username' from template '$TEMPLATE'."
  kubectl ate create actor "$username" -a "$ATESPACE" --template "$TEMPLATE"
fi
