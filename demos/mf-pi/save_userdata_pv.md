# mf-pi: Persist userdata in a PersistentVolume (drop MinIO)

Branch: `mf_pi_userdata_in_pv` (from `27b0fc873cb73102a00e9f4dd51e2907f6969d2f`).

## Goal
Replace the per-user MinIO object store + in-actor broker sync with a persistent volume
mounted at `/data/pi-agent`, so a user's data survives an **actor image-refresh redeploy**
(delete + recreate). Drop the MinIO broker entirely.

## Hard constraints (from user)
- **Do NOT modify existing substrate source files' behavior/logic** (no API/CRD/proto/type changes,
  no changes to actor lifecycle). "Extend functionality" is allowed → add NEW files (a volume plugin
  + registry). The only unavoidable substrate touches are the two `getVolumePlugin()` wiring lines
  that select the new plugin (see §3). If even those 2 lines are off-limits, this approach cannot
  mount a real sticky volume into the actor and we must fall back to a demo-external solution.

## Approach (overview)
1. **Reuse the existing `ExternalVolumeTemplate` ActorTemplate volume type** — no API change.
   It already provisions a per-actor volume and mounts it into the actor sandbox.
2. **Add a NEW sticky volume plugin** (`internal/volume/sticky`) that implements the existing
   `VolumePluginControlPlane` + `VolumePluginWorkerPlane` interfaces. Difference from the mock:
   - `CreateVolume(ctx, name, ...)` returns `name` (which is `<atespace>-<actorName>-<volName>`,
     already actor-stable — see `actorVolumeID` in `cmd/ateapi/internal/controlapi/volumes.go:113`)
     as the `volumeID` instead of a per-create counter.
   - Backing dir = `<baseDir>/<volumeID>`; created on first `MountVolume` (symlink into the actor
     sandbox host path, exactly like `MockVolumePlugin.MountVolume`).
   - `DeleteVolume` is a **no-op on disk** (keeps the backing dir), so a delete+recreate reuses the
     same `volumeID`/dir → data persists. (The mock never cleaned dirs either; we make that explicit.)
   - `AttachVolume`/`DetachVolume` are no-ops (single-node kind demo).
   This makes `ExternalVolumeTemplate` effectively **sticky per-user** with no API change.
3. **Add a NEW tiny registry** in `internal/volume` (`registry.go`) so plugins self-register and
   `getVolumePlugin()` can resolve a default/selected plugin without hardcoding the mock.
4. **Wire the new plugin** by changing the two `getVolumePlugin()` sites (the only substrate edits):
   - `cmd/ateapi/internal/controlapi/volumes.go:32` (control plane)
   - `cmd/atelet/volumes.go:29` (worker plane)
   from `volume.NewMockVolumePlugin()` to the new sticky plugin (via the registry default).
5. **mf-pi demo** declares one `externalVolumeTemplate` volume (capacity + storageClass) mounted at
   `/data/pi-agent`; no supervisor sync, no broker, no token.

> Note on "PV": in the kind/gVisor demo there is no CSI, so the practical "persistent volume" is a
> node-local directory that survives actor delete+recreate. The plugin interface is plugin-swappable,
> so a real `k8spvc` implementation can replace `sticky` later without touching mf-pi.

## §3 Substrate files touched (minimal, flagged for approval)
- NEW `internal/volume/sticky/sticky.go` — sticky volume plugin (implements both interfaces).
- NEW `internal/volume/registry.go` — `Register`/`Default` for plugins.
- EDIT `cmd/ateapi/internal/controlapi/volumes.go` — `globalVolumePlugin = volume.DefaultPlugin()`.
- EDIT `cmd/atelet/volumes.go` — `globalVolumePlugin = volume.DefaultPlugin()`.
- (No changes to `actortemplate_types.go`, proto, deepcopy, CRD, atelet mount logic, actor lifecycle.)

## §4 mf-pi demo changes (drop MinIO) — inventory
1. `demos/mf-pi/mf-pi.yaml.tmpl` — remove `mfpi-minio` Deployment/PVC/Service, `mfpi-minio-admin`
   Secret, in-actor supervisor `pull/push/do_sync` block + `.mfpi-profile-actor`, env
   `MFPI_ADMIN_URL`/`MFPI_PROFILE_TOKEN`/`MFPI_PROFILE_PUSH_INTERVAL`, `mfpi-profile-token` Secret +
   RBAC, MinIO `snapshotsConfig` note. ADD: `externalVolumeTemplate` volume + `/data/pi-agent` mount.
2. `demos/mf-pi/mf-pi-test.yaml.tmpl` — same.
3. `demos/mf-pi/admin/profile.go` — DELETE (entire S3 broker).
4. `demos/mf-pi/admin/profile_test.go` — DELETE.
5. `demos/mf-pi/admin/main.go` — remove `profiles`/`profileToken` fields, `MINIO_*` config/env,
   `/internal/actor/{name}/profile` route, `setProfileSync`, `ensureProfileBucket`.
6. `demos/mf-pi/admin/main_test.go` — drop `profileToken`/`profiles` in `newTestServer`.
7. `demos/mf-pi/deploy.sh` / `deploy-test.sh` — drop `MFPI_PROFILE_TOKEN`, `MINIO_ROOT_*`,
   `MINIO_IMAGE`, minio digest localization + `sed` substitutions.
8. `demos/mf-pi/check-minio-users.sh` — drop/repurpose (talks to MinIO).
9. `demos/mf-pi/validate-templates.sh` — rewrite: assert NO minio/profile-token artifacts; assert the
   new `externalVolumeTemplate` volume + `/data/pi-agent` mount present.
10. `demos/mf-pi/admin/index.html` — the "已同步" badge reads MinIO object; update/remove.
11. `demos/mf-pi/perUserMinioProfile.md` — rewrite as the sticky-PV design doc.
12. `demos/mf-pi/mfpi.md` + `README.md` — drop MinIO persistence / profile-token references.

## §5 Verification
- `go build ./...` and `go test ./cmd/ateapi/... ./cmd/atelet/... ./internal/volume/... ./demos/mf-pi/admin/...`.
- `demos/mf-pi/validate-templates.sh` passes.
- Manual demo check: create actor → write file in `/data/pi-agent` → refresh image (delete+recreate)
  → file still present in new instance.
- `make verify` (gofmt/boilerplate/licenses/go-modules/go-generate) after `hack/update/go-modules.sh`
  if new deps are added (the sticky plugin needs no new deps — pure stdlib + `ateompath`).

## Todo list (progress tracking)

- [x] **#7 Implement sticky volume plugin package** — `internal/volume/sticky/sticky.go`
      implementing both `VolumePluginControlPlane` + `VolumePluginWorkerPlane`. `CreateVolume`
      returns the actor-stable `name` (`<atespace>-<actorName>-<volName>`) as `volumeID` (no
      per-create counter); backing dir `baseDir/volumeID` created on `MountVolume` (symlink into
      sandbox host path, like `MockVolumePlugin`); `DeleteVolume` is a no-op on disk (keeps dir) so
      delete+recreate reuses the same data; `Attach`/`Detach` no-ops. No new deps (stdlib + `ateompath`).
- [x] **#8 Add volume plugin registry** — `internal/volume/registry.go` with `Register`/`Default`
      so plugins self-register and `getVolumePlugin()` resolves the selected plugin without
      hardcoding the mock.
- [x] **#9 Wire `getVolumePlugin` in controlapi + atelet** — change the only substrate wiring lines:
      `cmd/ateapi/internal/controlapi/volumes.go:32` and `cmd/atelet/volumes.go:29` from
      `volume.NewMockVolumePlugin()` to the new sticky plugin via `volume.DefaultPlugin()`.
- [x] **#10 Update `mf-pi.yaml.tmpl` (drop MinIO)** — remove `mfpi-minio` Deployment/PVC/Service,
      `mfpi-minio-admin` Secret, in-actor supervisor `pull/push/do_sync` block + `.mfpi-profile-actor`,
      env `MFPI_ADMIN_URL`/`MFPI_PROFILE_TOKEN`/`MFPI_PROFILE_PUSH_INTERVAL`, `mfpi-profile-token`
      Secret + RBAC, MinIO `snapshotsConfig` note; ADD `externalVolumeTemplate` volume + `/data/pi-agent` mount.
- [ ] **#11 Update `mf-pi-test.yaml.tmpl` (drop MinIO)** — same changes as #10 for the test template.
- [ ] **#12 Delete admin S3 broker** — delete `demos/mf-pi/admin/profile.go` and
      `demos/mf-pi/admin/profile_test.go` (entire `profileStore`/`s3ProfileStore` broker).
- [ ] **#13 Rewire `admin/main.go` + tests** — remove `profiles`/`profileToken` fields, `MINIO_*`
      config + env, `/internal/actor/{name}/profile` route, `setProfileSync`, `ensureProfileBucket`;
      update `main_test.go` (`newTestServer`).
- [ ] **#14 Update deploy scripts** — `deploy.sh` + `deploy-test.sh`: drop `MFPI_PROFILE_TOKEN`,
      `MINIO_ROOT_*`, `MINIO_IMAGE`, minio digest localization + `sed` substitutions.
- [ ] **#15 Update verifier scripts** — `check-minio-users.sh` drop/repurpose; rewrite
      `validate-templates.sh` to assert NO minio/profile-token artifacts and the new
      `externalVolumeTemplate` volume + `/data/pi-agent` mount present.
- [ ] **#16 Update docs + UI badge** — `admin/index.html` "已同步" badge (reads MinIO); rewrite
      `perUserMinioProfile.md` as sticky-PV doc; update `mfpi.md` + `README.md` (drop MinIO refs).
- [ ] **#17 Build, test, verify, manual check** — gofmt/build/vet/test (ateapi, atelet, internal/volume,
      mf-pi/admin); `validate-templates.sh`; `make verify`; manual: create actor → write
      `/data/pi-agent` file → refresh image (delete+recreate) → confirm file persists.
