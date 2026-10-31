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
# QA environment for the mf-pi demo (entry 58681): namespace ate-demo-mf-pi,
# atespace mfpi, workload label mf-pi. It deliberately keeps the historical
# resource names so existing users/volumes/snapshots do not need migrating.
# Defined in demos/mf-pi/env/qa.env.

ATE_DEMOS+=(demo-mf-pi-qa) # register demo-mf-pi-qa

demo-mf-pi-qa_cmdline() {
  case "${1}" in
    --deploy-demo-mf-pi-qa) mfpi_install_deploy qa ;;
    --delete-demo-mf-pi-qa) mfpi_install_delete qa ;;
    *)
      return 1
      ;;
  esac
  return 0
}

demo-mf-pi-qa_usage() {
  mfpi_install_usage qa
}
