#!/usr/bin/env bash
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
from collections import Counter
kinds = [d.get("kind") for d in docs]
counts = Counter(kinds)
# Order of docs is not significant, but the set of resources must be exact.
assert counts == Counter({
    "Namespace": 1,
    "Secret": 2,          # provider config + per-user key secret
    "Role": 5,            # env-sources + passwords + expirations + tiers + keys
    "RoleBinding": 5,
    "WorkerPool": 4,      # base mf-pi-workerpool + one per tier
    "ActorTemplate": 4,   # base mf-pi + one per tier
    "ConfigMap": 3,       # passwords + expirations + tiers
    "ServiceAccount": 1,
    "Deployment": 1,
    "Service": 1,
}), counts

def get(kind, name):
    return next(d for d in docs if d.get("kind") == kind and d.get("metadata", {}).get("name") == name)

# No MinIO / profile-token artifacts remain anywhere.
raw = open(path).read()
for banned in ("minio", "MinIO", "MINIO", "MFPI_PROFILE_TOKEN", "mfpi-profile-token",
               "MFPI_ADMIN_URL", "internal/actor"):
    assert banned not in raw, "leftover %r in %s" % (banned, path)

# ActorTemplates (base + per-tier): sticky userdata volume mounted at
# /data/pi-agent, and each tier template listed in the base template set.
atl_all = [d for d in docs if d["kind"] == "ActorTemplate"]
assert {d["metadata"]["name"] for d in atl_all} >= {"mf-pi", "mf-pi-small", "mf-pi-mid", "mf-pi-large"}, \
    [d["metadata"]["name"] for d in atl_all]
base_worker_label = None
for at in atl_all:
    spec = at["spec"]
    vols = spec.get("volumes") or []
    assert len(vols) == 1, (at["metadata"]["name"], vols)
    v = vols[0]
    assert v["name"] == "userdata", (at["metadata"]["name"], v)
    assert "externalVolumeTemplate" in v, v
    ev = v["externalVolumeTemplate"]
    assert ev["capacity"], (at["metadata"]["name"], ev)
    assert ev["storageClassName"], (at["metadata"]["name"], ev)
    c = spec["containers"][0]
    assert c["image"].startswith("localhost:5001/pi-web@"), c["image"]
    assert "command" not in c, "command must not be set (keeps image ENTRYPOINT)"
    assert c["args"][:2] == ["sh", "-c"], c["args"][:2]
    script = c["args"][2]
    assert "pull_profile" not in script and "push_profile" not in script, "sync must be gone"
    mounts = c.get("volumeMounts") or []
    assert mounts == [{"name": "userdata", "mountPath": "/data/pi-agent"}], (at["metadata"]["name"], mounts)
    env = {e["name"]: e for e in c["env"]}
    assert env["PI_WEB_PORT"].get("value") == "80", (at["metadata"]["name"], env)
    assert env["HOSTEXEC_MODE"].get("value") == "disabled", (at["metadata"]["name"], env)
    assert not any(k.startswith("MFPI_") for k in env), (at["metadata"]["name"], sorted(env))
    # Record the base worker label from mf-pi, then check tier templates route
    # to the matching per-tier worker label.
    worker = spec["workerSelector"]["matchLabels"]["workload"]
    if at["metadata"]["name"] == "mf-pi":
        base_worker_label = worker
    else:
        assert worker.startswith("mf-pi-"), (at["metadata"]["name"], worker)
        assert worker != base_worker_label, (at["metadata"]["name"], worker)
    assert base_worker_label is not None

# Each tier has a corresponding per-tier WorkerPool that pins CPU/memory.
wps = {d["metadata"]["name"]: d for d in docs if d["kind"] == "WorkerPool"}
for tier in ("small", "mid", "large"):
    wp = wps["mf-pi-wp-" + tier]
    res = wp["spec"]["template"]["resources"]
    assert res.get("requests") and res.get("limits"), (tier, res)
    assert wp["metadata"]["labels"]["workload"] == ("mf-pi-%s-test" % tier if "test" in name else "mf-pi-" + tier), wp

# mfpi-admin Deployment: no MinIO broker env; expiry/tier ConfigMaps wired.
admin = get("Deployment", "mfpi-admin")
admin_env = {e["name"]: e for e in admin["spec"]["template"]["spec"]["containers"][0]["env"]}
assert not any(k.startswith("MINIO_") or k.startswith("MFPI_") for k in admin_env), sorted(admin_env)
for k in ("EXPIRY_CONFIGMAP", "TIERS_CONFIGMAP", "DEFAULT_TIER"):
    assert k in admin_env, (k, sorted(admin_env))

# ate-api-server env-sources Role: only the provider-config Secret.
env_role = next(d for d in docs if d["kind"] == "Role" and d["metadata"]["name"] == "ate-api-server-env-sources")
assert len(env_role["rules"]) == 1, env_role
assert env_role["rules"][0]["resourceNames"] == ["mf-pi-provider-config"], env_role
print(name, "OK:", len(docs), "docs; sticky userdata volume wired, tiers wired, MinIO gone")
PYEOF
  rm -f /tmp/mfpi-render-check.yaml
done
