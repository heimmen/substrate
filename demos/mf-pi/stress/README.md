# mf-pi stress testing

Two pieces live here:

| Path | Purpose |
|---|---|
| [`deploy-mock-llm.sh`](./deploy-mock-llm.sh) + `mock-llm.yaml.tmpl` | deploy the OpenAI-compatible mock model and register it with mfpi-admin |
| [`../../tools/mfpi-loadgen`](../../../tools/mfpi-loadgen) | the load generator itself (`users` / `run` / `cleanup` / `serve-mock` / `analyze-log`) |

Read [`../docs/stress_test_plan.md`](../docs/stress_test_plan.md) first: it has
the environment inventory, scenarios, metrics and safety rules.

## Quick start (test environment, 59681)

```bash
cd demos/mf-pi
export GOCACHE=/tmp/mfpi-gocache            # known-good cache on this host

# 1) build the load generator
(cd ../.. && go build -o /tmp/mfpi-loadgen ./tools/mfpi-loadgen)

# 2) mock model (zero provider cost) + provider registration
./stress/deploy-mock-llm.sh --test

# 3) dedicated stress users (created SUSPENDED, so no worker is used yet)
/tmp/mfpi-loadgen users -admin-url http://127.0.0.1:59681 \
  -atespace mfpi-test -template ate-demo-mf-pi-test/mf-pi-small \
  -count 8 -creds /tmp/creds-test.json

# 4) wait ~60s for actors to merge models.json, then run
/tmp/mfpi-loadgen run -base-url http://127.0.0.1:59681 -creds /tmp/creds-test.json \
  -scenario agent-turn -vus 8 -ramp 30s -duration 10m \
  -provider mock -model mock-1 -report /tmp/turns-8.json

# 5) clean up (deletes actors AND purges their volumes)
/tmp/mfpi-loadgen cleanup -admin-url http://127.0.0.1:59681 \
  -creds /tmp/creds-test.json -prefix st
./stress/deploy-mock-llm.sh --test --delete
```

## Production (58681)

Same commands with `--prod` (the script default) and:

```bash
/tmp/mfpi-loadgen users -admin-url http://127.0.0.1:58681 \
  -atespace mfpi -template ate-demo-mf-pi/mf-pi-small \
  -count 8 -prefix st -creds /tmp/creds-prod.json
```

Keep `-vus` **at or below the number of free workers** (`kubectl get workerpool -A`),
pick a low-traffic window, and do not run suspend/resume scenarios in
production.
