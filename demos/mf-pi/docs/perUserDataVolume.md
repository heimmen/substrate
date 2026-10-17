# 将用户数据持久化到 sticky 持久卷（PV）（每用户独立存储，actor 重置后自动恢复）

> 本文档是 mf-pi 用户数据持久化的设计说明与进度跟踪（实施 TODO 见文末）。目标：
> **每个 mf-pi 用户（一个用户 == 一个运行 pi-web 的 Actor）的数据——安装的
> skill、会话 (sessions)、`auth.json`(DeepSeek key 等凭据)、`models.json`、
> `settings.json`、以及 `$HOME` 下用户自己安装的 Pi 包（`.pi/packages`）——保存在
> 一个 sticky 的 per-actor 持久卷上，用户之间完全隔离；若用户的 actor 实例被删除
> 重建（重置，例如刷新 actor 镜像后重新部署），这些数据不受影响，新实例自动看到
> 旧数据**。与既有 Full 快照 suspend/resume 形成「快照 + 持久卷双层」：同一实例
> 的热恢复仍走 Full 快照（进程内存 + 文件系统增量），持久卷保证实例被重置后的数据
> 恢复。范围覆盖**生产**（`ate-demo-mf-pi` / `mfpi`）与**测试**
> （`ate-demo-mf-pi-test` / `mfpi-test`）两套镜像环境。
>
> 根因分析与「为什么必须这样绕」见 `persistence_gaps.md`。

## 方案：复用 `externalVolumeTemplate` + sticky volume 插件

**不修改 substrate 既有代码逻辑**（无 API/CRD/proto/类型改动），通过**扩展**实现：

- ActorTemplate 的 `VolumeSource` 已有 `externalVolumeTemplate`（per-actor 外部
  卷），ateapi 经 volume 插件创建、atelet 挂载进 actor 沙箱——链路完整，直接复用。
- substrate 原有插件是 `MockVolumePlugin`：backing 目录按**每次创建自增的
  counter**（`mock-vol-N`）命名，删除重建拿到新目录 → 数据不粘。
- 新增 **sticky volume 插件**（`internal/volume/sticky/sticky.go`，新文件）：
  - `CreateVolume` 直接返回**稳定名** `name`（即
    `<atespace>-<actorName>-<volName>`，见 `controlapi.actorVolumeID`）作为
    `volumeID`，同一用户的每次创建都映射到同一 backing 目录。
  - backing 目录 `<BasePath>/stickyvolumes/<volumeID>`；`MountVolume` 创建目录并
    symlink 到挂载点（与 mock 相同，避免 atelet 需要双向 mount propagation）。
  - `DeleteVolume` **刻意不删目录**：用户数据在删除重建后保留，供新实例重挂。
  - `Attach/Detach` 为 no-op（单节点 kind 演示，无可拆卸硬件）。
- 新增 **volume 插件注册表**（`internal/volume/registry.go`，新文件）：
  `RegisterDefault` / `DefaultPlugin`，插件经 `init()` 自注册；未注册时回落到
  mock，既有行为与测试不受影响。仅在 `cmd/ateapi/internal/controlapi/volumes.go`
  与 `cmd/atelet/volumes.go` 两处把 `volume.NewMockVolumePlugin()` 换成
  `volume.DefaultPlugin()`（blank import sticky 完成注册）——这是对 substrate 既有
  文件的全部改动。

## mf-pi 侧（配合改动，替代 MinIO 方案）

- `mf-pi.yaml.tmpl` / `mf-pi-test.yaml.tmpl`：ActorTemplate 声明
  `volumes: [{name: userdata, externalVolumeTemplate: {capacity: 5Gi,
  storageClassName: standard}}]`，pi-web 容器 `volumeMounts` 挂载到
  `/data/pi-agent`；supervisor 脚本去掉 `pull/push/do_sync` 同步逻辑；**删除**
  `mfpi-minio` Deployment/PVC/Service、`mfpi-minio-admin` Secret、
  `mfpi-profile-token` Secret 及其 RBAC、`MFPI_ADMIN_URL` /
  `MFPI_PROFILE_TOKEN` / `MFPI_PROFILE_PUSH_INTERVAL` 环境变量。
- `admin/`：**删除** `profile.go` / `profile_test.go`（S3 broker、token 网关、同步
  徽标）；`main.go` 去掉 `profiles` / `profileToken`、`MINIO_*` 配置与
  `/internal/actor/{name}/profile` 路由；UI 用户列表去掉「MinIO Profile」徽标。
- 脚本：`deploy.sh` / `deploy-test.sh` / `hack/install-demo-mf-pi*.sh` 去掉 token
  与 MinIO 凭据/镜像处理；`validate-templates.sh` 改为断言**无** MinIO/token 残留
  且 userdata 卷 + `/data/pi-agent` 挂载存在；删除 `check-minio-users.sh`。

## 数据生命周期

| 事件 | 行为 |
|---|---|
| 首次创建用户 | `CreateVolume(稳定名)` → `MountVolume` 创建空 backing 目录并 symlink |
| 写入 `/data/pi-agent`（含 `$HOME`/`$XDG_CONFIG_HOME`） | 直接落在 worker 节点 backing 目录（持久卷） |
| 挂起 / 恢复（Full 快照） | 进程与 rootfs 走快照；卷目录原样保留，重挂即原数据 |
| **删除（重置）** | actor 删除；backing 目录**保留** |
| **重建同名用户 / 镜像更新后重新部署** | 同一稳定名 → 重新挂到同一目录，**旧数据自动可见**；新镜像功能 + 旧数据 |
| 卸载整个演示 | 命名空间资源删除；backing 目录不随之删除，需到节点手动清理 |

> [!IMPORTANT]
> **镜像更新后的 resume 必须绕过 golden base 快照。** 平台的 golden 快照记录的
> 是 golden actor 自己那份卷的 9p 挂载树，对每用户卷不可恢复（空卷 → `failed to
> walk "skills"` → 卡 `STATUS_RESUMING`）。因此：
> - `deploy.sh` / `deploy-test.sh` 在 apply 后自动执行 `ensure-no-golden.sh` 清空
>   `status.goldenSnapshot`（`MFPI_KEEP_GOLDEN=1` 可关闭，不推荐）；
> - 所有 refresh / 测试脚本的 resume 都带 `--boot`。
> 代价是模板变更后每个 actor 首次 resume 为冷启动（~25–40s，有载入提示页）。

## 边界与后续

- 演示环境（kind/gVisor）无 CSI，持久卷即节点本地目录：数据粘在**所在 worker 节
  点**，不随 actor 迁移到其他节点。`deploy.sh` / `deploy-test.sh` 默认拒绝
  **多节点**部署（`MFPI_ALLOW_MULTI_NODE=1` 覆盖），避免静默空卷。接口
  （`VolumePluginControlPlane` / `VolumePluginWorkerPlane`）保持可插拔，后续可实现
  真实 `k8spvc` 插件替换 sticky，mf-pi 模板无需改动。
- 同名用户被删除后重建会**继承**旧数据（与原 MinIO bucket 保留语义一致）；如需彻
  底重置某用户，需清理其 backing 目录。
- sticky 目录由插件创建后不再回收（与 mock 相同），节点磁盘回收交给演示环境管理；
  可用 `list-pvs.sh --detail` 巡检体积。
- `capacity` / `storageClassName` 被 sticky 插件忽略（无配额）。
- 项目工作目录若建在 `/data/pi-agent` 或 `$HOME` 之外（如 `/dtom2`），刷新后只会
  被重建为空目录；supervisor 会对这类 cwd 打 warning。

## Todo list（进度跟踪）

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
- [x] **#11 Update `mf-pi-test.yaml.tmpl` (drop MinIO)** — same changes as #10 for the test template.
- [x] **#12 Delete admin S3 broker** — delete `demos/mf-pi/admin/profile.go` and
      `demos/mf-pi/admin/profile_test.go` (entire `profileStore`/`s3ProfileStore` broker).
- [x] **#13 Rewire `admin/main.go` + tests** — remove `profiles`/`profileToken` fields, `MINIO_*`
      config + env, `/internal/actor/{name}/profile` route, `setProfileSync`, `ensureProfileBucket`;
      update `main_test.go` (`newTestServer`).
- [x] **#14 Update deploy scripts** — `deploy.sh` + `deploy-test.sh`: drop `MFPI_PROFILE_TOKEN`,
      `MINIO_ROOT_*`, `MINIO_IMAGE`, minio digest localization + `sed` substitutions.
- [x] **#15 Update verifier scripts** — `check-minio-users.sh` drop/repurpose; rewrite
      `validate-templates.sh` to assert NO minio/profile-token artifacts and the new
      `externalVolumeTemplate` volume + `/data/pi-agent` mount present.
- [x] **#16 Update docs + UI badge** — `admin/index.html` "已同步" badge (reads MinIO); rewrite
      `perUserMinioProfile.md` as sticky-PV doc; update `mfpi.md` + `demos/mf-pi/README.md` (drop MinIO refs).
- [ ] **#17 Build, test, verify, manual check** — gofmt/build/vet/test (ateapi, atelet, internal/volume,
      mf-pi/admin); `validate-templates.sh`; `make verify`; manual: create actor → write
      `/data/pi-agent` file → refresh image (delete+recreate) → confirm file persists.

## 镜像更新持久化修复（不改 substrate）

根因与取舍见 `persistence_gaps.md`；以下全部只改 `demos/mf-pi/`。

- [x] **D1 `$HOME` / `$XDG_CONFIG_HOME` 上卷** — 8 个 ActorTemplate 设
      `HOME=/data/pi-agent/home`、`XDG_CONFIG_HOME=/data/pi-agent/config`；supervisor
      `mkdir -p` + 从镜像 `/data/home` 非破坏播种。pi-web 用户级 Pi 包
      （`$HOME/.pi/packages`）因此跨镜像更新保留。
- [x] **D2 绕过 golden base 快照** — 新增 `ensure-no-golden.sh`；`deploy.sh` /
      `deploy-test.sh` 在 apply 后与 refresh 后各清一次（`MFPI_KEEP_GOLDEN=1` 关闭）；
      `refresh-actor*.sh` 与各 `test-*.sh` 的 resume 加 `--boot`。
- [x] **D3 多节点防护** — deploy 前置检查节点数，>1 时拒绝（`MFPI_ALLOW_MULTI_NODE=1`）。
- [x] **D4 模板变更钩子 + E2E** — `PI_WEB_DEPLOY_REV`（deploy 由 `MFPI_DEPLOY_REV`
      渲染）；新增 `test-image-update-persistence.sh`（skill / `$HOME/.pi/packages` /
      project 文件三种哨兵 + 真实 redeploy + 新用户空卷 resume）。
- [x] **D5 校验与文档** — `validate-templates.sh` 断言新 env/脚本；`exec-actor.sh` /
      `list-pvs.sh` / `recover-actors.sh` 注释；`persistence_gaps.md`、`actor_inside.md`、
      `README.md` 更新存活矩阵。
- [ ] **D6 真机验证** — 在 kind 上跑 `./test-image-update-persistence.sh` 与
      `./test-userdata-persistence.sh`，确认全绿（需要已部署的测试环境 + nginx）。
