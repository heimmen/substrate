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
# Validate that the mf-pi manifest templates render to valid YAML and that the
# per-user MinIO bucket-volume wiring is present (see mount_minio_v2.md).
set -euo pipefail
cd "$(dirname "$0")"

DIGEST="sha256:364a73cf0fe44fea69b3de0fdcb2b439e7e7153e66724de35c9c9e06fcb8759f"

for t in mf-pi.yaml.tmpl mf-pi-test.yaml.tmpl; do
  sed -e "s|\${DEEPSEEK_API_KEY}|sk-test|g" \
      -e "s|\${BUCKET_NAME}|ate-snapshots|g" \
      -e "s|\${MF_PI_DIGEST}|${DIGEST}|g" \
      -e "s|\${PAUSE_DIGEST}|sha256:placeholder|g" \
      -e "s|\${MFPI_WORKER_REPLICAS}|2|g" \
      -e "s|\${MINIO_DIGEST}|${DIGEST}|g" \
      -e "s|\${MINIO_ROOT_USER}|minioadmin|g" \
      -e "s|\${MINIO_ROOT_PASSWORD}|minioadmin|g" \
      "$t" > /tmp/mfpi-render-check.yaml
  # The in-actor broker must be gone from the raw template too.
  if grep -q "MFPI_PROFILE_TOKEN\|mfpi-profile-token\|MFPI_ADMIN_URL\|/internal/actor/" "$t"; then
    echo "FAIL $t: in-image profile-sync broker remnants found (MFPI_PROFILE_TOKEN / MFPI_ADMIN_URL / /internal/actor/)" >&2
    exit 1
  fi
  python3 - /tmp/mfpi-render-check.yaml "$t" <<'PYEOF'
import sys, yaml
path, name = sys.argv[1], sys.argv[2]
with open(path) as f:
    docs = [d for d in yaml.safe_load_all(f) if d]
kinds = [d.get("kind") for d in docs]
assert kinds == ["Namespace", "Secret", "Role", "RoleBinding",
                 "WorkerPool", "ActorTemplate", "ConfigMap", "ServiceAccount",
                 "Role", "RoleBinding", "Secret", "Role", "RoleBinding",
                 "Secret", "PersistentVolumeClaim", "Deployment", "Service",
                 "Deployment", "Service"], kinds
def get(kind, name):
    return next(d for d in docs if d.get("kind") == kind and d.get("metadata", {}).get("name") == name)

# Per-user MinIO store objects exist (the objectStoreBucket volume's store).
minio_secret = get("Secret", "mfpi-minio-admin")
assert minio_secret, "missing mfpi-minio-admin Secret"
assert minio_secret["stringData"].get("endpoint", "").startswith("http://mfpi-minio."), minio_secret["stringData"].keys()
assert get("PersistentVolumeClaim", "mfpi-minio-data"), "missing mfpi-minio-data PVC"
minio = get("Deployment", "mfpi-minio")
minio_img = minio["spec"]["template"]["spec"]["containers"][0]["image"]
assert minio_img.startswith("localhost:5001/minio@"), minio_img
minio_env = {e["name"]: e for e in minio["spec"]["template"]["spec"]["containers"][0]["env"]}
assert "MINIO_ROOT_USER" in minio_env and "MINIO_ROOT_PASSWORD" in minio_env, minio_env

# ActorTemplate: the bucket volume is declared and mounted at /data/pi-agent.
at = get("ActorTemplate", "mf-pi")
c = at["spec"]["containers"][0]
assert c["image"].startswith("localhost:5001/pi-web@"), c["image"]
assert "command" not in c, "command must not be set (keeps image ENTRYPOINT)"
assert c["args"][:2] == ["sh", "-c"], c["args"][:2]
script = c["args"][2]
assert "pull_profile()" not in script and "push_profile()" not in script, "in-image sync must be gone"
assert "MFPI_PROFILE_TOKEN" not in script and "MFPI_ADMIN_URL" not in script, "broker env must be gone"
assert "pi-web-sessiond" in script and "pi-web-server" in script, "plain sessiond+web supervisor expected"
mounts = {m["name"]: m for m in c.get("volumeMounts", [])}
assert mounts.get("pi-agent-data", {}).get("mountPath") == "/data/pi-agent", mounts
vols = {v["name"]: v for v in at["spec"].get("volumes", [])}
buck = vols.get("pi-agent-data", {}).get("objectStoreBucket")
assert buck, "missing objectStoreBucket volume pi-agent-data"
ref = buck["secretRef"]
assert ref["name"] == "mfpi-minio-admin", ref
assert ref["endpointKey"] == "endpoint" and ref["accessKeyIdKey"] == "root-user" \
    and ref["secretAccessKeyKey"] == "root-password", ref
assert not buck.get("bucketPrefix"), "no bucketPrefix: bucket name must equal the username (admin badge match)"
env = {e["name"]: e for e in c["env"]}
assert env["PI_WEB_PORT"].get("value") == "80", env
assert env["HOSTEXEC_MODE"].get("value") == "disabled", env
assert "MFPI_PROFILE_TOKEN" not in env and "MFPI_ADMIN_URL" not in env and "MFPI_PROFILE_PUSH_INTERVAL" not in env, env

# ate-api-server must be able to read the volume Secret for bucket resolution.
env_role = next(d for d in docs if d["kind"] == "Role" and d["metadata"]["name"] == "ate-api-server-env-sources")
names = [n for r in env_role["rules"] for n in (r.get("resourceNames") or [])]
assert "mfpi-minio-admin" in names and "mf-pi-provider-config" in names, names

# mfpi-admin Deployment: badge-only MinIO env wired, no broker token.
admin = get("Deployment", "mfpi-admin")
admin_env = {e["name"]: e for e in admin["spec"]["template"]["spec"]["containers"][0]["env"]}
assert admin_env["MINIO_ENDPOINT"]["value"].startswith("http://mfpi-minio."), admin_env
assert "MFPI_PROFILE_TOKEN" not in admin_env, admin_env
print(name, "OK:", len(docs), "docs; objectStoreBucket volume wired, broker removed")
PYEOF
  rm -f /tmp/mfpi-render-check.yaml
done
