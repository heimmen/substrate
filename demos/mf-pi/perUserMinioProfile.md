# 将用户数据持久化到 MinIO（每用户独立存储，actor 重置后自动恢复）

> 本文档是 mf-pi 新功能的实施计划与进度跟踪（TODO 见文末）。目标：**每个 mf-pi
> 用户（一个用户 == 一个运行 pi-web 的 Actor）的数据——安装的 skill、会话
> (sessions)、`auth.json`(DeepSeek key 等凭据)、`models.json`、`settings.json`——
> 持续同步到该用户专属的 MinIO bucket**，用户之间完全隔离；**若用户的 actor 实例
> 被删除重建（重置），这些持久化数据不受影响，actor 冷启动时自动从 MinIO 载入**。
> 与既有 Full 快照 suspend/resume 形成「快照 + profile 双层」：同一实例的热恢复仍
> 走 Full 快照（进程内存 + 文件系统增量），MinIO 保证实例被重置后的持久化恢复。
> 范围覆盖**生产**（`ate-demo-mf-pi` / `mfpi`）与**测试**（`ate-demo-mf-pi-test` /
> `mfpi-test`）两套镜像环境，各自一套独立 MinIO。

## 为什么采用「mfpi-admin 作为可信 S3 broker」（已调研确认，勿改方案）

平台层限制决定了不能给每个 actor 内置同步程序、也不能让 actor 直接持 MinIO 凭据：

- **pi-web actor 镜像在 pi-web 仓库构建**（`demos/mf-pi/deploy.sh:78-85` 引用
  `localhost:5001/pi-web@<digest>`），本仓库无法把新二进制烤进该镜像。
- 容器 env 只来自共享 ActorTemplate 上固定的 `secretKeyRef`，由 ate-api-server 在
  resume 时解析，**不存在 per-actor env**；ActorTemplate spec 不可变。actor 也无法
  mount k8s Secret / 访问 k8s API。
- 但 actor 沙箱**可直接访问集群 Service**（ClusterIP、集群 DNS、无默认拒绝），且
  pi-web 镜像自带 `bash`/`curl`/`tar`。

因此架构定案为：**mfpi-admin（本仓库 Go 服务）是唯一持有 MinIO 凭据的可信 S3
broker**；actor 内 supervisor 只通过 HTTP（带共享 bearer token）向
`mfpi-admin.<ns>.svc:8080/internal/actor/<user>/profile` 拉取 / 上推 profile。
`aws-sdk-go-v2/service/s3` 已是根模块直接依赖（`go.mod:16`，vendored），**无新增
Go 依赖**。

### 已确认决策

- 同步机制 = **actor 内 supervisor 周期上推 + 冷启动拉取**，叠加在既有 Full 快照上
  （“快照 + profile 双层”）。
- 隔离 = **每用户一个独立 bucket**，bucket 名 = `<username>`（DNS-1123 即合法 bucket
  名），全部 bucket 用**单一中心管理员 MinIO 凭据**写入（**不**为每用户 mint IAM
  key；用户于调研中确认“中心管理员凭证”）。
- 数据范围 = **仅 `/data/pi-agent`**（auth.json / skills/ / sessions/ / models.json /
  settings.json），tar 时排除 `*.log`、`*.sock` 等瞬态文件。
- MinIO = **新增一套真 MinIO**，专用于 mf-pi 用户数据；生产与测试各自 namespace 各
  一套；平台快照存储（rustfs/GCS）不动。
- actor→admin 内部代理鉴权 = **模板级共享 bearer token**（`MFPI_PROFILE_TOKEN`，
  取自 Secret `mfpi-profile-token`，actor 模板 env 与 admin Deployment env 同源）。
- actor 删除（`delete-user`）**保留其 MinIO bucket/对象**——重置后重建即自动恢复。
  显式清理 bucket 不在本期范围。

### 相对已批准计划的实现简化（本实现注）

- **不引入 `mfpi-user-profiles` 元数据 Secret / 其 RBAC**：`userSummary` 的「已同步」
  徽标直接以 S3 为准——`HeadObject(bucket=user, key=profile.tar.gz)` 存在与否 +
  `LastModified`。理由：bucket 名可由 username 直接推导、对象键固定，Secret 元数据
  只是冗余镜像且可能过期；S3 即真相，列表对每个用户一次快速 HeadObject（演示规模
  无压力）。MinIO 不可达时列表降级（徽标灰、不报错）。
- **bucket 懒创建**：admin 的 `PUT …/profile` handler 在对象上推前
  `EnsureBucket`（幂等），因此**任何路径**创建的用户（含 `create-user.sh` 直接
  `kubectl ate` 创建）首次运行时即自动获得自己的 bucket。REST/UI 创建路径额外
  best-effort 预创建以尽早暴露 MinIO 故障。

## supervisor 行为（actor 内 `args: sh -c`，只用 bash/curl/tar）

### 实现发现（关键，勿回退到「脚本启动时读一次 actor」）

真实 actor **一律是从模板 golden 基快照 Restore 而来，不存在冷 Run**：ActorTemplate
控制器用「以 AT-UID 命名的一次性 golden actor」冷启动约 20s（本 demo 无 readyz，取
满 warmup）后 Full 快照作为基快照；之后任何真实 actor（新建、delete+recreate 重置、
普通 resume）都是从该基快照 Restore。Restore 是**进程内存恢复**：shell 进程恢复到基
快照时所在的 `wait` 处继续执行，**脚本启动时算好的任何 shell 变量都被冻结在 golden
启动时刻的值**（此时 `/run/ate/actor-id` = AT-UID，非真实用户名）。所以 supervisor
**绝不把 actor 名 / sync 开关缓存成脚本级变量**，而是每个周期现读
`/run/ate/actor-id`——atelet 每次 Run/Restore 都会为真实 actor 重写该 per-actor
bind-mount（cmd/atelet/main.go:698-707）。

```
cur_actor() { cat /run/ate/actor-id 2>/dev/null || true; }  # 每次调用现读，不缓存
sync_on()   # MFPI_PROFILE_TOKEN 非空 && MFPI_ADMIN_URL 非空 && cur_actor 非空
tag=$agent/.mfpi-profile-actor        # 本地 profile 归属标记（= 最近同步的 actor）
agent=${PI_CODING_AGENT_DIR:-/data/pi-agent}
interval=${MFPI_PROFILE_PUSH_INTERVAL:-20}   # 秒
```

1. **已恢复判定**（`profile_matches`）：本地 `.mfpi-profile-actor` == 当前 actor →
   同一实例的 Full 快照恢复，本地为准，**跳过拉取**（避免用更旧的 MinIO 副本覆盖）。
   delete+recreate 重置时新 actor 从 golden 基恢复，基里的标记是 AT-UID（或不存在）
   ≠ 真实用户名 → 判定 fresh → 拉取。（不用 `.mfpi-profile` 等静态文件判定，正是为
   避免 golden 基里残留的标记让所有重置用户误判为「已有数据」而跳过恢复。）
2. **`do_sync`（一次单飞同步）**：现读 actor → `sync_on` 不满足则跳过；否则若
   `! profile_matches` → `pull_profile`（`GET …/profile`；200 → `tar xzf` 覆盖还原
   auth.json/skills/sessions/… 后 `tag_profile`；204 无 profile → 仅 `tag_profile`；
   其它/admin 不可达 → 仅记日志，绝不让启动失败）；随后 `push_profile`
   （`tar czf` 排除 `*.log`/`*.sock`/`*.tmp` → `PUT …/profile`；成功 `tag_profile`）。
   `mkdir` 锁单飞，慢上推不与下个周期重叠。
3. 启动流程：脚本顶层先 `do_sync`（golden 启动与假想的冷 Run 才会执行到；真实 actor
   恢复时进程已越过此段）。随后按既有 body 起 `pi-web-sessiond` → 等 unix socket →
   `pi-web-server`。
4. **周期循环**：后台每 `interval` 秒 `do_sync`。**真实 actor 的冷启动恢复正是靠这个
   循环**——进程从基快照的 `wait` 处恢复后，第一个周期（≤20s）就完成
   pull+tag+push，把用户的 MinIO profile 自动载回本地。
5. **SIGTERM 最后同步**：现有 trap 在 kill web/sessiond 前先 `do_sync` 一次，随后照常
   退出。

**数据新鲜度窗口** = 一个周期（≤20s）+ SIGTERM 最后上推。文档明确：重置恢复的是
「最近一次完成的上推」。同一实例 Full 快照 resume 是内存恢复，本地标记与当前 actor
一致 → 不拉取不覆盖——这正是热恢复本地为准的语义。

## 内部 REST 接口（admin，带共享 token）

- `GET  /internal/actor/{name}/profile`：token 校验（constant-time）+ DNS-1123 名校验
  → `HeadObject`；无对象（含 bucket 尚不存在）→ **204**；有 → 200 流式返回
  `profile.tar.gz`（Content-Type application/gzip）。
- `PUT  /internal/actor/{name}/profile`：token 校验 + 名校验 → **懒 `EnsureBucket`** →
  限长读请求体（上限约 64MiB，超限 413）→ `PutObject`（key `profile.tar.gz`）→ 200
  `{"bytes":N}`。

鉴权：`Authorization: Bearer <MFPI_PROFILE_TOKEN>`。token 为空则 admin 一律 503（防
误配开放）；actor 侧 token 为空则不拉不推（记日志）。

## 文件改动清单

### 新增
- `admin/profile.go` — `profileStore` 接口 + `s3ProfileStore`（aws-sdk v2；静态凭据 +
  `BaseEndpoint` + `UsePathStyle`）、`EnsureBucket`/`GetProfile`/`PutProfile`/
  `HasProfile`、MinIO NotFound 判定辅助、token 校验辅助、内部 handler
  `handleProfileGet`/`handleProfilePut`、配置解析。
- 本文档 `perUserMinioProfile.md`。

### 修改
- `admin/main.go` — 常量、`server` 字段（`profiles profileStore`）、
  `serverConfig`/`serverConfigFromEnv`（`MINIO_ENDPOINT`/`MINIO_ACCESS_KEY`/
  `MINIO_SECRET_KEY`/`MINIO_REGION`/`MINIO_BUCKET_PREFIX`/`MFPI_PROFILE_TOKEN`）、
  `userSummary` + `profileSynced`/`lastSync`、`handleListUsers` 用 HeadObject 填徽标、
  `handleCreateUser`/`handleCreateUser` 复用路径 EnsureBucket、`main()` 装配 store 与
  路由（`/internal/actor/`）。
- `admin/index.html` — 「MinIO Profile」徽标列（已同步/未同步 + 时间 title）。
- `admin/main_test.go` — fake store + handler 测试。
- `mf-pi.yaml.tmpl` 与 `mf-pi-test.yaml.tmpl` — 共享 token Secret、MinIO 对象（Secret/
  Deployment/Service/PVC）、扩展 ate-api-server-env-sources Role 的 resourceNames、
  supervisor args 扩展、ActorTemplate env、admin Deployment env（见下）。
- `validate-templates.sh` — doc-kind 断言列表更新 + 新占位符渲染。
- `deploy.sh` / `deploy-test.sh` — MinIO 镜像 digest 解析与推送指引、token 生成/复用
  、sed 占位符。
- `README.md` / `mfpi.md` — 文档。

### 不改
`mf-cc`、`cmd/`、`pkg/`、`internal/`、`nginx*.conf`、`stop-nginx.sh`、既有
`create/delete/list-user*.sh`。

## 清单模板设计（两套模板各自一份）

新增/修改对象（以 prod namespace `ate-demo-mf-pi` 为例；test 为
`ate-demo-mf-pi-test`）：

1. `ate-api-server-env-sources` Role：`resourceNames` 追加 `mfpi-profile-token`
   （ate-api-server 需读 ActorTemplate env 引用的 Secret）。
2. 共享 token Secret `mfpi-profile-token`：`stringData: { token: ${MFPI_PROFILE_TOKEN} }`
   （deploy 时生成/复用，见下）。
3. MinIO admin Secret `mfpi-minio-admin`：
   `stringData: { MINIO_ROOT_USER: minioadmin, MINIO_ROOT_PASSWORD: minioadmin }`
   （演示级固定默认，可覆盖）。MinIO Deployment 与 mfpi-admin Deployment 经
   secretKeyRef 同源引用。
4. MinIO PVC `mfpi-minio-data`（default StorageClass `standard`/local-path）。
5. MinIO Deployment `mfpi-minio`：`image localhost:5001/minio@${MINIO_DIGEST}`，
   `args: ["server","/data"]`，env `MINIO_ROOT_USER/PASSWORD`（secretKeyRef），
   挂载 PVC 到 `/data`，readiness/liveness probe `/minio/health/ready`、`/minio/health/live`。
6. MinIO Service `mfpi-minio`：`9000→9000`。
7. ActorTemplate 容器 env 追加：`MFPI_ADMIN_URL=http://mfpi-admin.<ns>.svc:8080`、
   `MFPI_PROFILE_TOKEN`（secretKeyRef `mfpi-profile-token`/`token`）、
   `MFPI_PROFILE_PUSH_INTERVAL=20`；`args` 的 sh -c 扩展为上节 supervisor。
8. admin Deployment env 追加：`MINIO_ENDPOINT=http://mfpi-minio.<ns>.svc:9000`、
   `MINIO_ACCESS_KEY`/`MINIO_SECRET_KEY`（secretKeyRef `mfpi-minio-admin`/
   `MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD`）、`MINIO_REGION=us-east-1`、
   `MFPI_PROFILE_TOKEN`（secretKeyRef `mfpi-profile-token`/`token`）。

doc-kind 顺序（两套模板一致，`validate-templates.sh:39-43` 断言）：
`Namespace, Secret(provider-config), Role, RoleBinding, Secret(profile-token),
WorkerPool, ActorTemplate, ConfigMap, ServiceAccount, Role, RoleBinding,
Secret(keys), Role, RoleBinding, Secret(minio-admin), PersistentVolumeClaim,
Deployment(minio), Service(minio), Deployment(admin), Service(admin)`

## deploy.sh / deploy-test.sh

- `MFPI_PROFILE_TOKEN`：若未导出，则读现存 `Secret/mfpi-profile-token` 的 token
  （与 DEEPSEEK_API_KEY 同法）；再无则 `openssl rand -hex 32` 生成。sed 替换
  `${MFPI_PROFILE_TOKEN}`（同一份渲染同时作用于 Secret/ActorTemplate/admin env，保证
  三方一致）。
- MinIO 镜像 digest：`docker inspect localhost:5001/minio:latest` 取 RepoDigest →
  `${MINIO_DIGEST}`；缺失时打印推送指引（`docker pull quay.io/minio/minio:RELEASE…` →
  `tag`/`push` 到 localhost:5001/minio:latest），与 pi-web/pause 同模式。
- `validate-templates.sh` 渲染额外替换 `${MINIO_DIGEST}`、`${MFPI_PROFILE_TOKEN}`。

## 测试计划（main_test.go + profile 相关）

- fake `fakeProfileStore`：内存实现 `profileStore`，记录 EnsureBucket/Put/Head 调用，
  可注入错误与预置 profile。
- `newTestServer` 默认接 `profiles: newFakeProfileStore()`（既有测试不受影响）。
- 用例：
  - 创建用户（REST）成功后 `EnsureBucket` 被调用（幂等：重复创建不重复建桶）。
  - 删除用户**不**删 bucket（fake 断言无 Delete 调用 / bucket 仍在）。
  - `PUT /internal/actor/alice/profile`：无 token → 401/403；token 错 → 401；token 对
    但名非法 → 400；正常 → EnsureBucket + PutProfile 被调、返回 200 + bytes。
  - `GET /internal/actor/alice/profile`：无 profile → 204；有 → 200 且 body 为 profile。
  - 列表在 store 有对象时 `profileSynced=true`、有 lastSync；store 报错 → 降级 false
    不失败。
  - profile 体超限 → 413。
  - （不新增 Go 依赖；supervisor 本身是模板 shell，用 `bash -n` + validate 覆盖。）

## 验证步骤

1. 静态：`gofmt -l demos/mf-pi/admin/` 无输出；`go vet ./demos/mf-pi/admin/...`；
   `go test ./demos/mf-pi/admin/...` ok；`bash demos/mf-pi/validate-templates.sh` ok；
   改动/新增 shell `bash -n` ok。
2. kind 端到端——因 **prod `ate-demo-mf-pi` 的 ActorTemplate 不可变**（且 alice/tom 是
   真实用户），E2E 在**干净重建的 test 环境**（`ate-demo-mf-pi-test` / `mfpi-test`）
   上执行：删除 test ns 后 `deploy-test.sh` 重部署，使控制器以**新 supervisor** 生成
   全新 golden 基快照（模板 UID `fe80e829-…`）。一次性用户 `e2e`、`iso`：
   a. broker 路径：admin `PUT/GET /internal/actor/{name}/profile` round-trip 正常
      （GET 200 流式 tar、无对象 204、懒 EnsureBucket）。
   b. **push**：`create-user-test.sh e2e` + `kubectl ate resume` → 首个周期内
      `/api/users` 显示 `profileSynced=true`（含 `lastSync`）；经
      `set-user-apikey-test.sh e2e sk-e2e-resetcheck-1234567890` 注入 DeepSeek key →
      pi-web `source=stored`；等 >1 周期后 broker GET 下载 tar 内 `auth.json` =
      reset-check key、`skills/` 齐全、`.mfpi-profile-actor`= `e2e`。
   c. **重置自动恢复**：`suspend` → `delete` → `create`（新 actor）→ `resume` →
      新 actor 从 golden 基恢复（基中无 auth.json、标记为 AT-UID）→ 周期循环首个
      do_sync 拉取 → 观测 pi-web providers `source` 在 ~10s 内回到 **stored**，无需
      重新注入。再 GET broker 确认 MinIO 中 profile 仍含 reset-check key（拉取后又被
      推回，标记 `e2e`）。
   d. **隔离**：另建 `iso` 并 resume → 其 bucket 只有 `iso` 的 profile（auth.json 为
      pi-web 默认 `{}`，无 deepseek key），与 `e2e` 的 reset-check key 互不串扰；
      两 bucket `.mfpi-profile-actor` 分别为各自用户名。
   e. **Full 快照热恢复不受影响**：`iso` suspend/resume（同一实例）后本地标记 == 自身
      → 跳过拉取，profile 不被覆盖（source 仍为 environment，未出现 e2e 的 key）。
   f. **golden 基快照无用户数据**：golden actor（AT-UID）的 bucket 不存在（GET 204）——
      golden 启动时 admin 尚未就绪、do_sync 未写标记；即便写了，标记为 AT-UID ≠ 真实
      用户名，重置恢复依旧会拉取。
   g. 收尾：删除 `e2e`/`iso` actor（bucket 保留，符合 delete 保留语义）。确认 MinIO
      与 rustfs 快照存储互不影响（prod alice/tom 未触碰）。

---

## TODO List（实施进度跟踪）

> 逐项完成后把 `- [ ]` 改为 `- [x]`。

- [x] 1. 撰写本文档（`perUserMinioProfile.md`）
- [x] 2. `admin/profile.go`：`profileStore`/`s3ProfileStore` + 内部 handler + token 校验
- [x] 3. `admin/main.go`：常量 / server 字段 / 配置 env / 路由 / list/create 集成 / main() 装配
- [x] 4. `admin/index.html`：MinIO Profile 徽标列
- [x] 5. `admin/main_test.go`：fake store + handler 测试
- [x] 6. `mf-pi.yaml.tmpl` / `mf-pi-test.yaml.tmpl`：token Secret + MinIO 对象 +
      env-sources Role 扩展 + supervisor args + ActorTemplate/admin env
- [x] 7. `validate-templates.sh` / `deploy.sh` / `deploy-test.sh`：doc-kind 断言 + 占位符
      + MinIO 镜像 digest 解析
- [x] 8. `README.md` / `mfpi.md` 文档
- [x] 9. `gofmt`、`go vet`、`go test ./demos/mf-pi/admin/...`、`validate-templates.sh`、
      `bash -n` 通过
- [x] 10. kind 端到端：provision → sync → reset(delete+recreate) → 自动恢复 → 隔离 →
      清理
