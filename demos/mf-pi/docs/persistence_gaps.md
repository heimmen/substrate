# 为什么用户自己安装的 skill / 文件在 actor 更新镜像后会丢

本文是一次根因调查的记录，以及 demo 侧（只改 `demos/mf-pi/`，不动 substrate）的修复方案。
结论先行：

1. **用户自己安装的 Pi 包 / skill 落在 `$HOME/.pi` 下，而唯一持久面只有
   `/data/pi-agent`**。镜像里 `HOME=/data/home`，所以 `POST /api/pi-packages/install`
   装出来的东西（`installedPath: "/home/alice/.pi/packages/..."`，
   `docs/user_agent_api.md` §6.1）实际在 `/data/home/.pi/...`——沙箱 rootfs 上，
   任何「新实例」都会丢。
2. **镜像更新只能 `delete actor` + `create actor`（冷启动）**。ActorTemplate spec
   不可变、镜像按 digest 固定，换镜像后 Full 快照不可复用，rootfs 由新镜像重建；
   只有 bind mount 的外部卷还在。
3. **平台的 golden base 快照与 per-actor 外部卷不兼容**，镜像更新恰好会重新生成
   golden 快照，于是新用户恢复直接失败、老用户的卷内容也不在快照的挂载树里。
   demo 以前靠 `recover-actors.sh` 清 `status.goldenSnapshot` 规避，现在 `deploy.sh`
   会自动清（`ensure-no-golden.sh`）。
4. **所谓 "PV" 其实是 worker 节点本地目录**（`/var/lib/ateom-gvisor/stickyvolumes`），
   没有 CSI/PVC、没有卷亲和。多节点集群下 resume/refresh 落到别的节点会静默挂空卷。
   因此 demo 现在默认拒绝多节点部署（`MFPI_ALLOW_MULTI_NODE=1` 可覆盖）。

---

## 1. 现状链路（代码证据）

| 事实 | 证据 |
|---|---|
| 只挂了一个 `userdata` 卷到 `/data/pi-agent` | `mf-pi.yaml.tmpl`（`volumeMounts` / `volumes`） |
| pi-web 数据目录/agent 目录被指到 PV，`HOME` 原来没有 | `mf-pi.yaml.tmpl` env（`PI_WEB_DATA_DIR`、`PI_CODING_AGENT_DIR`），`docs/mfpi.md`「镜像其余关键 ENV」 |
| 用户级 Pi 包安装到 `$HOME/.pi/packages` | `docs/user_agent_api.md` §6.1/§6.2（`piPackageService` 的 `installedPath`） |
| 镜像更新 = 删模板 + 删/建 actor | `deploy.sh`（`ensure_at_recreate_if_changed`、`refresh_existing_users`）、`refresh-actor.sh` |
| rootfs 每次重建 | `cmd/atelet`（`resetActorDirs` 在 Run/Restore 前清 overlay/bundle） |
| golden 快照由模板 controller 生成、resume 时优先使用 | `cmd/atecontroller/.../actortemplate_controller.go`、`cmd/ateapi/.../workflow_resume.go` |
| 9p 挂载恢复会 walk 快照记录的路径树 | `recover-actors.sh` 头部（`failed to walk "skills" ... no such file`） |
| sticky 卷是节点本地目录、Attach/Detach 为 no-op | `internal/volume/sticky/sticky.go`、`cmd/atecontroller/.../workerpool_apply.go`（hostPath） |
| 调度约束里只有「本地快照节点」，没有卷节点 | `cmd/ateapi/.../workflow_resume.go`（`schedulingConstraints`） |

### 1.1 三种「重置」的差异（修复前的真实语义）

| 操作 | 沙箱 | `/data/pi-agent` 卷 | `$HOME` / `$XDG_CONFIG_HOME`（修复前在 rootfs） | 镜像内置 skill |
|---|---|---|---|---|
| suspend / resume | 同一实例现场还原 | 不变 | 随 Full 快照还原 | 随快照 |
| delete + recreate（同镜像） | 新沙箱 | 原样重挂 | **丢** | supervisor 循环补装 |
| **镜像更新（换 digest / 改模板）** | 新沙箱 + 新镜像 | 原样重挂（但受 golden 快照问题影响） | **丢** | supervisor 循环补装 |
| 管理员托管 skill | — | 每 10s 从 admin PVC 自愈 | — | — |

用户级 Pi 包 / 用户自建 skill 属于第二、三行里「丢」的那一列——这就是问题。

## 2. golden base 快照为什么不兼容（镜像更新的直接触发点）

1. controller 用同一个模板在保留 atespace `ate-golden` 里建一个 golden actor，跑一次
   后 suspend，得到 Full 快照写到 `status.goldenSnapshot`。
2. 这个快照里记录了 9p 挂载 `mf-agent:/data/pi-agent` 的**路径树**——那是 **golden
   actor 自己那份卷**的内容（跑过 bootstrap，所以有 `skills` 等）。
3. 恢复时，runsc 拿这份路径树去 walk **目标 actor 的卷**。目标卷是空的（新用户）或
   内容不同（老用户），于是：
   - 缺路径 → `failed to walk "skills" ... no such file or directory`，actor 卡在
     `STATUS_RESUMING`（恢复失败不会置 CRASHED，工作流会一直重试）；
   - 多出来的用户文件不在快照树里，恢复后的可见性不可依赖。
4. 镜像更新必然重建 ActorTemplate → controller 重新生成 golden 快照 → 上述问题被
   **每一次镜像更新重新触发**。

平台侧的正确修法（本次不改 substrate，只记录）：

- 模板挂载了 `externalVolumeTemplate` 时，不生成、也不使用 golden 快照；
- 或者恢复前按快照 manifest 预置卷内的挂载路径；
- 长期：为外部卷提供真正可移植的存储（PVC/CSI），而不是节点本地目录。

## 3. demo 侧修复（已实施）

### 3.1 把 `$HOME` / `$XDG_CONFIG_HOME` 搬进 `userdata` 卷

8 个 ActorTemplate（`mf-pi`、`-small`、`-mid`、`-large` × 生产/测试）都加了：

```yaml
- name: HOME
  value: /data/pi-agent/home
- name: XDG_CONFIG_HOME
  value: /data/pi-agent/config
```

supervisor 启动时：

```sh
mkdir -p "$HOME" "${XDG_CONFIG_HOME:-$HOME/.config}"
[ -d /data/home ] && [ "$HOME" != /data/home ] && cp -an /data/home/. "$HOME"/ || true
```

- `~/.pi/packages`、`~/.npm`、`$XDG_CONFIG_HOME/*` 因此都在卷上，用户自己安装的
  Pi 包 / skill 跨镜像更新保留；
- 镜像 `/data/home` 里若有构建期 dotfile，会被非破坏性地播种进新 HOME；
- 不新增卷、不改现有卷布局：老用户的卷里只是多出 `home/`、`config/` 两个目录。

> [!NOTE]
> 本次变更第一次部署时，模板 spec 变化会触发**一次性**的「模板替换 + 所有用户
> refresh」：每个用户会冷启动一次（~25–40s）。旧实例 rootfs 里的 `$HOME` 内容本来
> 就会在冷启动时消失，无法迁移；能保留的是卷上的数据。

### 3.2 让 mf-pi 永不使用 golden base 快照

- 新增 `ensure-no-golden.sh`：等 4 个模板 `status.phase=Ready` 后把
  `status.goldenSnapshot` 清空。controller 对 `phase=Ready` 是 no-op，因此不会再生成；
  之后所有 resume 走「冷启动」分支（`--boot` 等价语义）。
- `deploy.sh` / `deploy-test.sh`：`ko apply` 后、user refresh 前调用一次，refresh 后再补
  一次（兜底慢集群/竞态）；`MFPI_KEEP_GOLDEN=1` 可关闭。
- `refresh-actor.sh`（及其 `-test` 变体）与各个 `test-*.sh` 的 resume 都显式加 `--boot`。
- 代价：模板变更后 actor 首次 resume 是冷启动（pi-web 约 25–40s，已有「Agent 正在
  载入」提示页覆盖）；之后 suspend/resume 仍走 actor 自己的快照，仍然很快。
- `recover-actors.sh` 保留为旧部署/应急路径。

### 3.3 多节点防护

`deploy.sh` / `deploy-test.sh` 在部署前检查节点数：>1 时直接报错退出（
`MFPI_ALLOW_MULTI_NODE=1` 覆盖），因为 sticky 卷是节点本地目录，跨节点会静默空卷。

### 3.4 测试

- 新增 `test-image-update-persistence.sh`：写三个哨兵（用户 skill、
  `$HOME/.pi/packages`、project 文件）→ `MFPI_DEPLOY_REV=<ts>` 触发真实模板变更
  redeploy → 断言哨兵逐字节存活、actor 不卡 `STATUS_RESUMING`、模板无 golden 快照、
  且新建用户（空卷）能正常 resume。
- `test-userdata-persistence.sh` 增加 HOME 在 PV 上的 in-actor 断言，并改用 `--boot`。

## 4. 修复后的存活矩阵

| 路径 | suspend/resume | delete+recreate（同镜像） | **镜像更新** |
|---|---|---|---|
| `/data/pi-agent/*`（auth.json、sessions、skills、projects.json…） | ✅ | ✅ | ✅ |
| `$HOME` = `/data/pi-agent/home`（含 `.pi/packages`、`.npm`） | ✅ | ✅ | ✅ |
| `$XDG_CONFIG_HOME` = `/data/pi-agent/config` | ✅ | ✅ | ✅ |
| `/data/pi-agent/projects/*`（推荐的项目根） | ✅ | ✅ | ✅ |
| project cwd 在持久面之外（如 `/dtom2`） | ✅ | ⚠️ 只重建空目录 | ❌ |
| `/usr`、`/opt`、`/etc`、`/data` 其他子目录、`/tmp` | 随快照 | ❌ | ❌ |
| 管理员托管 skill | ✅ | ✅（10s 自愈） | ✅ |

项目路径指引：在 UI 里新建 project 时请把路径放在 `$HOME`（`/data/pi-agent/home`）
或 `/data/pi-agent` 下；否则 supervisor 只会 `mkdir -p` 重建空目录并打 warning。

## 5. 仍然存在的限制（需要上游修复）

| 限制 | 影响 | 现状/建议 |
|---|---|---|
| sticky 卷是节点本地目录（无 CSI/PV） | 节点替换 / drain / `kind delete cluster` 丢数据；多节点无法保证 | demo 默认拒绝多节点部署；长期建议平台实现真正的卷插件 |
| 没有卷亲和（`AttachVolume` 是 no-op） | 多节点下 resume 可能落到没有数据的节点 | 上游应在 `ExternalVolume` 记录节点并加入 `scheduling.Constraints.RequiredNodes` |
| golden 快照与 per-actor 卷不兼容 | 每次模板变更都可能让 actor 卡死 | demo 用 `ensure-no-golden.sh` 规避；上游应跳过或预置 |
| `capacity` / `storageClassName` 被忽略 | 没有配额，单用户可写满节点盘 | 用 `list-pvs.sh --detail` 巡检；长期由卷插件落实 |
| `DeleteVolume` 不回收、同名用户继承旧数据 | 孤儿目录、租户数据残留 | 删除用户走 admin purge；文档提醒 |

## 6. 复现 / 验证

```bash
# 单元与模板校验
./validate-templates.sh
go test ./demos/mf-pi/admin/...

# 端到端（测试环境，kind + 已部署 + nginx 在 59681）
./test-userdata-persistence.sh          # 同镜像 delete+recreate
./test-image-update-persistence.sh      # 真实模板变更（镜像更新路径）

# 手工检查
kubectl get actortemplate -n ate-demo-mf-pi-test \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.goldenSnapshot}{"\n"}{end}'
./list-pvs.sh --test --detail           # 应能看到 home/.pi/packages 等
```
