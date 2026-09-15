# 合并计划：mf-pi 用户「激活有效期自动挂起」+「资源档位配额」

## 背景与目标

在 `demos/mf-pi` 的 usermanagement 后台 (`demos/mf-pi/admin`) 中，为每个用户(每个用户 = `mfpi` atespace 里的一个 Actor)增加两类管理能力：

1. **激活有效期 / 自动释放资源**：管理员为某用户设置激活时长，到期后由 mfpi-admin 后台定时检查，自动调用 `SuspendActor` 挂起该用户以释放 Pod/worker 资源(挂起保留快照与用户数据，可后续恢复，与删除区分)。

2. **资源档位配额**：管理员为某用户指定资源档位(如 small/mid/large)，每个档位对应独立的 ActorTemplate + WorkerPool，分别设置 CPU/内存请求限制与磁盘容量，使该用户的 actor 真正受资源约束。**改动仅影响新建/重建用户**；存量用户保持原模板不变。

## 用户已确认的决策

- 激活时间语义：**激活时长/有效期**(相对时长，如 `168h` / `24h`)
- 到期动作：**挂起(suspend)**
- 触发：**mfpi-admin 后台定时(reconciler)检查**
- 有效期存储：**新增 ConfigMap**
- 资源档位：**固定资源档位**(small/mid/large，部署时预建)，非任意值
- 存量用户：**仅新/重建用户生效**
- CPU/内存：**每档一个独立 WorkerPool**(通过 `spec.template.resources` 真正限制)

## 平台约束(已核实)

- `ateapipb.Actor`(`pkg/proto/ateapipb/ateapi.proto:141`) **没有** per-actor 的 CPU/内存/磁盘字段。
- CPU/内存由 **WorkerPool** `spec.template.resources`(`corev1.ResourceRequirements`)在池子级别统一配置(`pkg/api/v1alpha1/workerpool_types.go:48`)。
- 磁盘容量在 **ActorTemplate** 的 `volumes[].externalVolumeTemplate.capacity`(`resource.Quantity`)。
- ActorTemplate spec 与 WorkerPool 在部署后**不可变**(`pkg/api/v1alpha1/actortemplate_types.go:387` `self == oldSelf`)；`deploy.sh` 已有 `ensure_at_recreate_if_changed` 处理模板变更时删除重建。
- 因此「每用户资源」只能通过「每档模板+池子」在部署时预建落地。

---

## Part A：激活有效期自动挂起

### A1. 新增 `demos/mf-pi/admin/expiry.go`

参照 `configMapPasswordStore`(`main.go:138`)实现 `configMapExpiryStore`：
- 接口 `expiryStore`：
  - `get(name) (expiresAt string, ok bool)`
  - `set(name, expiresAt string) error`
  - `delete(name) error`
- 基于 `CoreV1().ConfigMaps`，含 `load`(内存镜像)/get/set/delete/persist(Get→Update、不 Create，与密码存储一致)。
- 生产用 ConfigMap 实现；测试用内存 fake。

> 语义：存的是 **绝对到期时间戳** (RFC3339)。设置时长时由 server 计算 `expiresAt = now + duration` 后写入。

### A2. `demos/mf-pi/admin/main.go`

- `server` 增加字段 `expiries expiryStore`。
- `userSummary` 增加字段：
  - `HasExpiry bool`
  - `Expiry string`(到期时间的展示，如 RFC3339 / human duration)
- `serverConfig` + `serverConfigFromEnv` 增加 `expiryConfigMap` / `expiryNamespace`(env `EXPIRY_CONFIGMAP` / `EXPIRY_NAMESPACE`，默认 `mfpi-user-expirations` / `ate-demo-mf-pi`)。
- `main()` 构建并 `load` expiry store，装入 `srv.expiries`。
- **路由** `handleUserSubresource`(`main.go:368`)增加 `/expiry` 分支：
  - `POST/PUT /api/users/{name}/expiry`，body `{"duration":"168h"}` → `time.ParseDuration` 校验(非法返回 400)，校验用户存在(`GetActor`，参照 apikey 处理)，计算 `expiresAt` 并 `set`。
  - `DELETE /api/users/{name}/expiry` → `delete`(幂等，撤销自动挂起)。
- `handleListUsers`(`main.go:393`)为每个用户填充 `HasExpiry` / `Expiry`。
- **reconciler**：在现有 30s tick(`startReconciler` `main.go:748` / `reconcileKeys` `main.go:766`)中新增 `reconcileExpirations(ctx)`：
  - ListActors(mfpi atespace)。
  - 对每个 **RUNNING** 且有生效到期的 actor，若 `now > expiresAt` → `SuspendActor` + 日志。
  - 已挂起 / 无到期 / 不存在 → 跳过(幂等)。
  - 挂起失败仅记日志，下个 tick 重试(best-effort)。
- `handleDeleteUser`(`main.go:834`)删除用户时清理该用户 expiry 条目(best-effort，参照 keys 清理)。

---

## Part B：资源档位配额

### B1. 档位定义(部署时预建)

在 manifest 中定义固定档位集合(默认 small/mid/large)，每档包含：

1. **ActorTemplate** `mf-pi-<tier>`：复用现有 `mf-pi` 的完整容器定义(pi-web + 单容器 supervisor args + volumeMounts userdata + env)，改动仅：
   - `workerSelector.matchLabels` → `workload: mf-pi-<tier>`
   - `volumes[].externalVolumeTemplate.capacity` → 该档磁盘容量(如 small=5Gi / mid=20Gi / large=50Gi)
   - `storageClassName` 沿用 `standard`
2. **WorkerPool** `mf-pi-wp-<tier>`：label `workload: mf-pi-<tier>`，`spec.template.resources` 设置该档 CPU/内存 requests & limits(如 small=0.5/1Gi、mid=1/2Gi、large=2/4Gi；具体值可在 manifest 变量化)，`replicas` 沿用 `${MFPI_WORKER_REPLICAS}`。

> 因容器 `args` 大块重复，`.yaml.tmpl` 里按档复制容器定义(演示场景可接受)。`deploy.sh` 的 `render` 需新增相应变量的替换(如 `MFPI_TIER_*`，可选)。

### B2. 新增 `demos/mf-pi/admin/tier.go`

参照 expiry/password store 模式实现 `configMapTierStore`：
- 接口 `tierStore`：`get(name) (tier string, ok bool)` / `set(name, tier string) error` / `delete(name) error`
- 基于 ConfigMap，`username -> tier`，load/get/set/delete/persist。

同时在 server 中建立 **档位→模板** 映射：
- 新增字段 `tiers`(`tierStore`)与 `tierTemplates map[string]string`(tier → 模板名，如 `small → mf-pi-small`)。
- `serverConfigFromEnv` 增加 `defaultTier`(env `DEFAULT_TIER`，默认 `small`)。

### B3. `demos/mf-pi/admin/main.go`

- `server` 增加 `tiers tierStore`、`tierTemplates map[string]string`、`defaultTier string`。
- `userSummary` 增加 `Tier string`(该用户当前档位；未设置显示默认档)。
- 路由 `handleUserSubresource` 增加 `/tier` 分支：
  - `POST/PUT /api/users/{name}/tier` body `{"tier":"mid"}` → 校验档位合法(在 tierTemplates 中，非法返回 400)，校验用户存在，`set`。
- **`handleCreateUser`(`main.go:429`) 改模板解析**：
  - 创建前读取用户 tier(`tiers.get(name)`，缺省 `defaultTier`) → 用 `tierTemplates[tier]` 作为 `CreateActorRequest.ActorTemplateNamespace/Name` 的模板。
  - 现有 `ACTOR_TEMPLATE_NAME=mf-pi` 仅作为默认/基础档模板名来源。
- `handleListUsers` 填充 `Tier`。
- `handleDeleteUser` 清理该用户 tier 条目(best-effort)。

> 说明：actor 模板创建时固定，修改档位只对「新建/重建(如 refresh 流中删除+重建且保留 PV)」生效，符合「仅影响新/重建用户」的决策。

---

## Part C：Web UI(`demos/mf-pi/admin/index.html`)

复用已修复的 fixed 定位下拉菜单，在「操作 ▾」中新增菜单项，并在列表中新增列：

- 新增列 **有效期**：显示 `Expiry`；未设置显示 `—`；已到期且 RUNNING 显示 `已到期`。
- 新增列 **档位**：显示 `Tier`。
- 下拉菜单新增：
  - `设置有效期` → `window.prompt` 输入时长(如 `168h`，输入 `0`/空 = 清除)调 POST `/expiry`。
  - `清除有效期` → DELETE `/expiry`。
  - `设置资源档位` → `window.prompt` 选择档位(small/mid/large)调 POST `/tier`。
- 在 `renderUsers`(`index.html:241`)中透出 `u.hasExpiry` / `u.expiry` / `u.tier` 并绑定各按钮事件。

---

## Part D：测试(`demos/mf-pi/admin/main_test.go`)

- 新增 `fakeExpiryStore` / `fakeTierStore`(内存 fake)。
- `newTestServer` 注入 fake；`userSummary` 断言相应更新。
- 用例：
  - expiry：set 成功 / 非法 duration / 用户不存在 / delete 幂等。
  - reconcile expire：RUNNING+已到期 → suspend；RUNNING+未到期 → 不挂起；非 RUNNING → 不处理。
  - tier：set 合法/非法档位 / 用户不存在。
  - create user 带 tier → 使用 `mf-pi-<tier>` 模板。
  - list users 带出 HasExpiry/Expiry/Tier。
  - delete user 清理 expiry 与 tier。

---

## Part E：Manifests(`demos/mf-pi/mf-pi.yaml.tmpl` 与 `demos/mf-pi/mf-pi-test.yaml.tmpl`)

两份同步改动：

1. 新增每档 WorkerPool + ActorTemplate(见 B1)。
2. 预创建空 ConfigMap：
   - `mfpi-user-expirations`
   - `mfpi-user-tiers`
3. 新增 Role/RoleBinding(`mfpi-admin-expirations`、`mfpi-admin-tiers`)授予 mfpi-admin SA 对这些 ConfigMap 的 `get,update`(参照 `mfpi-admin-passwords`)。
4. mfpi-admin Deployment env 增加：
   - `EXPIRY_CONFIGMAP` / `EXPIRY_NAMESPACE`
   - `TIERS_CONFIGMAP` / `TIERS_NAMESPACE`
   - `DEFAULT_TIER`
5. 更新文档注释(admin 现有 RBAC 说明)。

---

## 关键注意点

- 复用「Get→Update、不 Create、预创建 ConfigMap + 命名 RBAC」模式，避免 SA 需要 create 权限。
- reconciler 与现有 `startReconciler` 同 30s tick，可合并：一个 tick 内依次调用 `reconcileKeys` 与 `reconcileExpirations`。
- 挂起(suspend)与删除(delete)区分：挂起释放资源、保留数据可恢复；删除才会 purge 数据卷。
- ActorTemplate 不可变：档位变更需经 `deploy.sh` 的 `ensure_at_recreate_if_changed` 或重建模板落地。
- 档位作用于「新建/重建用户」，存量用户不动。

## 待修改文件清单

- `demos/mf-pi/admin/expiry.go`(新增)
- `demos/mf-pi/admin/tier.go`(新增)
- `demos/mf-pi/admin/main.go`
- `demos/mf-pi/admin/index.html`
- `demos/mf-pi/admin/main_test.go`
- `demos/mf-pi/mf-pi.yaml.tmpl`
- `demos/mf-pi/mf-pi-test.yaml.tmpl`
- (可选)`demos/mf-pi/deploy.sh`(render 变量)

## 验证

- `go test ./demos/mf-pi/admin/...` 通过。
- 验收 1：到期 RUNNING 演员在后台定时后被自动挂起(SUSPENDED)，worker/Pod 释放，用户数据保留。
- 验收 2：新建用户按所设档位使用对应 ActorTemplate + WorkerPool，CPU/内存/磁盘真正受限；存量用户不变。

---

## 实现进度

以下记录各 commit 的落地情况(倒序，最新在上)。

- [x] **A1 / B2(存储层)**：新增 `expiry.go`(configMapExpiryStore)与 `tier.go`(configMapTierStore)，均沿用「预建 ConfigMap + Get→Update/不 Create + 命名 RBAC」模式。
  - commit b6759ea8 / ff2b36e9(已于本次任务开始前完成)。
- [x] **A2 / B3 - main.go 服务端**：
  - `server` 增加 `expiries`/`tiers`/`tierTemplates`/`defaultTier`；`userSummary` 增加 `HasExpiry`/`Expiry`/`Tier`。
  - 路由新增 `/expiry`(POST/PUT set、DELETE clear)与 `/tier`(POST/PUT set)。
  - 新增 `handleSetExpiry`/`handleClearExpiry`/`handleSetTier`：校验用户名、校验用户存在(GetActor)、set/delete 到 store。
  - `handleCreateUser` 用 `tierTemplates[userTier(name)]` 选择 ActorTemplate(仅新建/重建生效)。
  - `reconcileExpirations` 并入现有 30s reconcile tick：RUNNING 且 `now>expiresAt` → SuspendActor(幂等、best-effort)。
  - `handleListUsers` 填充 `HasExpiry`/`Expiry`(Tier 已在 `summarize` 填充)。
  - `handleDeleteUser` 清理该用户 expiry 与 tier(best-effort)。
  - `serverConfig`/`serverConfigFromEnv` 增加 `EXPIRY_CONFIGMAP`/`EXPIRY_NAMESPACE`/`TIERS_CONFIGMAP`/`TIERS_NAMESPACE`/`DEFAULT_TIER`；`main()` 构建并 load 两个 store、装配 `tierTemplates`/`defaultTier`。
  - `tier.go` 增加 `tierNames`(=small/mid/large)与 `buildTierTemplates(base)`。
  - 测试侧 `main_test.go` 增加 `fakeExpiryStore`/`fakeTierStore` 并装配进 `newTestServer`。
- [x] **Part C - Web UI**：`index.html` 增加「有效期」「档位」两列(有效期列：未设置显示 `—`、已到期且 RUNNING 显示 `已到期`)；「操作 ▾」菜单新增「设置有效期」「清除有效期」「设置资源档位」。`renderUsers` 透出 `u.hasExpiry`/`u.expiry`/`u.tier`，新增 `setUserExpiry`/`clearUserExpiry`/`setUserTier` 与 `expiryDisplayHtml` 辅助函数。
- [x] **Part D - 测试**：`main_test.go` 新增 14 个用例，覆盖 expiry set/非法时长/用户不存在/delete 幂等、reconcile 到期挂起 + 未到期跳过 + 非 RUNNING 跳过、tier 合法/非法/用户不存在、create-with-tier(用 `mf-pi-<tier>`)与默认档、list 带出 HasExpiry/Expiry/Tier、delete 清理 expiry 与 tier。`go test ./demos/mf-pi/admin/` 通过。
- [ ] **Part E**：`mf-pi.yaml.tmpl` 与 `mf-pi-test.yaml.tmpl` 增加每档 WorkerPool+ActorTemplate、预建 ConfigMap、Role/RoleBinding、Deployment env。
