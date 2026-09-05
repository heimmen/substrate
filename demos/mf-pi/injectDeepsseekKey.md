# 为指定用户动态设置 DeepSeek API key

> 本文档是 mf-pi 新功能的实施计划与进度跟踪（TODO 见文末）。目标：让管理员能
> **针对某个 mf-pi 用户（一个用户 == 一个运行 pi-web 的 Actor）动态设置 / 清除其
> 专属 DeepSeek API key**，并提供 mfpi-admin Web UI + REST、以及 CLI 脚本两套入口，
> 同时覆盖**生产**（`ate-demo-mf-pi` / `mfpi`）与**测试**（`ate-demo-mf-pi-test` /
> `mfpi-test`）两套环境。

## 为什么采用「驱动 actor 内 auth 流程」（已调研确认，勿改方案）

平台层无法按用户注入 env/Secret，原因：

- 容器 env 只来自共享 ActorTemplate 上**固定**的 `secretKeyRef`，由 ate-api-server 在
  resume 时解析（`cmd/ateapi/internal/controlapi/workflow_resume.go:377`、
  `workload_spec.go:99-248`）。**不存在**任何 per-actor env 字段（Actor proto、
  CreateActor、CLI 均无）；ActorTemplate spec 不可变
  （`pkg/api/v1alpha1/actortemplate_types.go:385`）。
- **Full 快照 resume 是进程内存还原**（`cmd/ateom-gvisor/main.go:440-474`
  create + restore），env 被冻结在快照取值里；resume 时改 env / Secret 值不会到达进程
  （证据：`cmd/atelet/oci.go:36-46`、`docs/api-guide.md:108`）。
- 无外部途径写入 actor 文件系统：durableDir 是 node-local hostPath、每个 run 会话都会
  reset、按服务器内部 UID 命名；唯一的 external volume 插件是 node-local mock。

但 pi-web 在**每次模型调用**都会重新读取一个**可运行时写入**的凭据文件
（revision-checked reload，无需重启），且**已存储的凭据优先于** `DEEPSEEK_API_KEY`
env：

- 文件：`<agentDir>/auth.json` = `/data/pi-agent/auth.json`（agentDir =
  `PI_CODING_AGENT_DIR`，由 sessiond 导出）。
- 内容形状：`{ "deepseek": { "type": "api_key", "key": "sk-..." } }`。
- 可通过 HTTP（pi-web 自带的 api-key 登录流程）写入；真正调 DeepSeek 的是
  session daemon。由于「一个用户 == 一个 actor」，这正好构成 per-user 的 key 面。

## 已确认的决策

- 注入方式 = **驱动 pi-web 的 in-actor auth 流程**（经路由器，非 env）。
- 范围 = **mf-pi 生产 + 测试**两套镜像化改动。
- 入口 = **mfpi-admin Web UI + REST** 以及 **CLI 脚本**（`set/clear-user-apikey.sh`
  及 `-test`）。
- 持久化 = 每用户 key 也写入**预创建的空 Secret** `mfpi-user-provider-keys`
  （username -> key），供 UI 显示「已设/未设」并在 actor 删除重建后自动重放。

## pi-web 注入流程（来自 pi-web 源码的精确步骤）

集群内 router 基址 `http://atenet-router.ate-system.svc:80`，Host
`<user>.<atespace>.actors.resources.substrate.ate.dev`，路径**不带**任何
`/<user>/` 前缀（即 nginx 转发上游时剥掉前缀后的形态）。目标 actor 需处于
RUNNING：

1. `GET /api/machines/local/auth/providers?mode=login&authType=api_key` → 200
   （同时兼作 web-ready 探针）。
2. `POST /api/machines/local/auth/api-key/interactive`，body
   `{"providerId":"deepseek"}` → `{flowId, status:"running", prompt?:{requestId,...}}`。
3. 若第 2 步响应里没有 `prompt`，轮询
   `GET /api/machines/local/auth/oauth/<flowId>` 直到 `state.prompt.requestId` 出现。
4. `POST /api/machines/local/auth/oauth/<flowId>/respond`，body
   `{"requestId":"<id>","value":"sk-..."}` → `status:"complete"`（写盘到
   auth.json）。`requestId` 一次性。

清除：`POST /api/machines/local/auth/logout`，body `{"providerId":"deepseek"}` →
`{accepted:true}`（运行时删除 deepseek 条目，回退到 `DEEPSEEK_API_KEY` env）。

校验：`GET providers`，deepseek 的 `status.source` 在设置后应为 `"stored"`；清除后应
为 `"environment"`/`"fallback"` 或 `configured:false`。

## REST 接口设计

- `POST /api/users/{name}/apikey`，body `{"apiKey":"sk-..."}` — 设置（覆盖）该用户
  key。
- `DELETE /api/users/{name}/apikey` — 清除该用户 key（幂等）。
- `GET /api/users` 的每个 `userSummary` 增加 `hasPersonalKey` 布尔，供 UI 显示徽标。

设置顺序（提交记录优先）：**先写 Secret 持久化，再确保 actor RUNNING 并驱动流程**；
流程失败返回 502 并说明「key 已存储，可重试/下次 resume 应用」。

清除顺序（先外后内）：**先驱动 logout 成功，再删除 Secret 条目**；logout 失败返回
502 且保留 Secret 条目。

## 文件改动清单

### 新增
- `admin/apikey.go` — `keyStore`/`secretKeyStore`（镜像 `configMapPasswordStore`）、
  `actorAuthClient` 接口 + `httpActorAuthClient`、`actorHostname()`、驱动流程辅助。
- `set-user-apikey.sh` / `clear-user-apikey.sh` / `set-user-apikey-test.sh` /
  `clear-user-apikey-test.sh` — CLI 包装（port-forward 到 `svc/mfpi-admin`，curl REST）。

### 修改
- `admin/main.go` — 常量、`server` 字段、配置/env、路由、handlers、list/delete/create
  集成、`main()` 装配。
- `admin/index.html` — 新增「DeepSeek Key」徽标列 + 每行「设置 Key / 清除」按钮与 JS。
- `admin/main_test.go` — 新增 fake + handler/流程测试（含一个用 `httptest.Server`
  扮演 pi-web 的用例）。
- `mf-pi.yaml.tmpl` 与 `mf-pi-test.yaml.tmpl` — 空 Secret + SA Role/RoleBinding +
  Deployment env（两套 namespace 各一份）。
- `validate-templates.sh` — 更新精确的 doc-kind 断言列表。
- `README.md` / `mfpi.md` — 补充文档。

### 不改
`mf-cc`、`cmd/`、`pkg/`、`internal/`、`Dockerfile`、`nginx*.conf`、`deploy*.sh`。

## 实现要点

### 存储（apikey.go / main.go）
- `keyStore` 接口：`Get(name)(string,bool)`、`Set(name,key) error`、`Delete(name) error`。
- `secretKeyStore`：镜像 `configMapPasswordStore`（内存 map + RWMutex，
  load/Get/Set/Delete/persist），但后端为 Secret `mfpi-user-provider-keys`。
  `load` 遇到 Secret 不存在视为空（非错误）；`persist` **只 Get→Update，绝不 Create**
  （RBAC 无 create）；若 NotFound 返回明确错误提示需先在清单里预创建。
- `actorAuthClient` 接口：
  ```go
  setPersonalKey(ctx, host, apiKey string) error
  clearPersonalKey(ctx, host string) error
  ```
  `httpActorAuthClient{base string; hc *http.Client}`：
  `host = actorHostname(name, atespace) = name + "." + atespace +
  ".actors.resources.substrate.ate.dev"`，设 `req.Host`，路径不含 username 前缀。
  内部先轮询 web 返回 200（~1s 间隔，受 ctx 约束），再驱动流程；设置后校验
  `source=="stored"`，清除后校验已回退。不设全局 client 超时（由 handler ctx 决定），
  只设单跳 transport 超时。

### main.go 改动
- 常量：`defaultKeysSecret="mfpi-user-provider-keys"`、
  `defaultKeysNamespace="ate-demo-mf-pi"`、
  `defaultRouterAddr="http://atenet-router.ate-system.svc:80"`。
  （实现注：`actorHostSuffix` 实际放在 `apikey.go`、紧邻其唯一使用方
  `actorHostname`，保证每一步 commit 都可独立编译。）
- `server` 增加字段：`keys keyStore`、`actors actorAuthClient`（router 基址由
  `httpActorAuthClient.base` 持有，无需 server 字段）。（实现注：为保证每步 commit
  测试全绿，`newTestServer` 提前接好 `fakeKeyStore`/`fakeActorAuth` 两个 fake。）
- `serverConfig` + `serverConfigFromEnv` 增加 `keysSecret/keysNamespace/routerAddr`，
  对应 env `KEYS_SECRET`、`KEYS_NAMESPACE`、`ROUTER_ADDR`。
- `userSummary` 增加 `HasPersonalKey bool json:"hasPersonalKey"`；
  `handleListUsers` 依据 `s.keys` 填充。
- 路由分发（`handleUserSubresource`）：**在通用 `DELETE` 之前**增加
  `POST …/apikey` → `handleSetAPIKey`、`DELETE …/apikey` → `handleClearAPIKey`。
- handlers：
  - `handleSetAPIKey`：校验 name（DNS-1123、无 `/`）→ 解析 `{"apiKey"}`（非空）→
    2 分钟 ctx → `GetActor`（404 缺失）→ **先写 Secret** → `ensureRunningActor`
    （非 RUNNING 则 ResumeActor，轮询至 RUNNING）→ `actors.setPersonalKey`。
    成功 200 `{"message":"DeepSeek Key 已设置","name":…}`；注入失败 **502** 且说明
    key 已存储、可重试/下次 resume 应用。
  - `handleClearAPIKey`：校验 → 2 分钟 ctx → `GetActor`；若 **NotFound**：actor 已删除
    → 清 Secret 条目并返回 200（幂等）。否则 `ensureRunningActor` →
    `actors.clearPersonalKey`（先 logout）→ 成功后才 `keys.Delete(name)`；
    logout 失败 502 且保留 Secret 条目。
  - `ensureRunningActor(ctx,name)`：经 `s.client` 的共享辅助（controlClient 接口已有
    GetActor/ResumeActor）。
  - `handleDeleteUser`：在 `passwords.Delete` 之后 best-effort `keys.Delete(name)`。
  - `handleCreateUser`：**新建与复用两条路径**，若 `s.keys.Get(name)` 有存量 key，
    则在 resume 后 best-effort 重放注入；失败只在 `message` 里附警告，仍返回 200
    （创建不得失败）。
- `main()`：用同一 in-cluster k8s client 构建 `secretKeyStore` 并 `load`；
  `actors: &httpActorAuthClient{base: cfg.routerAddr}`。

### index.html
- 新列「DeepSeek Key」：根据 `u.hasPersonalKey` 显示 已设置/未设置 徽标。
- 每行按钮：`设置 Key` → `prompt()` 输入 key → `POST api/users/<name>/apikey`
  `{"apiKey":…}`；`清除` → confirm → `DELETE api/users/<name>/apikey`。复用现有
  `showMsg` 展示结果。

### CLI 脚本（仿 create-user.sh；经临时 port-forward curl）
- `set-user-apikey.sh <user> <key>` / `clear-user-apikey.sh <user>`：
  - 选空闲本地端口；`kubectl port-forward -n ate-demo-mf-pi svc/mfpi-admin <port>:8080 &`；
    trap 清理。
  - `curl -sS -X POST http://127.0.0.1:<port>/api/users/<user>/apikey -d '{"apiKey":…}'`
    （清除用 `-X DELETE …/apikey`）；打印 JSON。
  - `-test` 变体指向 `ate-demo-mf-pi-test`。admin API 集群内可信、无鉴权（与既有脚本一致）。

### 清单模板（mf-pi.yaml.tmpl / mf-pi-test.yaml.tmpl）
在 passwords RBAC 块与 Deployment 之间插入：

```yaml
# 预创建的空 Secret：per-user DeepSeek provider key（username -> key）。
# 预创建好，使 mfpi-admin SA 只需 get/update（RBAC 无法按 resourceName 限制 create）。
apiVersion: v1
kind: Secret
metadata: { name: mfpi-user-provider-keys, namespace: <ate-demo-mf-pi | ate-demo-mf-pi-test> }
type: Opaque
---
# Role/RoleBinding：允许 mfpi-admin SA 读/改该 Secret。
# （与既有的 ate-api-server-env-sources Role 相互独立，勿合并。）
rules: [secrets / get,update / resourceNames: [mfpi-user-provider-keys]]
```

Deployment env（两套都加）：`KEYS_SECRET=mfpi-user-provider-keys`、
`KEYS_NAMESPACE=<ns>`、`ROUTER_ADDR=http://atenet-router.ate-system.svc:80`。

### validate-templates.sh
新的 doc-kind 顺序（两套模板一致）：
`Namespace, Secret, Role, RoleBinding, WorkerPool, ActorTemplate, ConfigMap,
ServiceAccount, Role, RoleBinding, Secret, Role, RoleBinding, Deployment, Service`
→ 更新第 35-37 行的断言列表。

## 测试计划（main_test.go）

- 新增 fake：`fakeKeyStore`（内存，镜像 `fakePasswordStore`）；
  `fakeActorAuth`（实现 `actorAuthClient`，记录每 host 的 set/clear 调用，可配置错误
  与 `sourceAfterSet`）。
- `newTestServer` 接线 `keys: newFakeKeyStore()`、`actors: newFakeActorAuth()`、
  `routerBase: ""`（既有测试不受影响）。
- `doRequest` 已把 `/api/users/…` 路由到 `handleUserSubresource`（天然覆盖新子资源）。
- 新增用例：
  - 设置成功：写 Secret（store 有 key）、SUSPENDED 时触发 ResumeActor、驱动
    `actors.setPersonalKey`、返回 200。
  - RUNNING actor 设置时不触发 resume。
  - 非法 name / actor 缺失 → 400 / 404。
  - store 写入失败 → 500，且不调用 actor。
  - 注入失败 → 502，但 key 仍在 store。
  - 清除成功：RUNNING → logout → store 条目删除。
  - 清除时 actor 缺失 → 200 且 store 被清空。
  - 删除用户时清空存量 key。
  - 列表正确返回 `hasPersonalKey`。
  - `httpActorAuthClient` 对 `httptest.Server`（扮演 pi-web）的请求序列：GET providers
    → POST interactive → GET flowId（若首响应无 prompt）→ POST respond 携带该
    requestId；以及 logout 路径；断言 Host 头 =
    `<user>.<atespace>.actors.resources.substrate.ate.dev`。

## 验证步骤

1. `gofmt -l demos/mf-pi/admin/` 无输出；`go test ./demos/mf-pi/admin/...` ok；
   `bash demos/mf-pi/validate-templates.sh` ok。
2. kind 集群：`DEEPSEEK_API_KEY=<placeholder> ./deploy.sh`（重部署生产 ns）、
   `./create-user.sh alice`、`./run-nginx.sh`。
3. `./set-user-apikey.sh alice sk-test123` → 期望「已设置」；管理 UI 徽标置为已设置；
   经 nginx 代理带 alice 密码访问
   `GET /alice/api/machines/local/auth/providers?mode=login&authType=api_key`，
   deepseek `source:"stored"`。
4. 挂起/恢复 alice 后再查 `source:"stored"`（auth.json 由 Full 快照保留）。
5. `./clear-user-apikey.sh alice` → 期望「已清除」；providers 显示 `source` 回退到
   env/fallback（非 stored）。
6. `./delete-user.sh alice` → 确认 Secret 中该 key 已被清除（Secret data 为空）。
7. 模板改动后重跑 gofmt/go test/validate；收尾清理集群演示资源。

---

## TODO List（实施进度跟踪）

> 逐项完成后把 `- [ ]` 改为 `- [x]`。

- [x] 1. 撰写本文档（`injectDeepsseekKey.md`）并同步 `README.md` / `mfpi.md`
- [x] 2. `admin/apikey.go`：`keyStore`/`secretKeyStore` + `actorAuthClient`/`httpActorAuthClient` + 驱动流程
- [x] 3. `admin/main.go`：常量 / server 字段 / 配置 env / 路由 / handlers / list/delete/create 集成 / main() 装配
- [x] 4. `admin/index.html`：DeepSeek Key 徽标列 + 每行「设置 Key / 清除」按钮与 JS
- [ ] 5. `admin/main_test.go`：fake（key store、actor auth）+ handler/流程测试
- [ ] 6. `mf-pi.yaml.tmpl` / `mf-pi-test.yaml.tmpl`：空 Secret `mfpi-user-provider-keys` + SA Role/RoleBinding + Deployment env
- [ ] 7. `set-user-apikey.sh` / `clear-user-apikey.sh`（及 `-test`）CLI 脚本
- [ ] 8. `validate-templates.sh`：doc-kind 断言列表更新
- [ ] 9. `gofmt`、`go test ./demos/mf-pi/admin/...`、`validate-templates.sh` 通过
- [ ] 10. kind 端到端：部署 → 设置/清除 key → 挂起/恢复后仍在 → 删除用户清 key → 收尾清理
