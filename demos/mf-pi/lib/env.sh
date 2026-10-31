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

# Environment loader shared by every mf-pi script.
#
# Three fully isolated environments exist; the only source of truth for their
# names, ports and cookies is demos/mf-pi/env/<name>.env.
#
#   qa    58681  ate-demo-mf-pi      / mfpi        (default, historical 58681)
#   prod  57681  ate-demo-mf-pi-prod / mfpi-prod  (explicit: ./deploy.sh prod)
#   test  59681  ate-demo-mf-pi-test / mfpi-test
#
# Usage:
#   source "$(dirname "$0")/lib/env.sh"
#   mfpi_load_env "${1:-prod}"     # or rely on MFPI_ENV
#
# After loading, all MFPI_* variables are exported to child processes.

if [[ -n "${__MFPI_ENV_SH_LOADED:-}" ]]; then
  return 0
fi
__MFPI_ENV_SH_LOADED=1

_MFPI_ENV_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# All environments, in display order.
mfpi_envs() {
  printf '%s\n' prod qa test
}

mfpi_env_exists() {
  [[ -f "${_MFPI_ENV_ROOT}/env/${1}.env" ]]
}

# mfpi_load_env <name> [default]
#
# <name> may be empty, in which case $MFPI_ENV (or the optional default, or
# "qa") is used. qa is the default so a bare ./deploy.sh keeps deploying the
# historical 58681 environment; production must be selected explicitly. The resolved name is validated against env/*.env so a typo
# fails loudly instead of silently deploying the wrong environment.
mfpi_load_env() {
  local name="${1:-}" fallback="${2:-}"
  if [[ -z "${name}" ]]; then
    name="${MFPI_ENV:-${fallback}}"
  fi
  if [[ -z "${name}" ]]; then
    name="qa"
  fi
  if ! mfpi_env_exists "${name}"; then
    echo "error: unknown mf-pi environment '${name}' (valid: $(mfpi_envs | tr '\n' ' '))" >&2
    return 2
  fi

  local file="${_MFPI_ENV_ROOT}/env/${name}.env"
  # env files contain only KEY=value assignments; export them all so child
  # processes (ko, kubectl, nested scripts) inherit the same environment.
  set -a
  # shellcheck disable=SC1090
  source "${file}"
  set +a

  export MFPI_ENV="${MFPI_ENV_NAME}"

  # Derived endpoints used by the helper scripts. Explicit values win.
  export MFPI_PROXY="${MFPI_PROXY:-localhost:${MFPI_ENTRY_PORT}}"
  export MFPI_ADMIN="${MFPI_ADMIN:-127.0.0.1:${MFPI_ADMIN_PORT}}"
  export MFPI_TEMPLATE="${MFPI_TEMPLATE:-${MFPI_NAMESPACE}/mf-pi}"

  local missing=()
  local v
  for v in MFPI_ENV_NAME MFPI_NAMESPACE MFPI_ATESPACE MFPI_WORKLOAD_LABEL \
           MFPI_ENTRY_PORT MFPI_ROUTER_PORT MFPI_ADMIN_PORT MFPI_CICD_PORT \
           MFPI_NGINX_CONTAINER MFPI_NGINX_CONF MFPI_USER_COOKIE MFPI_AUTH_COOKIE; do
    if [[ -z "${!v:-}" ]]; then
      missing+=("${v}")
    fi
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    echo "error: env/${name}.env is missing: ${missing[*]}" >&2
    return 2
  fi
  return 0
}

# mfpi_env_summary prints a one-line identity useful for deploy/run logs.
mfpi_env_summary() {
  printf '%s (%s, namespace=%s, atespace=%s, entry=%s)\n' \
    "${MFPI_ENV_NAME}" "${MFPI_ENV_NAME}" "${MFPI_NAMESPACE}" "${MFPI_ATESPACE}" "${MFPI_ENTRY_PORT}"
}
