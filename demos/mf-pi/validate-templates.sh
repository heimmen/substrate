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
      -e "s|\${MFPI_WORKER_REPLICAS_SMALL}|3|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_MID}|4|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_LARGE}|5|g" \
      -e "s|\${MFPI_WORKER_REPLICAS_BASE}|2|g" \
      -e "s|\${MFPI_IDLE_TIMEOUT}|90m|g" \
      -e "s|\${MFPI_IDLE_TIERS}|small,mid|g" \
      -e "s|\${MFPI_DEPLOY_REV}|0|g" \
      "$t" > /tmp/mfpi-render-check.yaml
  python3 - /tmp/mfpi-render-check.yaml "$t" <<'PYEOF'
import sys, json, yaml
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
    "Role": 6,            # env-sources + passwords + expirations + tiers + keys + providers
    "RoleBinding": 6,
    "WorkerPool": 4,      # base mf-pi-workerpool + one per tier
    "ActorTemplate": 4,   # base mf-pi + one per tier
    "ConfigMap": 4,       # passwords + expirations + tiers + provider models
    "ServiceAccount": 1,
    "Deployment": 1,
    "Service": 1,
    "PersistentVolumeClaim": 1,  # mfpi-shared-skills
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
    assert c["image"].startswith("localhost:5001/mf-agent@"), c["image"]
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
    # Durable user state: HOME and XDG_CONFIG_HOME must live on the userdata
    # volume, because pi-web installs user-scope packages/skills under
    # $HOME/.pi/packages; anything outside /data/pi-agent is ephemeral.
    assert env["HOME"].get("value") == "/data/pi-agent/home", (at["metadata"]["name"], env.get("HOME"))
    assert env["XDG_CONFIG_HOME"].get("value") == "/data/pi-agent/config", \
        (at["metadata"]["name"], env.get("XDG_CONFIG_HOME"))
    # Deploy revision stamp (forces a real template change for the E2E test).
    assert env["PI_WEB_DEPLOY_REV"].get("value") == "0", (at["metadata"]["name"], env.get("PI_WEB_DEPLOY_REV"))
    # The supervisor must create the durable dirs and seed any build-time
    # dotfiles from the image's /data/home, plus a durable projects root.
    assert 'mkdir -p "$HOME" "${XDG_CONFIG_HOME:-$HOME/.config}"' in script, \
        (at["metadata"]["name"], "durable HOME mkdir missing")
    assert 'cp -an /data/home/. "$HOME"/' in script, (at["metadata"]["name"], "HOME seed missing")
    assert "mkdir -p /data/pi-agent/projects" in script, (at["metadata"]["name"], "projects dir missing")
    assert "outside the durable volume" in script, (at["metadata"]["name"], "project cwd warning missing")
    # Managed-skills pull loop: PI_WEB_SKILLS_URL set (pointing at the admin's
    # /internal/skills base) and the supervisor script contains the 10s loop.
    url = env["PI_WEB_SKILLS_URL"].get("value") or ""
    assert url.endswith("/internal/skills"), (at["metadata"]["name"], url)
    assert ".mfpi-managed-skills.json" in script, (at["metadata"]["name"], "pull loop missing")
    assert "fetch(base + \"/manifest\"" in script, (at["metadata"]["name"], "manifest fetch missing")
    # Provider-registry pull loop: PI_WEB_MODELS_URL points at the admin's
    # /internal/providers/models.json and the supervisor merges it into the
    # agent's models.json.
    models_url = env["PI_WEB_MODELS_URL"].get("value") or ""
    assert models_url.endswith("/internal/providers/models.json"), (at["metadata"]["name"], models_url)
    assert "provider-models: updated" in script, (at["metadata"]["name"], "provider-models loop missing")
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

# The small tier is the default tier for mf-pi users, so it is sized for
# concurrency: many workers with a small CPU request (what the scheduler
# reserves, i.e. what bounds concurrent users per node) but a larger limit so an
# active agent is not throttled to the request. This render substitutes
# MFPI_WORKER_REPLICAS_SMALL=3 (see the sed above).
small = wps["mf-pi-wp-small"]
assert small["spec"]["replicas"] == 3, small["spec"]["replicas"]
small_res = small["spec"]["template"]["resources"]
assert small_res["requests"]["cpu"] == "250m", small_res
assert small_res["limits"]["cpu"] == "1", small_res
assert small_res["requests"]["memory"] == "1Gi", small_res
assert small_res["limits"]["memory"] == "1Gi", small_res

# mid/large are per-worker dedicated cores (1 and 2), so they must be sized by
# their OWN replica variables — sharing MFPI_WORKER_REPLICAS with the small tier
# would ask for 16 and 32 cores respectively and pin the node at 100%.
# This render substitutes MID=4 / LARGE=5 (see the sed above).
mid, large = wps["mf-pi-wp-mid"], wps["mf-pi-wp-large"]
assert mid["spec"]["replicas"] == 4, mid["spec"]["replicas"]
assert large["spec"]["replicas"] == 5, large["spec"]["replicas"]
assert mid["spec"]["template"]["resources"]["requests"]["cpu"] == "1", mid
assert large["spec"]["template"]["resources"]["requests"]["cpu"] == "2", large

# mfpi-admin Deployment: no MinIO broker env; expiry/tier ConfigMaps wired;
# shared-skills PVC mounted with SKILLS_DIR set.
admin = get("Deployment", "mfpi-admin")
admin_pod = admin["spec"]["template"]["spec"]
admin_env = {e["name"]: e for e in admin_pod["containers"][0]["env"]}
assert not any(k.startswith("MINIO_") or k.startswith("MFPI_") for k in admin_env), sorted(admin_env)
for k in ("EXPIRY_CONFIGMAP", "TIERS_CONFIGMAP", "DEFAULT_TIER"):
    assert k in admin_env, (k, sorted(admin_env))
assert admin_env["SKILLS_DIR"].get("value") == "/var/lib/mfpi-skills", admin_env
admin_mounts = {m["name"]: m for m in admin_pod["containers"][0]["volumeMounts"]}
assert admin_mounts["skills"]["mountPath"] == "/var/lib/mfpi-skills", admin_mounts
admin_vols = {v["name"]: v for v in admin_pod["volumes"]}
assert admin_vols["skills"]["persistentVolumeClaim"]["claimName"] == "mfpi-shared-skills", admin_vols
# The admin-managed provider registry is reached through the API server (the
# usermanagement UI edits it), not mounted into the pod, and the admin SA is
# granted get/update on exactly that ConfigMap.
assert "PROVIDER_MODELS_FILE" not in admin_env, sorted(admin_env)
assert admin_env["PROVIDER_MODELS_CONFIGMAP"].get("value") == "mfpi-provider-models", admin_env
assert "provider-models" not in admin_mounts, admin_mounts
assert "provider-models" not in admin_vols, admin_vols
prov_role = get("Role", "mfpi-admin-providers")
prov_rule = prov_role["rules"][0]
assert prov_rule["resourceNames"] == ["mfpi-provider-models"], prov_rule
assert set(prov_rule["verbs"]) == {"get", "update"}, prov_rule
assert get("RoleBinding", "mfpi-admin-providers")["roleRef"]["name"] == "mfpi-admin-providers"

# Provider registry ConfigMap: the seed the admin reads and rewrites. It must be
# a parseable models.json, in the namespace the admin is pointed at, and every
# entry must carry a baseUrl, an api implementation and at least one model id.
# Custom endpoints do NOT have to appear in KEY_PROVIDERS: the UI list is the
# built-in curated set merged with this registry at runtime.
pm = get("ConfigMap", "mfpi-provider-models")
assert admin_env["PROVIDER_MODELS_NAMESPACE"].get("value") == pm["metadata"]["namespace"], pm["metadata"]
registry = json.loads(pm["data"]["provider-models.json"])
assert isinstance(registry.get("providers"), dict), registry
for pid, prov in registry["providers"].items():
    assert prov.get("baseUrl"), (pid, prov)
    assert prov.get("api"), (pid, prov)
    assert prov.get("models"), (pid, prov)
    for m in prov["models"]:
        assert m.get("id"), (pid, m)

# Shared skills PVC: RWO, standard storage class, 1Gi.
pvc = get("PersistentVolumeClaim", "mfpi-shared-skills")
assert pvc["spec"]["accessModes"] == ["ReadWriteOnce"], pvc
assert pvc["spec"]["storageClassName"] == "standard", pvc
assert pvc["spec"]["resources"]["requests"]["storage"] == "1Gi", pvc

# ate-api-server env-sources Role: only the provider-config Secret.
env_role = next(d for d in docs if d["kind"] == "Role" and d["metadata"]["name"] == "ate-api-server-env-sources")
assert len(env_role["rules"]) == 1, env_role
assert env_role["rules"][0]["resourceNames"] == ["mf-pi-provider-config"], env_role
print(name, "OK:", len(docs), "docs; sticky userdata volume wired (HOME/XDG on PV), tiers wired, MinIO gone")
PYEOF
  rm -f /tmp/mfpi-render-check.yaml
done
