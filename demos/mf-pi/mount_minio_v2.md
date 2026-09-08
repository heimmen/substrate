# Per-actor object-store bucket volume v2 — implementation plan (mount_minio_v2.md)

Branch: `mf_ax`. Supersedes the design in `mount_minio.md` (same locked architecture
decisions D3–D8), updated with corrections from code verification. Scope:
ate platform core + `demos/mf-pi` rewiring.

## Goal (the user's 3 requirements)

1. **Mount the user's MinIO bucket into the agent actor (pod) at `/data/pi-agent`** —
   as a real ate core volume type `objectStoreBucket`: a node-local host dir
   bind-served into the gVisor guest via runsc's gofer (same mechanism as
   durableDir/external volumes), synced by atelet with the bucket as one tarball.
2. **The agent writes user data directly to `/data/pi-agent`** — no in-image
   tar/curl supervisor, no HTTP broker: atelet syncs the dir with the bucket
   (periodic export, default 20s; final export on suspend).
3. **If the agent is reloaded (delete+recreate reset), the user bucket is mounted
   again automatically** — a cold restore lands on an empty host dir; atelet
   rehydrates the bucket object into the mount before the guest boots.

```
TODAY (mf-pi broker)                         AFTER (ate core bucket volume)
─────────────────────────                    ──────────────────────────────
actor guest                                  actor guest
  supervisor tar /data/pi-agent ──HTTP──►      supervisor = plain sessiond+web start
  every 20s + TERM (shell)                     (writes go straight to /data/pi-agent)
  pull on fresh/reset (shell)
      │                                            ▲
      ▼                                            │ bind (gofer)
mfpi-admin /internal/actor/{n}/profile       atelet host dir per actor
  (Go S3 broker, MINIO_* creds)                    │ rehydrate on empty mount /
      │ S3                                        │ periodic tar.gz export /
      ▼                                          │ final export on suspend
MinIO per-user bucket  profile.tar.gz        MinIO per-user bucket  profile.tar.gz (unchanged)
```

## Locked architecture decisions (from mount_minio.md, do not re-litigate)

- **D3 = new ate core VolumeSource kind** (`objectStoreBucket`): platform-wide change.
- **D4 = sync volume, NOT live FUSE mount.** Backing = a node-local host dir,
  bind-served into the gVisor guest by runsc's gofer exactly like
  durableDir/external volumes today. atelet syncs the dir to/from the bucket as
  one tarball. No `/dev/fuse`, no privileged pod, no new node dependency.
- **D5 = VolumeSource references an in-namespace Secret** (endpoint + access key
  + secret key). ateapi resolves it against the ActorTemplate namespace
  (precedent: env `secretKeyRef`) and passes endpoint/creds/**per-actor bucket**
  to atelet over RPC. atelet never reads a namespace Secret.
- **D6 = single `profile.tar.gz` object per user.** Byte-compatible with today:
  keeps `check-minio-users.sh`, the admin "已同步" `HeadObject` badge, and the
  delete-retains-bucket restore working without migration.
- **D7 = empty-bucket seeding happens in the guest.** pi-web's bootstrap already
  materializes the baked-in skills into `/data/pi-agent` at boot; the bind mount
  is in place from process start, so bootstrap seeds the volume. Platform
  seeding is impossible (atelet deliberately runs with no capabilities; the
  golden rootfs is only materialized inside the guest).
- **D8 = defer two-pod double-export fencing** (object-tag/`If-Match` epoch) as a
  TODO — fine at demo scale (≤4 workers, one actor each), required before
  platform-wide use.

## Verified code facts this plan rests on (corrections vs mount_minio.md)

1. **Full-snapshot fs exclusion is gVisor-side via pause annotations, not the
   ateom RPC list.** `cmd/atelet/main.go:726-734` sets
   `dev.gvisor.spec.mount.durabledir.{type,share,source}` on the *pause*
   container spec (source = host dir path); the patched runsc excludes those
   host dirs from Full checkpoints and re-attaches them on restore. The RPC
   `durable_dir_volumes` list (`buildAteomWorkloadSpec` →
   `cmd/ateom-gvisor/main.go:289-300` → `runsc fscheckpoint -path`) only drives
   DATA-scope. → The bucket volume sets the same pause annotations (host path =
   `ateompath.VolumeHostPath(actorUID, vol)`). The annotation is a single slot:
   **a template must not combine `durableDir` and `objectStoreBucket` volumes**
   → enforced by a new CEL rule.
2. **`resetActorDirs` does `os.Remove` on every volumes-dir entry and fails on
   non-empty dirs** (`cmd/atelet/main.go:1113-1128`). Bucket host dirs hold
   rehydrated data → they must be **skipped** in `resetActorDirs`. This also
   preserves Flow C: same-instance suspend/resume keeps the local dir, which
   stays locally authoritative (no rehydrate, no stale clobber).
3. **Bucket-name derivation must be `bucketPrefix + actorName`** to byte-match
   the admin badge (`demos/mf-pi/admin/profile.go:118-120`:
   `prefix + name`; `MINIO_BUCKET_PREFIX` defaults empty → bucket = username).
4. **Real actors always Restore from the shared golden snapshot**
   (`workflow_resume.go:417-439`); `Run` only happens for the golden actor boot.
   So: golden boot seeds the volume; the golden Full snapshot excludes
   `/data/pi-agent` (annotations); real actors start with an empty host dir
   (new actorUID) and rehydrate from MinIO.
5. **Pause/suspend build env-less specs** via `workloadSpecFromActorTemplate`
   (`workflow_pause.go:144`, `workflow_suspend.go:144`). The Checkpoint RPC must
   carry the resolved bucket volume (creds) so atelet's suspend-time final
   export works → the env-less builder gains kubeClient + secretCache
   (`workflow.go:213,242` step construction).
6. atelet has an S3 client for snapshots (`cmd/atelet/internal/ategcs`) but **no
   constructor with explicit endpoint+creds** and none of
   EnsureBucket/HeadObject/file-backed PUT → add a dedicated `BucketClient`.
7. Template immutability (`self == oldSelf`) + `ensure_at_recreate_if_changed`
   (`deploy.sh`) handles the golden regen when the template changes. CRDs
   regenerate via `go generate ./pkg/api/...`; protos via
   `go generate ./internal/proto/...` (`hack/protoc.sh`).

## Milestones and files

### M1 — API / CRD / proto type

**`pkg/api/v1alpha1/actortemplate_types.go`**:

- Add `ObjectStoreBucketVolumeSource`:
  `{ secretRef: ObjectStoreBucketSecretRef (required), bucketPrefix: string
  (optional, default ""), exportIntervalSeconds: *int32 (optional, 1..86400,
  default 20) }`.
- Add `ObjectStoreBucketSecretRef`:
  `{ name (DNS subdomain), endpointKey, accessKeyIdKey, secretAccessKeyKey }` —
  key patterns mirror `SecretKeySelector`.
- `VolumeSource`: add
  `ObjectStoreBucket *ObjectStoreBucketVolumeSource json:"objectStoreBucket,omitempty"`;
  extend `+kubebuilder:validation:ExactlyOneOf={durableDir,externalVolumeTemplate,objectStoreBucket}`.
- `ActorTemplateSpec` new CEL rules:
  - ban `objectStoreBucket` when `sandboxClass == 'microvm'` (only the gVisor
    gofer path is implemented);
  - ban templates having both a `durableDir` volume and an `objectStoreBucket`
    volume (single `durabledir` annotation slot).

**`internal/proto/ateletpb/atelet.proto`**:

- `VOLUME_TYPE_OBJECT_STORE_BUCKET = 3`.
- New message `ObjectStoreBucketVolumeSource { endpoint, access_key_id,
  secret_access_key, bucket, export_interval_seconds }` — resolved values,
  carried in the `Volume.source` oneof (field 5) on Run/Checkpoint/Restore.

Regenerate: `go generate ./pkg/api/...` (controller-gen deepcopy + CRD
`manifests/ate-install/generated/ate.dev_actortemplates.yaml`) and
`go generate ./internal/proto/...`.

### M2 — ateapi resolution + RPC threading

**`cmd/ateapi/internal/controlapi/workload_spec.go`**:

- `workloadSpecFromActorTemplate` gains `(ctx, kubeClient, secretCache)` params:
  builds volumes (durableDir + external + **resolved bucket volumes**) but not
  env. `workloadSpecFromActorTemplateWithEnv` calls it, then resolves env.
- New `resolveObjectStoreBucketVolumes(ctx, resolver *envResolver, template,
  actor, workloadSpec)`:
  - for each template volume with `ObjectStoreBucket != nil` and mounted: read
    the Secret keys (endpoint/accessKeyId/secretAccessKey) via the existing
    `envResolver.secret` + `secretCache`; missing Secret/key →
    `codes.FailedPrecondition` (matches env semantics); actor nil → error;
  - bucket = `bucketPrefix + actor.GetMetadata().GetName()`;
  - append `&ateletpb.Volume{Name, Type: VOLUME_TYPE_OBJECT_STORE_BUCKET,
    Source: &ateletpb.Volume_ObjectStoreBucket{...}}`.

**`cmd/ateapi/internal/controlapi/workflow.go`**: add `kubeClient` +
`secretCache` to `CallAteletSuspendStep` (line 213) and `CallAteletPauseStep`
(line 242) construction; **`workflow_pause.go`/`workflow_suspend.go`**: those
steps call the new builder so the Checkpoint carries the resolved bucket volume
(suspend-time final export has creds).

### M3 — atelet mount + rehydrate/export + exclusion

**`cmd/atelet/internal/ategcs/bucket.go` (new)** — `BucketClient` over
aws-sdk-go-v2 (already a dependency), static creds + `BaseEndpoint` +
`UsePathStyle` (MinIO behind a Service):

- `NewBucketClient(ctx, endpoint, accessKeyID, secretAccessKey) (*BucketClient, error)`
- `EnsureBucket(ctx, bucket)` — HeadBucket → CreateBucket; tolerate
  `BucketAlreadyOwnedByYou` / `BucketAlreadyExists`
- `HeadObject(ctx, bucket, key) (present bool, err error)` — NotFound
  (`NotFound`/`NoSuchKey`/`NoSuchBucket`) → `(false, nil)`; other errors →
  error (fail-closed, never "treat 5xx as no data")
- `GetObjectToFile(ctx, bucket, key, dst)` / `PutObjectFromFile(ctx, bucket,
  key, src)` — file-backed for S3 signing (no streamed PUT)

**`cmd/atelet/bucketsync.go` (new)**:

- `const profileObjectKey = "profile.tar.gz"` (byte-matches admin badge and the
  old broker object).
- Pure, unit-testable functions:
  - `tarGzDir(dir, w) (files int, err error)` — regular files/dirs/symlinks
    only; excludes `*.log`, `*.sock`, `*.tmp`; rejects path escapes.
  - `untarGzDir(dst, r) error` — rejects `..`/absolute paths/symlink escapes.
  - `exportDirToBucket(ctx, dir, client, bucket) (bool, error)` — tar.gz to a
    temp file; **skip (no PUT) when 0 files**; one PUT of `profile.tar.gz`.
  - `rehydrateDirFromBucket(ctx, dir, client, bucket) (found bool, err error)`
    — EnsureBucket; HeadObject; if present: GET to temp file, untar into a
    `dir+".rehydrate"` sibling, atomic swap (RemoveAll the empty dir → Rename);
    `found=false` when the object is absent (guest seeds, D7).
- `bucketSyncManager` keyed by actorUID+volName:
  - `mountBucketVolume` (Run/Restore, before the guest boots): MkdirAll the
    host dir; **non-empty → local authoritative, no rehydrate**; empty →
    rehydrate-or-leave-empty;
  - `register` — ticker goroutine (default 20s), single-flight per item;
  - `finalExportAndStop(ctx, actorUID, volumes)` — synchronous export per
    bucket volume, fail-closed, stop the ticker.

**`cmd/atelet/main.go`**:

- `Run`/`Restore`: extend volume mounting to handle bucket volumes (mount +
  rehydrate decision) before `prepareOCIBundles`; register the syncer after the
  ateom RPC succeeds; unregister on error (defer).
- `Checkpoint`: after `CheckpointWorkload` returns, **before**
  `unmountExternalVolumes`/`resetActorDirs`: `finalExportAndStop` — an export
  error fails the Checkpoint (fail-closed → controller retries, no silent data
  loss).
- `resetActorDirs(actorUID, volumes)`: skip bucket-volume dirs (never
  `os.Remove` them — preserves data across suspend and avoids the
  non-empty-dir error).
- `buildAteomWorkloadSpec`: include bucket-volume mount paths in the
  per-container `DurableDirVolumes` list (DATA-scope `fscheckpoint -path`
  exclusion).
- `prepareOCIBundles` pause annotations: set the
  `dev.gvisor.spec.mount.durabledir.*` annotations from the **bucket volume
  host path** (`ateompath.VolumeHostPath`) when there is no durableDir volume
  (CEL guarantees no collision); keep existing durableDir behavior otherwise →
  Full-snapshot fs deltas exclude `/data/pi-agent`.

**`cmd/atelet/oci.go`**: `buildActorOCISpec` volume switch:
`VOLUME_TYPE_OBJECT_STORE_BUCKET` → bind from
`ateompath.VolumeHostPath(actorUID, vol)` (same as external).

### M4 — mf-pi rewiring (deletes the broker)

**`demos/mf-pi/mf-pi.yaml.tmpl` + `mf-pi-test.yaml.tmpl`**:

- ActorTemplate: add

  ```yaml
  volumes:
  - name: pi-agent-data
    objectStoreBucket:
      secretRef:
        name: mfpi-minio-admin
        endpointKey: endpoint
        accessKeyIdKey: root-user
        secretAccessKeyKey: root-password
  ```

  (no bucketPrefix → bucket = username, byte-matches the admin badge); the
  pi-web container gets `volumeMounts: [{name: pi-agent-data, mountPath:
  /data/pi-agent}]`.
- Supervisor `args`: delete the whole sync block (`sync_on`/`profile_matches`/
  `pull_profile`/`push_profile`/`do_sync`/sync loop/trap-sync, template lines
  ~133-280), leaving plain sessiond + socket-wait + web startup with a
  TERM/INT child-kill trap.
- env: remove `MFPI_ADMIN_URL`, `MFPI_PROFILE_TOKEN`, `MFPI_PROFILE_PUSH_INTERVAL`.
- `mfpi-minio-admin` Secret: add
  `endpoint: "http://mfpi-minio.ate-demo-mf-pi[-test].svc:9000"` (consumed by
  the volume Secret resolution).
- RBAC Role `ate-api-server-env-sources`: resourceNames =
  `mf-pi-provider-config` + `mfpi-minio-admin` (drop `mfpi-profile-token`);
  delete the `mfpi-profile-token` Secret doc.
- mfpi-admin Deployment env: drop `MFPI_PROFILE_TOKEN` (keep `MINIO_*` — badge
  only).

**`demos/mf-pi/admin/`**:

- `profile.go`: delete `GetProfile`/`PutProfile`, `handleInternalActor`,
  `profileGate`, `handleProfileGet/Put`, `maxProfileBytes`; keep
  `s3ProfileStore` with `EnsureBucket` + `HasProfile` (create-user bucket
  pre-create + the "已同步" badge via HeadObject).
- `main.go`: remove the `/internal/actor/` route + `profileToken` field/env;
  update the doc comment.
- `profile_test.go`/`main_test.go`: drop the internal-endpoint tests +
  `profileToken` in `newTestServer`; keep/adjust the ensure-bucket + badge
  tests.

**`demos/mf-pi/deploy.sh`, `deploy-test.sh`, `hack/install-demo-mf-pi.sh`,
`hack/install-demo-mf-pi-test.sh`**: drop `MFPI_PROFILE_TOKEN`
resolution/render; keep MinIO cred/digest logic;
`ensure_at_recreate_if_changed` regenerates the golden on template change.

**`demos/mf-pi/validate-templates.sh`**: rendered doc kinds without
`mfpi-profile-token`; assert the ActorTemplate declares
`volumes[].objectStoreBucket` + mount at `/data/pi-agent`; assert the
supervisor script has NO `pull_profile`/`push_profile`/`/internal/actor`;
assert no `MFPI_PROFILE_TOKEN`/`mfpi-profile-token` anywhere; assert the Role
includes `mfpi-minio-admin`; admin env has `MINIO_ENDPOINT`, no token.

### M5 — tests + docs

- `pkg/api/v1alpha1/actortemplate_validation_test.go`: new-source ExactlyOneOf;
  microvm ban; durableDir+objectStoreBucket ban; valid objectStoreBucket
  template.
- `cmd/ateapi/.../workload_spec_test.go`: bucket resolution on Run + env-less
  pause/suspend builders (fake Secret); missing key → FailedPrecondition;
  bucket = prefix+actorName; update existing call sites to the new signature.
- `cmd/atelet/bucketsync_test.go`: mount decision (empty+present→rehydrate;
  empty+absent→leave; non-empty→untouched; transient 5xx→error) with a fake
  `BucketClient`; export skip-empty; exclusions; tar/untar round-trip incl.
  unsafe-path rejection; suspend final export fail-closed.
- Docs: rewrite `demos/mf-pi/perUserMinioProfile.md` (two critical flows +
  bucket-name rule); update `README.md` §「每用户 MinIO Profile 持久化」+
  troubleshooting; trim `mfpi.md`. `injectDeepsseekKey.md` unchanged
  (auth.json now lands in the volume → bucket).
- `make fmt`, `go build ./...`, `make test`, `make verify`,
  `./demos/mf-pi/validate-templates.sh`.

## Progress tracker

- [x] **M1 — API / CRD / proto types**
  - [x] `pkg/api/v1alpha1/actortemplate_types.go`: `ObjectStoreBucketVolumeSource` +
        `ObjectStoreBucketSecretRef`; `VolumeSource.objectStoreBucket` member;
        ExactlyOneOf extended; CEL rules (microvm ban, durableDir+objectStoreBucket ban)
  - [x] `internal/proto/ateletpb/atelet.proto`: `VOLUME_TYPE_OBJECT_STORE_BUCKET` +
        `ObjectStoreBucketVolumeSource` message + oneof member
  - [x] Regenerate deepcopy/CRD (`go generate ./pkg/api/...`) + protos
        (`go generate ./internal/proto/...`)
- [x] **M2 — ateapi resolution + RPC threading**
  - [x] `cmd/ateapi/internal/controlapi/workload_spec.go`: builder gains
        ctx/kubeClient/secretCache; new `resolveObjectStoreBucketVolumes`
        (Secret keys → endpoint/creds; bucket = `bucketPrefix + actorName`;
        missing → FailedPrecondition)
  - [x] `workflow.go` + `workflow_pause.go`/`workflow_suspend.go`: thread
        kubeClient+secretCache into CallAteletPauseStep/CallAteletSuspendStep
        so Checkpoint carries the resolved bucket volume
  - [x] `workload_spec_test.go`: update call sites + bucket-resolution cases
- [x] **M3 — atelet mount + rehydrate/export + exclusion**
  - [x] `cmd/atelet/internal/ategcs/bucket.go` (new): `BucketClient` with
        explicit endpoint+static creds — `EnsureBucket`, `HeadObject`
        (NotFound→false,nil), file-backed `GetObjectToFile`/`PutObjectFromFile`
  - [x] `cmd/atelet/bucketsync.go` (new): `tarGzDir` (excl. `*.log|*.sock|*.tmp`),
        `untarGzDir` (safe), `exportDirToBucket` (skip 0-file),
        `rehydrateDirFromBucket` (atomic swap), `bucketSyncManager`
        (mount decision / register ticker / finalExportAndStop fail-closed)
  - [x] `cmd/atelet/main.go`: Run/Restore mount+rehydrate+register (defer
        unregister); Checkpoint final export before cleanup; `resetActorDirs`
        skips bucket dirs; `buildAteomWorkloadSpec` includes bucket mounts;
        pause `dev.gvisor.spec.mount.durabledir.*` annotations from the bucket
        host path (when no durableDir)
  - [x] `cmd/atelet/oci.go`: `buildActorOCISpec` binds the new volume type
  - [x] `cmd/atelet/bucketsync_test.go`: mount decision (empty+present→rehydrate;
        empty+absent→seed; non-empty→untouched; 5xx→error), skip-empty export,
        exclusions, round-trip + unsafe-path rejection, fail-closed suspend
- [x] **M4 — mf-pi rewiring (deletes the broker)**
  - [x] `mf-pi.yaml.tmpl` + `mf-pi-test.yaml.tmpl`: `volumes[].objectStoreBucket`
        (secretRef mfpi-minio-admin: endpoint/root-user/root-password) + mount
        `/data/pi-agent`; strip supervisor sync block + `MFPI_ADMIN_URL`/
        `MFPI_PROFILE_TOKEN`/`MFPI_PROFILE_PUSH_INTERVAL`; `mfpi-minio-admin`
        Secret gains `endpoint` key; RBAC resourceNames swap
        (`mfpi-profile-token` → `mfpi-minio-admin`); delete `mfpi-profile-token`
        Secret; admin Deployment drops token env
  - [x] `admin/profile.go` + `admin/main.go` + tests: delete Get/PutProfile +
        `/internal/actor/*` handlers + token gate; keep EnsureBucket + HasProfile
        badge
  - [x] `deploy.sh`/`deploy-test.sh`/`hack/install-demo-mf-pi.sh`/
        `install-demo-mf-pi-test.sh`: drop `MFPI_PROFILE_TOKEN` logic
  - [x] `validate-templates.sh`: assert volume+mount present, broker gone,
        Role lists `mfpi-minio-admin`
- [x] **M5 — tests + docs + verify**
  - [x] `pkg/api/v1alpha1/actortemplate_validation_test.go`: ExactlyOneOf,
        microvm ban, mutual-ban, valid bucket template
  - [x] Docs: rewrite `perUserMinioProfile.md`; update `README.md` §
        「每用户 MinIO Profile 持久化」; trim `mfpi.md`
  - [ ] `make fmt`, `go build ./...`, `make test`, `make verify`,
        `./demos/mf-pi/validate-templates.sh`

## Sequence diagrams

### Flow A — first mount of a brand-new user (empty bucket)

```
ateapi: ResumeActor
  resolve ActorTemplate spec
    read mfpi-minio-admin Secret (ns ate-demo-mf-pi)
    bucket := bucketPrefix + actor  -> HeadObject (NoSuchKey)
  send Restore{vol: {type:BUCKET, creds, bucket}, ...} ──► atelet
atelet Restore:
  resetActorDirs(uid)                       # skips new bucket host dir
  MkdirAll(bucketVolHostPath(uid,vol))
  empty dir + bucket empty -> NO rehydrate  # guest seeds (D7)
  prepareOCIBundles -> bind /data/pi-agent  # host dir shadows rootfs path
  ateom.RestoreWorkload (guest boots)
    bootstrap: materializes baked-in skills into /data/pi-agent (seed)
    pi-web runs; user writes -> /data/pi-agent (direct, local)
atelet ticker (20s): tar -> PUT profile.tar.gz to bucket   [export now on]
```

### Flow B — delete+recreate reset (same user, bucket has object)

```
delete-user alice  (suspend -> Checkpoint: final export PUTs latest tar -> delete actor)
create-user alice  (new actor UID) + resume
ateapi: Restore from golden Full snapshot (fs-delta has NO /data/pi-agent: excluded)
  resolve Secret + bucket; HeadObject -> PRESENT
  send Restore{vol: {creds, bucket}} ──► atelet
atelet Restore:
  resetActorDirs(new uid) ; MkdirAll(host dir)   # empty
  bucket object present -> GET profile.tar.gz -> untar into host dir (atomic swap)
  bind /data/pi-agent -> guest sees prior data (auth.json, sessions, skills)
  bootstrap: dir NOT empty -> seed is a no-op
pi-web runs: DeepSeek key (auth.json) present without re-injection
```

### Flow C — same-instance suspend/resume (local authoritative)

```
suspend: Checkpoint -> final export (dir quiescent) -> Full snapshot (fs-delta
         excludes mount) -> resetActorDirs KEEPS the bucket host dir
resume:  host dir NON-empty -> NO rehydrate (local is newer than bucket)
         bind -> guest resumes via process-memory restore, data intact
```

## Failure modes and handling

| Codepath | Failure | Handling |
|---|---|---|
| Rehydrate GET error vs NoSuchKey | Transient 5xx misread as "no data" → empty dir, lost data | Only NotFound → empty (guest seeds); network/5xx → fail Run/Restore so the controller retries (fail-closed, no silent data loss) |
| Secret resolution fails | Actor starts half-configured | `FailedPrecondition` abort, matching env semantics |
| Suspend export fails | Fresh data not flushed | Final export after `CheckpointWorkload`, before `resetActorDirs`; fail-closed → controller retries |
| Seed vs first export race | 20s export catches a half-seeded skills tree | Export skips when tar file count == 0; bootstrap seed is idempotent, so a torn first object self-heals next tick |
| Two pods export same bucket | Double-write of same object | One actor per worker + controller serialization; host dir is pod-local (`VolumeHostPath(uid)`). Object-tag/`If-Match` epoch deferred (D8) |
| durableDir + objectStoreBucket combined | Single `durabledir` annotation slot overwritten | Rejected by a new CEL rule |
| atelet loses the node-local dir (pod reschedule) | Local copy gone | Bucket is the source of truth; next mount rehydrates. Loss window = export interval (bounded, same as today) |

## Test review

Framework: Go unit (`pkg/api` envtest, `cmd/ateapi`, `cmd/atelet`) + template
`validate-templates.sh` + kind E2E. No JS test infra.

```
CODE PATHS                                              USER FLOWS
[+] pkg/api CEL/ExactlyOneOf                            [+] New user first mount (Flow A)
    [GAP] add: 3-source ExactlyOneOf + microvm ban          [→E2E] empty bucket seeds skills,
        + durableDir/objectStoreBucket mutual ban                  badge "未同步" then "已同步"
[+] ateapi workload_spec resolution                     [+] Reset auto-restore (Flow B)
    [GAP] add: bucket volume on Run AND env-less            [→E2E] delete+recreate -> prior data present,
          pause/suspend builders (creds threaded);                 DeepSeek key back w/o re-inject
          missing key → FailedPrecondition                         (acceptance)
[+] atelet buildActorOCISpec + volume type              [+] Same-instance suspend/resume (Flow C)
    [GAP] add: bind added; pause annotations;           [→E2E] local marker wins, no stale clobber
          RPC DurableDirVolumes list
[+] atelet prepare/mount decision                       [+] Suspend flush (Flow C + export)
    [GAP] add: empty+present→untar; empty+absent→seed;  [→E2E] marker in bucket after suspend
          non-empty→untouched (fake BucketClient);
          transient error → fail
[+] atelet sync ticker + export skip-empty + single-flight
    [GAP] add: unit tests
[+] atelet suspend final export (fail-closed)
    [GAP] add: unit test (export error → Checkpoint fails)
[+] mf-pi admin trim                                    [→E2E] failure drill: MinIO scaled to 0,
    [GAP] add: no /internal/actor/* routes; badge tests        suspend fails/retries, not silent drop
COVERAGE (planned): all new code paths get unit coverage; 3 E2E flows on kind
```

**E2E acceptance (kind, must still pass — the perUserMinioProfile.md flow):**

1. Deploy test env fresh (`deploy-test.sh`); template has the bucket volume; the
   supervisor no longer syncs.
2. Golden cold boot → golden Full snapshot fs-delta excludes `/data/pi-agent`.
3. Create `alice`, resume, write a marker into `/data/pi-agent`, wait >1
   interval, suspend → `alice/profile.tar.gz` in MinIO, admin badge "已同步".
4. Delete `alice`, recreate, resume → marker present (rehydrate). **This is the
   reset auto-restore acceptance test.**
5. Same-instance suspend/resume → local marker wins (no stale clobber).
6. Failure drill: MinIO down → suspend fails/retries rather than silently
   dropping.

## NOT in scope (deferred, with rationale)

- **Live FUSE mount (D4 A)** — needs `/dev/fuse` capability + an S3-POSIX-safe
  client; the sync volume meets the demo's reset guarantee today.
- **Raw file-per-object bucket mirror (D6 B)** — bucket stays `profile.tar.gz`;
  migrate when the FUSE feature lands.
- **Two-pod double-export fencing (object-tag epoch)** — deferred as a TODO
  (D8); add before platform-wide use.
- **Per-user bucket deletion / GC in MinIO** — delete-user still retains the
  bucket (settled); explicit bucket cleanup remains out of scope.
- **Migration of pre-existing user buckets** — the new atelet writes the same
  `profile.tar.gz` contract, so existing user data is read as-is; no migration
  needed.
- **pi-web seed-path env (mount_minio.md T6)** — unnecessary: the image's own
  bootstrap already materializes skills into `/data/pi-agent` at boot, which is
  exactly the D7 seed.

## What already exists (reuse, don't rebuild)

- atelet S3/MinIO object-store client (`cmd/atelet/internal/ategcs`) — pattern
  for the new per-volume endpoint+creds `BucketClient`.
- ateapi env `secretKeyRef` resolution + `secretCache`
  (`cmd/ateapi/internal/controlapi/workload_spec.go`) — reused for the
  VolumeSource Secret resolution.
- durableDir/external bind-mount + gofer path (`cmd/atelet/oci.go:302-318`,
  `volumes.go`) — mount mechanism reused verbatim for the new source kind.
- gVisor `dev.gvisor.spec.mount.durabledir.*` pause annotations (Full-snapshot
  exclusion) and the ateom RPC per-container `durable_dir_volumes` →
  `fscheckpoint -path` (DATA-scope exclusion) — exclusion mechanisms reused.
- mf-pi's dedicated per-namespace MinIO + `profile.tar.gz` object + admin
  `HeadObject` badge + `check-minio-users.sh` — kept unchanged (D5/D6).
- `ensure_at_recreate_if_changed` — handles the immutable-template golden regen
  when the template gains the volume.
