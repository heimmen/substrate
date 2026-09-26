# 管理员统一安装 Skill 实施计划

## 产品概述

为 `demos/mf-pi` 增加"管理员统一安装 skill"能力：管理员在 `/usermanagement/` 页面上传/删除 skill，平台自动把它分发到**所有用户（每个用户 = 一个 pi-web Actor）**，安装到每个 Actor 的 `${PI_CODING_AGENT_DIR:-/data/pi-agent}/skills`，新会话自动生效，已打开会话可通过"立即应用"热加载。

## 核心功能

- **统一托管**：`mfpi-admin` 是唯一权威源，skill 内容存于 PVC，对外暴露只读 manifest + tgz。
- **管理员操作界面**：usermanagement 页新增"共享 Skills"卡片——列表（名称/大小/更新时间）、上传（tgz/zip）、删除、立即应用。
- **自动分发**：Actor 内 supervisor 新增 10s 拉取循环，按 sha256 增量覆盖安装托管 skill；挂起/新建用户恢复后自动补齐。
- **托管与私有边界**：仅覆盖"管理员托管过"的 skill；用户自建且同名未被托管的 skill 不删除、不覆盖；管理员卸载只删托管清单内的目录。
- **立即应用（可选加速）**：对 RUNNING 用户经 router 扇出 pi-web 的 session reload，使已打开会话无需重启即加载新版。
- **双环境**：生产（`ate-demo-mf-pi` / `mfpi`）与测试（`ate-demo-mf-pi-test` / `mfpi-test`）同步支持。

## 实施方案（方案 B：Actor 主动拉取）

**权威源在 admin，交付靠 Actor 自拉。** admin 把 skills 目录打成"清单 + 单 skill tgz"，Actor 的后台循环比对本地状态做增量覆盖安装。

为什么选它（相对共享只读卷）：
1. **不动卷**——规避 `VolumeMount` 无 `readOnly` 字段、嵌套挂载未定义、`purge_actor_volumes.go` 会在删除用户时误删共享目录的三个既有坑。
2. **跨节点天然一致**——不依赖每个 worker 节点挂载同一 NFS。
3. **失败降级友好**——admin 不可达时 Actor 只是"暂不更新"，已有 skill 照常工作。
4. **对挂起用户友好**——resume 后循环自动补齐，无需管理员逐个处理。

### 数据流

```mermaid
flowchart LR
  A[管理员 /usermanagement 上传] --> B[mfpi-admin]
  B --> C[(PVC /var/lib/mfpi-skills)]
  C --> D[manifest.json + tgz 端点]
  D -.10s 轮询 If-None-Match.-> E[Actor 拉取循环]
  E --> F[/data/pi-agent/skills]
  F --> G[sessiond resourceLoader]
  B -.立即应用: 经 router 扇出.-> H[POST /api/sessions/:id/reload]
  H --> G
```

## 实施细节（执行要点）

### 1. admin 存储与端点
- PVC `mfpi-shared-skills`（1Gi，`storageClassName: standard`）挂到 admin 的 `/var/lib/mfpi-skills`；env `SKILLS_DIR=/var/lib/mfpi-skills`。
- 每次写操作后原子重写 manifest（tmp + rename），`version = sha256(canonicalJSON)`，响应带 `ETag`。
- 端点（只读、无鉴权）：
  - `GET /internal/skills/manifest` → `{version, skills:[{name,sha256,size,updatedAt}]}`，支持 `If-None-Match`。
  - `GET /internal/skills/<name>.tgz` → 顶层目录为 `<name>/`。
- 不做鉴权：Actor 侧 env 会冻结进 golden 快照，token 轮换对已恢复的 Actor 不生效。

### 2. Actor 拉取循环（模板脚本）
- env `PI_WEB_SKILLS_URL`（不能以 `MFPI_` 开头）。
- 状态文件 `$PI_CODING_AGENT_DIR/.mfpi-managed-skills.json`。
- 每 10s：`node -e` 拉 manifest（带 `If-None-Match`）→ 与状态比对 → 仅下载 sha 变化的 → 落到 `$dst/.mfpi-tmp-<name>` → `rm -rf` + `mv` 原子替换 → 更新状态。
- manifest 中消失且曾被托管的 → 删除；从未托管的用户自建 skill 一律不动。
- 网络/解析失败静默跳过（`|| true`），绝不阻塞 sessiond/web 主进程。

### 3. 立即应用（扇出 reload）
对每个 RUNNING 用户（复用 `ensureRunningActor`）：
`GET /api/projects` → 对每个 project path `GET /api/sessions?cwd=` → `POST /api/sessions/:id/reload`。

### 4. 安全与性能
- 上传校验：name 必须是 slug、包内必须含 `SKILL.md`、tar/zip 解包必须拒绝 `../` 与绝对路径条目（zip-slip）。
- PVC 为 RWO：admin `replicas` 必须保持 1。

## Todo List

- [x] #1 P0 验证：actor 沙箱内能否解析并访问 mfpi-admin Service（不通则回退方案 A）
- [x] #2 P1 验证：reload 后新 skill 是否出现在 `/api/sessions/:id/commands`
- [x] #3 新增 `admin/skills.go`：PVC 布局、manifest/ETag、tgz 端点、上传校验
- [x] #4 `main.go` 接线 `SKILLS_DIR` 与 `/api/skills`、`/internal/skills` 路由
- [x] #5 新增 `admin/skills_test.go` 覆盖 manifest、校验、增删改、apply 扇出
- [x] #6 两个模板的 4 个 ActorTemplate 加 10s 拉取循环与 `PI_WEB_SKILLS_URL`
- [x] #7 模板加 PVC `mfpi-shared-skills` 并挂到 admin Deployment
- [x] #8 `index.html` 新增共享 Skills 卡片（列表/上传/删除/立即应用）
- [x] #9 更新 `validate-templates.sh` 断言（拉取循环、env、PVC、资源计数）
- [x] #10 新增 `install/list/remove/apply skill` 脚本及 `-test` 变体
- [x] #11 `README.md` 与 `mfpi.md` 增加统一安装 skill 章节与引用
- [x] #12 `make verify` + `validate-templates.sh` + 端到端验证
  - 已通过：`go test ./demos/mf-pi/admin`（-count=1 全绿，含 HTTP apply reloader 单测）、`go build` / `go vet` / `gofmt` 无告警。
  - `validate-templates.sh`：两套模板各 28 docs 校验全部通过，含 PVC、拉取循环及 fsGroup。
  - 8 个 skill CLI 脚本（install/list/remove/apply 及 -test 变体）语法和执行均通过。
  - 真实集群端到端（E2E）验证全部通过：
    1. 上传新 Skill（hello-skill）：admin 解析打包并生成 sha256 清单；
    2. Actor 自动增量拉取：各运行中的 Actor 在 10s 内通过后台拉取循环自动下载、校验并解压安装到 `/data/pi-agent/skills/hello-skill`；
    3. 增量更新（v2）：内容更新后重新打包上传，Actor 自动检测到 sha256 变更并原子覆盖更新；
    4. 卸载与隔离边界保护：用户自建的自定义 Skill 不会被清理，删除托管 Skill 后 Actor 仅删除受托目录并清理 `.mfpi-managed-skills.json`；
    5. 立即应用（扇出 reload）：修复了 reloadActor 对 projects/sessions 数组结构的兼容解析并传入 `{"cwd": "..."}` 请求体，成功将集群内 11 个在线 Actor 全部 reload 成功（`{"online": 11, "reloaded": 11, "failed": 0}`）。
