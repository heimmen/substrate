# 将用户数据持久化到 MinIO（objectStoreBucket 卷，actor 重置后自动恢复）

> 本文档描述 mf-pi **现行**的每用户数据持久化机制（v2）。目标：**每个 mf-pi 用户
> （一个用户 == 一个运行 pi-web 的 Actor）的数据——安装的 skills、会话
> (sessions)、`auth.json`(DeepSeek key 等凭据)、`models.json`、`settings.json`——
> 持续备份到该用户专属的 MinIO bucket**，用户之间完全隔离；**若 actor 实例被删除
> 重建（重置），bucket 会再次自动挂载，数据无需任何手工恢复**。
>
> 与 v1（mfpi-admin 作为 S3 broker + actor 内 shell 脚本 tar/curl 同步）不同：v2
> 把这件事做成了 **ate 核心卷类型 `objectStoreBucket`**，同步由节点代理 atelet 完
> 成，actor 内不再有任何同步逻辑。完整设计与实施记录见 `mount_minio_v2.md`。
> 范围覆盖**生产**（`ate-demo-mf-pi` / `mfpi`）与**测试**（`ate-demo-mf-pi-test` /
> `mfpi-test`）两套环境，各自一套独立 MinIO。

## 三条需求 → 机制

1. **把用户的 MinIO bucket 挂进 agent actor 的 `/data/pi-agent`**：ActorTemplate
   声明一个 `objectStoreBucket` 卷（`volumes[].objectStoreBucket`，secretRef 指向
   `mfpi-minio-admin` Secret 的 `endpoint`/`root-user`/`root-password` 键）并挂载到
   `/data/pi-agent`。节点上它是一个 per-actor 的本地目录，经 runsc gofer 以 bind 方
   式进入 gVisor 沙箱（与 durableDir/external 卷同一机制）。
2. **agent 直接写 `/data/pi-agent`**：actor 内只剩「起 sessiond + 等 socket + 起
   web」的普通 supervisor。atelet 周期（默认 20s）把目录打成**一个**
   `profile.tar.gz` 对象推回 bucket（排除 `*.log`/`*.sock`/`*.tmp`）；挂起 / 删除
   前再做一次**最终推送（fail-closed）**。
3. **agent 重载（delete+recreate 重置）后自动重新挂载用户 bucket**：重置后的全新
   文件系统上，atelet 发现本地目录为空且 bucket 里有对象 → 下载并原子解包回挂载目
   录（rehydrate）；bucket 里没对象（全新用户）→ 留空，由 pi-web bootstrap 播种内
   置 skills。同一实例的 suspend/resume（Full 快照热恢复）本地目录仍在 → **本地为
   准，不覆盖**。

## 关键规则

- **bucket 命名 = `bucketPrefix + actorName`**。mf-pi 模板不设前缀，所以 bucket 名
  就是用户名（DNS-1123 名即合法 bucket 名）。该规则必须与 mfpi-admin 徽标的推导
  （`MINIO_BUCKET_PREFIX` + username）逐字节一致，两方才能看到同一个对象。
- **对象布局 = 每 bucket 一个 `profile.tar.gz`**（key 与 v1 兼容）：
  `check-minio-users.sh`、管理页「已同步」徽标（`HeadObject` LastModified）、以及
  存量用户 bucket 都**原样可用**，无需迁移。
- **凭据路径**：只有 ate-api-server 读 `mfpi-minio-admin` Secret（RBAC
  `ate-api-server-env-sources` 已列），解析后经 Run/Checkpoint/Restore RPC 把
  endpoint/凭据/bucket 传给 atelet；actor 与 atelet 都不读 k8s Secret。
- **快照隔离**：Full 快照的文件系统增量**排除** `/data/pi-agent`（gVisor
  durabledir 注解 + ateom RPC 排除列表），bucket 即真相；golden 基快照因此不含任
  何用户数据。
- **delete 保留 bucket**：`delete-user.sh` / 管理页删除只删 Actor，bucket 与对象
  刻意保留——这正是「重置 = 删除 + 重建」能自动恢复的原因。

## 三个关键流程

### A — 全新用户（bucket 为空）
```
resume → ateapi 解析 Secret + bucket → 从 golden 基 Restore（fs 增量不含 /data/pi-agent）
atelet: 新 actorUID → 本地目录为空；bucket 无对象 → 不 rehydrate（留空）
        bind /data/pi-agent → bootstrap 播种内置 skills → 用户开始写
atelet: 每 20s 导出 profile.tar.gz → 首个对象落 bucket，徽标变「已同步」
```

### B — 重置（delete+recreate，验收路径）
```
suspend: 最终导出把最新数据推上 bucket → 删除 actor（bucket 保留）
create + resume: 新 actorUID → 本地目录为空；bucket 有对象 → 下载 + 原子解包
        bind → auth.json/skills/sessions 全部回来，DeepSeek key 无需重新注入
```

### C — 同实例 suspend/resume（本地为准）
```
suspend: 最终导出 → Full 快照（fs 增量排除挂载点）→ resetActorDirs 保留 bucket 目录
resume:  本地目录非空 → 不 rehydrate（绝不被更旧的 bucket 副本覆盖）
```

## 故障语义（fail-closed）

| 场景 | 处理 |
|---|---|
| rehydrate 时对象存储 5xx/网络错误 | 只有确认 404 才视为「无数据」；其它错误让 Run/Restore 失败，控制器重试（绝不静默当空） |
| Secret/键缺失 | Run/Restore 直接 `FailedPrecondition`（与 env secretKeyRef 同语义） |
| 挂起时最终导出失败 | Checkpoint 失败（DataLoss，与快照上传失败同一契约），不静默丢弃 |
| 周期导出失败 | 仅告警重试；损失窗口 = 导出间隔（≤20s），最终导出兜底 |
| 空 tree | 不上传（0 文件跳过），空 tar 永不覆盖真实 profile |

## mfpi-admin 的角色（只读）

`admin/profile.go` 只保留两件事：create-user 时 best-effort `EnsureBucket` 预建
bucket（atelet 首次挂载也会懒建，双保险），以及用户列表「MinIO Profile」徽标——直
接 `HeadObject` S3 对象（MinIO 即真相，无需镜像 Secret）。v1 的
`GET/PUT /internal/actor/{name}/profile` 网关与 `MFPI_PROFILE_TOKEN` 已整体删除。

## 重置一个用户并验证自动恢复

```bash
./delete-user.sh alice        # 删除 Actor（bucket alice 保留）
./create-user.sh alice        # 重建同名 Actor（挂起态）
kubectl ate resume actor alice -a mfpi   # 挂载时 atelet 自动从 bucket rehydrate
```

拉回后，alice 的专属 DeepSeek key（`auth.json`）、已安装的 skills 与会话历史自动
出现，**无需**重新注入。管理页徽标在首个导出周期后恢复「已同步」。

## 环境隔离

| 项 | 生产 | 测试 |
|---|---|---|
| Namespace | `ate-demo-mf-pi` | `ate-demo-mf-pi-test` |
| MinIO（profile 存储） | `mfpi-minio`@`ate-demo-mf-pi` | `mfpi-minio`@`ate-demo-mf-pi-test` |
| MinIO Secret（含 endpoint 键） | `mfpi-minio-admin` | 同左（test ns 内） |
| bucket 名 | `<username>` | `<username>` |

> [!NOTE]
> 卸载（`--delete-demo-mf-pi*`）会连同命名空间删除 `mfpi-minio` 的 PVC 与其中所
> 有 bucket/对象。profile 持久化是**随命名空间**的；若需在卸载后保留，请另行备份
> `mfpi-minio-data` PVC。
