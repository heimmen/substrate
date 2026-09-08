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
# Validate that the mf-pi manifest templates render to valid YAML and wire the
# sticky per-user userdata volume (no MinIO / profile-token artifacts).
set -euo pipefail
cd "$(dirname "$0")"

DIGEST="sha256:364a73cf0fe44fea69b3de0fdcb2b439e7e7153e66724de35c9c9e06fcb8759f"

for t in mf-pi.yaml.tmpl mf-pi-test.yaml.tmpl; do
  sed -e "s|\${DEEPSEEK_API_KEY}|sk-test|g" \
      -e "s|\${BUCKET_NAME}|ate-snapshots|g" \
      -e "s|\${MF_PI_DIGEST}|${DIGEST}|g" \
      -e "s|\${PAUSE_DIGEST}|sha256:placeholder|g" \
      -e "s|\${MFPI_WORKER_REPLICAS}|2|g" \
      "$t" > /tmp/mfpi-render-check.yaml
  python3 - /tmp/mfpi-render-check.yaml "$t" <<'PYEOF'
import sys, yaml
path, name = sys.argv[1], sys.argv[2]
with open(path) as f:
    docs = [d for d in yaml.safe_load_all(f) if d]
kinds = [d.get("kind") for d in docs]
assert kinds == ["Namespace", "Secret", "Role", "RoleBinding",
                 "WorkerPool", "ActorTemplate", "ConfigMap", "ServiceAccount",
                 "Role", "RoleBinding", "Secret", "Role", "RoleBinding",
                 "Deployment", "Service"], kinds

def get(kind, name):
    return next(d for d in docs if d.get("kind") == kind and d.get("metadata", {}).get("name") == name)

# No MinIO / profile-token artifacts remain anywhere.
raw = open(path).read()
for banned in ("minio", "MinIO", "MINIO", "MFPI_PROFILE_TOKEN", "mfpi-profile-token",
               "MFPI_ADMIN_URL", "internal/actor"):
    assert banned not in raw, "leftover %r in %s" % (banned, path)

# ActorTemplate: sticky userdata volume mounted at /data/pi-agent.
at = next(d for d in docs if d["kind"] == "ActorTemplate")
spec = at["spec"]
vols = spec.get("volumes") or []
assert len(vols) == 1, vols
v = vols[0]
assert v["name"] == "userdata", v
assert "externalVolumeTemplate" in v, v
ev = v["externalVolumeTemplate"]
assert ev["capacity"], ev
assert ev["storageClassName"], ev
c = spec["containers"][0]
assert c["image"].startswith("localhost:5001/pi-web@"), c["image"]
assert "command" not in c, "command must not be set (keeps image ENTRYPOINT)"
assert c["args"][:2] == ["sh", "-c"], c["args"][:2]
script = c["args"][2]
assert "pull_profile" not in script and "push_profile" not in script, "sync must be gone"
mounts = c.get("volumeMounts") or []
assert mounts == [{"name": "userdata", "mountPath": "/data/pi-agent"}], mounts
env = {e["name"]: e for e in c["env"]}
assert env["PI_WEB_PORT"].get("value") == "80", env
assert env["HOSTEXEC_MODE"].get("value") == "disabled", env
assert not any(k.startswith("MFPI_") for k in env), sorted(env)

# mfpi-admin Deployment: no MinIO broker env.
admin = get("Deployment", "mfpi-admin")
admin_env = {e["name"]: e for e in admin["spec"]["template"]["spec"]["containers"][0]["env"]}
assert not any(k.startswith("MINIO_") or k.startswith("MFPI_") for k in admin_env), sorted(admin_env)

# ate-api-server env-sources Role: only the provider-config Secret.
env_role = next(d for d in docs if d["kind"] == "Role" and d["metadata"]["name"] == "ate-api-server-env-sources")
assert len(env_role["rules"]) == 1, env_role
assert env_role["rules"][0]["resourceNames"] == ["mf-pi-provider-config"], env_role
print(name, "OK:", len(docs), "docs; sticky userdata volume wired, MinIO gone")
PYEOF
  rm -f /tmp/mfpi-render-check.yaml
done
