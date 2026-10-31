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
#
# This is sourced as part of install-ate.sh. Do not run directly.
#
# TEST environment for the mf-pi demo (entry 59681): namespace
# ate-demo-mf-pi-test, atespace mfpi-test, workload label mf-pi-test.
# Defined in demos/mf-pi/env/test.env.

ATE_DEMOS+=(demo-mf-pi-test) # register demo-mf-pi-test

demo-mf-pi-test_cmdline() {
  case "${1}" in
    --deploy-demo-mf-pi-test) mfpi_install_deploy test ;;
    --delete-demo-mf-pi-test) mfpi_install_delete test ;;
    *)
      return 1
      ;;
  esac
  return 0
}

demo-mf-pi-test_usage() {
  mfpi_install_usage test
}
