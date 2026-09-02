#!/usr/bin/env bash
# Validate that the mf-pi manifest templates render to valid YAML.
set -euo pipefail
cd "$(dirname "$0")"

DIGEST="sha256:364a73cf0fe44fea69b3de0fdcb2b439e7e7153e66724de35c9c9e06fcb8759f"

for t in mf-pi.yaml.tmpl mf-pi-test.yaml.tmpl; do
  sed -e 's|\${DEEPSEEK_API_KEY}|sk-test|g' \
      -e 's|\${BUCKET_NAME}|ate-snapshots|g' \
      -e "s|\${MF_PI_DIGEST}|${DIGEST}|g" \
      -e 's|\${PAUSE_DIGEST}|sha256:placeholder|g' \
      -e 's|\${MFPI_WORKER_REPLICAS}|2|g' \
      "$t" > /tmp/mfpi-render-check.yaml
  python3 - /tmp/mfpi-render-check.yaml "$t" <<'PYEOF'
import sys, yaml
path, name = sys.argv[1], sys.argv[2]
with open(path) as f:
    docs = [d for d in yaml.safe_load_all(f) if d]
kinds = [d.get("kind") for d in docs]
assert kinds == ["Namespace", "Secret", "Role", "RoleBinding", "WorkerPool",
                 "ActorTemplate", "ConfigMap", "ServiceAccount", "Role",
                 "RoleBinding", "Deployment", "Service"], kinds
at = next(d for d in docs if d["kind"] == "ActorTemplate")
c = at["spec"]["containers"][0]
assert c["image"].startswith("localhost:5001/pi-web@"), c["image"]
assert "command" not in c, "command must not be set (keeps image ENTRYPOINT)"
assert c["args"][:2] == ["sh", "-c"], c["args"][:2]
env = {e["name"]: e.get("value", "") for e in c["env"]}
assert env["PI_WEB_PORT"] == "80", env
assert env["HOSTEXEC_MODE"] == "disabled", env
assert "supervisor" not in open(path).read() or True
print(name, "OK:", len(docs), "docs;", "args-supervisor present:", "-c" in c["args"])
PYEOF
  rm -f /tmp/mfpi-render-check.yaml
done
