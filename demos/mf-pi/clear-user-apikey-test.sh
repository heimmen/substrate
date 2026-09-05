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
# Clear the per-user DeepSeek API key in the TEST environment (namespace
# ate-demo-mf-pi-test, atespace mfpi-test). Thin wrapper around
# clear-user-apikey.sh, which honors the MFPI_NAMESPACE env var.

set -euo pipefail
export MFPI_NAMESPACE=ate-demo-mf-pi-test
exec "$(dirname "$0")/clear-user-apikey.sh" "$@"
