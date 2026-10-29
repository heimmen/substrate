# mfpi-loadgen

Load generator and mock LLM for the [mf-pi demo](../mf-pi). It exists to answer
the questions in
[mf-pi/docs/stress_test_plan.md](../../demos/mf-pi/docs/stress_test_plan.md):
how many concurrent users the 58681 gateway can serve, where the bottleneck is,
and how the system fails when overloaded.

Why a purpose-built tool instead of `ab`/`vegeta`/Locust:

- pi-web traffic is a mix of SPA assets, polled JSON APIs, two kinds of
  WebSocket, and (optionally) real agent turns. A plain HTTP hammer misses the
  interesting parts.
- Authentication is HTTP Basic **once**, then a `mfpi_auth` cookie. The two
  paths have very different cost in `mfpi-admin` (PBKDF2-100k vs an HMAC check),
  so the tool can replay either and lets you compare.
- The tool doubles as an OpenAI-compatible mock model so capacity tests do not
  spend real provider budget or depend on external latency.

## Build

```bash
# from the repository root
go build -o /tmp/mfpi-loadgen ./tools/mfpi-loadgen
```

If the default Go build cache is broken in your environment (the repo README
mentions this), use a fresh one:

```bash
GOCACHE=/tmp/mfpi-gocache go build -o /tmp/mfpi-loadgen ./tools/mfpi-loadgen
```

## Commands

| Command | Purpose |
|---|---|
| `users` | create suspended stress actors and assign passwords in one shot |
| `cleanup` | delete them (admin delete also purges their persistent volume) |
| `run` | run a scenario: `idle-browser`, `agent-turn` or `admin` |
| `serve-mock` | OpenAI-compatible mock model server (`/v1/chat/completions`, `/v1/models`) |
| `analyze-log` | summarize an nginx access log to calibrate the traffic model |

### 1. Create stress users

Creating an actor directly with `kubectl ate create actor` leaves it
**SUSPENDED**, so preparing 64 users does not consume workers. Passwords are
then assigned through the admin API (`POST /api/users/<name>/password`), which
returns the plaintext once. The result is a `creds.json` (mode 0600).

```bash
# test environment (59681, atespace mfpi-test)
/tmp/mfpi-loadgen users \
  -admin-url http://127.0.0.1:59681 \
  -atespace mfpi-test \
  -template ate-demo-mf-pi-test/mf-pi-small \
  -count 16 -creds creds-test.json

# production (58681, atespace mfpi) -- only use dedicated st-* users!
/tmp/mfpi-loadgen users \
  -admin-url http://127.0.0.1:58681 \
  -atespace mfpi -template ate-demo-mf-pi/mf-pi-small \
  -count 8 -prefix st -creds creds-prod.json
```

> The tool never touches existing users. It only creates names with the
> `-prefix` you pass; do not point it at a prefix that collides with real users.

### 2. Run a scenario

```bash
# one-user smoke test (also verifies auth + WebSocket + sessions)
/tmp/mfpi-loadgen run \
  -base-url http://127.0.0.1:59681 -creds creds-test.json \
  -scenario idle-browser -vus 1 -duration 2m -report smoke.json

# ramp to 16 concurrent browsers, 10 minutes at the top
/tmp/mfpi-loadgen run \
  -base-url http://127.0.0.1:59681 -creds creds-test.json \
  -scenario idle-browser -vus 16 -ramp 60s -duration 10m \
  -think 3s-10s -report idle-16.json

# agent turns against the in-cluster mock model
/tmp/mfpi-loadgen run \
  -base-url http://127.0.0.1:59681 -creds creds-test.json \
  -scenario agent-turn -vus 8 -ramp 30s -duration 10m \
  -provider mock -model mock-1 -turn-interval 60s -report turns.json

# management-plane load (ListActors through mfpi-admin)
/tmp/mfpi-loadgen run \
  -base-url http://127.0.0.1:59681 -scenario admin -vus 4 -duration 5m

# compare auth paths: Basic on every request vs cookie (browser behaviour)
/tmp/mfpi-loadgen run ... -auth basic ...
/tmp/mfpi-loadgen run ... -auth cookie ...
```

Useful flags:

| Flag | Default | Meaning |
|---|---|---|
| `-vus` | 1 | virtual users; each cycles through the creds file |
| `-ramp` | 0 | spread VU starts over this duration |
| `-duration` | 5m | measurement window, starting after the ramp and after every VU is ready |
| `-think` | `3s-10s` | think time between iterations |
| `-auth` | `cookie` | `cookie` (Basic once), `basic` (every request), `mixed` |
| `-cwd` | `/data/home` | session working directory inside the actor (must exist) |
| `-provider`/`-model` | — | agent-turn: model selected per session (e.g. `mock`/`mock-1`) |
| `-provider-api-key` | `sk-mock` for `mock` | agent-turn: key stored through mfpi-admin so the custom provider becomes selectable |
| `-model-timeout` | 120s | how long to wait for the key/model to become available |
| `-allow-default-model` | false | keep running on the session default model if selection fails (may cost real money) |
| `-turn-interval` | 60s | agent-turn: delay between prompts |
| `-turn-timeout` | 120s | agent-turn: max wait for one turn |
| `-ready-timeout` | 180s | max wait for a suspended/cold actor to answer |
| `-metrics-listen` | — | expose Prometheus metrics, e.g. `127.0.0.1:9091` |
| `-report` | auto | JSON report path |

The run prints a table and writes a JSON report:

```
endpoint              count    p50ms    p95ms    p99ms    maxms  transport
bootstrap                 1    42000.0  42000.0  42000.0  42000.0          0
poll.users              120       12.0     40.0     80.0    210.0          0
...
turns: ok=8 failed=0 p50=900ms p95=1200ms p99=1400ms
```

Each VU's actor may be cold at the start (`bootstrap` includes the resume); the
tool retries 403/503 with backoff until `-ready-timeout`. The readiness probe is
an API request (`/api/machines/local/status`), not a browser navigation, because
the gateway rewrites a not-ready actor to a **200 loading page** for navigations.
In cookie mode the first successful request establishes `mfpi_auth`; later
requests only carry the cookie, like a browser.

For `agent-turn` with a custom provider, the provider is only selectable after
`mfpi-admin` stores an API key for it (`docs/user_agent_api.md` §4.6). The tool
calls `POST /api/users/<name>/apikey` at VU startup (default key `sk-mock` for
the `mock` provider) and retries model selection for `-model-timeout`. If
selection still fails, the VU **aborts** rather than silently spending real
provider budget; pass `-allow-default-model` to accept the session default.

### 3. Mock LLM

Run it locally:

```bash
/tmp/mfpi-loadgen serve-mock -addr :18080 -ttft 100ms -chunk-delay 10ms -chunks 32
curl -sN -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"mock-1","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

Or deploy it in-cluster and register it as a provider:

```bash
cd demos/mf-pi
./stress/deploy-mock-llm.sh --test        # 59681 test environment
./stress/deploy-mock-llm.sh               # 58681 production environment
./stress/deploy-mock-llm.sh --delete --test
```

The deploy script builds the binary image with `ko`, applies a Deployment +
Service into the demo namespace, and registers the endpoint through
`POST /usermanagement/api/providers` as provider `mock`. Actor supervisors merge
`models.json` every 60 s, so the model shows up within about a minute.

Flags for tuning the fake model: `-ttft` (time to first token), `-chunk-delay`,
`-chunks`, `-error-rate` (inject 500s), `-log-bodies` (verbose).

> The mock returns plain text only. Tool-call loops are intentionally left out of
> v1: they need a matching tool schema and can turn one prompt into an unbounded
> loop.

### 4. Analyze an access log

```bash
docker logs mfpi-nginx --since 24h 2>&1 | /tmp/mfpi-loadgen analyze-log -top 10
/tmp/mfpi-loadgen analyze-log -file /var/tmp/nginx.log -json
```

It reports status mix, request rate (average and peak per minute), top users and
top normalized paths (`/{user}/...`), and the 5xx paths. This is how the traffic
weights in the load scenarios were derived.

## Metrics

With `-metrics-listen 127.0.0.1:9091` the tool exposes:

- `mfpi_loadgen_requests_total{scenario,endpoint,code}`
- `mfpi_loadgen_request_duration_seconds{scenario,endpoint}` (histogram)
- `mfpi_loadgen_turns_total{scenario,result}`
- `mfpi_loadgen_turn_duration_seconds{scenario}` (histogram)
- `mfpi_loadgen_ws_events_total{scenario,event}`

Scrape it from Prometheus (or just `curl` it) while a run is in progress and
correlate with the platform metrics listed in the stress plan
(`atenet_router_route_duration_seconds`, `ate_workerpool_workers`,
`process_cpu_seconds_total`, cadvisor per-pod CPU/throttling).

## Safety notes

- `users` only creates `-prefix`-named actors; `cleanup` refuses to delete names
  that do not match an explicit `-prefix` when one is given. Still, review
  `creds.json` before running cleanup.
- `cleanup` uses the admin delete endpoint, which **purges** the user's
  persistent volume. That is intended for disposable stress users.
- Never point the generator at real user actors, and never run
  suspend/resume stress in production: snapshot restore is currently unreliable
  (see the mf-pi README).
- Each virtual user needs a worker while its actor is RUNNING. Check
  `kubectl get workerpool -A` before raising `-vus`; over-subscribing produces
  503 `no free workers` for real users too if you share the atespace.
