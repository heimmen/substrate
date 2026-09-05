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
# Validate that the mf-pi manifest templates render to valid YAML.
set -euo pipefail
cd "$(dirname "$0")"

DIGEST="sha256:364a73cf0fe44fea69b3de0fdcb2b439e7e7153e66724de35c9c9e06fcb8759f"

for t in mf-pi.yaml.tmpl mf-pi-test.yaml.tmpl; do
  sed -e "s|\${DEEPSEEK_API_KEY}|sk-test|g" \
      -e "s|\${BUCKET_NAME}|ate-snapshots|g" \
      -e "s|\${MF_PI_DIGEST}|${DIGEST}|g" \
      -e "s|\${PAUSE_DIGEST}|sha256:placeholder|g" \
      -e "s|\${MFPI_WORKER_REPLICAS}|2|g" \
      -e "s|\${MFPI_PROFILE_TOKEN}|test-profile-token|g" \
      -e "s|\${MINIO_DIGEST}|${DIGEST}|g" \
      -e "s|\${MINIO_ROOT_USER}|minioadmin|g" \
      -e "s|\${MINIO_ROOT_PASSWORD}|minioadmin|g" \
      "$t" > /tmp/mfpi-render-check.yaml
  python3 - /tmp/mfpi-render-check.yaml "$t" <<'PYEOF'
import sys, yaml
path, name = sys.argv[1], sys.argv[2]
with open(path) as f:
    docs = [d for d in yaml.safe_load_all(f) if d]
kinds = [d.get("kind") for d in docs]
assert kinds == ["Namespace", "Secret", "Role", "RoleBinding", "Secret",
                 "WorkerPool", "ActorTemplate", "ConfigMap", "ServiceAccount",
                 "Role", "RoleBinding", "Secret", "Role", "RoleBinding",
                 "Secret", "PersistentVolumeClaim", "Deployment", "Service",
                 "Deployment", "Service"], kinds
# Per-user MinIO profile sync objects (mfpi-minio + shared token Secret) exist.
def get(kind, name):
    return next(d for d in docs if d.get("kind") == kind and d.get("metadata", {}).get("name") == name)

assert get("Secret", "mfpi-profile-token"), "missing mfpi-profile-token Secret"
assert get("Secret", "mfpi-minio-admin"), "missing mfpi-minio-admin Secret"
assert get("PersistentVolumeClaim", "mfpi-minio-data"), "missing mfpi-minio-data PVC"
minio = get("Deployment", "mfpi-minio")
minio_img = minio["spec"]["template"]["spec"]["containers"][0]["image"]
assert minio_img.startswith("localhost:5001/minio@"), minio_img
minio_env = {e["name"]: e for e in minio["spec"]["template"]["spec"]["containers"][0]["env"]}
assert "MINIO_ROOT_USER" in minio_env and "MINIO_ROOT_PASSWORD" in minio_env, minio_env
# ActorTemplate: supervisor script + env for the sync endpoints.
at = next(d for d in docs if d["kind"] == "ActorTemplate")
c = at["spec"]["containers"][0]
assert c["image"].startswith("localhost:5001/pi-web@"), c["image"]
assert "command" not in c, "command must not be set (keeps image ENTRYPOINT)"
assert c["args"][:2] == ["sh", "-c"], c["args"][:2]
script = c["args"][2]
assert "pull_profile()" in script and "push_profile()" in script, "supervisor must pull/push"
assert "/internal/actor/$actor/profile" in script, "supervisor must call the broker endpoint"
env = {e["name"]: e for e in c["env"]}
assert env["PI_WEB_PORT"].get("value") == "80", env
assert env["HOSTEXEC_MODE"].get("value") == "disabled", env
assert env["MFPI_ADMIN_URL"]["value"].startswith("http://mfpi-admin."), env
assert env["MFPI_PROFILE_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == "mfpi-profile-token", env
# mfpi-admin Deployment: MinIO broker env wired.
admin = get("Deployment", "mfpi-admin")
admin_env = {e["name"]: e for e in admin["spec"]["template"]["spec"]["containers"][0]["env"]}
assert admin_env["MINIO_ENDPOINT"]["value"].startswith("http://mfpi-minio."), admin_env
assert admin_env["MFPI_PROFILE_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == "mfpi-profile-token", admin_env
# ate-api-server must be able to read the token Secret for ActorTemplate env.
env_role = next(d for d in docs if d["kind"] == "Role" and d["metadata"]["name"] == "ate-api-server-env-sources")
assert any("mfpi-profile-token" in (r.get("resourceNames") or []) for r in env_role["rules"]), env_role
print(name, "OK:", len(docs), "docs; MinIO profile sync wired")
PYEOF
  rm -f /tmp/mfpi-render-check.yaml
done
