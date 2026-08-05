# mf-cc Demo

This directory contains a demo that runs the **mf-cc** (cc-haha) web UI as an
actor on Agent Substrate. mf-cc is a Bun HTTP + WebSocket server that serves a
React-based AI coding workbench (REST API at `/api/*`, WebSocket at `/ws/*`,
static frontend at `/*`).

Running it as a Substrate actor gives it suspend/resume, snapshot, and
persistent-storage behavior, so its sessions and configuration survive across
suspends and resumes.

## Prerequisites

- A k8s cluster with Agent Substrate installed
  (`./hack/install-ate.sh --deploy-ate-system`).
- The mf-cc image built locally as `mf-cc:latest` (see
  `docker-build.sh` in the cc-haha repo). The image must be reachable by the
  cluster nodes.

> [!NOTE]
> **No real GCS bucket is needed on a local cluster.** Snapshots go to the
> in-cluster object store (rustfs) on kind/k3s; `BUCKET_NAME` is just a logical
> bucket name inside it, and `install-ate-kind.sh` already sets it to
> `ate-snapshots`. A real GCS bucket (`gs://${BUCKET_NAME}`) is only used on
> GKE.

> [!IMPORTANT]
> On a local cluster the nodes cannot reach external registries, so both the
> mf-cc image **and** the pause image must be pushed to the local registry
> (`localhost:5001`). The deploy script resolves their digests automatically
> once they are present.

## How to Run on Agent Substrate

### 1. Push the images to the cluster's registry

On a kind cluster (`KO_DOCKER_REPO=localhost:5001`), localize the images first:

```bash
# mf-cc workload image (built by the cc-haha repo's docker-build.sh)
docker tag mf-cc:latest localhost:5001/mf-cc:latest
docker push localhost:5001/mf-cc:latest

# pause image (3.10.2; use any reachable mirror, e.g. rancher/mirrored-pause)
docker tag rancher/mirrored-pause:3.10.2 localhost:5001/pause:3.10.2
docker push localhost:5001/pause:3.10.2
```

### 2. Deploy

Set the provider env vars (see `.env` in the cc-haha repo for reference), then
deploy:

```bash
# On GKE (BUCKET_NAME = real GCS bucket):
BUCKET_NAME=<bucket> \
ANTHROPIC_AUTH_TOKEN=<token> \
ANTHROPIC_BASE_URL=<base-url> \
ANTHROPIC_MODEL=<model> \
./hack/install-ate.sh --deploy-demo-mf-cc

# On kind/k3s (install-ate-kind.sh sets KO_DOCKER_REPO=localhost:5001 and
# BUCKET_NAME=ate-snapshots for the in-cluster rustfs store):
ANTHROPIC_AUTH_TOKEN=<token> \
ANTHROPIC_BASE_URL=<base-url> \
ANTHROPIC_MODEL=<model> \
./hack/install-ate-kind.sh --deploy-demo-mf-cc
```

This command will:

- Resolve the digest-pinned mf-cc and pause image references from the registry.
- Create the `ate-demo-mf-cc` namespace.
- Create the provider-config `Secret` and the RBAC that lets
  `ate-api-server` read it for env resolution.
- Create the `WorkerPool` and `ActorTemplate`.

The provider config is stored in a Secret and referenced via
`valueFrom.secretKeyRef`, so keys never appear in git.

### 3. Create an Actor

Actors live in an **atespace**, which must exist before you create actors in
it. Create one (e.g., `mfcc`), then create the mf-cc actor:

```bash
# Install the CLI as a kubectl plugin if not already installed
go install ./cmd/kubectl-ate

# Create the atespace (required before creating actors).
kubectl ate create atespace mfcc

# Create the actor in the atespace, using the mf-cc template.
kubectl ate create actor mfcc -a mfcc --template ate-demo-mf-cc/mf-cc
```

The actor starts as `STATUS_SUSPENDED` — it will auto-resume on the first
request through the router (see step 4). Check the actor status with:

```bash
kubectl ate get actor mfcc -a mfcc
```

### 4. Port-Forward the Router

To reach the actor through the Substrate router:

```bash
# Port-forward the Atenet Router. Pick a free host port (8080 may already be
# taken by a local service, so 58880 is a safe choice).
kubectl port-forward -n ate-system svc/atenet-router 58880:80
```

The actor is reachable at the DNS name
`<actor-name>.<atespace>.actors.resources.substrate.ate.dev`, i.e.
`mfcc.mfcc.actors.resources.substrate.ate.dev`.

## How to Use

The actor is reachable through the atenet-router port-forward. There are two
ways to access it:

- **Option A: mfcc-nginx proxy (recommended).** Build and run the local nginx
  reverse proxy that sets the `Host` header automatically — no `/etc/hosts`
  editing needed.
- **Option B: /etc/hosts + Host header.** Manually set up the Host header
  via `/etc/hosts` or curl.

### Option A: mfcc-nginx proxy (recommended)

The `mfcc-nginx` image is a thin nginx reverse proxy that forwards all requests
to the atenet-router port-forward (`58880`) with the correct `Host` header
(`mfcc.mfcc.actors.resources.substrate.ate.dev`). It listens on port `58881`.

No `/etc/hosts` edits are required — just point your browser at
`http://localhost:58881`.

```bash
# Build the image (from the demos/mf-cc directory)
./build-image.sh

# Run the container
docker run -d -p 58881:58881 --name mfcc-nginx --network host mfcc-nginx
```

> [!NOTE]
> The container uses `--network host` so it can reach the kubectl
> port-forward on the host's loopback. If using Docker Desktop (macOS / Windows),
> omit `--network host` and replace `127.0.0.1:58880` in `nginx.conf` with
> `host.docker.internal:58880` (proxy target), then run:
> `docker run -d -p 58881:58881 --name mfcc-nginx mfcc-nginx`.

To stop and remove the container:

```bash
docker stop mfcc-nginx && docker rm mfcc-nginx
```

### Option B: /etc/hosts + Host header

1. Point a browser at the actor's DNS name, either by adding an `/etc/hosts`
   entry or by using a tool that sends the `Host` header:

   ```bash
   # /etc/hosts (one-time):
   echo "127.0.0.1 mfcc.mfcc.actors.resources.substrate.ate.dev" | sudo tee -a /etc/hosts
   # then open http://mfcc.mfcc.actors.resources.substrate.ate.dev:58880
   ```

   Or with curl, using the `Host` header instead of `/etc/hosts`:

   ```bash
   curl -H "Host: mfcc.mfcc.actors.resources.substrate.ate.dev" http://127.0.0.1:58880/
   ```

2. The first request through the router resumes the actor automatically.
   The initial response may be `503` while the server starts up — wait a few
   seconds and retry. Verify the actor is now `STATUS_RUNNING` and assigned to a
   worker pod:

   ```bash
   kubectl ate get actor mfcc -a mfcc
   ```

3. Health check (should return `200` once the server is up):

   ```bash
   curl -H "Host: mfcc.mfcc.actors.resources.substrate.ate.dev" http://127.0.0.1:58880/health
   ```

4. The web UI and WebSocket (session chat at `/ws/<sessionId>`) work through the
   router — WebSocket upgrades are allowed by the router's RouteAction
   `upgradeConfigs`.

### Verify persistence

Session data lives in the `durableDir` mounted at `/root/.claude`. To confirm
it survives a suspend/resume:

```bash
# Create some state in the UI (settings, a project, …), then suspend:
kubectl ate suspend actor mfcc -a mfcc
# Resume it again:
kubectl ate resume actor mfcc -a mfcc
```

The saved state should still be present after resume.

## How to Uninstall

To remove the mf-cc demo resources from your cluster:

```bash
./hack/install-ate.sh --delete-demo-mf-cc
```

> [!NOTE]
> The demo uses `onPause: Data` / `onCommit: Data` snapshots. Because mf-cc is
> a long-lived web server, a full memory snapshot on every pause would be slow;
> sessions persist via the `durableDir` instead. On resume the process restarts
> from the snapshot and reads its persisted state from disk, so active
> WebSocket connections drop and the page should be refreshed.
