# mf-pi 用户 Agent API 文档

> 面向**外部组件开发者**的接口文档：第三方服务、自动化脚本、CI/CD、独立管理后台等。
>
> - 适用组件：`mfpi-admin`（平台管理面，本仓库 `demos/mf-pi`）+ `pi-web`（用户 Agent 面，默认镜像 `@jmfederico/pi-web 1.202609.0`）
> - 网关：`demos/mf-pi/nginx.conf`（生产）/ `nginx-test.conf`（测试）
> - 文档中的 `file:line` 引用均指向本仓库或 `pi-web` 源码，便于核对。

---

## 目录

- [1. 概述](#1-概述) — 两个 API 平面、入口地址、鉴权模型、通用约定
- [2. 快速开始](#2-快速开始) — 建用户、拿密码、发起第一次调用
- [3. 错误处理](#3-错误处理) — 错误结构、状态码、网关改写规则、重试建议（**外部组件必读**）
- [4. 平台管理 API（mfpi-admin）](#4-平台管理-apimfpi-admin) — 用户/密码/Key/有效期/档位/Skill
- [5. 用户 Agent API：会话、事件与认证](#5-用户-agent-api会话事件与认证)
- [6. 用户 Agent API：项目、工作区与文件](#6-用户-agent-api项目工作区与文件)
- [7. 用户 Agent API：状态、配置、机器、插件与包管理](#7-用户-agent-api状态配置机器插件与包管理)
- [8. 附录](#8-附录) — 术语、端点总览、源码索引、安全建议、版本

> **文档状态说明**：第 5–7 章的字段与示例由 pi-web 源码推导，未经真实服务器逐一验证；其中无法从源码确证的细节以 `<!-- TODO: unverified -->` 标注，接入前建议以目标版本实测为准。第 4 章（管理面）基于本仓库实现与其单元测试。

---

## 1. 概述

### 1.1 两个 API 平面

mf-pi 把「平台」和「用户 Agent」拆成两个独立的 HTTP 服务，经同一个 nginx 网关按路径前缀暴露：

| 平面 | 服务 | 职责 | 经网关的路径前缀（生产 / 测试） |
|---|---|---|---|
| **平台管理面** | `mfpi-admin`（单副本 Deployment，ClusterIP `mfpi-admin:8080`） | 用户（Actor）生命周期、访问密码、专属 DeepSeek Key、有效期、资源档位、共享 Skill 分发 | `/usermanagement/` |
| **用户 Agent 面** | `pi-web`（每个用户 = 一个 Actor，跑在 gVisor 沙箱里，容器内监听 80） | 项目、会话、消息、文件、终端、插件、包管理、认证等 Agent 能力 | `/<username>/` |

一个用户 == 一个 Actor（`mfpi` / `mfpi-test` atespace 下的独立实体），因此**每个用户的 Agent API 是同一个应用、不同的数据与生命周期**。外部组件通常先用管理面创建用户、拿到密码，再以该用户的身份调用 Agent 面。

### 1.2 部署拓扑与地址

```
            外部组件 / 浏览器
                   │  HTTP(S)
                   ▼
        ┌──────────────────────────┐
        │  mfpi-nginx 网关          │   生产 :58681 / 测试 :59681
        │  · /usermanagement/ → Basic Auth
        │  · /<user>/         → auth_request 就绪门
        └───────┬──────────┬───────┘
                │          │
      :58682/59882│          │:58680/59880（集群内为 atenet-router）
                ▼          ▼
        ┌────────────┐  ┌───────────────────────────────┐
        │ mfpi-admin │  │ atenet-router（Host 路由）      │
        │ 用户的增删改 │  │  <user>.mfpi.actors... → Actor │
        │ 查 / Skill  │  │  （自动唤醒 SUSPENDED 的 Actor）│
        └────────────┘  └──────────────┬────────────────┘
                                       ▼
                              Actor 沙箱（pi-web :80）
```

| 环境 | 网关地址 | atespace | admin namespace | 说明 |
|---|---|---|---|---|
| 生产 | `http://<host>:58681` | `mfpi` | `ate-demo-mf-pi` | 默认入口 |
| 测试 | `http://<host>:59681` | `mfpi-test` | `ate-demo-mf-pi-test` | 与生产完全隔离 |

`<host>` 是运行 `run-nginx.sh` / `run-nginx-test.sh` 的机器地址；nginx 以 `--network host` 监听，防火墙放行即可被外部访问。

**网关的三个路径规则**（`demos/mf-pi/nginx.conf:95-163`）：

| 路径 | 行为 |
|---|---|
| `/usermanagement/<path>` | 去掉 `/usermanagement` 前缀后转发给 `mfpi-admin`，即 `/usermanagement/api/users` → admin 的 `/api/users`；加 **HTTP Basic Auth** |
| `/<username>/<path>` | 以 `Host: <username>.<atespace>.actors.resources.substrate.ate.dev` 转发给 atenet-router，最终到达该用户 Actor 的 `pi-web`；经**就绪门**鉴权 |
| 其它（如 `/favicon.ico`） | 按 `mfpi_user` cookie 路由到对应用户，经 `/_mfpi_auth` 鉴权 |

**仅集群内可达、不属于外部 API** 的端点（下面会标注）：admin 的 `/internal/skills/*`，以及 nginx 声明为 `internal` 的 `/_mfpi_auth`、`/_mfpi_gate`、`/_mfpi_loading`。

> 开发期可使用 `kubectl port-forward`（`58680`→router、`58682`→admin，`run-nginx.sh:83-84`），但它们只绑定 `127.0.0.1`，且 **admin 端口没有任何鉴权**——不要把 `mfpi-admin:8080` 直接暴露给不可信网络。

### 1.3 Base URL 约定

本文所有示例使用占位符：

```bash
BASE=http://<host>:58681          # 测试环境改成 http://<host>:59681
USER=alice                        # 用户名 == Actor 名 == Agent API 的 URL 段
ADMIN_USER=admin                  # 管理面 Basic Auth 用户名（部署时确定）
ADMIN_PASSWORD='mf@pass2026'      # 管理面 Basic Auth 密码（run-nginx.sh 默认值）
USER_PASSWORD='<创建用户时返回的密码>'
```

- 管理面 Base：`${BASE}/usermanagement`
- 用户 Agent Base：`${BASE}/${USER}`

### 1.4 鉴权模型

| 平面 | 方案 | 凭据 | 失败响应 |
|---|---|---|---|
| 管理面 | nginx `auth_basic`（htpasswd） | 部署方设定；`run-nginx.sh` 默认 `admin` / `mf@pass2026`，可用 `ADMIN_USER` / `ADMIN_PASSWORD` 覆盖 | `401` + `WWW-Authenticate: Basic realm="mfpi admin"` |
| 用户 Agent 面 | nginx `auth_request` → admin `/_mfpi_gate` | HTTP Basic，**用户名必须等于 URL 中的 `<username>`**，密码是该用户密码 | `401`（凭据错/未分配密码）；`403`（已认证但 Actor 未就绪或不存在） |

要点：

- **没有** Bearer Token、OAuth、API Key 形式的网关鉴权；外部组件请使用 HTTP Basic。
- 用户密码在**创建用户时一次性返回**，之后只能通过 `POST /usermanagement/api/users/<name>/password` 重置。
- 用户在 admin 里没有密码记录前，Agent 面一律 `401`（`main.go:1203-1208`）。
- 每次成功的 Agent 面鉴权都会 `touch` 该用户的空闲计时器，从而**重置自动挂起倒计时**（默认 90 分钟，仅 `small`/`mid` 档；`main.go:1213-1219`）。外部组件长时间轮询即视为「活跃」。

### 1.5 通用约定

| 项 | 约定 |
|---|---|
| 编码 | 请求/响应体 UTF-8；JSON 请求带 `Content-Type: application/json` |
| 缓存 | 管理面 JSON 响应带 `Cache-Control: no-store`（`main.go:1260-1265`） |
| 请求体上限 | nginx 层 `client_max_body_size 512m`；管理面 Skill 包 ≤ **64 MiB**（`skills.go:52`）；pi-web 默认 ≤ **64 MiB**（`DEFAULT_MAX_UPLOAD_BYTES`，可用 `PI_WEB_MAX_UPLOAD_BYTES` 或配置文件覆盖） |
| 命名规则 | 用户名/Skill 名必须匹配 DNS-1123：`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`（`main.go:112`） |
| WebSocket | 网关透传 `Upgrade`/`Connection` 头；与 HTTP 使用同一端口与 Basic Auth |
| 幂等性 | 管理面的创建用户、清除 Key/有效期均可重复调用；详见各端点说明 |

### 1.6 版本与兼容性

- Agent 面版本：`GET /api/pi-web/version`（无需 `cwd`）。
- 会话/项目类接口需要 `cwd` 的，`cwd` 指**项目根目录的绝对路径**（见 `sessionRoutes.ts:39`）。
- pi-web 的每个业务路径都同时挂载在两个前缀下：`/api/...` 与 `/api/machines/local/...`（`app.ts:248-264`）。两者行为一致，前者是常用形式。

---

## 2. 快速开始

### 2.1 创建用户并获取密码

```bash
BASE=http://<host>:58681

curl -sS -u admin:'mf@pass2026' \
  -X POST "${BASE}/usermanagement/api/users" \
  -H 'Content-Type: application/json' \
  -d '{"name":"alice"}'
```

```json
{
  "message": "创建成功",
  "user": {
    "name": "alice",
    "template": "ate-demo-mf-pi/mf-pi-small",
    "status": "STATUS_RUNNING",
    "ateomPod": "ate-demo-mf-pi/alice-xxxxx",
    "ip": "10.0.0.12",
    "version": 3,
    "age": "1s",
    "hasPersonalKey": false,
    "hasExpiry": false,
    "expiry": "",
    "tier": "small"
  },
  "password": "k7Qm2f..."
}
```

`password` 只在创建/重置时返回，请立即保存。

### 2.2 调用用户 Agent API

```bash
# 健康/版本
curl -sS -u alice:"${USER_PASSWORD}" "${BASE}/alice/api/pi-web/version"

# 项目列表
curl -sS -u alice:"${USER_PASSWORD}" \
  -H 'Accept: application/json' \
  "${BASE}/alice/api/projects"
```

### 2.3 处理「Agent 未就绪」

用户 Actor 可能处于 `SUSPENDED`（省资源）或 Web 服务启动中。此时：

- 浏览器导航（`Accept: text/html`）会拿到 **HTTP 200 的加载页**；
- **API/WS 客户端应始终发送 `Accept: application/json`**，这样会拿到原始 `503`（或首次请求时网关阻塞等待唤醒后直接成功）。

推荐流程（详见 §3.4）：

```bash
for i in 1 2 3 4 5; do
  code=$(curl -sS -o /tmp/out.json -w '%{http_code}' \
    -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
    "${BASE}/alice/api/projects")
  [ "$code" = "200" ] && break
  sleep $((i*2))
done
```

---

## 3. 错误处理

### 3.1 错误响应结构

| 平面 | 结构 | 示例 |
|---|---|---|
| 管理面（mfpi-admin） | `{"error":"<中文或英文说明>"}` | `{"error":"非法用户名 \"Bad_Name\"：必须匹配 DNS-1123：..."}` |
| 用户 Agent 面（pi-web / Fastify） | `{"error":"<message>"}`；插件依赖类错误额外带 `code`、`detail`；Fastify 内建校验错误带 `statusCode`、`error`、`message` | `{"error":"cwd query parameter is required"}` |

成功响应统一为 JSON 对象（少数端点为 HTML，已在对应章节标注）。

### 3.2 状态码语义

| 状态码 | 含义 | 典型触发 |
|---|---|---|
| `200` | 成功 | —— |
| `400` | 参数非法 | 用户名/档位/Skill 名不合法、JSON 解析失败、`duration` 非正数、缺少 `cwd`、上传缺少 `file` |
| `401` | 未认证 | 管理面 Basic Auth 失败；Agent 面用户名与 URL 不符、密码错误、该用户尚未分配密码 |
| `403` | 已认证但目标不可用 | Agent 面就绪门：Actor 非 `RUNNING`，或该用户不存在（`gate.go:52-63`） |
| `404` | 资源不存在 | 用户、Skill、项目、会话等不存在；未注册的路径 |
| `409` | 冲突 | 部分 pi-web 端点在状态冲突时返回（见各端点） |
| `413` | 请求体过大 | 超过 nginx 512m / admin 64MiB / pi-web 上传上限 |
| `500` | 服务端错误 | 管理面内部失败（查询 ateapi、写 ConfigMap/Secret 失败） |
| `502` | 下游失败 | 管理面「Key 已存储但注入 Agent 失败」（可重试）；上游 pi-web 不可达 |
| `503` | 暂不可用 | **无空闲 worker**（WorkerPool 满）；Agent Web 服务仍在启动；pi-web 的 agent profile 不可用 |
| `504` | 上游超时 | 网关到 router/Actor 超时 |

### 3.3 网关对错误码的改写规则（**外部组件必读**）

nginx 的 `location ~ ^/<user>/` 配置了 `proxy_intercept_errors on; error_page 403 502 503 504 = /_mfpi_loading;`（`nginx.conf:124-150`）。而 `/_mfpi_loading` 内部逻辑是：

```nginx
if ($http_accept !~* "text/html") { return 503; }
```

因此：

- `Accept` **含 `text/html`**（浏览器）→ 返回 **200 + HTML 加载页**，并带 `Retry-After: 3`（`loading.go:103-105`）。
- `Accept` **不含 `text/html`**（API/WS 客户端）→ 统一返回 **`503`**，即使是原来的 `403`/`502`/`504`。

**结论**：外部组件调用 Agent 面时务必设置 `Accept: application/json`（或任何不含 `text/html` 的值），否则会把「未就绪」误判为成功（200）。同时注意：未就绪时的 403 在 API 客户端眼里是 503。

### 3.4 重试与超时建议

| 场景 | 建议 |
|---|---|
| 首次访问某用户（Actor 被懒唤醒） | 网关在 router 层可能阻塞数秒后直接返回真实响应；建议客户端超时 ≥ 30s，并配合下方重试 |
| 收到 `503`（含 `Retry-After`） | 指数退避重试：1s、2s、4s、8s…，总预算 60–120s；期间不要并发发起大量请求 |
| 唤醒 `SUSPENDED` Actor | 首次请求触发后台 resume；若网关返回 503，等待 `Retry-After` 后重试直至 200 |
| 收到 `401` | **不要重试**，修正凭据（用户密码可在管理面重置） |
| 收到 `403`（经由非 HTML Accept 已变成 503） | 同 503 处理；若持续失败，用管理面 `GET /api/users` 确认用户是否存在及其 `status` |
| 管理面 `502`（设置专属 Key） | Key 已落库，可安全重试 `POST /api/users/<name>/apikey`（幂等覆盖） |
| 幂等写操作 | 创建用户、清除 Key/有效期、删除 Skill 等可安全重试；`POST /api/users` 重复调用不会重置已有用户数据 |

### 3.5 错误排查信息

- 管理面 `GET /healthz` 返回 `{"status":"ok","atespace":"mfpi"}`，可用于探活。
- `GET /api/users` 返回每个用户的 `status`，是判断「为什么 503」的第一手依据（`STATUS_SUSPENDED`/`STATUS_RESUMING` 表示正在唤醒；无该用户则说明被删除）。

---

## 4. 平台管理 API（mfpi-admin）

**Base URL**：`${BASE}/usermanagement`（生产 `http://<host>:58681/usermanagement`，测试 `:59681`）
**鉴权**：HTTP Basic（nginx 层，见 §1.4）
**响应**：JSON，`Cache-Control: no-store`

> 路由注册见 `demos/mf-pi/admin/main.go:1421-1440`。
> 直连 `mfpi-admin:8080`（集群内或 port-forward）时 URL **去掉** `/usermanagement` 前缀，且**没有任何鉴权**——仅供集群内组件使用。

### 4.1 端点总览

| 方法 | 路径（经网关） | 用途 |
|---|---|---|
| GET | `/usermanagement/api/users` | 列出用户 |
| POST | `/usermanagement/api/users` | 创建用户（幂等） |
| DELETE | `/usermanagement/api/users/{name}` | 删除用户并回收其持久卷 |
| POST | `/usermanagement/api/users/{name}/password` | 重置访问密码 |
| POST | `/usermanagement/api/users/{name}/apikey` | 设置用户专属 DeepSeek Key |
| DELETE | `/usermanagement/api/users/{name}/apikey` | 清除专属 DeepSeek Key |
| POST / PUT | `/usermanagement/api/users/{name}/expiry` | 设置账号有效期 |
| DELETE | `/usermanagement/api/users/{name}/expiry` | 清除有效期 |
| POST / PUT | `/usermanagement/api/users/{name}/tier` | 设置资源档位 |
| GET | `/usermanagement/api/skills` | 列出托管 Skill |
| POST | `/usermanagement/api/skills` | 上传托管 Skill（multipart） |
| DELETE | `/usermanagement/api/skills/{name}` | 删除托管 Skill |
| POST | `/usermanagement/api/skills/apply` | 向在线 Agent 热加载 Skill |
| GET | `/usermanagement/healthz` | 健康检查 |
| GET | `/usermanagement/_mfpi_auth` | 网关鉴权子请求（内部） |
| GET | `/usermanagement/_mfpi_gate` | 网关就绪门（内部） |
| GET | `/usermanagement/_mfpi_loading` | 加载页（内部） |
| GET | `/usermanagement/internal/skills/manifest` | Actor 拉取清单（**集群内**） |
| GET | `/usermanagement/internal/skills/{name}.tgz` | Actor 拉取 Skill 包（**集群内**） |

> 说明：`/internal/skills/*` 虽然能被 `/usermanagement/` 前缀转发到，但该前缀需要管理面 Basic Auth；**Actor 的拉取循环走的是集群内 Service DNS**（`http://mfpi-admin.ate-demo-mf-pi.svc.cluster.local:8080/internal/skills`，见 `mf-pi.yaml.tmpl:399`），不经过 nginx。

---

### 4.2 `GET /api/users` — 列出用户

列出该 atespace 下的全部 Actor（分页内部完成，最多 1000/页循环拉取）。

**请求参数**：无。

**成功响应** `200`：

```json
{
  "atespace": "mfpi",
  "users": [
    {
      "name": "alice",
      "template": "ate-demo-mf-pi/mf-pi-small",
      "status": "STATUS_RUNNING",
      "ateomPod": "ate-demo-mf-pi/alice-6f8c...",
      "ip": "10.0.0.12",
      "version": 3,
      "age": "2h10m",
      "hasPersonalKey": true,
      "hasExpiry": true,
      "expiry": "2026-01-01T00:00:00Z",
      "tier": "small"
    }
  ]
}
```

**字段说明**（`userSummary`，`main.go:372-389`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `name` | string | 用户名（= Actor 名） |
| `template` | string | 实际使用的 `ActorTemplate` `<namespace>/<name>`；档位 `<t>` 映射为 `<base>-<t>`（如 `mf-pi-small`），未设档位的历史用户可能是基础模板 `mf-pi`（`tier.go:27-39`） |
| `status` | string | Actor 状态枚举：`STATUS_SUSPENDED`/`STATUS_RESUMING`/`STATUS_RUNNING`/`STATUS_SUSPENDING`/`STATUS_PAUSING`/`STATUS_PAUSED`/`STATUS_CRASHED` |
| `ateomPod` | string | 承载该 Actor 的 Pod（`namespace/name`），无则为 `"<none>"` |
| `ip` | string | Pod IP，可能为空 |
| `version` | int64 | Actor 资源版本号 |
| `age` | string | 人类可读的创建时长 |
| `hasPersonalKey` | bool | 是否已存储该用户的专属 DeepSeek Key |
| `hasExpiry` | bool | 是否设置了有效期 |
| `expiry` | string | RFC3339 到期时间，未设置时为空串 |
| `tier` | string | 资源档位（未设置时返回服务端默认 `small`） |

**错误**：

| 状态码 | 响应 | 场景 |
|---|---|---|
| `500` | `{"error":"列出用户失败: ..."}` | ateapi 查询失败 |

```bash
curl -sS -u admin:'mf@pass2026' "${BASE}/usermanagement/api/users"
```

---

### 4.3 `POST /api/users` — 创建用户

幂等：若同名 Actor 已存在，则**复用**（保留历史数据），并尝试补注已存储的专属 Key。

**请求体**：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 是 | 用户名，必须匹配 DNS-1123；前后空白会被去掉 |

```json
{ "name": "alice" }
```

**成功响应** `200`（新建并成功 resume）：

```json
{
  "message": "创建成功",
  "user": { "name": "alice", "status": "STATUS_RUNNING", "tier": "small", "...": "同 §4.2 userSummary" },
  "password": "k7Qm2f..."
}
```

其它 `200` 变体：

| 场景 | 响应 |
|---|---|
| 用户已存在 | `{"message":"用户已存在，复用现有会话","name":"alice"}`（若有存储 Key，message 追加提示） |
| 创建成功但立即 resume 失败（如无空闲 worker） | `{"message":"创建成功，但立即恢复失败（可稍后打开页面触发恢复）：...","user":{...},"password":"..."}` |

> 创建流程（`main.go:517-618`）：确保 atespace 存在 → 按用户档位选择 ActorTemplate 创建 Actor → 生成并保存密码 → 立即 Resume。
> **`password` 只在此响应中返回**，请妥善保存。

**错误**：

| 状态码 | 响应 | 场景 |
|---|---|---|
| `400` | `{"error":"请求体不是合法 JSON"}` | body 解析失败（读取上限 1 MiB） |
| `400` | `{"error":"非法用户名 \"X\"：必须匹配 DNS-1123：..."}` | `name` 不合法 |
| `500` | `{"error":"查询 atespace 失败: ..."}` / `{"error":"创建 atespace 失败: ..."}` | 初始化 atespace 失败 |
| `500` | `{"error":"查询用户失败: ..."}` / `{"error":"创建用户失败: ..."}` | ateapi 调用失败 |
| `500` | `{"error":"生成访问密码失败: ..."}` | 密码写入 ConfigMap 失败 |

```bash
curl -sS -u admin:'mf@pass2026' -X POST "${BASE}/usermanagement/api/users" \
  -H 'Content-Type: application/json' -d '{"name":"alice"}'
```

---

### 4.4 `DELETE /api/users/{name}` — 删除用户

流程：`Suspend`（幂等）→ `DeleteActor` → 删除密码/Key/有效期/档位记录 → **purge 该用户的 sticky 持久卷**（`main.go:1113-1176`）。

> 注意：`DeleteActor` 本身**不会**删除持久卷（这是「删+建」刷新能保留数据的原因）；本端点会额外 purge，因此数据不可恢复。

**路径参数**：`name`（string，必填，DNS-1123）。

**成功响应** `200`：

```json
{ "message": "删除成功", "name": "alice" }
```

**错误**：

| 状态码 | 响应 | 场景 |
|---|---|---|
| `400` | `{"error":"非法用户名"}` | name 不合法或含 `/` |
| `404` | `{"error":"用户不存在"}` | Actor 不存在 |
| `500` | `{"error":"挂起用户失败: ..."}` / `{"error":"删除用户失败: ..."}` | 生命周期操作失败 |

> purge 卷失败**不会**让请求失败，只会记录日志（可能残留孤儿卷）。

```bash
curl -sS -u admin:'mf@pass2026' -X DELETE "${BASE}/usermanagement/api/users/alice"
```

---

### 4.5 `POST /api/users/{name}/password` — 重置访问密码

生成新密码并覆盖旧哈希，返回明文（一次性）。**不校验用户是否存在**——对不存在的用户调用也会返回 200 并写入一条无主哈希。

**路径参数**：`name`（string，必填，DNS-1123）。

**请求体**：无（忽略 body）。

**成功响应** `200`：

```json
{ "message": "密码已重置", "name": "alice", "password": "Zq8x1..." }
```

**错误**：

| 状态码 | 响应 | 场景 |
|---|---|---|
| `400` | `{"error":"非法用户名"}` | name 不合法 |
| `500` | `{"error":"重置密码失败: ..."}` | 写 ConfigMap 失败 |

```bash
curl -sS -u admin:'mf@pass2026' -X POST "${BASE}/usermanagement/api/users/alice/password"
```

---

### 4.6 `POST /api/users/{name}/apikey` — 设置专属 DeepSeek Key

先持久化到 Secret（`mfpi-user-provider-keys`），再唤醒该用户并把 Key 注入其 pi-web 认证存储（优先级高于环境变量 `DEEPSEEK_API_KEY`）。**存储优先**：注入失败时 Key 仍保留，可重试，且会在下次 resume 时自动补注。

**路径参数**：`name`（string，必填，DNS-1123）。

**请求体**：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `apiKey` | string | 是 | 非空；前后空白会被去掉 |

```json
{ "apiKey": "sk-xxxxxxxx" }
```

**成功响应** `200`：

```json
{ "message": "DeepSeek Key 已设置", "name": "alice" }
```

**错误**：

| 状态码 | 响应 | 场景 |
|---|---|---|
| `400` | `{"error":"请求体不是合法 JSON"}` / `{"error":"apiKey 不能为空"}` / `{"error":"非法用户名"}` | 参数问题 |
| `404` | `{"error":"用户不存在"}` | Actor 不存在（此时不会写 Secret） |
| `500` | `{"error":"查询用户失败: ..."}` / `{"error":"保存 DeepSeek Key 失败: ..."}` | ateapi / Secret 写入失败 |
| `502` | `{"error":"DeepSeek Key 已存储，但无法恢复用户以注入（可重试，恢复后会自动应用）: ...","name":"alice"}` | resume 失败 |
| `502` | `{"error":"DeepSeek Key 已存储，但注入失败（可重试，恢复后会自动应用）: ...","name":"alice"}` | 注入 pi-web 失败 |

```bash
curl -sS -u admin:'mf@pass2026' -X POST "${BASE}/usermanagement/api/users/alice/apikey" \
  -H 'Content-Type: application/json' -d '{"apiKey":"sk-xxxxxxxx"}'
```

---

### 4.7 `DELETE /api/users/{name}/apikey` — 清除专属 DeepSeek Key

先让该用户的 Actor 登出该 provider，再从 Secret 删除记录（**清除优先**，登出失败则保留记录以便重试）。幂等：Actor 已不存在时会直接清理存储条目。

**路径参数**：`name`（string，必填，DNS-1123）。

**成功响应** `200`：

```json
{ "message": "DeepSeek Key 已清除", "name": "alice" }
```

**错误**：

| 状态码 | 响应 | 场景 |
|---|---|---|
| `400` | `{"error":"非法用户名"}` | name 不合法 |
| `500` | `{"error":"清除存储的 DeepSeek Key 失败: ..."}` / `{"error":"已退出登录，但清除存储条目失败: ..."}` | Secret 操作失败 |
| `502` | `{"error":"清除失败（无法恢复用户）: ..."}` | resume 失败 |
| `502` | `{"error":"清除失败，已保留存储的 Key（可重试）: ..."}` | agent 登出失败 |

```bash
curl -sS -u admin:'mf@pass2026' -X DELETE "${BASE}/usermanagement/api/users/alice/apikey"
```

---

### 4.8 `POST|PUT /api/users/{name}/expiry` — 设置有效期

设置一个**相对时长**，服务端换算为绝对 RFC3339 时间并持久化；到期后由协调器自动挂起该用户。

**路径参数**：`name`（string，必填，DNS-1123）。

**请求体**：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `duration` | string | 是 | Go `time.ParseDuration` 格式且为正数，如 `168h`、`24h30m`、`90m` |

```json
{ "duration": "168h" }
```

**成功响应** `200`：

```json
{ "message": "有效期限已设置", "name": "alice", "expiresAt": "2026-01-08T10:00:00Z" }
```

**错误**：

| 状态码 | 响应 | 场景 |
|---|---|---|
| `400` | `{"error":"请求体不是合法 JSON"}` / `{"error":"非法时长：应为格式如 168h / 24h30m 的正数"}` / `{"error":"非法用户名"}` | 参数问题 |
| `404` | `{"error":"用户不存在"}` | Actor 不存在 |
| `500` | `{"error":"查询用户失败: ..."}` / `{"error":"保存有效期失败: ..."}` | ateapi / ConfigMap 失败 |

```bash
curl -sS -u admin:'mf@pass2026' -X POST "${BASE}/usermanagement/api/users/alice/expiry" \
  -H 'Content-Type: application/json' -d '{"duration":"168h"}'
```

---

### 4.9 `DELETE /api/users/{name}/expiry` — 清除有效期

幂等；清除后该用户不再被自动挂起。

**成功响应** `200`：

```json
{ "message": "有效期限已清除", "name": "alice" }
```

**错误**：`400 {"error":"非法用户名"}`；`500 {"error":"清除有效期失败: ..."}`。

```bash
curl -sS -u admin:'mf@pass2026' -X DELETE "${BASE}/usermanagement/api/users/alice/expiry"
```

---

### 4.10 `POST|PUT /api/users/{name}/tier` — 设置资源档位

档位决定后续**新建/重建**该用户时使用的 ActorTemplate；对已存在的 Actor 不生效（需删除重建）。

**路径参数**：`name`（string，必填，DNS-1123）。

**请求体**：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `tier` | string | 是 | 允许值：`small`、`mid`、`large`（`tier.go` / `main.go:832`） |

```json
{ "tier": "mid" }
```

**成功响应** `200`：

```json
{ "message": "资源档位已设置（作用于下次新建/重建用户）", "name": "alice", "tier": "mid" }
```

**错误**：

| 状态码 | 响应 | 场景 |
|---|---|---|
| `400` | `{"error":"请求体不是合法 JSON"}` / `{"error":"非法资源档位：可选值 small, mid, large"}` / `{"error":"非法用户名"}` | 参数问题 |
| `404` | `{"error":"用户不存在"}` | Actor 不存在 |
| `500` | `{"error":"查询用户失败: ..."}` / `{"error":"保存资源档位失败: ..."}` | ateapi / ConfigMap 失败 |

```bash
curl -sS -u admin:'mf@pass2026' -X POST "${BASE}/usermanagement/api/users/alice/tier" \
  -H 'Content-Type: application/json' -d '{"tier":"mid"}'
```

---

### 4.11 `GET /api/skills` — 列出托管 Skill

**成功响应** `200`（`skills.go:599-629`）：

```json
{
  "version": "3f2a9c...",
  "skills": [
    { "name": "my-skill", "sha256": "e3b0c4...", "size": 20480, "updatedAt": "2025-12-01T09:30:00Z" }
  ]
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `version` | string | 内容哈希（等同内部 manifest 的 ETag），内容变化即变化 |
| `skills[].name` | string | Skill 名（DNS-1123） |
| `skills[].sha256` | string | Skill 目录打包内容的 sha256 |
| `skills[].size` | int64 | 字节数 |
| `skills[].updatedAt` | string | RFC3339 更新时间 |

**错误**：`500 {"error":"读取 skill 列表失败: ..."}`。

```bash
curl -sS -u admin:'mf@pass2026' "${BASE}/usermanagement/api/skills"
```

---

### 4.12 `POST /api/skills` — 上传托管 Skill

`multipart/form-data` 上传一个 Skill 包；服务端解包校验后存入 PVC，并重写 manifest。

**表单字段**：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 是 | Skill 名，DNS-1123 |
| `file` | file | 是 | `.tgz` / `.zip`；解包后必须包含 `SKILL.md` |

**约束**：

- 单包 ≤ **64 MiB**（`maxUploadBytes`，`skills.go:52`）；包内单文件 ≤ 32 MiB；托管数量上限 1024（`skills.go:55-61`）。
- 拒绝 `../`、绝对路径等 zip-slip 条目；不合法时返回 400。

**成功响应** `200`：

```json
{
  "message": "已安装 skill my-skill",
  "skill": { "name": "my-skill", "sha256": "e3b0c4...", "size": 20480, "updatedAt": "2025-12-01T09:30:00Z" }
}
```

**错误**：

| 状态码 | 响应 | 场景 |
|---|---|---|
| `400` | `{"error":"上传解析失败: ..."}` | multipart 解析失败（如超限） |
| `400` | `{"error":"缺少上传文件字段 file"}` | 未提供 `file` |
| `400` | `{"error":"<校验失败原因>"}` | 名称非法、缺少 `SKILL.md`、含非法路径条目等 |

```bash
curl -sS -u admin:'mf@pass2026' -X POST "${BASE}/usermanagement/api/skills" \
  -F name=my-skill -F file=@./my-skill.tgz
```

> 仓库内的 CLI 包装脚本：`demos/mf-pi/install-skill.sh`（自动打 tgz/zip 并经 port-forward 调用本接口）。

---

### 4.13 `DELETE /api/skills/{name}` — 删除托管 Skill

**路径参数**：`name`（string，必填，DNS-1123）。

**成功响应** `200`：

```json
{ "message": "已删除 skill my-skill" }
```

**错误**：

| 状态码 | 响应 | 场景 |
|---|---|---|
| `400` | `{"error":"非法 skill 名称"}` | 名称不匹配 DNS-1123 |
| `404` | `{"error":"skill 不存在"}` | 名称为空或含 `/` |
| `500` | `{"error":"删除 skill 失败: ..."}` | 删除目录/重写 manifest 失败 |

```bash
curl -sS -u admin:'mf@pass2026' -X DELETE "${BASE}/usermanagement/api/skills/my-skill"
```

> 该操作只影响 admin 管理的目录；Agent 侧下一次拉取循环（或手动 `apply`）后同步删除，用户自建的 Skill 不受影响。

---

### 4.14 `POST /api/skills/apply` — 向在线 Agent 热加载 Skill

对**当前处于 `RUNNING`** 的所有用户执行尽力而为的会话热加载：`GET /api/projects` → 每项目 `GET /api/sessions?cwd=` → 对每个会话 `POST /api/sessions/:id/reload`（`skills.go:751-897`）。新会话无需此步。

**请求体**：无（表单字段被忽略）。

**成功响应** `200`（即使个别用户失败）：

```json
{
  "message": "已尝试应用",
  "online": 3,
  "reloaded": 2,
  "failed": 1,
  "failures": ["bob: list projects: ..."]
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `online` | int | 参与尝试的在线用户数 |
| `reloaded` | int | 成功触发 reload 的会话数 |
| `failed` | int | 处理失败的用户数 |
| `failures` | string[] | `<user>: <原因>` 列表 |

**错误**：ateapi 列用户失败时返回 `200` + `{"error":"列出用户失败: ..."}`（注意：HTTP 仍为 200，需检查 `error` 字段）。

```bash
curl -sS -u admin:'mf@pass2026' -X POST "${BASE}/usermanagement/api/skills/apply"
```

---

### 4.15 `GET /healthz` — 健康检查

**成功响应** `200`：

```json
{ "status": "ok", "atespace": "mfpi" }
```

```bash
curl -sS -u admin:'mf@pass2026' "${BASE}/usermanagement/healthz"
```

---

### 4.16 网关内部端点（不供外部组件调用）

这些端点由 nginx 在请求处理链中调用，外部组件**不应**直接依赖其行为；此处仅作文档完整性说明。

| 端点 | 行为 | 备注 |
|---|---|---|
| `GET /_mfpi_auth` | 纯鉴权。成功 `200` 空响应；失败 `401` + `WWW-Authenticate: Basic realm="mfpi agent"`，body `{"error":"missing user"\|"no password assigned"\|"invalid credentials"}` | 目标用户取自 `X-Original-URI` 或 `X-Mfpi-User`（`main.go:1184-1240`） |
| `GET /_mfpi_gate` | 鉴权 + 就绪门。`200` Actor 为 `RUNNING`；`401` 鉴权失败；`403` Actor 非 `RUNNING` 或不存在（并在后台触发 resume）；ateapi 异常时放行 `200` | `gate.go:41-70` |
| `GET /_mfpi_loading` | 渲染 HTML 加载页，`200` + `Cache-Control: no-store`，可自动刷新时带 `Retry-After: 3`；用户不存在时页面显示「用户不存在」且不再刷新 | `loading.go:52-107` |

### 4.17 集群内端点（Actor 专用）

| 端点 | 行为 |
|---|---|
| `GET /internal/skills/manifest` | 返回 §4.11 的 manifest JSON，带 `ETag`；请求头 `If-None-Match` 命中时返回 `304`（`skills.go:563-576`） |
| `GET /internal/skills/{name}.tgz` | 返回 `application/gzip` 的 Skill 包，`Content-Disposition: attachment`；名称非法或不存在返回 `404`（`skills.go:579-597`） |

Actor 的拉取循环每 10s 访问一次这些端点（`mf-pi.yaml.tmpl` 注入的脚本），**无鉴权**，因此这两个端点绝不可暴露到集群外。

---

## 5. 用户 Agent API：会话、事件与认证

本节覆盖 pi-web 的**会话（session）/ 认证（auth）/ 会话守护进程（sessiond）** HTTP 与 WebSocket API。

- 源码位于只读仓库 `/home/liuchong/git/cliu-pi-web`，下文所有 `src/...` 路径均以该仓库为根，行号来自当前检出。
- pi-web 是**同机双进程**结构：`web`（浏览器 HTTP 边缘，`src/server/app.ts`）把 `/api/...` 请求反向代理给 `sessiond`（会话守护进程，`src/server/sessiond.ts`）。真正的会话/认证业务逻辑全部注册在 `sessiond` 上，`web` 只做代理与 WebSocket 桥接（`src/server/sessiond/sessionProxyRoutes.ts:18-54`）。
- `sessiond` 自身不监听 `/api` 前缀：`registerSessionRoutes(app, sessions, eventHub)` 与 `registerAuthRoutes(app, auth)` 都以空前缀注册（`src/server/sessiond.ts:354-355`），因此守护进程上的路径是 `/sessions/...`、`/auth/...`、`/events`、`/health`、`/runtime`。`web` 层再加 `/api` 前缀并转发（`src/server/app.ts:251-252`）。
- 浏览器默认也是走 `/api/machines/local/...` 这条别名访问本机会话（`src/client/src/api/sockets.ts:4-15,17-19`）。

### 路径别名：`/api/...` 同时挂在 `/api/machines/local/...`

`app.ts` 把同一套会话代理路由注册了**两次**：

```ts
registerSessionProxyRoutes(app, sessionDaemon);                          // 前缀默认 "/api"
registerSessionProxyRoutes(app, sessionDaemon, "/api/machines/local");   // app.ts:251-252
```

`registerSessionProxyRoutes` 的默认前缀就是 `"/api"`（`src/server/sessiond/sessionProxyRoutes.ts:18`）。因此：

> **本节列出的每一个 `/api/...` 路径，都同时等价地服务在 `/api/machines/local/...` 上。** 例如 `/api/sessions/:id/events` ⇔ `/api/machines/local/sessions/:id/events`。两者由同一段代码处理，请求体、响应体、状态码完全一致。

代理内部会先 `stripPrefix` 掉 `/api` 或 `/api/machines/local`，再以 `SessionDaemonClient` 转发到守护进程（`sessionProxyRoutes.ts:19-30,56-61`；`src/sessiond/sessionDaemonClient.ts:30-68`）。守护进程通过 Unix socket（`PI_WEB_SESSIOND_SOCKET`，默认 `<data-dir>/sessiond.sock`）或 TCP（`PI_WEB_SESSIOND_URL`）通信（`src/sessiond/config.ts:4-21`）。

### 端点清单

下表为本节范围内注册的全部路由：前 12 行是 `web` 层显式注册的代理/WebSocket 桥接入口，其余行是转发后在 `sessiond` 上由具体处理器实现的端点。所有行都同样存在于 `/api/machines/local/...` 别名下（已在上文确认），表中不再逐行重复。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/api/sessiond/health` | 守护进程健康检查；代理到 `sessiond` 的 `GET /health` |
| GET | `/api/sessiond/runtime` | 守护进程运行时/版本组件信息；代理到 `GET /runtime` |
| ALL | `/api/status` | 机器状态快照；代理到 `GET /status`（`sessiond` 上只注册了 GET） |
| ALL | `/api/notices` | 服务器通知快照；代理到 `sessiond` 的 `GET /notices` |
| ALL | `/api/notices/dismiss` | 关闭一条服务器通知；代理到 `sessiond` 的 `POST /notices/dismiss` |
| ALL | `/api/auth` | 认证路由兜底代理（无此项时不构成有效端点） |
| ALL | `/api/auth/*` | 认证子路径兜底代理：`/auth/providers`、`/auth/api-key/*`、`/auth/logout`、`/auth/oauth/*` |
| ALL | `/api/sessions` | 会话集合兜底代理：转发 `GET/POST /sessions` |
| ALL | `/api/sessions/*` | 会话子路径兜底代理：转发全部 `/sessions/:sessionId/...` 与 `/sessions/notifications`、`/sessions/unread`、`/sessions/cleanup*`、`/sessions/bulk/*`（GET 的 `/sessions/events` 由上面的显式 WebSocket 路由接管） |
| GET (WS) | `/api/sessions/:sessionId/events` | 单会话事件 WebSocket；`web` 显式桥接到守护进程同路径 |
| GET (WS) | `/api/sessions/events` | 全局事件 WebSocket；桥接到守护进程 `/sessions/events` |
| GET (WS) | `/api/events` | 全局事件 WebSocket（与上一行同义）；桥接到 `/events` |
| GET | `/api/sessions` | 列出某工作目录下的会话（**必填** `cwd` 查询参数） |
| POST | `/api/sessions` | 新建会话（可带 `startupToken`） |
| GET | `/api/sessions/notifications` | 全部会话的通知目录快照 |
| GET | `/api/sessions/unread` | 未读会话目录快照 |
| POST | `/api/sessions/:sessionId/unread/acknowledge` | 把某会话的未读确认到指定完成序号 |
| POST | `/api/sessions/cleanup/preview` | 清理预演（只统计不执行） |
| POST | `/api/sessions/cleanup` | 执行清理（归档 + 删除已归档） |
| POST | `/api/sessions/bulk/archive` | 批量归档 |
| POST | `/api/sessions/bulk/delete-archived` | 批量彻底删除已归档会话 |
| GET | `/api/sessions/:sessionId/notifications` | 单会话通知收件箱快照 |
| POST | `/api/sessions/:sessionId/notifications/dismiss` | 关闭单条通知 |
| POST | `/api/sessions/:sessionId/notifications/dismiss-all` | 关闭到指定序号为止的全部通知 |
| GET | `/api/sessions/:sessionId/messages` | 分页读取会话消息 |
| GET | `/api/sessions/:sessionId/status` | 会话状态快照 |
| GET | `/api/sessions/:sessionId/stream-snapshot` | 加入时的在途流快照（`seq` + `partial`） |
| GET | `/api/sessions/:sessionId/models` | 会话当前可选模型列表 |
| GET | `/api/sessions/:sessionId/models/catalog` | 机器全量模型目录 + 启用状态 |
| POST | `/api/sessions/:sessionId/models/enabled` | 启用/停用单个模型 |
| POST | `/api/sessions/:sessionId/models/scope` | 批量设置模型范围（`all` / `current`） |
| POST | `/api/sessions/:sessionId/model` | 选择具体 provider/model |
| POST | `/api/sessions/:sessionId/model/cycle` | 按方向循环切换模型 |
| GET | `/api/sessions/:sessionId/defaults` | 读取会话默认 provider/model/thinking 设置 |
| POST | `/api/sessions/:sessionId/defaults` | 写入会话默认设置（两种互斥形式） |
| GET | `/api/sessions/:sessionId/thinking-levels` | 可用 thinking level 列表 |
| POST | `/api/sessions/:sessionId/thinking-level` | 设置 thinking level |
| POST | `/api/sessions/:sessionId/thinking-level/cycle` | 循环切换 thinking level |
| GET | `/api/sessions/:sessionId/commands` | 可用斜杠命令列表 |
| POST | `/api/sessions/:sessionId/prompt` | 发送用户提示（可带附件/流式行为） |
| POST | `/api/sessions/:sessionId/queue/clear` | 清空排队中的消息 |
| POST | `/api/sessions/:sessionId/ask/submit` | 提交 `ask_user` 问题集的回答 |
| POST | `/api/sessions/:sessionId/ask/cancel` | 取消 `ask_user` 问题集 |
| POST | `/api/sessions/:sessionId/dialogs/answer` | 回答扩展 UI 对话框 |
| POST | `/api/sessions/:sessionId/dialogs/cancel` | 取消扩展 UI 对话框 |
| POST | `/api/sessions/:sessionId/warnings/dismiss` | 关闭某条会话警告 |
| POST | `/api/sessions/:sessionId/attachments` | 把附件写入工作区并返回路径 |
| POST | `/api/sessions/:sessionId/shell` | 执行 `!` / `!!` shell 命令 |
| POST | `/api/sessions/:sessionId/commands/run` | 运行斜杠命令 |
| POST | `/api/sessions/:sessionId/commands/respond` | 回应需要选择的命令（`select` 结果） |
| POST | `/api/sessions/:sessionId/tree/navigate` | 会话树导航（可带摘要策略） |
| POST | `/api/sessions/:sessionId/tree/fork` | 从会话树某个 entry 分叉出新会话 |
| POST | `/api/sessions/:sessionId/abort` | 中止当前生成 |
| POST | `/api/sessions/:sessionId/stop` | 停止会话运行时 |
| POST | `/api/sessions/:sessionId/archive` | 归档单个会话 |
| POST | `/api/sessions/:sessionId/archive-tree` | 归档该会话及其子会话树 |
| POST | `/api/sessions/:sessionId/restore` | 取消归档 |
| POST | `/api/sessions/:sessionId/reload` | 从磁盘重新加载会话运行时 |
| POST | `/api/sessions/:sessionId/detach-parent` | 与父会话解绑 |
| GET | `/api/auth/providers` | 登录/登出可用的 provider 列表 |
| POST | `/api/auth/api-key/interactive` | 启动交互式 API-key 登录流 |
| POST | `/api/auth/logout` | 删除某 provider 的已存凭据 |
| POST | `/api/auth/oauth` | 启动 OAuth 登录流 |
| GET | `/api/auth/oauth/:flowId` | 轮询登录流状态 |
| POST | `/api/auth/oauth/:flowId/respond` | 回应登录流的 prompt/select |
| POST | `/api/auth/oauth/:flowId/cancel` | 取消登录流 |

> `sessiond` 上还注册了机器状态、服务器通知、工作区目录、插件后端、工作区移除等路由（`src/server/sessiond.ts:351-376`），以及 `/health`、`/runtime`（`sessiond.ts:378-392`）。其中只有 `/status`、`/notices`、`/notices/dismiss`、`/health`、`/runtime` 属于本节的代理范围；其余工作区/插件路由不在本节展开。

### 通用约定

#### 会话引用与 `cwd`

几乎所有单会话路由都用 `{ id, cwd }` 作为会话引用（`SessionRef`，`src/shared/apiTypes.ts:432-435`）。`cwd` 是**会话所属工作目录的绝对路径**，它的作用是选择该 id 要在哪个会话存储里解析（`sessionRoutes.ts:562-573`）。

- GET 路由：`cwd` 走查询参数；缺失或空串时处理器统一返回 `400 {"error":"cwd query parameter is required"}`（`sessionRoutes.ts:545-560`）。
- POST 路由：`cwd` 走 JSON body；非字符串返回 `400 {"error":"cwd field must be a string"}`，空串返回 `400 {"error":"cwd field must not be empty"}`（`sessionRoutes.ts:569-573`）。
- 所有 `cwd` 都经 `normalizeRequestCwd` 严格规范化：必须是字符串、非空、**绝对路径**，否则抛错；通过后按 `resolve()` 归一化（`src/server/workingDirectory.ts:21-30`）。

```ts
interface SessionRef {   // src/shared/apiTypes.ts:432-435
  id: string;
  cwd: string;
}
```

#### 错误响应与状态码

错误体统一为 `{"error": "..."}`。状态码由处理器选择：

| 辅助函数 | 位置 | 映射 |
|---|---|---|
| `mutationErrorStatus(error)` | `sessionRoutes.ts:744-746` | 错误消息严格等于 `"Session not found"` 或 `"Archived session not found"` → `404`；其余 → `400` |
| `notificationErrorStatus(error)` | `sessionRoutes.ts:748-750` | 与 `mutationErrorStatus` **完全相同**（`400`/`404`），虽然名字暗示 503，但源码不返回 503 |
| `isSessionNotFoundError(error)` | `sessionRoutes.ts:752-755` | 上述两个字符串的判定实现 |
| `errorMessage(error)` | `sessionRoutes.ts:740-742` | `Error.message`，非 Error 用 `String(error)` |

另外几类固定策略：

- `GET /sessions`、`POST /sessions`、`POST /cleanup*`、`GET/POST /defaults`、`GET /sessions/notifications`：捕获任何错误后一律 `400`（`sessionRoutes.ts:38-58,60-66,97-111,256-271`）。
- `GET /sessions/unread`：任何错误 → `503`（`sessionRoutes.ts:68-74`）；`POST /sessions/:sessionId/unread/acknowledge` 解析错误 `400`、服务错误 `503`（`sessionRoutes.ts:76-95`）。
- `messages`、`status`、`stream-snapshot`、`models`、`models/catalog`、`thinking-levels`、`commands` 这些只读 GET：`cwd` 解析失败 `400`，其余服务错误一律 `404`（`sessionRoutes.ts:164-214,273-281,303-311`）。

**代理层额外错误**：`web` 无法连上 `sessiond` 时返回 `502 {"error":"Session daemon unavailable: <原因>"}`（`sessionProxyRoutes.ts:68-70`）。守护进程正在关闭时，任何请求返回 `503 {"error":"Session daemon is shutting down"}`（`src/server/sessiond.ts:100-106`）。

Fastify 自身的请求解析错误（非法 JSON、超过 body 上限）不受上述处理器控制，会走框架默认错误体。<!-- TODO: unverified: 框架级 400/413 的具体 body 未在本节范围内验证 -->

### 会话生命周期与列表

#### `GET /api/sessions`

列出某个工作目录下的会话（`sessionRoutes.ts:38-45`）。

**路径参数**：无。

**查询参数**

| 名称 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `cwd` | string | **是** | — | 工作目录绝对路径；空串或缺失 → `400` |

**请求体**：无。

**成功响应** `200`：`SessionInfo[]`（`ClientSession[]`，服务契约 `sessionService.ts:51`；类型 `apiTypes.ts:605-617`）。

```json
[
  {
    "id": "0f2ab9c1",
    "cwd": "/data/work/demo",
    "path": "/data/pi-agent/sessions/20251010T101500-0f2ab9c1.jsonl",
    "persisted": true,
    "name": "Fix the build",
    "created": "2025-10-10T10:15:00.000Z",
    "modified": "2025-10-10T10:42:11.000Z",
    "messageCount": 12,
    "firstMessage": "Fix the build",
    "parentSessionPath": "/data/pi-agent/sessions/20251009T090000-parent.jsonl",
    "archived": false,
    "archivedAt": "2025-10-10T10:50:00.000Z"
  }
]
```

可选字段：`persisted`、`name`、`parentSessionPath`、`archived`、`archivedAt`（`apiTypes.ts:605-617`）。

**错误**
- `400 {"error":"cwd query parameter is required"}` — `cwd` 缺失或空串（`sessionRoutes.ts:39`）。
- `400 {"error":"<message>"}` — `normalizeRequestCwd` 或服务层抛错（相对路径、非法目录等；`sessionRoutes.ts:42-44`）。

```bash
curl -u alice:secret 'http://localhost:58681/alice/api/sessions?cwd=%2Fdata%2Fwork%2Fdemo'
```

#### `POST /api/sessions`

新建会话（`sessionRoutes.ts:47-58`）。

**路径参数**：无。**查询参数**：无。

**请求体**（源码类型 `{ cwd?: unknown; startupToken?: unknown } | undefined`，`sessionRoutes.ts:47`）

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `cwd` | string | 是 | 绝对路径；`requireString` + `normalizeRequestCwd`，非字符串 → `400 "cwd field must be a string"` |
| `startupToken` | string | 否 | 调用方自带的不透明标签，服务原样回显到启动进度事件；非空字符串，非字符串 → `400 "startupToken field must be a string"`（`sessionRoutes.ts:53`） |

**成功响应** `200`：新会话的 `SessionInfo`（与 `GET /sessions` 的元素同形；`sessionService.ts:57`）。

```json
{
  "id": "1c7d2e44",
  "cwd": "/data/work/demo",
  "path": "/data/pi-agent/sessions/20251010T110000-1c7d2e44.jsonl",
  "persisted": true,
  "created": "2025-10-10T11:00:00.000Z",
  "modified": "2025-10-10T11:00:00.000Z",
  "messageCount": 0,
  "firstMessage": ""
}
```

**错误**：`400 {"error":"request body must be an object"}`；`400`（`cwd` 相关）；`400 {"error":"cwd must be an absolute path"}`（`sessionRoutes.ts:49-57`）。

```bash
curl -u alice:secret -X POST \
  -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","startupToken":"row-7"}' \
  'http://localhost:58681/alice/api/sessions'
```

#### 简单状态变更端点（共享 `{cwd}` 请求体）

以下 6 个端点结构一致：POST、路径参数 `sessionId`、body 为 `{cwd}` 记录。源码里 `Body` 泛型把 `cwd` 标成可选，但处理器用 `optionalRecord`（缺省/null → 空对象；`sessionRoutes.ts:664-667`）后立即调用 `sessionRefFromBody` → `requireRefCwd`，因此 **`cwd` 是事实必填**：缺失或 `null` 会得到 `400 {"error":"cwd field must be a string"}`（`sessionRoutes.ts:562-573`）。成功都返回一个带布尔标志的对象，错误都走 `mutationErrorStatus`（`400`/`404`）。

| 方法 + 路径 | 源码 | 成功响应体 | 语义 |
|---|---|---|---|
| `POST /api/sessions/:sessionId/abort` | `sessionRoutes.ts:437-444` | `{"aborted":true}` | 中止当前生成 |
| `POST /api/sessions/:sessionId/stop` | `sessionRoutes.ts:446-453` | `{"stopped":true}` | 停止会话运行时 |
| `POST /api/sessions/:sessionId/archive` | `sessionRoutes.ts:455-462` | `{"archived":true}` | 归档 |
| `POST /api/sessions/:sessionId/restore` | `sessionRoutes.ts:472-479` | `{"restored":true}` | 取消归档 |
| `POST /api/sessions/:sessionId/reload` | `sessionRoutes.ts:481-488` | `{"reloaded":true}` | 从磁盘重新加载 |
| `POST /api/sessions/:sessionId/detach-parent` | `sessionRoutes.ts:490-497` | `{"detached":true}` | 与父会话解绑 |

```ts
// 六个端点共用的 Body 泛型，例如 sessionRoutes.ts:437
app.post<{ Params: { sessionId: string }; Body: { cwd?: unknown } | undefined }>(...)
```

**路径参数**

| 名称 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `sessionId` | string | 是 | 会话 id |

**查询参数**：无。

**成功响应** `200`：见上表，例如 `{"aborted":true}`。

**错误**
- `400 {"error":"cwd field must be a string"}` / `"cwd field must not be empty"` / `"cwd must be an absolute path"`（`sessionRoutes.ts:569-573`）。
- `404 {"error":"Session not found"}` 或 `404 {"error":"Archived session not found"}`（`sessionRoutes.ts:752-755`）。
- 其余服务错误 `400 {"error":"<message>"}`。

```bash
BASE=http://localhost:58681/alice
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo"}' "$BASE/api/sessions/0f2ab9c1/abort"
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo"}' "$BASE/api/sessions/0f2ab9c1/stop"
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo"}' "$BASE/api/sessions/0f2ab9c1/archive"
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo"}' "$BASE/api/sessions/0f2ab9c1/restore"
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo"}' "$BASE/api/sessions/0f2ab9c1/reload"
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo"}' "$BASE/api/sessions/0f2ab9c1/detach-parent"
```

#### `POST /api/sessions/:sessionId/archive-tree`

归档该会话连同其子会话树（`sessionRoutes.ts:464-470`）。请求形状与上表相同（`{cwd?}`）。成功响应为 `ArchiveSessionsResponse`（`apiTypes.ts:619-624`；服务签名 `sessionService.ts:102`）：

```json
{
  "archived": true,
  "sessionIds": ["1c7d2e44", "2aa91b02"],
  "archivedCount": 2,
  "skippedAlreadyArchivedCount": 0
}
```

`sessionIds`、`archivedCount`、`skippedAlreadyArchivedCount` 在该类型中都是可选字段（`apiTypes.ts:619-624`）。错误同 `mutationErrorStatus`。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo"}' "$BASE/api/sessions/1c7d2e44/archive-tree"
```

#### `POST /api/sessions/bulk/archive` 与 `POST /api/sessions/bulk/delete-archived`

批量操作（`sessionRoutes.ts:113-127`），共用 body 类型与解析：

```ts
// src/shared/apiTypes.ts:626-633
interface SessionBulkMutationRef { id: string; cwd: string; }
interface SessionBulkMutationRequest { sessions: SessionBulkMutationRef[]; }
```

解析在 `bulkMutationRefsFromBody` / `parseBulkMutationRef`（`sessionRoutes.ts:514-526`）：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `sessions` | array | 是 | 必须是数组，否则 `400 "sessions field must be an array"` |
| `sessions[].id` | string | 是 | `trim()` 后不得为空，否则 `400 "id field must not be empty"` |
| `sessions[].cwd` | string | 是 | 同 `requireRefCwd`：必须是非空绝对路径，并规范化（`sessionRoutes.ts:525,569-573`） |

**成功响应** `200`

`POST .../bulk/archive` → `SessionBulkArchiveResponse`（`apiTypes.ts:640-645`；服务 `sessionService.ts:92`）：

```json
{
  "archived": true,
  "archivedSessionIds": ["0f2ab9c1"],
  "failures": [{ "sessionId": "2aa91b02", "error": "Session not found" }],
  "generatedAt": "2025-10-10T11:05:00.000Z"
}
```

`POST .../bulk/delete-archived` → `SessionBulkDeleteArchivedResponse`（`apiTypes.ts:647-652`）：

```json
{
  "deleted": true,
  "deletedSessionIds": ["3bb0c1d9"],
  "failures": [],
  "generatedAt": "2025-10-10T11:06:00.000Z"
}
```

注意：单个会话失败记在 `failures[]` 中，**不会**让整个请求变成非 200。

**错误**：解析错误或服务抛出的非 not-found 错误 → `400 {"error":"..."}`；`"Session not found"` / `"Archived session not found"` → `404`（`sessionRoutes.ts:117,125` + `744-755`）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"sessions":[{"id":"0f2ab9c1","cwd":"/data/work/demo"}]}' \
  "$BASE/api/sessions/bulk/archive"

curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"sessions":[{"id":"3bb0c1d9","cwd":"/data/work/demo"}]}' \
  "$BASE/api/sessions/bulk/delete-archived"
```

#### `POST /api/sessions/cleanup/preview` 与 `POST /api/sessions/cleanup`

会话清理（`sessionRoutes.ts:97-111`）。两者都用 `normalizeSessionCleanupRequest(optionalRecord(request.body))` 解析同样的 body：

```ts
// src/shared/apiTypes.ts:654-661
interface SessionCleanupRequest {
  archiveIdleDays?: number | null;
  deleteArchivedDays?: number | null;
  projectCwds?: string[] | null;
}
```

| 字段 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `archiveIdleDays` | number \| null | 否 | 禁用归档 | 修改时间早于「N 天前」的未归档会话会被归档；必须是 `>= 0` 的整数（`sessionCleanup.ts:139-144`） |
| `deleteArchivedDays` | number \| null | 否 | 禁用删除 | `archivedAt` 早于「N 天前」的已归档会话会被彻底删除 |
| `projectCwds` | string[] \| null | 否 | 全部已发现项目/工作区 | 只处理这些 cwd；必须是字符串数组，重复项会被去重（`sessionCleanup.ts:146-151`） |

`null` 与缺省等价，均表示禁用/不过滤（`sessionCleanup.ts:139-151`）。正在工作的会话会跳过并出现在 `skippedBusySessionIds`（`sessionCleanup.ts:56-84,115-122`）。

**成功响应** `200`

`preview` → `SessionCleanupPreviewResponse`（`apiTypes.ts:679-685`）：

```json
{
  "generatedAt": "2025-10-10T11:10:00.000Z",
  "thresholds": { "archiveIdleDays": 30, "deleteArchivedDays": 90 },
  "projects": [{ "cwd": "/data/work/demo", "archiveCount": 2, "deleteCount": 1 }],
  "totals": { "archiveCount": 2, "deleteCount": 1 },
  "skippedBusySessionIds": ["9f1c0b7a"]
}
```

`cleanup` → `SessionCleanupExecuteResponse`，即预览响应加上两个 id 数组（`apiTypes.ts:687-690`）：

```json
{
  "generatedAt": "2025-10-10T11:11:00.000Z",
  "thresholds": { "archiveIdleDays": 30, "deleteArchivedDays": 90 },
  "projects": [{ "cwd": "/data/work/demo", "archiveCount": 2, "deleteCount": 1 }],
  "totals": { "archiveCount": 2, "deleteCount": 1 },
  "archivedSessionIds": ["0f2ab9c1", "2aa91b02"],
  "deletedSessionIds": ["3bb0c1d9"]
}
```

**错误**：一律 `400 {"error":"<message>"}`，例如 `"archiveIdleDays field must be a non-negative integer"`、`"projectCwds field must be an array of strings"`（`sessionRoutes.ts:100-110`；`sessionCleanup.ts:139-151`）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"archiveIdleDays":30,"deleteArchivedDays":90,"projectCwds":["/data/work/demo"]}' \
  "$BASE/api/sessions/cleanup/preview"

curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"archiveIdleDays":30,"deleteArchivedDays":90}' \
  "$BASE/api/sessions/cleanup"
```

### 消息与流快照

#### `GET /api/sessions/:sessionId/messages`

分页读取会话消息（`sessionRoutes.ts:164-174`）。响应会先经 `projectBrowserMessageResponse` 去掉 thinking 块里的 `thinkingSignature`（`browserMessageProjection.ts:8-26`）。

**路径参数**

| 名称 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `sessionId` | string | 是 | 会话 id |

**查询参数**

| 名称 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `cwd` | string | **是** | — | 绝对路径；缺失 → `400` |
| `before` | string（数字） | 否 | 消息总数 | 读取该下标之前的消息；空串或非有限数字视为未提供（`sessionRoutes.ts:730-734`） |
| `limit` | string（数字） | 否 | `100` | 单页上限，源码限制为 `1..500`（`messagePaging.ts:19`） |

分页会在「turn 边界」（`role === "user"` 的消息）处向前扩展起点，保证不把一轮对话截断（`messagePaging.ts:15-36`）。若 `before` 与 `limit` 都未提供，则直接返回全部消息且 `start=0`（`messagePaging.ts:17`）。

**成功响应** `200`：`MessagePage`（`apiTypes.ts:1295-1299`）

```json
{
  "messages": [
    { "role": "user", "content": [{ "type": "text", "text": "Fix the build" }] },
    { "role": "assistant", "content": [{ "type": "text", "text": "Looking into it." }] }
  ],
  "start": 0,
  "total": 2
}
```

<!-- TODO: unverified: `messages[]` 元素是 pi SDK 的原生 Message 对象，元素内部字段未在本仓库中规范化 -->

**错误**
- `400 {"error":"cwd query parameter is required"}` 或 `400 {"error":"cwd must be an absolute path"}`（`sessionRoutes.ts:553-560`）。
- `404 {"error":"..."}` — 其余任何服务错误（`sessionRoutes.ts:171-173`）。

```bash
curl -u alice:secret "$BASE/api/sessions/0f2ab9c1/messages?cwd=%2Fdata%2Fwork%2Fdemo&before=200&limit=50"
```

#### `GET /api/sessions/:sessionId/stream-snapshot`

加入事件 WebSocket 时用来播种在途 assistant 流的快照（`sessionRoutes.ts:186-194`）。

**路径参数**：`sessionId`（string，必填）。

**查询参数**：`cwd`（string，**必填**）。

**成功响应** `200`：`SessionStreamSnapshot`（`apiTypes.ts:1309-1313`）

```json
{
  "seq": 137,
  "partial": null
}
```

- `seq`：事件中心当前每会话序号水位；客户端随后只应用 `seq > snapshot.seq` 的缓冲事件（`sessionEventHub.ts:55-63`）。
- `partial`：浏览器投影后的在途 `AssistantMessage`，空闲时为 `null`（`apiTypes.ts:1305-1307`）。

**错误**：`400`（`cwd`）；其余 `404 {"error":"..."}`（`sessionRoutes.ts:191-193`）。

```bash
curl -u alice:secret "$BASE/api/sessions/0f2ab9c1/stream-snapshot?cwd=%2Fdata%2Fwork%2Fdemo"
```

### 状态、模型、thinking level 与命令

#### `GET /api/sessions/:sessionId/status`

会话状态快照（`sessionRoutes.ts:176-184`）。

**路径参数**：`sessionId`（string，必填）。**查询参数**：`cwd`（string，**必填**）。

**成功响应** `200`：`SessionStatus`（`apiTypes.ts:1138-1171`）

```json
{
  "sessionId": "0f2ab9c1",
  "persisted": true,
  "model": { "provider": "deepseek", "id": "deepseek-chat", "name": "DeepSeek Chat", "contextWindow": 65536 },
  "thinkingLevel": "medium",
  "isStreaming": false,
  "isCompacting": false,
  "isBashRunning": false,
  "pendingMessageCount": 0,
  "queuedMessages": [],
  "messageCount": 12,
  "tokens": { "input": 12000, "output": 800, "cacheRead": 0, "cacheWrite": 0, "total": 12800 },
  "cost": 0.0123,
  "contextUsage": { "tokens": 12800, "contextWindow": 65536, "percent": 19.53 },
  "warnings": [
    { "severity": "warning", "message": "Skill X failed to load", "source": "skill", "path": "/data/pi-agent/skills/x/SKILL.md" }
  ],
  "pendingAsk": {
    "askId": "ask-1",
    "askedAt": "2025-10-10T11:20:00.000Z",
    "questions": [
      { "id": "q1", "question": "Which environment?", "options": [{ "value": "prod", "label": "Prod" }], "multiple": false }
    ]
  },
  "pendingDialogs": [
    { "dialogId": "dlg-1", "kind": "confirm", "title": "Overwrite file?", "message": "a.txt exists", "askedAt": "2025-10-10T11:21:00.000Z", "runScoped": true }
  ]
}
```

必填字段：`sessionId`、`isStreaming`、`isCompacting`、`isBashRunning`、`pendingMessageCount`、`queuedMessages`、`tokens`、`cost`；其余（`persisted`、`model`、`thinkingLevel`、`messageCount`、`contextUsage`、`warnings`、`pendingAsk`、`pendingDialogs`）可选（`apiTypes.ts:1138-1171`）。`queuedMessages[]` 元素为 `{ kind: "steer" | "followUp", text: string }`（`apiTypes.ts:708-711`）。

**错误**：`400`（`cwd`）；其余 `404 {"error":"..."}`（`sessionRoutes.ts:181-183`）。

```bash
curl -u alice:secret "$BASE/api/sessions/0f2ab9c1/status?cwd=%2Fdata%2Fwork%2Fdemo"
```

#### 模型相关端点

| 方法 + 路径 | 源码 | 成功响应 |
|---|---|---|
| `GET /api/sessions/:sessionId/models` | `sessionRoutes.ts:196-204` | `{"models": SessionModel[]}` |
| `GET /api/sessions/:sessionId/models/catalog` | `sessionRoutes.ts:206-214` | `{"models": SessionModelCatalogEntry[]}` |
| `POST /api/sessions/:sessionId/models/enabled` | `sessionRoutes.ts:216-223` | `{"models": SessionModelCatalogEntry[]}` |
| `POST /api/sessions/:sessionId/models/scope` | `sessionRoutes.ts:225-234` | `{"models": SessionModelCatalogEntry[]}` |
| `POST /api/sessions/:sessionId/model` | `sessionRoutes.ts:236-243` | `SessionStatus` |
| `POST /api/sessions/:sessionId/model/cycle` | `sessionRoutes.ts:245-254` | `SessionStatus` |

**路径参数**：`sessionId`（string，必填）。两个 GET 的查询参数：`cwd`（string，**必填**）。

**POST 请求体**

```ts
// models/enabled — sessionRoutes.ts:216
Body: { cwd?: unknown; provider?: unknown; modelId?: unknown; enabled?: unknown } | undefined

// models/scope — sessionRoutes.ts:225
Body: { cwd?: unknown; mode?: unknown } | undefined

// model — sessionRoutes.ts:236
Body: { cwd?: unknown; provider?: unknown; modelId?: unknown } | undefined

// model/cycle — sessionRoutes.ts:245
Body: { cwd?: unknown; direction?: "forward" | "backward" } | undefined
```

| 端点 | 字段 | 类型 | 必填 | 默认 | 允许值/说明 |
|---|---|---|---|---|---|
| `models/enabled` | `provider` | string | 是 | — | `requireString` |
| `models/enabled` | `modelId` | string | 是 | — | `requireString` |
| `models/enabled` | `enabled` | boolean | 是 | — | `requireBoolean`，否则 `400 "enabled field must be a boolean"` |
| `models/scope` | `mode` | string | 是 | — | `"all"` 或 `"current"`，其余 → `400 "mode field must be all or current"`（`sessionRoutes.ts:229`） |
| `model` | `provider` / `modelId` | string | 是 | — | `requireString` |
| `model/cycle` | `direction` | string | 否 | `"forward"` | `"forward"` 或 `"backward"`，其余 → `400 "direction must be forward or backward"`（`sessionRoutes.ts:249-250`） |

`SessionModel`（`apiTypes.ts:1002-1008`）所有字段可选：

```json
{ "models": [{ "provider": "deepseek", "id": "deepseek-chat", "name": "DeepSeek Chat", "contextWindow": 65536 }] }
```

`SessionModelCatalogEntry`（`apiTypes.ts:1017-1028`）必填 `provider`、`id`、`enabled`；`models/catalog` 与两个写端点的响应把已启用模型排在前面，`catalogIndex` 保留原始目录位置（`apiTypes.ts:1030-1039`）：

```json
{
  "models": [
    { "provider": "deepseek", "id": "deepseek-chat", "name": "DeepSeek Chat", "enabled": true, "editable": true, "catalogIndex": 0 },
    { "provider": "deepseek", "id": "deepseek-reasoner", "name": "DeepSeek Reasoner", "enabled": false, "editable": true, "catalogIndex": 1 }
  ]
}
```

`POST .../model` 与 `POST .../model/cycle` 返回完整 `SessionStatus`（同上一节示例）。

**错误**
- `GET models`、`GET models/catalog`、`GET thinking-levels`、`GET commands`：`400`（`cwd` 缺失/非法）+ 其余 `404 {"error":"..."}`（`sessionRoutes.ts:199-213`）。
- 4 个 POST：`mutationErrorStatus` → `404`（not found）或 `400`（校验/其他错误）。

```bash
curl -u alice:secret "$BASE/api/sessions/0f2ab9c1/models?cwd=%2Fdata%2Fwork%2Fdemo"
curl -u alice:secret "$BASE/api/sessions/0f2ab9c1/models/catalog?cwd=%2Fdata%2Fwork%2Fdemo"

curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","provider":"deepseek","modelId":"deepseek-reasoner","enabled":true}' \
  "$BASE/api/sessions/0f2ab9c1/models/enabled"

curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","mode":"current"}' \
  "$BASE/api/sessions/0f2ab9c1/models/scope"

curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","provider":"deepseek","modelId":"deepseek-reasoner"}' \
  "$BASE/api/sessions/0f2ab9c1/model"

curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","direction":"backward"}' \
  "$BASE/api/sessions/0f2ab9c1/model/cycle"
```

#### 会话默认设置：`GET` / `POST /api/sessions/:sessionId/defaults`

`sessionRoutes.ts:256-271`。GET 的 `cwd` 走查询参数，POST 走 body；两者成功都返回 `SessionDefaults`。

```ts
// src/shared/apiTypes.ts:1098-1108
interface SessionDefaults {
  defaultProvider?: string;
  defaultModel?: string;
  defaultThinkingLevel?: ThinkingLevel;
}
interface SessionDefaultsUpdate {
  provider?: string;
  modelId?: string;
  thinkingLevel?: ThinkingLevel;
}
```

POST body 的实际校验在 `parseSessionDefaultsUpdate`（`src/shared/sessionDefaults.ts:5-16`）：

| 形式 | 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|---|
| 模型形式 | `provider` + `modelId` | string | 是（成对） | 两者都必须是 **非空** 字符串（`trim() !== ""`）；不能与 `thinkingLevel` 同时出现 |
| thinking 形式 | `thinkingLevel` | string | 是（二选一） | 必须是已知等级：`off`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max`（`src/shared/thinkingLevels.ts:16`） |

同时给 `thinkingLevel` 与 `provider`/`modelId` → `400 {"error":"Specify either provider/modelId or thinkingLevel"}`；等级非法 → `400 {"error":"Invalid thinking level"}`；模型形式缺字段 → `400 {"error":"provider and modelId must both be non-empty strings"}`（`sessionDefaults.ts:8-14`）。

**成功响应** `200`

```json
{ "defaultProvider": "deepseek", "defaultModel": "deepseek-chat", "defaultThinkingLevel": "medium" }
```

（字段都可选；`{provider,modelId}` 形式返回 `defaultProvider`/`defaultModel`，thinking 形式返回 `defaultThinkingLevel`。）

**错误**：两种方法都固定 `400 {"error":"<message>"}`（`sessionRoutes.ts:260,269`）。

```bash
curl -u alice:secret "$BASE/api/sessions/0f2ab9c1/defaults?cwd=%2Fdata%2Fwork%2Fdemo"

curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","provider":"deepseek","modelId":"deepseek-chat"}' \
  "$BASE/api/sessions/0f2ab9c1/defaults"

curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","thinkingLevel":"high"}' \
  "$BASE/api/sessions/0f2ab9c1/defaults"
```

#### thinking level 端点

| 方法 + 路径 | 源码 | 请求体 | 成功响应 |
|---|---|---|---|
| `GET /api/sessions/:sessionId/thinking-levels` | `sessionRoutes.ts:273-281` | 无（`cwd` 查询参数） | `{"levels": string[]}` |
| `POST /api/sessions/:sessionId/thinking-level` | `sessionRoutes.ts:283-292` | `{ cwd?, level? }` | `SessionStatus` |
| `POST /api/sessions/:sessionId/thinking-level/cycle` | `sessionRoutes.ts:294-301` | `{ cwd? }` | `SessionStatus` |

`POST .../thinking-level` 的 `level` 必须是**非空字符串**（`requireThinkingLevel`，`sessionRoutes.ts:721-724`）；实际等级是否可用由服务层对照会话的实时可用集合校验（注释见 `sessionRoutes.ts:286-287`），因此这里只保证 `400 "level field is invalid"` 这一层。`GET .../thinking-levels` 的 `levels` 是服务端当前可用集合（不是固定枚举）。

```json
{ "levels": ["off", "minimal", "low", "medium", "high", "xhigh", "max"] }
```

**错误**：GET → `400`（`cwd`）/其余 `404`；POST → `mutationErrorStatus`（`400`/`404`）。

```bash
curl -u alice:secret "$BASE/api/sessions/0f2ab9c1/thinking-levels?cwd=%2Fdata%2Fwork%2Fdemo"
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo","level":"high"}' "$BASE/api/sessions/0f2ab9c1/thinking-level"
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo"}' "$BASE/api/sessions/0f2ab9c1/thinking-level/cycle"
```

#### 命令端点

| 方法 + 路径 | 源码 | 请求体 | 成功响应 |
|---|---|---|---|
| `GET /api/sessions/:sessionId/commands` | `sessionRoutes.ts:303-311` | 无（`cwd` 查询参数） | `SlashCommand[]` |
| `POST /api/sessions/:sessionId/commands/run` | `sessionRoutes.ts:401-408` | `{ cwd?, text }` | `CommandResult` |
| `POST /api/sessions/:sessionId/commands/respond` | `sessionRoutes.ts:410-417` | `{ cwd?, requestId, value }` | `CommandResult` |

`SlashCommand`（`apiTypes.ts:1173-1185`）必填 `name`、`source`（`"extension" | "prompt" | "skill" | "builtin"`）：

```json
[
  { "name": "review", "description": "Review a PR", "argumentHint": "<PR-URL>", "source": "prompt" },
  { "name": "skill:deploy", "description": "Deploy the app", "source": "skill" }
]
```

`CommandResult`（`apiTypes.ts:1315-1319`）是四选一联合：

```json
{ "type": "done", "message": "Command finished", "promptDraft": "..." }
```

```json
{ "type": "select", "requestId": "req-1", "title": "Pick a branch", "options": [{ "value": "main", "label": "main" }] }
```

```json
{ "type": "tree", "tree": { "nodes": [{ "id": "e1", "parentId": null, "kind": "user", "summary": "Fix the build" }], "activeLeafId": "e1", "activePathIds": ["e1"] } }
```

```json
{ "type": "unsupported", "message": "Command not supported in this environment" }
```

`commands/run` 与 `commands/respond` 的 `text`、`requestId`、`value` 都经 `requireString`，缺失/非字符串 → `400`（`sessionRoutes.ts:404,413`）。

**错误**：GET → `400`/`404`；POST → `mutationErrorStatus`（`400`/`404`）。

```bash
curl -u alice:secret "$BASE/api/sessions/0f2ab9c1/commands?cwd=%2Fdata%2Fwork%2Fdemo"
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo","text":"/review"}' "$BASE/api/sessions/0f2ab9c1/commands/run"
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo","requestId":"req-1","value":"main"}' "$BASE/api/sessions/0f2ab9c1/commands/respond"
```

### 提示、队列、shell 与附件

#### `POST /api/sessions/:sessionId/prompt`

发送用户提示（`sessionRoutes.ts:313-321`）。

**路径参数**：`sessionId`（string，必填）。**查询参数**：无。

**请求体**（源码类型 `PromptRequestBody | undefined`，`sessionRoutes.ts:19-24,313`）

```ts
interface PromptRequestBody {
  cwd?: unknown;
  text?: unknown;
  streamingBehavior?: unknown;
  attachments?: unknown;
}
```

| 字段 | 类型 | 必填 | 默认 | 允许值/说明 |
|---|---|---|---|---|
| `cwd` | string | 是 | — | 绝对路径（`sessionRefFromBody`） |
| `text` | string | 是 | — | 非字符串 → 服务层 `400 {"error":"Prompt text is required"}`（`piSessionService.ts:286-289`） |
| `streamingBehavior` | string | 否 | 会话正在流式/压缩时默认 `"followUp"` | 仅 `"steer"` 或 `"followUp"`；其他值 → `400 'Prompt streamingBehavior must be "steer" or "followUp"'`（`piSessionService.ts:291-295`；`2518-2519`） |
| `attachments` | `PromptImageAttachment[]` | 否 | `[]` | 最多 16 个（`promptAttachments.ts:21`）；每项 `{kind:"image", mimeType, data(base64), name?}`；`mimeType` 限 `image/jpeg|png|gif|webp`；`prompt` 路由不接受 `kind:"file"`（`parsePromptAttachments` 未开 `allowFileAttachments`，`piSessionService.ts:2512`；`promptAttachments.ts:73-79`） |

**成功响应** `200`（处理器的固定返回，**不代表生成已完成**；`sessionRoutes.ts:317`）

```json
{ "accepted": true }
```

**错误**
- `404 {"error":"Session not found"}` / `404 {"error":"Archived session not found"}`。
- 其余全部 `400 {"error":"..."}`：缺 `cwd`、`text` 非字符串、`streamingBehavior` 非法、附件非法/过多、会话只读、树导航进行中等（`sessionRoutes.ts:319` + `744-755`）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","text":"Summarize the README","streamingBehavior":"followUp"}' \
  "$BASE/api/sessions/0f2ab9c1/prompt"
```

#### `POST /api/sessions/:sessionId/queue/clear`

清空排队消息并返回最新状态（`sessionRoutes.ts:323-329`；服务 `sessionService.ts:67`）。Body 为 `{cwd?}`。成功响应为 `SessionStatus`（见上文示例）。错误走 `mutationErrorStatus`。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' -d '{"cwd":"/data/work/demo"}' "$BASE/api/sessions/0f2ab9c1/queue/clear"
```

#### `POST /api/sessions/:sessionId/shell`

执行 shell 命令（`sessionRoutes.ts:391-399`；实现 `piSessionService.ts:2585-2604`）。

**请求体**：`{ cwd?: unknown; text?: unknown } | undefined`（`sessionRoutes.ts:391`）。`text` 必填、`requireString`。

- `text` 以 `!` 开头：命令计入上下文；以 `!!` 开头：`excludeFromContext = true`（`piSessionService.ts:2590-2591`）。
- 去掉前缀后为空 → `400 {"error":"Usage: !<shell command>"}`；已有 bash 在跑 → `400 {"error":"A bash command is already running"}`（`piSessionService.ts:2592-2593`）。

**成功响应** `200`：`{"accepted": true}`（`sessionRoutes.ts:395`）。命令输出通过 WebSocket 的 `shell.start` / `shell.chunk` / `shell.end` 事件推送。

**错误**：`mutationErrorStatus` → `400`（校验/忙）或 `404`（会话不存在）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","text":"!ls -la"}' \
  "$BASE/api/sessions/0f2ab9c1/shell"
```

#### `POST /api/sessions/:sessionId/attachments`

把附件写入工作区并返回路径（`sessionRoutes.ts:379-389`；实现 `piSessionService.ts:2558-2568`）。

**请求体**（源码类型 `AttachmentsRequestBody | undefined`，`sessionRoutes.ts:26-30,379`）

```ts
interface AttachmentsRequestBody {
  cwd?: unknown;
  attachments?: unknown;
  folder?: unknown;
}
```

| 字段 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `cwd` | string | 是 | — | 绝对路径 |
| `attachments` | `PromptAttachment[]` | 是（可为空数组） | — | 最多 16 个；此处**允许 `kind:"file"`**（`piSessionService.ts:2559`）；图片和文件都要求合法 base64（文件允许空 `data`） |
| `folder` | string | 否 | 工作区有效 attachments 配置，最终回退 `.pi-web/attachments` | 非字符串 → `400 {"error":"folder field must be a string"}`（`sessionRoutes.ts:383`）；目录解析被限制在工作区内（`attachmentService.ts:55-60`） |

**成功响应** `200`：`{"attachments": SavedPromptAttachment[]}`（`apiTypes.ts:995-1000`）

```json
{
  "attachments": [
    { "path": ".pi-web/attachments/attachment-20251010-112000-000-notes.pdf", "mimeType": "application/pdf", "size": 20481 }
  ]
}
```

**错误**：`mutationErrorStatus` → `400`（附件非法、目录越界、会话只读等）或 `404`（会话不存在）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","folder":".pi-web/attachments","attachments":[{"kind":"file","mimeType":"application/pdf","data":"JVBERi0xLjQK","name":"notes.pdf"}]}' \
  "$BASE/api/sessions/0f2ab9c1/attachments"
```

### ask_user 与扩展对话框

#### `POST /api/sessions/:sessionId/ask/submit`

提交 `ask_user` 问题集的回答（`sessionRoutes.ts:331-339`）。

**请求体**（声明类型 `{ cwd?: unknown; askId?: unknown; answers?: unknown } | undefined`）

| 字段 | 类型 | 必填 | 上限/说明 |
|---|---|---|---|
| `cwd` | string | 是 | 绝对路径 |
| `askId` | string | 是 | 非空且 ≤ 128 字符（`ASK_USER_ID_MAX_LENGTH`，`apiTypes.ts:724`），否则 `400 "askId field is too long"` |
| `answers` | `AskUserAnswer[]` | 是 | 必须是数组；条数 ≤ 20（`ASK_USER_QUESTION_LIMIT`，`apiTypes.ts:720`） |

`AskUserAnswer`（`apiTypes.ts:777-784`，校验见 `sessionRoutes.ts:625-638`）：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `id` | string | 是 | 非空、≤128 字符 |
| `values` | string[] | 是 | 最多 12 项（`ASK_USER_OPTION_LIMIT`）；每项非空、≤128 字符 |
| `otherText` | string | 否 | 必须是字符串，且 ≤ 4000 字符（`ASK_USER_OTHER_TEXT_MAX_LENGTH`） |

「答案是否匹配实际提问」由 pending ask store 判定；这里只做传输层形状检查（`sessionRoutes.ts:613-617`）。

**成功响应** `200`：`AskUserCloseResponse`（`apiTypes.ts:829-834`）。`result: "stale"` 是正常竞态（该 ask 已被提交/被新 ask 取代/随运行时消失），不是错误（`apiTypes.ts:821-828`）。

```json
{
  "result": "closed",
  "outcome": {
    "askId": "ask-1",
    "reason": "submitted",
    "askedAt": "2025-10-10T11:20:00.000Z",
    "closedAt": "2025-10-10T11:21:30.000Z",
    "questions": [
      {
        "question": { "id": "q1", "question": "Which environment?", "options": [{ "value": "prod", "label": "Prod" }], "multiple": false },
        "answered": true,
        "values": ["prod"]
      }
    ],
    "answeredCount": 1,
    "unansweredIds": [],
    "summary": "Answered 1 of 1"
  },
  "sessionStatus": { "sessionId": "0f2ab9c1" }
}
```

`outcome` 仅在本调用真正关闭该 ask 时出现；`sessionStatus` 是完整的 `SessionStatus`（示例中为节省篇幅省略字段）。

**错误**：`mutationErrorStatus` → `400`（形状/边界/竞态错误由 store 抛出的普通错误）或 `404`（会话不存在）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","askId":"ask-1","answers":[{"id":"q1","values":["prod"]}]}' \
  "$BASE/api/sessions/0f2ab9c1/ask/submit"
```

#### `POST /api/sessions/:sessionId/ask/cancel`

取消打开的 ask（`sessionRoutes.ts:341-348`）。请求体 `{ cwd?: unknown; askId?: unknown } | undefined`；`askId` 校验同上。成功响应同为 `AskUserCloseResponse`（`reason` 为 `"cancelled"`）。错误同 `mutationErrorStatus`。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","askId":"ask-1"}' \
  "$BASE/api/sessions/0f2ab9c1/ask/cancel"
```

#### `POST /api/sessions/:sessionId/dialogs/answer` 与 `.../dialogs/cancel`

扩展 UI 对话框（`confirm` / `select` / `input`），可同时存在多个（`apiTypes.ts:861-890`）。

| 方法 + 路径 | 源码 | 请求体 | 成功响应 |
|---|---|---|---|
| `POST .../dialogs/answer` | `sessionRoutes.ts:350-358` | `{ cwd?, dialogId, value }` | `ExtensionDialogCloseResponse` |
| `POST .../dialogs/cancel` | `sessionRoutes.ts:360-368` | `{ cwd?, dialogId }` | `ExtensionDialogCloseResponse` |

`dialogId`：非空、≤128 字符（`EXTENSION_DIALOG_ID_MAX_LENGTH`，`apiTypes.ts:837`）。`value` 必须是 **boolean 或 string**，否则 `400 {"error":"value field must be a string or a boolean"}`；字符串长度 ≤4000，否则 `400 {"error":"value field is too long"}`（`sessionRoutes.ts:649-658`）。「值是否符合对话框类型」由 pending dialog store 判定，不符合是 `400` 且对话框保持打开（`apiTypes.ts:907-917`）。

**成功响应** `200`：`ExtensionDialogCloseResponse`（`apiTypes.ts:932-937`）

```json
{
  "result": "closed",
  "outcome": { "dialogId": "dlg-1", "reason": "answered", "answer": true, "askedAt": "2025-10-10T11:21:00.000Z", "closedAt": "2025-10-10T11:21:05.000Z" },
  "sessionStatus": { "sessionId": "0f2ab9c1" }
}
```

`result: "stale"` 同样是正常竞态；`reason` 取值 `"answered" | "cancelled" | "timeout" | "aborted" | "session-ended"`（`apiTypes.ts:859`）。

**错误**：`mutationErrorStatus`（`400`/`404`）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","dialogId":"dlg-1","value":true}' \
  "$BASE/api/sessions/0f2ab9c1/dialogs/answer"

curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","dialogId":"dlg-1"}' \
  "$BASE/api/sessions/0f2ab9c1/dialogs/cancel"
```

#### `POST /api/sessions/:sessionId/warnings/dismiss`

关闭一条有持久关闭开关的会话警告（`sessionRoutes.ts:370-377`）。请求体 `{ cwd?: unknown; dismissId?: unknown } | undefined`；`dismissId` 经 `requireString`（非字符串 → `400 {"error":"dismissId field must be a string"}`）。成功返回最新 `SessionStatus`。错误走 `mutationErrorStatus`。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","dismissId":"skill:/data/pi-agent/skills/x/SKILL.md"}' \
  "$BASE/api/sessions/0f2ab9c1/warnings/dismiss"
```

### 会话树：导航与分叉

#### `POST /api/sessions/:sessionId/tree/navigate`

在会话树中导航到某个节点，可选择是否生成摘要（`sessionRoutes.ts:419-426`；解析 `sessionRoutes.ts:575-605`）。

> 注意：该路由声明的 Body 泛型是 `unknown`，但处理器先 `requireRecord(request.body)` 再从中读取 `cwd`，所以实际 body 是「会话引用 + 下列字段」。

```ts
// src/shared/apiTypes.ts:1268-1273
interface SessionTreeNavigateRequest {
  targetId: string;
  expectedLeafId: string | null;
  summary: SessionTreeSummaryChoice;
}
// src/shared/apiTypes.ts:1263-1266
type SessionTreeSummaryChoice =
  | { mode: "none" }
  | { mode: "default" }
  | { mode: "custom"; instructions: string };
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `cwd` | string | 是 | 绝对路径 |
| `targetId` | string | 是 | 非空（`requireNonEmptyString`） |
| `expectedLeafId` | string \| null | 是（键必须存在） | 可为 `null`（空/根位置）；缺失 → `400 "expectedLeafId field is required"`；空串 → `400`；非字符串/非 null → `400`（`sessionRoutes.ts:692-699`） |
| `summary` | object | 是 | 见下 |

`summary` 的严格校验（`sessionRoutes.ts:587-605`）：

- `{ "mode": "none" }` 或 `{ "mode": "default" }`：**不得**出现 `instructions`，且除了 `mode` 之外不允许其他键，否则 `400`。
- `{ "mode": "custom", "instructions": "<非空、≤10000 字符>" }`：`instructions` trim 后不得为空，长度上限 `SESSION_TREE_CUSTOM_INSTRUCTIONS_MAX_LENGTH = 10000`（`apiTypes.ts:1261`）。
- 其他 `mode` → `400 {"error":"summary mode is invalid"}`。

**成功响应** `200`：`SessionTreeNavigateResult`（`apiTypes.ts:1275-1277`）

```json
{ "cancelled": false, "editorText": "continue from here" }
```

或

```json
{ "cancelled": true, "aborted": false }
```

**错误**：`mutationErrorStatus`（`400`/`404`）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","targetId":"e12","expectedLeafId":"e20","summary":{"mode":"default"}}' \
  "$BASE/api/sessions/0f2ab9c1/tree/navigate"
```

#### `POST /api/sessions/:sessionId/tree/fork`

从选定 entry 分叉出新会话（`sessionRoutes.ts:428-435`；解析 `sessionRoutes.ts:581-585`）。

```ts
// src/shared/apiTypes.ts:1279-1283
interface SessionTreeForkRequest {
  entryId: string;
  expectedLeafId: string | null;
}
```

`entryId` 非空字符串；`expectedLeafId` 规则同上一节。成功响应 `SessionTreeForkResult`（`apiTypes.ts:1291-1293`）：

```json
{
  "cancelled": false,
  "session": { "id": "77aa10bb", "cwd": "/data/work/demo", "path": "/data/pi-agent/sessions/20251010T113000-77aa10bb.jsonl", "created": "2025-10-10T11:30:00.000Z", "modified": "2025-10-10T11:30:00.000Z", "messageCount": 4, "firstMessage": "Fix the build" },
  "promptDraft": "Fix the build"
}
```

或 `{ "cancelled": true }`。用户消息 entry 会以 `promptDraft` 形式带回文本；其他 entry 从「该 entry 处」分叉（`apiTypes.ts:1285-1290`）。

**错误**：`mutationErrorStatus`（`400`/`404`）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","entryId":"e12","expectedLeafId":null}' \
  "$BASE/api/sessions/0f2ab9c1/tree/fork"
```

### 未读与通知

#### `GET /api/sessions/unread`

守护进程持有的未读目录快照（`sessionRoutes.ts:68-74`）。无路径/查询/请求体参数。

**成功响应** `200`：`SessionUnreadCatalogSnapshot`（`apiTypes.ts:491-498`）

```json
{
  "catalogId": "b0a1c2d3",
  "catalogRevision": 42,
  "sessions": [
    { "sessionId": "9f1c0b7a", "cwd": "/data/work/demo", "completionOrder": 42, "completedAt": "2025-10-10T10:41:00.000Z" }
  ]
}
```

`catalogId` 在一个持久化 catalog epoch 内稳定；`catalogRevision` 单调递增；`sessions` 最多 1000 条（`SESSION_UNREAD_LIMIT`，`apiTypes.ts:477`）且最新完成在前（`apiTypes.ts:483-498`）。

**错误**：任何错误 → `503 {"error":"<message>"}`（`sessionRoutes.ts:72`）。

```bash
curl -u alice:secret "$BASE/api/sessions/unread"
```

#### `POST /api/sessions/:sessionId/unread/acknowledge`

把某会话的未读确认到指定完成序号（`sessionRoutes.ts:76-95`）。

**路径参数**：`sessionId`（string，必填，非空且 ≤512 字符，`SESSION_UNREAD_SESSION_ID_MAX_LENGTH`）。

**请求体**

| 字段 | 类型 | 必填 | 上限 | 说明 |
|---|---|---|---|---|
| `cwd` | string | 是 | 32768 | 非空、≤32768 字符，再经 `normalizeRequestCwd` |
| `catalogId` | string | 是 | 512 | 观测到 `throughCompletionOrder` 时所在的 catalog epoch |
| `throughCompletionOrder` | number | 是 | — | **正**安全整数（`requirePositiveSafeInteger`，`0` 也报错） |

**成功响应** `200`：确认后的 `SessionUnreadCatalogSnapshot`（同上一节结构）。

**错误**
- `400 {"error":"..."}`：body 形状/边界错误，例如 `"cwd field must not be empty"`、`"throughCompletionOrder field must be positive"`。
- `503 {"error":"..."}`：服务层错误（含 catalog 不匹配）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","catalogId":"b0a1c2d3","throughCompletionOrder":42}' \
  "$BASE/api/sessions/9f1c0b7a/unread/acknowledge"
```

#### `GET /api/sessions/notifications`

全部会话的通知目录快照（`sessionRoutes.ts:60-66`）。无参数。

**成功响应** `200`：`SessionNotificationCatalogSnapshot`（`apiTypes.ts:554-558`）

```json
{
  "daemonInstanceId": "d-3f9c2a",
  "catalogRevision": 7,
  "sessions": [
    { "sessionId": "9f1c0b7a", "cwd": "/data/work/demo", "inboxRevision": 3, "retainedCount": 2, "discardedCount": 0, "highestSeverity": "warning" }
  ]
}
```

`highestSeverity` 可选，取值 `"info" | "warning" | "error"`（`apiTypes.ts:521`）。**错误**：任何错误 → `400 {"error":"..."}`（`sessionRoutes.ts:64`）。

```bash
curl -u alice:secret "$BASE/api/sessions/notifications"
```

#### `GET /api/sessions/:sessionId/notifications`

单会话通知收件箱快照（`sessionRoutes.ts:129-135`）。

**路径参数**：`sessionId`（必填，非空、≤512 字符）。**查询参数**：`cwd`（**必填**，非空、≤32768 字符）。

**成功响应** `200`：`SessionNotificationInboxSnapshot`（`apiTypes.ts:546-552`）

```json
{
  "daemonInstanceId": "d-3f9c2a",
  "catalogRevision": 7,
  "summary": { "sessionId": "9f1c0b7a", "cwd": "/data/work/demo", "inboxRevision": 3, "retainedCount": 2, "discardedCount": 0, "highestSeverity": "warning" },
  "notifications": [
    { "id": "n1", "message": "Build finished", "truncated": false, "severity": "info", "receivedAt": "2025-10-10T10:40:00.000Z", "order": 1 }
  ],
  "dismissThrough": { "order": 0, "overflowWatermark": 0 }
}
```

`SESSION_NOTIFICATION_LIMIT = 100`、单条消息上限 8KiB，超出会置 `truncated: true`（`apiTypes.ts:518-530`）。

**错误**：`notificationErrorStatus` → `404`（`"Session not found"` / `"Archived session not found"`）或 `400`（其余，含 `cwd` 缺失，`sessionRoutes.ts:528-536` 抛出的错误）。

```bash
curl -u alice:secret "$BASE/api/sessions/9f1c0b7a/notifications?cwd=%2Fdata%2Fwork%2Fdemo"
```

#### `POST /api/sessions/:sessionId/notifications/dismiss` 与 `.../dismiss-all`

| 方法 + 路径 | 源码 | 请求体（body 中的 `cwd` 必填） | 成功响应 |
|---|---|---|---|
| `POST .../notifications/dismiss` | `sessionRoutes.ts:137-148` | `{ cwd, daemonInstanceId, notificationId }` | `SessionNotificationInboxSnapshot` |
| `POST .../notifications/dismiss-all` | `sessionRoutes.ts:150-162` | `{ cwd, daemonInstanceId, throughOrder, throughOverflowWatermark }` | `SessionNotificationInboxSnapshot` |

字段边界：`daemonInstanceId` 非空、≤512（`MAX_NOTIFICATION_DAEMON_ID_LENGTH`）；`notificationId` 非空、≤1024（`MAX_NOTIFICATION_ID_LENGTH`）；`throughOrder` 与 `throughOverflowWatermark` 均为**非负**安全整数（`requireNonNegativeSafeInteger`，`sessionRoutes.ts:708-713`）。

**成功响应** `200`：更新后的 inbox 快照（结构与上一节相同）。

**错误**：`notificationErrorStatus`（`400`/`404`）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","daemonInstanceId":"d-3f9c2a","notificationId":"n1"}' \
  "$BASE/api/sessions/9f1c0b7a/notifications/dismiss"

curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"cwd":"/data/work/demo","daemonInstanceId":"d-3f9c2a","throughOrder":5,"throughOverflowWatermark":0}' \
  "$BASE/api/sessions/9f1c0b7a/notifications/dismiss-all"
```

### sessiond 控制面（代理）

这三个路径在 `web` 层显式注册并转发到守护进程（`sessionProxyRoutes.ts:32-33,47-49`）。

| 方法 | 路径 | 上游 | 说明 |
|---|---|---|---|
| GET | `/api/sessiond/health` | `GET /health` | 守护进程健康检查 |
| GET | `/api/sessiond/runtime` | `GET /runtime` | 运行时/版本组件描述 |
| ALL | `/api/status` | `GET /status` | 机器状态快照 |
| ALL | `/api/notices` | `GET /notices` | 服务器通知快照 |
| ALL | `/api/notices/dismiss` | `POST /notices/dismiss` | 关闭通知 |

代理会原样透传上游 `content-type` 与状态码，并把非空响应体按 JSON 解析后转发（`sessionProxyRoutes.ts:19-30,63-66`）。上游返回空体时响应体为空（`sessionProxyRoutes.ts:25`）。

**`GET /api/sessiond/health` 成功响应**（守护进程实现，`sessiond.ts:378-390`）

```json
{
  "ok": true,
  "activeSessions": 3,
  "checkedAt": "2025-10-10T11:40:00.000Z",
  "version": {
    "component": "sessiond",
    "label": "Session daemon",
    "runtimeVersion": "0.9.4",
    "piVersion": "0.84.0",
    "stale": false,
    "available": true
  }
}
```

`runtimeVersion`、`piVersion` 只在运行时组件报告了该字段时出现（`sessiond.ts:385-386`）；`label` 由 `getPiWebRuntimeComponent` 固定为 `"Session daemon"`（`src/server/piWebStatus.ts:88-98`）。

**`GET /api/sessiond/runtime` 成功响应**：`PiWebRuntimeComponent` 加 `activeAgentProfile`（`sessiond.ts:309-316,392`；`apiTypes.ts:1198-1211`）

```json
{
  "component": "sessiond",
  "label": "Session daemon",
  "runtimeVersion": "0.9.4",
  "piVersion": "0.84.0",
  "available": true,
  "capabilities": [],
  "activeAgentProfile": { "schemaVersion": 2, "dir": "/data/pi-agent" },
  "deprecatedAgentInputs": []
}
```

`sessiond` 的运行时 capability 集合当前为空（`SESSIOND_RUNTIME_CAPABILITIES = []`，`src/shared/capabilities.ts:16`），所以 `capabilities` 通常是 `[]`；`deprecatedAgentInputs` 只在检测到弃用输入时出现（`src/server/piWebStatus.ts:88-98`；`sessiond.ts:309-316`）。

**错误**：`502 {"error":"Session daemon unavailable: <原因>"}`（`sessionProxyRoutes.ts:68-70`）；守护进程关闭中为 `503 {"error":"Session daemon is shutting down"}`（`sessiond.ts:100-106`）。

```bash
curl -u alice:secret "$BASE/api/sessiond/health"
curl -u alice:secret "$BASE/api/sessiond/runtime"
curl -u alice:secret "$BASE/api/status"
curl -u alice:secret "$BASE/api/notices"
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"daemonInstanceId":"d-3f9c2a","noticeId":"n1"}' "$BASE/api/notices/dismiss"
```

> `/api/status`、`/api/notices`、`/api/notices/dismiss` 在 `web` 层是 `app.all`（`sessionProxyRoutes.ts:47-49`），因此任何方法都会被转发；而守护进程只注册了对应的 GET/POST（`src/server/status/machineStatusRoutes.ts:13`；`src/server/notices/serverNoticeRoutes.ts:11-14`），用错方法会得到守护进程的 `404`。

### WebSocket 通道

守护进程只定义了 **3 个** WebSocket 通道（单会话、全局 `/sessions/events`、全局 `/events`）；由于 `/api` 与 `/api/machines/local` 两个前缀各注册一次，对外共有 6 个 URL（任务中提到的 `/api/machines/local/sessions/:id/events` 就是单会话通道的别名之一）：

| 通道 | 路径（`/api` 形式） | 守护进程上游 | 源码 |
|---|---|---|---|
| 单会话事件 | `GET /api/sessions/:sessionId/events` | `/sessions/:sessionId/events` | `sessionProxyRoutes.ts:35-37`；`sessionRoutes.ts:499-503` |
| 全局事件 | `GET /api/sessions/events` | `/sessions/events` | `sessionProxyRoutes.ts:39-41`；`sessionRoutes.ts:505-507` |
| 全局事件（同义） | `GET /api/events` | `/events` | `sessionProxyRoutes.ts:43-45`；`sessionRoutes.ts:509-511` |
| 别名 | `/api/machines/local/...` 下的以上三者 | 同上 | `app.ts:252` |

#### 连接与握手

- `web` 用 `@fastify/websocket` 接受升级请求（`app.ts:182`），随后 `bridgeSockets` 在浏览器 socket 与 `SessionDaemonClient.connectWebSocket(...)` 建立的上游 socket 之间**双向原样转发帧**：`message` → 对端 `send`，任一端 `close`/`error` → 关闭对端（`sessionProxyRoutes.ts:72-85`）。因此载荷格式完全由守护进程决定。
- 守护进程端同样是 `@fastify/websocket`（`sessiond.ts:97`），并在 `onRequest` 钩子里对「正在关闭」的服务器直接回 `503`（`sessiond.ts:100-106`）。
- 浏览器侧：单会话通道会带 `?cwd=<工作目录>`，全局通道不带查询串（`src/client/src/api/sockets.ts:4-15`）。
- **`cwd` 对单会话通道只具有占位意义**：守护进程只取 `:sessionId` 订阅事件，`cwd` 被有意忽略，以免非法值在 WebSocket 处理器里抛错（`sessionRoutes.ts:499-503`）。因此连接本身不做 cwd 校验。
- 上游连接通过 `PI_WEB_SESSIOND_URL`（HTTP/WS）或 Unix socket（`ws+unix:`）建立（`sessionDaemonClient.ts:47-54`）。

#### 消息形态

**单会话通道**：每条帧是 `SessionUiEvent`，即 `SessionUiEventBody & { seq?: number }`（`apiTypes.ts:1327`）。`seq` 由事件中心在序列化时按会话单调递增（`sessionEventHub.ts:48-53`）。事件类型全集（`apiTypes.ts:1329-1353`）：

| `type` | 关键字段 |
|---|---|
| `message.append` | `message` |
| `assistant.delta` | `text` |
| `assistant.thinking.delta` | `text` |
| `tool.start` | `toolName`、`toolCallId`、`summary`、`args?` |
| `tool.update` | `toolName`、`toolCallId`、`text`、`content?`、`details?` |
| `tool.end` | `toolName`、`toolCallId`、`text`、`isError`、`content?`、`details?` |
| `shell.start` | `command`、`excludeFromContext?` |
| `shell.chunk` | `chunk` |
| `shell.end` | `output?`、`exitCode?`、`cancelled?`、`truncated?`、`fullOutputPath?`、`isError?` |
| `agent.start` / `agent.end` | — |
| `message.end` | `message?` |
| `status.update` | `status`（完整 `SessionStatus`） |
| `activity.update` | `activity`（`SessionActivity`） |
| `command.output` | `level`（`"info" \| "success" \| "error"`）、`message` |
| `notifications.inbox` | `daemonInstanceId`、`catalogRevision`、`summary`、`dismissThrough`、`delta` |
| `session.error` | `message` |
| `ask.opened` | `ask`（`PendingAskUser`） |
| `ask.closed` | `askId`、`reason` |
| `dialog.opened` | `dialog`（`PendingExtensionDialog`） |
| `dialog.closed` | `dialogId`、`reason`、`answer?` |
| `session.name` | `sessionId`、`name?` |
| `session.created` | `session`（`SessionInfo`） |
| `pi.event` | `eventType` |

示例帧（`seq` 为事件中心追加）：

```json
{ "type": "assistant.delta", "text": "Looking ", "seq": 138 }
```

**全局通道**（`/api/sessions/events` 与 `/api/events`）：载荷为 `RealtimeEvent = GlobalSessionEvent | MachineStatusUiEvent`（`apiTypes.ts:1368`），即下列子集（`apiTypes.ts:1361-1368`）：`status.update`、`activity.update`、`session.name`、`session.created`、`notifications.summary`、`sessions.unread`、`session.startup`、`models.changed`、`notices.updated`，以及 `machine.status`。

```json
{ "type": "sessions.unread", "catalogId": "b0a1c2d3", "catalogRevision": 43, "sessionId": "9f1c0b7a", "cwd": "/data/work/demo", "unread": { "sessionId": "9f1c0b7a", "cwd": "/data/work/demo", "completionOrder": 43, "completedAt": "2025-10-10T11:42:00.000Z" } }
```

```json
{
  "type": "machine.status",
  "status": {
    "epochId": "e-20251010T110000",
    "revision": 12,
    "machine": { "core:working": true, "core:unread": true },
    "projects": { "p1": { "core:working": true } },
    "workspaces": { "w1": { "core:working": true, "core:terminal": true } },
    "unattributed": {},
    "generatedAt": "2025-10-10T11:42:01.000Z"
  }
}
```

`MachineStatusSnapshot` 的完整字段为 `epochId`、`revision`、`machine`、`projects`、`workspaces`、`unattributed`、`generatedAt`；各状态节点是 `Record<string, boolean>`，只发已置位的 flag（`src/shared/machineStatus.ts:24,30-44`）。

**加入帧（join frame）**：全局通道在订阅建立的瞬间会先收到一帧当前 `machine.status` 快照（`sessionEventHub.ts:30-46`；在守护进程启动时通过 `eventHub.setGlobalJoinFrame(...)` 注入，`sessiond.ts:256-258`）。单会话通道**没有**加入帧，客户端需要自行拉取 `/stream-snapshot`（拿到 `seq` 水位与 `partial`）和 `/status`（`apiTypes.ts:1301-1307`；`sessionEventHub.ts:55-63`）。

#### mf-pi 网关的 WebSocket 转发

mf-pi 的 nginx 对用户路径显式转发升级头（`demos/mf-pi/nginx.conf:124-139`）：

```nginx
location ~ ^/(?<username>[a-z0-9-]+)/(?<rest>.*)$ {
    auth_request /_mfpi_gate;
    ...
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    ...
}
```

即 `Upgrade` / `Connection` 会原样传给 atenet-router 后端（`nginx.conf:132-133`），WebSocket 升级不会被 nginx 中断。要点：

- 路径前缀 `/<username>` 由 nginx 用 `proxy_pass http://127.0.0.1:58680/$rest$is_args$args;` 剥掉，后端看到的就是 `/api/sessions/...`（`nginx.conf:129`）。
- 用户路径的鉴权/就绪检查是 `auth_request /_mfpi_gate`（`nginx.conf:125`），该子请求把 `Authorization` 与 `mfpi_user` cookie 转给 admin（`nginx.conf:81-89`）。
- 若 actor 未 RUNNING 或上游 502/503/504，nginx 会把响应改写到 `/_mfpi_loading`；对非 `text/html` 的 Accept（API/WebSocket 客户端）该位置直接返回 `503`（`nginx.conf:128,142-149`）。
- `client_max_body_size 512m`（`nginx.conf:54`），所以附件/上传不会被 nginx 的 1m 默认值挡掉。

从浏览器连接（`wss`/`ws` 会自动跟随页面协议与 base，`resolveAppWebSocketUrl`）：

```bash
# 仅演示握手；浏览器 WebSocket API 无法携带 Authorization，因此走 mfpi_user cookie
curl -i -u alice:secret -N \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  'http://localhost:58681/alice/api/sessions/0f2ab9c1/events?cwd=%2Fdata%2Fwork%2Fdemo'
```

<!-- TODO: unverified: 是否需要 Sec-WebSocket-Protocol 等额外头未在源码中体现，实际握手头由浏览器/ws 客户端生成 -->

### 认证（provider / API key / OAuth）

认证路由全部定义在 `src/server/sessions/authRoutes.ts`，在守护进程上以空前缀注册（`sessiond.ts:354`），因此外部路径就是 `/api/auth/...`（以及 `/api/machines/local/auth/...`）。**认证是守护进程级（即整机 agent profile 级）的**，不按 `cwd`/会话区分：路由不解析 `cwd`。

`AuthService` 的模型运行时以 `<agentDir>/auth.json` + `models.json` 为凭据/模型文件（`authService.ts:45-51`）。`GET /auth/providers` 与两个启动登录流的路径会先 `runtime.refresh({ allowNetwork: false })`（`authService.ts:85,148,160`），而 `POST /auth/logout` 直接调用 `runtime.logout(providerId)`、不刷新（`authService.ts:90-92`）。因此**认证请求路径不会因 provider 目录网络抓取而阻塞**。

#### `GET /api/auth/providers`

列出 provider（`authRoutes.ts:5-11`；实现 `authService.ts:84-88`）。

**查询参数**

| 名称 | 类型 | 必填 | 默认 | 允许值/说明 |
|---|---|---|---|---|
| `mode` | string | 否 | `"login"` | `"login"` 或 `"logout"`（`authRoutes.ts:5,7`） |
| `authType` | string | 否 | 不过滤 | `"oauth"` 或 `"api_key"`；只在 `mode=login` 时用于过滤（`authRoutes.ts:5,7`；`authProviderOptions.ts:78-80`） |

- `mode=login`：候选集合由两轮扫描组成——先加入所有支持 OAuth 的 provider，再加入所有支持交互式 API-key（`auth.apiKey.login` 存在）的 provider；最后统一按 `name` → `authType` → `id` 排序（`authProviderOptions.ts:29-55,78-80`）。每个 provider 的 `status` 会被「truthful」化——运行时声称已配置但实际无法解析全部凭据字段时降级为 `{"configured": false}`（`authProviderOptions.ts:71-76`）。API-key provider 会带 `loginFlow: "interactive"`（`authProviderOptions.ts:50`）。
- `mode=logout`：列出 `runtime.listCredentials()` 中**确实存有凭据**的 provider（`authProviderOptions.ts:57-69`）。
- 结果按 `name` → `authType` → `id` 排序（`authProviderOptions.ts:80`）。

**成功响应** `200`：`AuthProvidersResponse`（`apiTypes.ts:1067-1069`）

```json
{
  "providers": [
    { "id": "deepseek", "name": "DeepSeek", "authType": "api_key", "status": { "configured": true, "source": "stored" }, "loginFlow": "interactive" }
  ]
}
```

`AuthProviderStatus` 的 `label` 是可选字段，由 SDK 运行时提供，其取值不在本仓库内定义（`apiTypes.ts:1052-1056`）。`source` 取值：`"stored" | "runtime" | "environment" | "fallback" | "models_json_key" | "models_json_command"`（`apiTypes.ts:1050`）。

**错误**：任何错误 → `404 {"error":"<message>"}`（`authRoutes.ts:8-10`）。

```bash
curl -u alice:secret "$BASE/api/auth/providers?mode=login&authType=api_key"
curl -u alice:secret "$BASE/api/auth/providers?mode=logout"
```

#### `POST /api/auth/api-key/interactive`

启动**交互式 API-key 登录流**（`authRoutes.ts:13-19`；`authService.ts:96-105`）。

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `providerId` | string | 是 | 必须是「支持 `apiKey.login` 的 provider」；否则 `400`。已知 provider 但不支持交互式设置 → `"<Name> does not support interactive API-key setup"`；完全未知 → `"API key provider not found: <id>"`（`authService.ts:147-157`） |

**成功响应** `200`：`OAuthFlowState`（`apiTypes.ts:1071-1092`），初始 `status: "running"`（`oauthLoginFlowService.ts:77-85`）

```json
{
  "flowId": "c2f0e6d1-3a4b-4c5d-8e9f-0a1b2c3d4e5f",
  "providerId": "deepseek",
  "providerName": "DeepSeek",
  "status": "running",
  "progress": []
}
```

**错误**：任何错误 → `400 {"error":"<message>"}`（`authRoutes.ts:16-18`）。body 缺失时读取 `request.body.providerId` 抛出的错误也在同一个 `try` 内，因此同样走 `400`（错误文本由运行时决定）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"providerId":"deepseek"}' \
  "$BASE/api/auth/api-key/interactive"
```

#### `POST /api/auth/logout`

删除某 provider 的已存凭据（`authRoutes.ts:21-27`；`authService.ts:90-94`）。

请求体：`{ "providerId": string }`（必填）。成功响应：

```json
{ "accepted": true }
```

失败（如 `runtime.logout` 抛错）→ `400 {"error":"<message>"}`（`authRoutes.ts:24-26`）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"providerId":"deepseek"}' \
  "$BASE/api/auth/logout"
```

#### `POST /api/auth/oauth`

启动 OAuth 登录流（`authRoutes.ts:29-35`；`authService.ts:107-116`）。

请求体：`{ "providerId": string }`（必填）；必须是支持 OAuth 的 provider，否则 `400 {"error":"OAuth provider not found: <id>"}`（`authService.ts:159-164`）。成功响应：`OAuthFlowState`，`status: "running"`。错误 → `400`（`authRoutes.ts:32-34`）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"providerId":"anthropic"}' \
  "$BASE/api/auth/oauth"
```

#### `GET /api/auth/oauth/:flowId`（轮询）

读取登录流状态（`authRoutes.ts:37-43`；`authService.ts:118-120`；`oauthLoginFlowService.ts:111-115`）。

**路径参数**

| 名称 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `flowId` | string | 是 | 启动响应返回的 `flowId`（`crypto.randomUUID()`，`oauthLoginFlowService.ts:72`） |

**成功响应** `200`：`OAuthFlowState`。完整字段（`apiTypes.ts:1071-1092`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `flowId` | string | 流的 UUID |
| `providerId` / `providerName` | string | provider 身份 |
| `status` | `"running" \| "complete" \| "error" \| "cancelled"` | 流状态 |
| `auth` | object，可选 | `{ url, instructions?, deviceCode?: { userCode, intervalSeconds?, expiresInSeconds? } }`，来自 pi-ai 的 `auth_url` / `device_code` 事件（`oauthLoginFlowService.ts:158-176`） |
| `prompt` | object，可选 | `{ requestId, message, placeholder?, allowEmpty?, promptType: "text" \| "secret" \| "manual_code" }`，等待用户输入时出现（`oauthLoginFlowService.ts:201-228`；`apiTypes.ts:1081-1087`） |
| `select` | object，可选 | `{ requestId, message, options: CommandOption[] }`，等待用户选择时出现（`oauthLoginFlowService.ts:230-254`） |
| `progress` | string[] | 进度消息累积；完成时会再追加一条 `"Login complete"`（`oauthLoginFlowService.ts:299`） |
| `info` | array，可选 | `{ message, links?: [{ url, label? }] }[]`（`oauthLoginFlowService.ts:177-194`） |
| `error` | string，可选 | `status` 为 `error` 时的原因（如 `"Login cancelled"`、`"Login flow expired"`，`oauthLoginFlowService.ts:104,137,358`） |

`prompt.requestId` 是提交时用的凭据；`prompt` / `select` 在提交后会从状态里清除（`oauthLoginFlowService.ts:126,375-380`）。

**成功示例（等待 OAuth 授权）**

```json
{
  "flowId": "e5b1c2d3-...",
  "providerId": "anthropic",
  "providerName": "Anthropic",
  "status": "running",
  "auth": { "url": "https://claude.ai/oauth/authorize?code=...", "instructions": "Open the URL and paste the code" },
  "progress": [],
  "info": [{ "message": "Waiting for authorization", "links": [{ "url": "https://claude.ai/oauth/authorize?code=...", "label": "Open" }] }]
}
```

**成功示例（交互式 API-key 等待输入）**

```json
{
  "flowId": "c2f0e6d1-...",
  "providerId": "deepseek",
  "providerName": "DeepSeek",
  "status": "running",
  "prompt": { "requestId": "9a7c...", "message": "Enter your DeepSeek API key", "promptType": "secret" },
  "progress": []
}
```

**成功示例（完成）**

```json
{
  "flowId": "c2f0e6d1-...",
  "providerId": "deepseek",
  "providerName": "DeepSeek",
  "status": "complete",
  "progress": ["Login complete"]
}
```

**流的生命周期**：`running` 流最多存活 30 分钟（`DEFAULT_RUNNING_TTL_MS`，超时后 `status: "error"`、`error: "Login flow expired"`）；终态记录保留 5 分钟（`DEFAULT_TERMINAL_TTL_MS`）后被驱逐（`oauthLoginFlowService.ts:41-42,328-360`）。

**错误**：`flowId` 未知（含已驱逐）→ `404 {"error":"Login flow not found"}`（`authRoutes.ts:40-42`；`oauthLoginFlowService.ts:111-115`）。

```bash
curl -u alice:secret "$BASE/api/auth/oauth/c2f0e6d1-3a4b-4c5d-8e9f-0a1b2c3d4e5f"
```

#### `POST /api/auth/oauth/:flowId/respond`

回应登录流的 `prompt` 或 `select`（`authRoutes.ts:45-51`；`oauthLoginFlowService.ts:117-129`）。

**路径参数**：`flowId`（string，必填）。

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `requestId` | string | 是 | 必须等于当前 `prompt.requestId` / `select.requestId`；不等或没有待处理项 → `400 {"error":"Login request expired"}`（`oauthLoginFlowService.ts:121-122`） |
| `value` | string | 是 | 文本/密钥/prompt 的答案，或 `select` 的选项 `value`。`allowEmpty` 为 false（选择类/密钥类）时空白串 → `400 {"error":"A value is required"}`；不在允许集合内 → `400 {"error":"Invalid login selection"}`（`oauthLoginFlowService.ts:123-124`） |

**行为**：提交会解析 pending 请求并清空 `prompt`/`select`，返回的流通常仍是 `running`；后台的 `runtime.login(...)` 完成后再把状态置为 `complete`（`oauthLoginFlowService.ts:98-106,286-300`）。若在提交前流已进入终态，则原样返回该终态（`oauthLoginFlowService.ts:120`）。

**成功响应** `200`：`OAuthFlowState`（结构同上）。

**错误**：未知 `flowId`、无 pending 请求、`requestId` 不匹配、值非法 → `400 {"error":"<message>"}`（`authRoutes.ts:48-50`）。

```bash
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"requestId":"9a7c-...","value":"sk-xxxxxxxx"}' \
  "$BASE/api/auth/oauth/c2f0e6d1-3a4b-4c5d-8e9f-0a1b2c3d4e5f/respond"
```

#### `POST /api/auth/oauth/:flowId/cancel`

取消登录流（`authRoutes.ts:53-59`；`oauthLoginFlowService.ts:131-141`）。请求体为 `{}`/可省略。成功响应：`OAuthFlowState`，`status: "cancelled"`、`error: "Login cancelled"`（`oauthLoginFlowService.ts:137`）；已经处于终态时原样返回终态。未知 `flowId` → `400 {"error":"Login flow not found"}`（注意：取消路由把 `"Login flow not found"` 映射为 **400**，而查询路由映射为 404；`authRoutes.ts:40-42` vs `53-58`）。

```bash
curl -u alice:secret -X POST "$BASE/api/auth/oauth/c2f0e6d1-3a4b-4c5d-8e9f-0a1b2c3d4e5f/cancel"
```

#### 完整交互式 API-key 流程（mf-pi admin 实际使用的序列）

pi-web 把「OAuth 流」和「交互式 API-key 流」放在同一套 `AuthInteraction` 传输上：`OAuthLoginFlowService` 的类名与 wire 名是历史遗留，`authType` 决定 `runtime.login(providerId, authType, interaction)`（`oauthLoginFlowService.ts:45-49,98`）。因此 API-key 流程**也**使用 `/auth/oauth/:flowId*` 端点。mf-pi 的多用户 admin 就是按这个序列把每个用户的 DeepSeek key 写进各自 actor 的 `auth.json`。

下述步骤与行号均来自 `demos/mf-pi/admin/apikey.go`：

1. **探测 web 就绪**：轮询 `GET /api/machines/local/auth/providers?mode=login&authType=api_key` 直到返回 200（`apikey.go:313-329,384-392`）。
2. **启动流程**：`POST /api/machines/local/auth/api-key/interactive`，body `{"providerId":"deepseek"}`，解析响应为含 `flowId`/`status`/`prompt`/`error` 的结构；缺 `flowId` 视为失败（`apikey.go:203-209,457-463`）。
3. **拿 `requestId`**：若启动响应里已带 `prompt`，直接用 `prompt.requestId`；否则每秒轮询 `GET /api/machines/local/auth/oauth/<flowId>`，直到出现 `prompt`（最多 2 分钟），期间 `status` 变成 `error`/`cancelled` 则中止（`apikey.go:211-218,331-356`）。
4. **提交 key**：`POST /api/machines/local/auth/oauth/<flowId>/respond`，body `{"requestId":"<prompt.requestId>","value":"<apiKey>"}`。注释明确指出：`respond` 只解析 prompt，返回的流通常仍是 `running`（`apikey.go:254-265`）。
5. **等完成**：若 respond 已返回 `complete` 则直接继续；`error`/`cancelled` 直接失败；否则每秒轮询 `GET /api/machines/local/auth/oauth/<flowId>`，直到 `status == "complete"`（最多 2 分钟）（`apikey.go:266-308`）。
6. **验证落盘**：`GET /api/machines/local/auth/providers?mode=login&authType=api_key`，确认目标 provider 的 `status.source == "stored"`（`apikey.go:277-280,358-382,384-392`）。
7. **清除 key**（登出）：`POST /api/machines/local/auth/logout`，body `{"providerId":"deepseek"}`，要求响应 `{"accepted":true}`，随后再次列表确认 `status.source != "stored"`（`apikey.go:238-252`）。

admin 侧解码的字段子集（与 `OAuthFlowState` 一致）：`flowId`、`status`、`prompt.requestId`、`error`（`apikey.go:452-463`），以及 provider 的 `id`/`name`/`status.configured`/`status.source`（`apikey.go:440-450`）。`promptType` 由 pi-ai 的 prompt 类型决定（API-key 场景通常是 `"secret"`）；`allowEmpty` 只在 `promptType === "text"` 时出现，因此 secret 类 prompt 提交空值会得到 `400 "A value is required"`（`oauthLoginFlowService.ts:208-226`）。

```bash
# 1) 探测（同时拿到 provider 列表）
curl -u alice:secret "$BASE/api/auth/providers?mode=login&authType=api_key"

# 2) 启动
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"providerId":"deepseek"}' "$BASE/api/auth/api-key/interactive"
# => {"flowId":"c2f0...","status":"running","progress":[]}

# 3) 轮询拿 requestId
curl -u alice:secret "$BASE/api/auth/oauth/c2f0..."
# => {"flowId":"c2f0...","status":"running","prompt":{"requestId":"9a7c...","message":"...","promptType":"secret"},"progress":[]}

# 4) 提交 key
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"requestId":"9a7c...","value":"sk-xxxxxxxx"}' "$BASE/api/auth/oauth/c2f0.../respond"

# 5) 轮询到 complete
curl -u alice:secret "$BASE/api/auth/oauth/c2f0..."   # => status":"complete"

# 6) 验证
curl -u alice:secret "$BASE/api/auth/providers?mode=login&authType=api_key"
# => deepseek.status.source == "stored"

# 7) 清除
curl -u alice:secret -X POST -H 'content-type: application/json' \
  -d '{"providerId":"deepseek"}' "$BASE/api/auth/logout"
# => {"accepted":true}
```

---

## 6. 用户 Agent API：项目、工作区与文件

本部分覆盖 pi-web agent server 的 **项目（projects）/ 工作区（workspaces）/ 工作区文件浏览器（workspace explorer）/ 项目信任（project trust）/ 工作区删除** 全部 HTTP 路由，以及 sessiond 侧对应的内部工作区目录协议。

来源（read-only checkout：`/home/liuchong/git/cliu-pi-web`）：

- `src/server/app.ts` — `registerLocalProjectRoutes`（app.ts:67）、前缀装配（app.ts:248-264）
- `src/server/workspaceExplorerRoutes.ts`
- `src/server/projects/projectService.ts`、`src/server/projects/directorySuggestions.ts`
- `src/server/projectTrustRoutes.ts`
- `src/server/workspaces/workspaceDeletionRoutes.ts`、`workspaceRouteErrors.ts`、`workspaceCatalog.ts`、`fileContentService.ts`、`fileTreeService.ts`、`filePreviewService.ts`、`fileSuggestions.ts`、`pathSafety.ts`、`pathAccessPolicy.ts`、`effectivePathAccess.ts`、`projectPiWebConfig.ts`
- `src/server/sessiond/workspaceCatalogRoutes.ts`、`src/server/sessiond/workspaceRemovalRoutes.ts`、`src/server/sessiond.ts`
- `src/server/types.ts`、`src/shared/apiTypes.ts`、`src/shared/pluginApiTypes.ts`、`src/shared/workspaceFiles.ts`、`src/shared/workspaceRemovalProtocol.ts`、`src/shared/federatedRoutes.ts`
- 行为细节参照测试：`src/server/app.projects.test.ts`、`src/server/app.workspaceFiles.test.ts`、`src/server/projectTrustRoutes.test.ts`、`src/server/workspaces/workspaceDeletionRoutes.test.ts`、`src/server/sessiond/workspaceCatalogRoutes.test.ts`

> 说明：本文的 JSON 示例由 handler 代码与类型/测试推导，未通过真实启动的服务器验证；不确定的字段按规则标注 `<!-- TODO: unverified -->`。

### 1. 路径前缀与「本地镜像」规则

`buildApp` 对同一批路由函数调用两次，分别挂到两个前缀：

```ts
registerLocalProjectRoutes(app, projects, workspaces, "/api", { config: configService });
registerLocalProjectRoutes(app, projects, workspaces, "/api/machines/local", { config: configService });
registerWorkspaceExplorerRoutes(app, projects, workspaces, "/api", { config: configService });
registerWorkspaceExplorerRoutes(app, projects, workspaces, "/api/machines/local", { config: configService });
registerProjectTrustRoutes(app, projects, workspaces, projectTrustDeps);
registerProjectTrustRoutes(app, projects, workspaces, projectTrustDeps, "/api/machines/local");
registerWorkspaceDeletionRoutes(app, sessionDaemon);
registerWorkspaceDeletionRoutes(app, sessionDaemon, "/api/machines/local");
```

结论（app.ts:248-264）：

- **每一条 `/api/...` 路由都会在同一进程内、以完全相同的行为再服务一份 `/api/machines/local/...`**。例如 `/api/projects` 与 `/api/machines/local/projects` 是两个独立注册但等价的端点（删除操作也是两份独立 handler，而非代理）。
- `/api/machines/local/...` 只是本地别名；`/api/machines/:machineId/...`（`machineId !== "local"`）由 `registerMachineProxyRoutes` 转发到远端机器（machineProxyRoutes.ts:56），其可代理的路由白名单是 `FEDERATED_HTTP_ROUTES`（shared/federatedRoutes.ts:41-181）。本部分涉及的路由**全部**在该白名单中，因此远端机器同样可用 `/api/machines/<machineId>/projects/...` 访问。
- sessiond 的 **工作区目录协议**（`/workspace-catalog/...`）与 **工作区删除协议**（`/workspace-removals/...`）**不在 `/api` 下**，也不做 `/api/machines/local` 镜像；它们是 web ↔ sessiond 的内部协议（默认前缀见 workspaceCatalogRoutes.ts:24、workspaceRemovalRoutes.ts:39；web 侧客户端常量见 sessionDaemonWorkspaceCatalog.ts:33）。浏览器只经 `/api/projects/:projectId/workspaces` 与 `/api/projects/:projectId/workspaces/:workspaceId`（DELETE）间接使用它们。

### 2. 端点清单

「浏览器面」列出的路径均同时存在 `/api/...` 与 `/api/machines/local/...` 两个等价版本（下表只写 `/api` 形式）。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/api/projects` | 列出已登记项目（app.ts:68） |
| POST | `/api/projects` | 新增/打开项目（app.ts:70） |
| DELETE | `/api/projects/:projectId` | 关闭（移除登记）项目，返回 `{closed:true}`（app.ts:78） |
| GET | `/api/project-directories` | 目录候选（新增项目对话框的路径补全）（app.ts:87） |
| GET | `/api/projects/:projectId/workspaces` | 解析项目的工作区所有者并附 `effectiveConfig`（app.ts:95） |
| GET | `/api/projects/:projectId/workspaces/:workspaceId/tree` | 列出工作区某目录的一层条目（workspaceExplorerRoutes.ts:23） |
| GET | `/api/projects/:projectId/workspaces/:workspaceId/file` | 读取工作区文件内容（JSON，≤512 KiB 前缀）（workspaceExplorerRoutes.ts:32） |
| PUT | `/api/projects/:projectId/workspaces/:workspaceId/file` | 写入/覆盖工作区文件（原始字节 body）（workspaceExplorerRoutes.ts:41） |
| DELETE | `/api/projects/:projectId/workspaces/:workspaceId/file` | 删除工作区文件（不删目录）（workspaceExplorerRoutes.ts:54） |
| POST | `/api/projects/:projectId/workspaces/:workspaceId/file/move` | 移动/重命名工作区文件（workspaceExplorerRoutes.ts:63） |
| GET | `/api/projects/:projectId/workspaces/:workspaceId/file/preview` | 内联预览（图片/HTML/PDF）或按附件下载原始字节（workspaceExplorerRoutes.ts:75） |
| GET | `/api/projects/:projectId/workspaces/:workspaceId/files` | 文件/路径建议搜索（workspaceExplorerRoutes.ts:96） |
| GET | `/api/projects/trust` | 按原始路径查询已存的信任决定（新增项目对话框）（projectTrustRoutes.ts:63） |
| GET | `/api/projects/:projectId/workspaces/:workspaceId/trust` | 读取该工作区（服务端解析出的路径）的信任状态（projectTrustRoutes.ts:75） |
| PUT | `/api/projects/:projectId/workspaces/:workspaceId/trust` | 写入信任决定 `{trusted: boolean}`（projectTrustRoutes.ts:84） |
| DELETE | `/api/projects/:projectId/workspaces/:workspaceId` | 删除（移除）一个 provider 工作区；body `{precondition}`（workspaceDeletionRoutes.ts:16） |
| GET | `/workspace-catalog/provider-runtime` | sessiond 内部：工作区 provider 运行时快照（workspaceCatalogRoutes.ts:26） |
| GET | `/workspace-catalog/projects/:projectId/workspaces` | sessiond 内部：项目的工作区权威解析（workspaceCatalogRoutes.ts:28） |
| GET | `/workspace-catalog/projects/:projectId/workspaces/:workspaceId` | sessiond 内部：单个工作区（workspaceCatalogRoutes.ts:37） |
| DELETE | `/workspace-removals/projects/:projectId/workspaces/:workspaceId` | sessiond 内部：执行工作区移除（workspaceDeletionRoutes.ts 的上游）（workspaceRemovalRoutes.ts:41） |

### 3. 错误模型

浏览器面工作区/项目路由使用统一的错误助手：

```ts
// src/server/workspaces/workspaceRouteErrors.ts:4-12
export function sendWorkspaceRequestError(reply, error, fallbackStatus): FastifyReply {
  return reply.code(workspaceCatalogHttpStatus(error, fallbackStatus)).send({
    error: error instanceof Error ? error.message : String(error),
  });
}
```

状态映射 `workspaceCatalogHttpStatus`（workspaceCatalog.ts:71-79）：

| 错误类型 | 状态码 |
| --- | --- |
| `WorkspaceCatalogUnavailableError`（sessiond 不可达） | 503 |
| `WorkspaceCatalogProtocolError`（协议/JSON 不兼容） | 502 |
| `WorkspaceCatalogRequestError` 且 `statusCode === 503` | 503 |
| `WorkspaceCatalogRequestError` 且 `statusCode >= 500` | 502 |
| 其他 | 调用方传入的 `fallbackStatus` |

各路由的 `fallbackStatus`：

| 路由 | fallback | 其他显式状态 |
| --- | --- | --- |
| `GET /api/projects/:projectId/workspaces` | 404（app.ts:100） | — |
| workspace explorer 全部路由（tree/file/PUT/DELETE/move/preview/files） | 400（workspaceExplorerRoutes.ts:28,37,50,59,71,92,104） | — |
| `POST /api/projects` 失败 | 400（app.ts:74） | — |
| `DELETE /api/projects/:projectId` 失败 | 404（app.ts:83） | — |
| `GET /api/project-directories` 失败 | 400（app.ts:91） | — |
| 三个 trust 路由 | 400（projectTrustRoutes.ts:66,71,80,86,97） | — |
| `DELETE .../workspaces/:workspaceId` | 400（body 非法）、502（sessiond 不可达 / 返回非 JSON）（workspaceDeletionRoutes.ts:24,37,59） | 上游状态码原样透传 |

**错误响应体始终是 `{"error":"<message>"}`**（workspaceRouteErrors.ts:9-11、app.ts:74、projectTrustRoutes.ts:66、workspaceDeletionRoutes.ts:24 等）。`GET /api/projects` 没有 try/catch（app.ts:68），存储读失败会走 Fastify 默认 500。

### 4. 端点详细说明

以下 `curl` 示例统一针对 **mf-pi 多用户网关**：

- 入口：`http://<host>:58681/<username>`，每个用户的流量按 URL 路径前缀路由到对应 actor（`<substrate>/demos/mf-pi/nginx.conf`；端口 58681/59681 见 `<substrate>/demos/mf-pi/docs/mfpi.md`）。
- 鉴权：HTTP Basic，`-u <username>:<password>`（nginx `auth_request` 校验每用户密码）。
- 网关会剥离 `/<username>` 前缀，因此后端看到的仍是 `/api/...`。
- 非网关（直连 pi-web）部署把 `http://<host>:58681/<username>` 换成实际监听地址即可（Basic 认证可省略）。

#### 4.1 `GET /api/projects` — 列出项目

无参数、无 body。

成功响应 `200`，`Project[]`（`Project` 见 apiTypes.ts:355-360）：

```json
[
  {
    "id": "0f4b0a0e-6f3e-4b2a-9a2f-1f0a2c9d3e77",
    "name": "my-app",
    "path": "/home/alice/src/my-app",
    "createdAt": "2026-07-27T10:00:00.000Z"
  }
]
```

- `id`：`randomUUID()`（projectStore.ts:62）；`createdAt`：ISO 时间（projectStore.ts:65）。
- `path` 为 `realpath` 后的绝对路径（projectService.ts:18）。
- 存储文件位置：`<PI_WEB_DATA_DIR>/projects.json`，可用 `PI_WEB_PROJECTS_FILE` 覆盖（projectStore.ts:34-42）。同一 `path` 只登记一次（projectStore.ts:53-55）。

错误响应：`500`（Fastify 默认；无显式错误助手）。

```bash
curl -u alice:secret \
  'http://host:58681/alice/api/projects'
```

#### 4.2 `POST /api/projects` — 新增/打开项目

请求体（JSON，app.ts:70）：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | 是 | 项目根路径；`~`/相对路径会先展开（projectService.ts:16，expandUserPath 见 directorySuggestions.ts:6-10），首尾空白会被 trim（projectService.ts:16） |
| `name` | string | 否 | 项目显示名；缺省取路径最后一段（projectStore.ts:57-63） |
| `create` | boolean | 否 | 严格等于 `true` 时先 `mkdir(path, {recursive:true})` 再校验（projectService.ts:17） |

成功响应 `200`，单个 `Project`（同 4.1）：

```json
{
  "id": "0f4b0a0e-6f3e-4b2a-9a2f-1f0a2c9d3e77",
  "name": "my-app",
  "path": "/home/alice/src/my-app",
  "createdAt": "2026-07-27T10:00:00.000Z"
}
```

若 `path` 已登记，直接返回既有记录（幂等，projectStore.ts:54-55）。

错误响应：

| 状态 | 原因 |
| --- | --- |
| 400 | 路径不存在（`realpath` ENOENT，projectService.ts:18）；路径不是目录（`"Project path must be a directory"`，projectService.ts:20）；`mkdir` 失败等 |

```bash
curl -u alice:secret -X POST \
  -H 'Content-Type: application/json' \
  -d '{"name":"my-app","path":"/home/alice/src/my-app","create":true}' \
  'http://host:58681/alice/api/projects'
```

#### 4.3 `DELETE /api/projects/:projectId` — 关闭项目

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id` |

成功响应 `200`：

```json
{ "closed": true }
```

（app.ts:81；客户端解析器 `parseClosed` 见 client/src/api/parsers.ts:2039-2043。）

错误响应：`404` `{"error":"Project not found"}`（app.ts:83；projectService.ts:25）。

```bash
curl -u alice:secret -X DELETE \
  'http://host:58681/alice/api/projects/0f4b0a0e-6f3e-4b2a-9a2f-1f0a2c9d3e77'
```

#### 4.4 `GET /api/project-directories` — 目录候选

| Query 参数 | 类型 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- | --- |
| `q` | string | 否 | `""` | 路径前缀；`""` 或 `~` 时列出 home 目录（directorySuggestions.ts:15, expandUserPath） |

成功响应 `200`，`FileSuggestion[]`（apiTypes.ts:1187-1190）：

```json
[
  { "path": "/home/alice/src/", "kind": "other" },
  { "path": "/home/alice/scripts/", "kind": "other" }
]
```

- 只返回**目录**，`path` 带结尾 `/`（directorySuggestions.ts:32）；`kind` 恒为 `"other"`。
- 结果按 `path` 排序并截断到 **80 条**（directorySuggestions.ts:35）。
- 符号链接指向目录时按目录处理（directorySuggestions.ts:25-31）。

错误响应：`400`（例如父目录不存在时 `readdir` 的 ENOENT，directorySuggestions.ts:18）。

```bash
curl -u alice:secret \
  'http://host:58681/alice/api/project-directories?q=/home/alice/sr'
```

#### 4.5 `GET /api/projects/:projectId/workspaces` — 项目工作区解析

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id`；不存在时 404 `{"error":"Project not found"}` |

无 query、无 body。

成功响应 `200`，`WorkspaceProviderResolution`（apiTypes.ts:403-409）——注意 web 层在每个 workspace 上额外附加了 `effectiveConfig`（app.ts:105-118），故与 sessiond 内部协议的 `WorkspaceListing` 不同：

```json
{
  "status": "provider",
  "projectId": "0f4b0a0e-6f3e-4b2a-9a2f-1f0a2c9d3e77",
  "ownerPluginId": "atelier",
  "workspaces": [
    {
      "id": "3f2a91c4d0b8",
      "projectId": "0f4b0a0e-6f3e-4b2a-9a2f-1f0a2c9d3e77",
      "path": "/home/alice/src/my-app",
      "label": "main",
      "isMain": true,
      "provider": {
        "pluginId": "atelier",
        "capabilities": { "request": true, "remove": true },
        "metadata": { "branch": "main" }
      },
      "removal": {
        "actionLabel": "Delete worktree",
        "confirmation": "This removes the worktree and its files.",
        "precondition": "v1.9pQ…"
      },
      "effectiveConfig": {
        "uploads": { "defaultFolder": ".pi-web/uploads" },
        "attachments": { "defaultFolder": ".pi-web/attachments" }
      }
    }
  ],
  "diagnostics": []
}
```

非 git / 无 provider 的项目返回单工作区（app.projects.test.ts 断言）：

```json
{
  "status": "folder",
  "projectId": "…",
  "workspaces": [
    {
      "id": "…",
      "projectId": "…",
      "path": "/home/alice/src/plain",
      "label": "Plain",
      "isMain": true,
      "effectiveConfig": {
        "uploads": { "defaultFolder": ".pi-web/uploads" },
        "attachments": { "defaultFolder": ".pi-web/attachments" }
      }
    }
  ],
  "diagnostics": []
}
```

要点：

- `status`：`"provider"`（插件拥有，必有 `ownerPluginId`）/ `"folder"`（普通目录，必无 `ownerPluginId`）/ `"degraded"`（owner 失败，返回 folder 兜底工作区 + `diagnostics`）（apiTypes.ts:390、parsers.ts:157-165、workspaceProviderRegistry.ts:773-786）。
- `diagnostics[].code`：`"probe-failed" | "claim-conflict" | "list-failed"`；`tier`：`"primary" | "fallback"`（apiTypes.ts:391-400）。
- `effectiveConfig` 由 `workspaceEffectiveConfig` 计算：`$.pi-web/config.json`（`PROJECT_PI_WEB_CONFIG_PATH`）覆盖全局配置（app.ts:120-126、projectPiWebConfig.ts:21,39-45）。默认值为 `.pi-web/uploads` / `.pi-web/attachments`（config.ts:53,59）。
- `provider` / `removal` 仅对 provider 工作区出现（workspaceProviderRegistry.ts:688-698）。`removal.precondition` 是不透明校验令牌，删除该工作区时必须回传（apiTypes.ts:367-371、workspaceProviderRegistry.ts:772-788）。

错误响应：项目不存在 → `404 {"error":"Project not found"}`；sessiond 权威不可达 → `503`（app.ts:99-101、app.projects.test.ts 有 `503` 断言）；协议/5xx → `502`。

```bash
curl -u alice:secret \
  'http://host:58681/alice/api/projects/0f4b0a0e-6f3e-4b2a-9a2f-1f0a2c9d3e77/workspaces'
```

#### 4.6 `GET /api/projects/:projectId/workspaces/:workspaceId/tree` — 目录树一层

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id` |
| `workspaceId` | string | 是 | `Workspace.id`（或别名工作区 id，见 4.5） |

| Query 参数 | 类型 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- | --- |
| `path` | string | 否 | `""`（工作区根） | 工作区相对路径，或已授权的绝对路径 / `~` 路径（pathAccessPolicy.ts:44-59） |

成功响应 `200`，`FileTreeResponse`（pluginApiTypes.ts:30-43）：

```json
{
  "path": "src",
  "entries": [
    { "name": "components", "path": "src/components", "type": "directory", "size": 4096, "modifiedAt": "2026-07-27T09:00:00.000Z" },
    { "name": "index.ts", "path": "src/index.ts", "type": "file", "size": 8123, "modifiedAt": "2026-07-27T10:00:00.000Z" },
    { "name": "current", "path": "src/current", "type": "symlink", "size": 12, "modifiedAt": "2026-07-27T08:00:00.000Z" }
  ],
  "scannedAt": "2026-07-27T10:05:00.000Z",
  "truncated": false
}
```

- 目录优先、再按名称排序（fileTreeService.ts:14-17）；最多 **1000** 条，超出时 `truncated: true`（fileTreeService.ts:6,27）。
- `entries[].path` 与请求的 `path` 形式一致：相对路径保持相对，绝对路径保持绝对（fileTreeService.ts:21,30-35）。
- `type` 由 dirent 判定：`directory` / `symlink` / `file`（fileTreeService.ts:23）。
- 绝对路径只有在项目 `.pi-web/config.json` 的 `pathAccess.allowedPaths`（与全局配置合并）允许时才可用（effectivePathAccess.ts:6-10、projectPiWebConfig.ts:34-37）。

错误响应（`sendWorkspaceRequestError`，fallback 400）：`path` 不存在、不是目录、`"Path traversal is not allowed"`、`"Absolute paths are not allowed"`、`"Path is outside allowed paths"`、`"Path escapes workspace"`（pathSafety.ts:24-31、pathAccessPolicy.ts:57,77,82、fileTreeService.ts:11）。

```bash
curl -u alice:secret \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/tree?path=src'
```

#### 4.7 `GET /api/projects/:projectId/workspaces/:workspaceId/file` — 读取文件内容（JSON）

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id` |
| `workspaceId` | string | 是 | `Workspace.id` |

| Query 参数 | 类型 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- | --- |
| `path` | string | **是** | — | 工作区相对路径，或已授权绝对路径；缺省/空串 → 400 `"path query parameter is required"`（fileContentService.ts:9） |

成功响应 `200`，`FileContentResponse`（pluginApiTypes.ts:45-58）：

```json
{
  "path": "src/index.ts",
  "language": "typescript",
  "encoding": "utf8",
  "size": 8123,
  "modifiedAt": "2026-07-27T10:00:00.000Z",
  "content": "export const answer = 42;\n",
  "truncated": false,
  "binary": false
}
```

解析行为（fileContentService.ts:8-32）：

- 只读取前 **512 KiB**（`MAX_WORKSPACE_FILE_CONTENT_BYTES`，workspaceFiles.ts:5）；`size` 是**文件真实大小**，`truncated` 表示 `size > 512 KiB`。
- `language` 仅在扩展名命中映射表时出现（ts/tsx/js/jsx/json/md/markdown/css/htm/html/py/rs/go/sh/yml/yaml，fileContentService.ts:156-178）。
- `mediaType` / `mimeType` 仅在扩展名属于白名单分类时出现（png/jpg/svg/html/pdf/md 等，workspaceFiles.ts:25-42）；`mimeType` 仅在分类带 `previewMimeType` 时出现（fileContentService.ts:180-186）。
- `binary`：分类为 `source: "stream"`（如 PNG/JPEG/PDF）或未分类且前 8 KiB 含 `0x00` 字节时为 `true`（fileContentService.ts:20,151-154）；此时 `content` 为 `""`（fileContentService.ts:28）。
- `encoding` 恒为 `"utf8"`（pluginApiTypes.ts:52）。
- `.svg`/`.htm`/`.html`/`.md` 属于文本源格式，保留字面源码（fileContentService.ts:17-19）。

错误响应（fallback 400）：`"path query parameter is required"`、`"Path is not a file"`（fileContentService.ts:12）、`"Path traversal is not allowed"`、`"Absolute paths are not allowed"`（未配置 allowedPaths 时）、`"Path is outside allowed paths"`、`"Path does not exist"`。

```bash
curl -u alice:secret \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/file?path=src%2Findex.ts'
```

#### 4.8 `PUT /api/projects/:projectId/workspaces/:workspaceId/file` — 写入文件（原始字节）

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id` |
| `workspaceId` | string | 是 | `Workspace.id` |

| Query 参数 | 类型 | 必填 | 默认 | 允许值 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `path` | string | **是** | — | 工作区相对路径 | 缺省/空串 → 400 `"path query parameter is required"`（fileContentService.ts:47） |
| `createDirs` | string | 否 | `"true"` | `"false"` 关闭，其余值均视为开启 | 仅 `=== "false"` 关闭父目录自动创建（workspaceExplorerRoutes.ts:45；fileContentService.ts:49,69） |
| `overwrite` | string | 否 | `"true"` | `"false"` 禁止覆盖，其余值均视为允许 | 仅 `=== "false"` 时若文件已存在则报错（workspaceExplorerRoutes.ts:46；fileContentService.ts:50,57） |

**请求体：原始字节，不是 JSON。**

- 允许的 Content-Type（workspaceExplorerRoutes.ts:109-115）：
  - `text/plain` → 按字符串接收后转 `Buffer`；
  - `application/octet-stream` → `Buffer`；
  - 任意匹配 `/^([a-z]+\/[a-z0-9.+-]+)$/` 的类型 → `Buffer`（例如 `image/png`）。
- **不要用 `application/json`**：Fastify 默认 JSON 解析器会把它解析成对象，而 handler 需要 `Buffer`（`Body: Buffer`，workspaceExplorerRoutes.ts:41），写文件会失败。<!-- TODO: unverified：JSON Content-Type 下客户端实际看到的错误码/消息未在测试中固定 -->
- 最大体积：Fastify 实例级 `bodyLimit`，由 `maxUploadBytes()` 决定，默认 **64 MiB**（`DEFAULT_MAX_UPLOAD_BYTES = 64 * 1024 * 1024`，config.ts:51,142-150；server/index.ts:6；app.ts:174）；可用 `PI_WEB_MAX_UPLOAD_BYTES` 或配置项 `maxUploadBytes` 覆盖。超限由 Fastify 返回 `413`。<!-- TODO: unverified：413 响应体在 pi-web 中的形状未在源码中显式定义 -->
- mf-pi 网关的 nginx `client_max_body_size 512m`（demos/mf-pi/nginx.conf），因此实际生效上限是上面这个应用级限制。
- `path` 指向已存在目录时返回 400（fileContentService.ts:56，app.workspaceFiles.test.ts 断言）。
- 路径安全：**不支持绝对路径**（write 走 `resolveInsideWorkspace` / `resolveParentInsideWorkspace`，pathSafety.ts:4-22，不允许 `..` 穿越）；父目录 symlink 会被 `realpath` 解析并再次做工作区包含校验（fileContentService.ts:71-74）。

成功响应 `200`，`WriteWorkspaceFileResponse`（pluginApiTypes.ts:65-70）：

```json
{
  "path": "deep/nested/dir/file.txt",
  "size": 12,
  "modifiedAt": "2026-07-27T10:10:00.000Z",
  "created": true
}
```

- `created`：`true` 表示新建，`false` 表示覆盖（fileContentService.ts:82）。
- `path` 为归一化后的工作区相对路径（`../`、`./` 已折叠，pathSafety.ts:24-31）。

错误响应（fallback 400）：`"path query parameter is required"`、`"File already exists: <relativePath>"`（`overwrite=false`）、`"Path is not a file"`、`"Path traversal is not allowed"`（pathSafety.ts:29）、`"Absolute paths are not allowed"`（pathSafety.ts:27）、`"Path does not exist"`（父目录不存在且 `createDirs=false`）、`"Path escapes workspace"`（pathSafety.ts:40-41）。

```bash
# 文本
curl -u alice:secret -X PUT \
  -H 'Content-Type: text/plain' \
  --data-binary 'hello world' \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/file?path=hello.txt'

# 二进制，禁止覆盖，不自动建目录
curl -u alice:secret -X PUT \
  -H 'Content-Type: application/octet-stream' \
  --data-binary @logo.png \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/file?path=assets%2Flogo.png&createDirs=false&overwrite=false'
```

#### 4.9 `DELETE /api/projects/:projectId/workspaces/:workspaceId/file` — 删除文件

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id` |
| `workspaceId` | string | 是 | `Workspace.id` |

| Query 参数 | 类型 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- | --- |
| `path` | string | **是** | — | 工作区相对路径；缺省/空串 → 400 `"path query parameter is required"`（fileContentService.ts:87） |

成功响应 `200`，`DeleteWorkspaceFileResponse`（pluginApiTypes.ts:72-75）：

```json
{ "path": "to-delete.txt", "existed": true }
```

文件不存在时**仍返回 200**，`existed: false`（fileContentService.ts:107-108）。

解析行为与限制：

- 只删**普通文件或符号链接**；目录返回 400 `"Path is a directory, use directory deletion instead"`（fileContentService.ts:103）。
- 删除 symlink 时删的是链接本身（使用 `lstat` + 不解析最后一段，fileContentService.ts:88-101）。
- **不支持绝对路径 / `..`**（走 `resolveParentInsideWorkspace`，fileContentService.ts:92、pathSafety.ts:16-22,29）。

```bash
curl -u alice:secret -X DELETE \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/file?path=to-delete.txt'
```

#### 4.10 `POST /api/projects/:projectId/workspaces/:workspaceId/file/move` — 移动/重命名

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id` |
| `workspaceId` | string | 是 | `Workspace.id` |

| Query 参数 | 类型 | 必填 | 默认 | 允许值 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `fromPath` | string | **是** | — | 工作区相对路径 | 缺省/空串 → 400 `"fromPath query parameter is required"`（fileContentService.ts:114） |
| `toPath` | string | **是** | — | 工作区相对路径 | 缺省/空串 → 400 `"toPath query parameter is required"`（fileContentService.ts:115） |
| `createDirs` | string | 否 | `"true"` | 仅 `"false"` 关闭 | 目标父目录是否 `mkdir -p`（workspaceExplorerRoutes.ts:67；fileContentService.ts:117,128） |
| `overwrite` | string | 否 | `"false"`（注意与其他端点默认相反） | 仅 `"true"` 开启 | `overwrite=true` 时才允许覆盖已存在的目标文件（workspaceExplorerRoutes.ts:68；fileContentService.ts:118,135-144） |

无请求体（`POST` 但无正文，workspaceExplorerRoutes.ts:63）。

成功响应 `200`，`MoveWorkspaceFileResponse`（pluginApiTypes.ts:82-87）：

```json
{
  "fromPath": "original.txt",
  "toPath": "moved.txt",
  "size": 7,
  "modifiedAt": "2026-07-27T10:20:00.000Z"
}
```

错误响应（fallback 400）：`"fromPath query parameter is required"` / `"toPath query parameter is required"`、`"Source path is not a file"`（fileContentService.ts:123）、`"File already exists: <toPath>"`（默认 `overwrite=false` 且目标为文件）、`"Path traversal is not allowed"`、`"Path does not exist"`（源不存在）。

```bash
curl -u alice:secret -X POST \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/file/move?fromPath=original.txt&toPath=archive%2Foriginal.txt&createDirs=true&overwrite=false'
```

#### 4.11 `GET /api/projects/:projectId/workspaces/:workspaceId/file/preview` — 内联预览 / 附件下载

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id` |
| `workspaceId` | string | 是 | `Workspace.id` |

| Query 参数 | 类型 | 必填 | 默认 | 允许值 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `path` | string | **是** | — | 工作区相对路径或已授权绝对路径 | 缺省/空串 → 400 `"path query parameter is required"`（filePreviewService.ts:31） |
| `download` | string | 否 | 不下载（内联） | `"1"` 或 `"true"` 才视为下载 | 下载模式：任意类型、`application/octet-stream` + `attachment`、**无大小上限**（workspaceExplorerRoutes.ts:78；filePreviewService.ts:48-52） |
| `v` | string | 否 | — | 任意 | 客户端缓存破坏参数，服务端忽略（urls.ts:41） |

**响应不是 JSON，而是文件字节**。响应头（workspaceExplorerRoutes.ts:81-89、filePreviewResponsePolicy.ts:23-63）：

| 头 | 内联预览 | 下载 |
| --- | --- | --- |
| `Content-Type` | 分类的 `previewMimeType`：`image/png`、`image/svg+xml`、`text/html; charset=utf-8`、`application/pdf` 等（workspaceFiles.ts:25-42） | `application/octet-stream` |
| `Content-Disposition` | `inline; filename="…"; filename*=UTF-8''…`（RFC 5987，filePreviewResponsePolicy.ts:61-64） | `attachment; …` |
| `Content-Security-Policy` | 按 image / html / pdf 分别给出沙箱策略（filePreviewResponsePolicy.ts:10-19） | 最严格的 `sandbox; default-src 'none'; …; frame-ancestors 'none'`（filePreviewResponsePolicy.ts:20） |
| `Cache-Control` | `private, max-age=3600`（workspaceExplorerRoutes.ts:83） | 同左 |
| `Content-Length` | 实际发送字节数（快照，workspaceExplorerRoutes.ts:84；filePreviewService.ts:57-58） | 校验时的文件大小（filePreviewService.ts:51） |
| `Last-Modified` | `mtime` 的 UTC 字符串（workspaceExplorerRoutes.ts:87） | 同左 |
| `X-Content-Type-Options` | `nosniff` | `nosniff` |

行为：

- 内联预览只对**分类白名单**扩展名可用：`.png .jpg .jpeg .gif .webp .avif .bmp .ico .svg .htm .html .pdf`（workspaceFiles.ts:25-42，另有 `.md/.markdown` 分类为 markdown 但**无** `previewMimeType`，因此内联预览会被拒绝——filePreviewService.ts:55）。
- 内联大小上限 **10 MiB**（`MAX_INLINE_PREVIEW_BYTES`，workspaceFiles.ts:3；filePreviewService.ts:56），超限报 `"File is too large to preview (limit 10 MB)"`。
- 下载模式**不限制体积**（流式发送，但仍以校验时的 size 为界，filePreviewService.ts:86-101）。
- 响应字节来自打开的文件描述符快照：校验后的重命名/替换/扩容不会改变本次响应内容（filePreviewService.ts:34-38）。

错误响应（fallback 400，额外套用错误响应的安全头 `application/json; charset=utf-8` + `nosniff`，workspaceExplorerRoutes.ts:91-92、filePreviewResponseHeaders.ts:18-20）：

| 状态 | 消息示例 |
| --- | --- |
| 400 | `"path query parameter is required"`、`"Path is not a file"`、`"Inline preview is not supported for this file type"`、`"File is too large to preview (limit 10 MB)"`、`"Path traversal is not allowed"`、`"Path is outside allowed paths"`、`"Workspace file path must include a filename"`（空文件名，filePreviewResponsePolicy.ts:25） |

```bash
# 内联预览图片
curl -u alice:secret -o preview.png \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/file/preview?path=diagram.png'

# 作为附件下载任意文件
curl -u alice:secret -OJ \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/file/preview?path=r%C3%A9sum%C3%A9%20notes.txt&download=1'
```

#### 4.12 `GET /api/projects/:projectId/workspaces/:workspaceId/files` — 文件/路径建议

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id` |
| `workspaceId` | string | 是 | `Workspace.id` |

| Query 参数 | 类型 | 必填 | 默认 | 允许值 | 说明 |
| --- | --- | --- | --- | --- | --- |
| `q` | string | 否 | `""` | 任意 | 搜索文本；默认 `""`（workspaceExplorerRoutes.ts:99） |
| `kind` | string | 否 | 不过滤 | `"tracked"` \| `"untracked"` \| `"other"` | 按建议类型过滤（workspaceExplorerRoutes.ts:96；fileSuggestions.ts:57,66） |
| `mode` | string | 否 | `"file"` | `"file"` \| `"path"` | `mode=path` 时走目录/路径补全 `listPathSuggestions`，否则走文件搜索 `listFileSuggestions`（workspaceExplorerRoutes.ts:101-102） |
| `scope` | string | 否 | git 已跟踪 + 未跟踪 | `"tracked"` \| `"all"` | `tracked` → `git ls-files`；`all` → git 文件 + `rg --files --hidden --no-ignore`（含被忽略文件，排除 `.git`）；缺省 → `git ls-files` + `git ls-files --others --exclude-standard`，git 不可用时回退 `rg --files`（fileSuggestions.ts:245-274；workspaceExplorerRoutes.ts:96） |

成功响应 `200`，`FileSuggestion[]`（apiTypes.ts:1187-1190），最多 **80** 条（fileSuggestions.ts:14）：

```json
[
  { "path": "src/index.ts", "kind": "tracked" },
  { "path": "src/utils/", "kind": "tracked" },
  { "path": "notes/draft.md", "kind": "untracked" }
]
```

- `kind` 取值含义：`tracked` = git 已跟踪；`untracked` = git 未跟踪但未被忽略；`other` = 非 git 来源（`rg`/文件系统遍历、目录补全、路径补全）（fileSuggestions.ts:251-285,548-563）。
- 目录建议以 `/` 结尾（fileSuggestions.ts:101）。
- `q` 为绝对路径或 `~` 开头（`isAbsoluteishFileSuggestionQuery`，fileSuggestions.ts:49-51）时启用 `pathAccess.allowedPaths`；未配置时绝对路径报 400 `"Absolute paths are not allowed"`（fileSuggestions.ts:118）。
- 有 `fzf` 时用 `fzf` 排序，否则本地打分排序（fileSuggestions.ts:343-384）。<!-- TODO: unverified：fzf 排序的确切打分权重与结果顺序不保证稳定 -->

```bash
curl -u alice:secret \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/files?q=index&scope=all&kind=tracked'

curl -u alice:secret \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/files?q=src%2F&mode=path'
```

#### 4.13 `GET /api/projects/trust` — 按路径查询信任决定

| Query 参数 | 类型 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- | --- |
| `path` | string | **是** | — | 任意路径（会做 `~`/相对路径展开 + 符号链接规范化）；trim 后为空 → 400 `"path is required"`（projectTrustRoutes.ts:64-67） |

成功响应 `200`，`WorkspaceTrustResponse`（apiTypes.ts:377-384）：

```json
{ "path": "/home/alice/src/my-app", "decision": null, "trusted": false }
```

- `path` 是**规范化后**用于 key 的路径（`realpathSync`，不存在时退回展开后的路径，projectTrustRoutes.ts:50-57）。
- `decision`：`true`/`false` 为显式记录，`null` 表示未记录（apiTypes.ts:380-381）。
- `trusted`：`decision ?? (SettingsManager.getDefaultProjectTrust() === "always")`（projectTrustRoutes.ts:35-37）。

错误响应：`400`（`"path is required"`，或 store 读取失败）。

```bash
curl -u alice:secret \
  'http://host:58681/alice/api/projects/trust?path=%2Fhome%2Falice%2Fsrc%2Fmy-app'
```

#### 4.14 `GET /api/projects/:projectId/workspaces/:workspaceId/trust` — 读取工作区信任

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id` |
| `workspaceId` | string | 是 | `Workspace.id` |

成功响应 `200`，`WorkspaceTrustResponse`；`path` 是**服务端解析出的工作区根路径**（projectTrustRoutes.ts:77-78）：

```json
{ "path": "/home/alice/src/my-app", "decision": true, "trusted": true }
```

错误响应：`400`（handler 对所有失败统一 `reply.code(400)`，因此**项目不存在与工作区不存在都返回 400**，不会返回 404，projectTrustRoutes.ts:75-82）。

```bash
curl -u alice:secret \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/trust'
```

#### 4.15 `PUT /api/projects/:projectId/workspaces/:workspaceId/trust` — 写入信任决定

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | `Project.id` |
| `workspaceId` | string | 是 | `Workspace.id` |

请求体（JSON）：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `trusted` | boolean | 是 | 非 boolean（含缺失）→ 400 `"trusted must be a boolean"`（projectTrustRoutes.ts:84-87） |

成功响应 `200`，写入后立刻回读的 `WorkspaceTrustResponse`（projectTrustRoutes.ts:94-95）：

```json
{ "path": "/home/alice/src/my-app", "decision": true, "trusted": true }
```

- 写入落到 **active agent profile 的 agentDir 下的 `trust.json`**，与 Pi CLI 共享（projectTrustRoutes.ts:35,94；`ProjectTrustStore`）。`agentDir` 由 app 装配注入（app.ts:258-260）。
- 客户端**不能**指定任意路径：路径由 `projectId`/`workspaceId` 服务端解析（projectTrustRoutes.ts:20-23,89）。
- 文件锁写入失败（如只读 `trust.json` 的 EACCES）会作为 400 错误消息返回（projectTrustRoutes.ts:91-98）。

```bash
curl -u alice:secret -X PUT \
  -H 'Content-Type: application/json' \
  -d '{"trusted":true}' \
  'http://host:58681/alice/api/projects/<projectId>/workspaces/<workspaceId>/trust'
```

#### 4.16 `DELETE /api/projects/:projectId/workspaces/:workspaceId` — 移除工作区

该路由是 **sessiond `/workspace-removals/...` 的薄代理**（编码参数、透传状态与 JSON body）。远端机器场景下由 `FEDERATED_HTTP_ROUTES` 指定 **30 s** 联邦超时（federatedRoutes.ts:71-76，`WORKSPACE_REMOVAL_FEDERATION_TIMEOUT_MS`）；工作区文件类端点联邦超时为 30 s（federatedRoutes.ts:20,77-111）。

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | 会 `encodeURIComponent` 后转发（workspaceDeletionRoutes.ts:31） |
| `workspaceId` | string | 是 | 可含 `/`（测试用 `view%2Fone`，workspaceDeletionRoutes.test.ts:46,60） |

请求体（JSON，**上限 4 KiB**：`WORKSPACE_REMOVAL_REQUEST_BODY_MAX_BYTES`，workspaceRemovalProtocol.ts:4；路由级 `bodyLimit` 见 workspaceDeletionRoutes.ts:18）：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `precondition` | string | 是 | 非空且长度 ≤ 256（workspaceRemovalProtocol.ts:9,16-27）；来自 4.5 响应里的 `workspace.removal.precondition` |

成功响应 `200`，`TerminalCommandRun`（pluginApiTypes.ts:89-105；示例取自 workspaceDeletionRoutes.test.ts:14-30）：

```json
{
  "id": "run1",
  "origin": "core",
  "projectId": "project one",
  "workspaceId": "main",
  "terminalId": "terminal1",
  "title": "Disconnect board view",
  "command": "boardctl view disconnect roadmap --keep-files",
  "status": "running",
  "exitCode": 0,
  "createdAt": "2026-07-27T00:00:00.000Z",
  "startedAt": "2026-07-27T00:00:00.100Z",
  "completedAt": "2026-07-27T00:00:01.000Z",
  "metadata": {
    "pi.operation": "workspace.delete",
    "target.workspaceId": "view/one",
    "target.workspacePath": "/views/roadmap"
  }
}
```

> `status` 取值 `"queued" | "running" | "succeeded" | "failed"`（pluginApiTypes.ts:89）；`exitCode`/`startedAt`/`completedAt` 仅在相应阶段出现。示例中同时给出全部可选字段只为展示形状。

错误响应：

| 状态 | 原因 |
| --- | --- |
| 400 | body 不是对象或 `precondition` 缺失/空/超长（workspaceDeletionRoutes.ts:24、workspaceRemovalProtocol.ts:12-27） |
| 404 | 项目不存在（sessiond 返回） |
| 409 | 工作区已变化/不再属于该项目/provider 不再拥有/不支持移除（workspaceRemovalService.ts:247-265） |
| 502 | sessiond 不可达（`"Session daemon unavailable: …"`）或上游返回非 JSON（`"Invalid session daemon workspace removal response: …"`）（workspaceDeletionRoutes.ts:37-39,59-61） |
| 其他 | 上游 sessiond 状态码与 body 原样透传（workspaceDeletionRoutes.ts:47-56） |

```bash
curl -u alice:secret -X DELETE \
  -H 'Content-Type: application/json' \
  -d '{"precondition":"v1.confirmed"}' \
  'http://host:58681/alice/api/projects/project%20one/workspaces/view%2Fone'
```

#### 4.17 sessiond 内部：工作区目录协议（`/workspace-catalog`）

> 这些端点**不在 `/api` 下**，也**不做 `/api/machines/local` 镜像**；它们服务于 web 进程（`SessionDaemonWorkspaceCatalog.daemon.request`）。sessiond 默认监听 unix socket 或 `PI_WEB_SESSIOND_PORT`（sessiond.ts:395-415），外部一般不应直接调用。

**`GET /workspace-catalog/provider-runtime`**（workspaceCatalogRoutes.ts:26）

成功响应 `200`，`WorkspaceProviderRuntimeSnapshot`（workspaceCatalog.ts:22-29,35-52）:

```json
{
  "protocolVersion": 2,
  "terminalMode": "required",
  "safeStart": "bundled-only",
  "records": [
    {
      "pluginId": "atelier",
      "source": "@acme/pi-web-atelier",
      "scope": "user",
      "moduleRevision": "12",
      "settingsRevision": "3",
      "machineSpecific": false,
      "state": "started",
      "name": "Atelier",
      "phase": "ready",
      "message": null
    }
  ],
  "health": [
    { "pluginId": "atelier", "health": { "status": "ok" }, "phase": "health" }
  ],
  "diagnostics": [
    { "code": "load-failed", "source": "@acme/pi-web-atelier", "message": "…", "pluginId": "atelier" }
  ]
}
```

- `protocolVersion` 恒为 `2`（workspaceCatalog.ts:10）。
- `terminalMode`：`"required" | "recovery-disabled"`（shared/requiredTerminalPlugin.ts:3-7）。
- `records[]` 字段见 serverPluginRuntime.ts:54-70；`health[]` 见 serverPluginRuntime.ts:92-97；`diagnostics[]` 见 piWebPluginCatalog.ts:65-70。<!-- TODO: unverified：`state`/`phase`/`health` 的全部枚举取值未逐一核对，示例值仅供参考 -->

**`GET /workspace-catalog/projects/:projectId/workspaces`**（workspaceCatalogRoutes.ts:28）

| 路径参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `projectId` | string | 是 | 不存在 → 404 `{"error":"Project not found"}` |

成功响应 `200`，`WorkspaceProviderAuthorityResolution`（apiTypes.ts:427-430）：与 4.5 相同，但 **workspace 上没有 `effectiveConfig`**（该字段由 web 路由层附加，app.ts:105-118）。

错误响应（workspaceCatalogRoutes.ts:50-53）：`404`（`"Project not found"`）或 `500`（其他异常）。

**`GET /workspace-catalog/projects/:projectId/workspaces/:workspaceId`**（workspaceCatalogRoutes.ts:37）

成功 `200` 返回单个 `WorkspaceListing`；工作区已不在当前解析结果中 → `404 {"error":"Workspace not found"}`（workspaceCatalogRoutes.ts:42；sessiond/workspaceCatalogRoutes.test.ts 有断言）。

错误响应：`404`（项目不存在 / 工作区不存在）或 `500`。

```bash
# 仅在能直连 sessiond 时可行（unix socket 可用 socat/curl --unix-socket）
curl --unix-socket /run/pi-web/sessiond.sock \
  'http://localhost/workspace-catalog/projects/<projectId>/workspaces'
```

#### 4.18 sessiond 内部：工作区移除协议（`/workspace-removals`）

**`DELETE /workspace-removals/projects/:projectId/workspaces/:workspaceId`**（workspaceRemovalRoutes.ts:41）

- 请求体与 4.16 相同：`{"precondition":"…"}`，`bodyLimit` 4 KiB（workspaceRemovalRoutes.ts:43）。
- 成功 `200` 返回 `TerminalCommandRun`（同上）。
- 错误：`400`（precondition 非法）、`404`/`500`（项目不存在/其他）、其余由 `workspaceRemovalHttpStatus` 决定（默认 500，`WorkspaceRemovalError`/`WorkspaceProviderRemovalError` 用其自带 `statusCode`，workspaceRemovalService.ts:240-245）。
- 无论成败都会调用 `onWorkspacesMutated()` 使工作区投影失效（workspaceRemovalRoutes.ts:70-73）。

### 5. 对象模型：项目 → 工作区 → 文件

```
Project (projects.json)
  id            string   项目 UUID（randomUUID，projectStore.ts:62）
  name          string   显示名；缺省为路径最后一段（projectStore.ts:57-63）
  path          string   规范化绝对路径（realpath，projectService.ts:18）
  createdAt     string   ISO 8601 创建时间（projectStore.ts:65）
      │
      └── Workspace（由 sessiond 工作区权威按项目解析）
            id             string   sha1(`${projectId}:${providerKey}`) 前 12 位十六进制（workspaceProviderRegistry.ts:804-806；folder 时 providerKey = project.path，workspaceProviderRegistry.ts:795-801）
            projectId      string   所属项目 id（apiTypes.ts:414）
            path           string   工作区根绝对路径（apiTypes.ts:416）
            label          string   展示名；folder 工作区 = 项目 name（workspaceProviderRegistry.ts:797-799）
            isMain         boolean  每个解析结果恰有一个 main（workspaceProviderRegistry.ts:653-657,710-713）
            provider?      { pluginId, capabilities: { request, remove }, metadata? }（apiTypes.ts:418、pluginApiTypes.ts:11-22）
            removal?       { actionLabel, confirmation, precondition }（apiTypes.ts:419,367-371）
            effectiveConfig  { uploads?: { defaultFolder? }, attachments?: { defaultFolder? } }（apiTypes.ts:362-365,421；由 web 层附加）
      │
      └── File（工作区浏览器）
            path       string  相对工作区根（或在授权 allowedPaths 下的绝对路径）
            type       "file" | "directory" | "symlink"（FileTreeEntry，pluginApiTypes.ts:30-36）
            size       number  字节数
            modifiedAt string  ISO 8601 mtime
```

解析状态语义（`WorkspaceProviderResolution.status`，apiTypes.ts:390-409）：

| `status` | 含义 | `ownerPluginId` |
| --- | --- | --- |
| `provider` | 某个 server plugin 声明拥有该项目的工作区（含 linked worktree 等） | 必存在 |
| `folder` | 无 provider 认领，退化为「项目目录 = 唯一 main 工作区」 | 必须不存在 |
| `degraded` | 原 owner provider 解析失败，返回 folder 兜底工作区 + `diagnostics` 说明 | 可选 |

外部组件使用建议：

1. 先 `GET /api/projects` 拿 `projectId`；`POST /api/projects` 时优先带上 `create:true`（否则必须先自行创建目录）并考虑用 `GET /api/projects/trust?path=…` 预读信任状态（4.13）。
2. 再 `GET /api/projects/:projectId/workspaces` 解析工作区；**不要假设只有一个工作区**，用 `isMain` 找主工作区，用 `status`/`diagnostics` 解释降级。`effectiveConfig` 可直接用于上传/附件默认目录。
3. 文件操作使用 `workspaceId` + 工作区相对 `path`；跨根访问（`~`、绝对路径）仅在项目 `.pi-web/config.json` 配了 `pathAccess.allowedPaths` 时对 **读/树/预览/建议** 生效，**写/删/移** 一律只允许工作区内相对路径。
4. 删除工作区前先取得并回传 `workspace.removal.precondition`；没有 `removal`（或 `provider.capabilities.remove !== true`）的工作区不可删除。

---

## 7. 用户 Agent API：状态、配置、机器、插件与包管理

> 本章覆盖 pi-web Agent 面里**除「会话 / 项目 / 文件」之外**的全部 HTTP 与 WebSocket 接口：
> 状态与版本、运行时、配置、机器与机器状态、机器代理（federation）、插件与插件后端、pi-packages、
> 部署身份资产、通知（notices）、终端，以及仅供 sessiond 内部使用的路由。
>
> 本章一手来源（`file:line` 均指 pi-web 源码，即本仓库的 `cliu-pi-web` 检出）：
>
> - 装配与注册：`src/server/app.ts`、`src/server/index.ts`、`src/server/sessiond.ts`
> - 配置：`src/server/configRoutes.ts`
> - 状态/版本/运行时：`src/server/piWebStatus.ts`、`src/server/piWebStatusCache.ts`
> - 机器：`src/server/machines/machineRoutes.ts`、`machineProxyRoutes.ts`、`machinePluginProxyRoutes.ts`、`machineService.ts`、`machineClient.ts`、`machineStore.ts`
> - 机器状态：`src/server/status/machineStatusRoutes.ts`、`machineStatusService.ts`、`src/shared/machineStatus.ts`
> - 插件：`src/server/piWebPluginService.ts`、`src/server/plugins/pluginBackendProxyRoutes.ts`、`pluginBackendChannelProxyRoutes.ts`、`src/server/sessiond/pluginBackendRoutes.ts`、`pluginBackendChannelRoutes.ts`、`src/shared/pluginBackendProtocol.ts`
> - 包管理：`src/server/piPackageRoutes.ts`、`piPackageService.ts`
> - 部署身份：`src/server/deploymentIdentityRoutes.ts`、`deploymentIdentity.ts`
> - 通知：`src/server/notices/serverNoticeRoutes.ts`、`serverNoticeService.ts`、`serverNoticeStore.ts`
> - 终端：`dist/pi-web-plugins/terminal/server-plugin.js`（内置必需插件 `pi-web.terminal`）
> - 类型：`src/shared/apiTypes.ts`、`src/shared/pluginApiTypes.ts`、`src/shared/federatedRoutes.ts`
>
> **不在本章**（由「会话 / 项目 / 文件」章节覆盖）：`sessionRoutes.ts`、`authRoutes.ts`、`sessionProxyRoutes.ts`、
> `workspaceExplorerRoutes.ts`、`workspaceCatalogRoutes.ts`、`workspaceDeletionRoutes.ts`、`projectTrustRoutes.ts`
> 以及 `app.ts` 内项目 list/add/close 路由本身（但它们的**机器代理形态**会在 §4.5 一并列出，以保证清单完整）。

---

### 0. 通用约定

#### 0.1 Base URL 与鉴权

沿用总览文档的约定（`00-front.md:66-93`）：

```bash
BASE=http://<host>:58681          # 生产网关；测试环境为 http://<host>:59681
USER=alice                        # 用户名 == Actor 名 == Agent API 的 URL 段
USER_PASSWORD='<创建用户时返回的密码>'

# 用户 Agent 面的根：${BASE}/${USER}
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/pi-web/status"
```

- 鉴权只有网关的 HTTP Basic（用户名必须等于 URL 段），pi-web 自身不实现鉴权（`00-front.md:85-86`）。
- API/WS 客户端务必带 `Accept: application/json`，否则网关会把 403/502/503/504 改写成 200 + HTML 加载页（`00-front.md:211-222`）。
- 本章所有示例的路径都省略了 `${BASE}/${USER}` 前缀，只写 pi-web 内部路径。

#### 0.2 双前缀别名与 static-local 路由

`buildApp` 把同一批业务路由注册两次：`/api/...` 与 `/api/machines/local/...`（`app.ts:248-264`），
`pi-packages` 与 session 代理同样双挂（`app.ts:239-240`、`app.ts:251-257`）。两者行为完全一致。
本章按同样规则列出别名行。

另有若干**只挂在 local 前缀下**的静态路由：

| 路径 | 注册位置 | 说明 |
|---|---|---|
| `/api/machines/local/config` | `configRoutes.ts:68,76` | 选中机器的配置读写（不是全局配置） |
| `/api/machines/local/plugins` | `app.ts:238` | 等价于 `/api/plugins` |

注意：静态 `/api/machines/local/...` 路由优先于参数化 `/api/machines/:machineId/...` 路由。
因此 `/api/machines/local/config`、`/api/machines/local/plugins`、`/api/machines/local/pi-packages[...]`、
`/api/machines/local/status`、`/api/machines/local/notices` 都命中本地静态实现；
`/api/machines/local/health` 与 `/api/machines/local/runtime` 由 `machineRoutes` 的参数化处理器直接支持 `local`
（`machineService.ts:38,90,101`）。只有 `machineProxyRoutes` 注册的 federated 代理路径（例如
`/api/machines/local/pi-web/status`、`/api/machines/local/sessions/:sessionId/messages`）在 `machineId === "local"` 时
返回 **501** `{"error":"Local machine route is not registered for this endpoint"}`（`machineProxyRoutes.ts:130-132`），
`/api/machines/local/pi-web-plugins/manifest.json` 则由插件代理显式返回 **400**
（`machinePluginProxyRoutes.ts:45-47`）。本地等价调用请使用 §1.1 中不带 `:machineId` 的本地路由。

#### 0.3 请求体上限、压缩与超时

| 项 | 值 | 来源 |
|---|---|---|
| 全局 HTTP bodyLimit | `maxUploadBytes(env, config)`；`PI_WEB_MAX_UPLOAD_BYTES` > 配置文件 `maxUploadBytes` > 默认 **64 MiB** | `index.ts:6`、`config.ts:51,142-149`、`app.ts:174` |
| sessiond 自身 bodyLimit | 同上（读 daemon 捕获的 env + 配置快照） | `sessiond.ts:87` |
| 插件后端 POST 请求体 | `PLUGIN_BACKEND_REQUEST_BODY_MAX_BYTES` = 256 KiB + 4 KiB = **266240** 字节 | `shared/pluginBackendProtocol.ts:4,8`、`plugins/pluginBackendProxyRoutes.ts:37`、`sessiond/pluginBackendRoutes.ts:63` |
| 插件后端响应体上限 | `PLUGIN_BACKEND_RESPONSE_BODY_MAX_BYTES` = 8 MiB + 4 KiB | `shared/pluginBackendProtocol.ts:6,10` |
| 插件后端 JSON input 上限 | `PLUGIN_BACKEND_JSON_MAX_BYTES` = 256 KiB，最大 JSON 深度 64 | `shared/pluginBackendProtocol.ts:4,58,112-123` |
| 工作区删除请求体 | 4 KiB（由「文件」章节详述） | `sessiond/workspaceRemovalRoutes.ts:43` |
| 响应压缩 | `@fastify/compress`，threshold 1024 字节 | `app.ts:177-181` |
| 远程机器默认超时 | `DEFAULT_REMOTE_REQUEST_TIMEOUT_MS` = 30 000 ms（health/runtime 探测 3 000 ms） | `machineClient.ts:29-30` |
| 插件后端远程代理超时 | `PLUGIN_BACKEND_FEDERATION_TIMEOUT_MS` = 30 000 ms | `shared/pluginBackendProtocol.ts:16` |
| pi-packages 变更代理超时 | `PI_PACKAGE_MUTATION_PROXY_TIMEOUT_MS` = 5 分钟 | `shared/federatedRoutes.ts:17` |

#### 0.4 错误响应结构

绝大多数错误是 Fastify 风格的 `{"error":"<message>"}`（`app.ts:74,83,91,100`、`machineRoutes.ts:11,17,23,29,36,39,46,49` 等）。
偏差形态：

| 形态 | 出现位置 |
|---|---|
| `{"error":"...","code":"...","detail":"...","machineId":"..."}` | 远程机器代理网关错误（`machineProxyRoutes.ts:476-485`）、插件生命周期不兼容（`machinePluginProxyRoutes.ts:268-275`） |
| `{"error":"...","code":"required-plugin-runtime-<status>","detail":"..."}` | 插件运行时依赖缺失（`app.ts:162-168`） |
| `{"error":"...","code":"daemon-unavailable"/"daemon-protocol-error","pluginId":"...","operation":"..."}` | 插件后端代理（`plugins/pluginBackendProxyRoutes.ts:46-51,98-105`） |
| `{"error":"...","code":"<code>","pluginId":"...","operation":"..."}` | sessiond 插件后端（`sessiond/pluginBackendRoutes.ts:138-147`） |
| `{"statusCode":404,"error":"Not Found","message":"Route ... not found"}` | sessiond 未注册路径（Fastify 默认，`pluginBackendProxyRoutes.ts:107-112` 会识别） |

#### 0.5 插件/Profile 依赖失败：`withProfileDependency`（409 / 503）

`/pi-web-plugins/manifest.json`、`/pi-web-plugins/:pluginId/*`、`/api/plugins`、`/api/machines/local/plugins`
共用包装器 `withProfileDependency`（`app.ts:157-171`）：

| 抛出 | HTTP | 响应体 |
|---|---|---|
| `ActiveAgentProfileAccessError`（sessiond 不可达或 profile 非法） | **503** | `{"error":"Active agent profile is unavailable: <reason>"}` / `"Active agent profile is invalid: <reason>"`（`activeAgentProfileProvider.ts:21-30`、`app.ts:161`） |
| `PiWebPluginManifestRuntimeError("unavailable")` | **503** | `{"error":"Required Terminal plugin runtime is unavailable","code":"required-plugin-runtime-unavailable","detail":"<detail>"}`（`piWebPluginService.ts:37-45`、`app.ts:162-168`） |
| `PiWebPluginManifestRuntimeError("incompatible")` | **409** | `{"error":"Required Terminal plugin runtime is incompatible","code":"required-plugin-runtime-incompatible","detail":"<detail>"}`（同上） |

`detail` 的构造见 `piWebPluginService.ts:258-261`。
`/api/plugins` 本身不会因 sessiond 不可用而报错：它在 `serverRuntime.status` 字段里报告 `unavailable`/`incompatible`
（`piWebPluginService.ts:126-128`、`piWebPluginLifecycle.ts:46-48`）。

#### 0.6 服务拓扑（本章涉及的两个进程）

- **web 边缘**（`buildApp`，默认监听 `config.host:config.port`，默认 `127.0.0.1:8504`，`index.ts:5-7`）：本文件绝大多数的 `/api/...`、`/pi-web-plugins/...` 路由都在这里。
- **sessiond**（`sessiond.ts`）：监听 unix socket（`sessiondSocketPath`）或 `PI_WEB_SESSIOND_HOST:PI_WEB_SESSIOND_PORT`
  （默认 host `127.0.0.1`，`sessiond.ts:395-414`）。它提供 `/status`、`/notices`、`/notices/dismiss`、`/health`、
  `/runtime`、插件后端与工作区删除等内部路由；web 边缘通过 `SessionDaemonClient` 代理其中一部分（`sessionProxyRoutes.ts:18-54`）。
  sessiond 关停期间任何请求都会被 `onRequest` 钩子拦成 **503** `{"error":"Session daemon is shutting down"}`（`sessiond.ts:100-106`）。

---

### 1. 端点总清单

> `WS` = WebSocket 升级路由。`别名` 指同一处理器在 `/api/machines/local` 前缀下的等价路径。

#### 1.1 web 边缘（浏览器/外部组件可见）

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/pi-web-plugins/manifest.json` | 浏览器插件清单（`app.ts:213`） |
| GET | `/pi-web-plugins/:pluginId/*` | 插件静态资产；`pluginId` 为 `machine.<hex>.<id>` 时转发到远程机器（`app.ts:215-227`、`machinePluginProxyRoutes.ts:68-95`） |
| GET | `/api/pi-web/status` | pi-web 状态（含 release/commands/messages），`?refresh=1` 强制刷新（`app.ts:229-231`） |
| GET | `/api/pi-web/version` | 仅版本/安装信息（`app.ts:232-235`） |
| GET | `/api/pi-web/runtime` | 运行时组件与能力（`app.ts:236`） |
| GET | `/api/plugins` | 插件列表 + 诊断 + serverRuntime（`app.ts:237`） |
| GET | `/api/machines/local/plugins` | 同 `/api/plugins` 的 local 别名（`app.ts:238`） |
| GET | `/api/pi-packages` | 列出已配置 Pi 包（`piPackageRoutes.ts:11`） |
| POST | `/api/pi-packages/install` | 安装 Pi 包（`piPackageRoutes.ts:19`） |
| POST | `/api/pi-packages/remove` | 移除 Pi 包（`piPackageRoutes.ts:27`） |
| POST | `/api/pi-packages/update` | 更新一个或全部 Pi 包（`piPackageRoutes.ts:36`） |
| GET/POST | `/api/machines/local/pi-packages` 与 `/api/machines/local/pi-packages/{install,remove,update}` | 上述 4 条的 local 别名（`app.ts:240`） |
| GET | `/api/config` | 读取全局配置（`configRoutes.ts:49`） |
| PUT | `/api/config` | 覆盖写全局配置（`configRoutes.ts:57`） |
| GET | `/api/machines/local/config` | 读取「选中机器」配置投影（`configRoutes.ts:68`） |
| PUT | `/api/machines/local/config` | 局部合并写「选中机器」配置（`configRoutes.ts:76`） |
| GET | `/api/machines` | 机器列表（含合成的 `local`）（`machineRoutes.ts:5`） |
| POST | `/api/machines` | 新增远程机器（`machineRoutes.ts:7`） |
| GET | `/api/machines/:machineId` | 单台机器（`machineRoutes.ts:27`） |
| PATCH | `/api/machines/:machineId` | 修改机器（`machineRoutes.ts:33`） |
| DELETE | `/api/machines/:machineId` | 删除机器（`machineRoutes.ts:43`） |
| GET | `/api/machines/:machineId/health` | 机器健康（`machineRoutes.ts:15`） |
| GET | `/api/machines/:machineId/runtime` | 机器运行时，`?refresh=1` 绕过缓存（`machineRoutes.ts:21`） |
| GET | `/api/machines/:machineId/pi-web-plugins/manifest.json` | 远程机器插件清单（重写 module 为机器作用域 URL）（`machinePluginProxyRoutes.ts:44-65`） |
| GET | `/api/machines/:machineId/config` | 远程「选中机器」配置读取（代理，`shared/federatedRoutes.ts:43`） |
| PUT | `/api/machines/:machineId/config` | 远程「选中机器」配置合并写（代理，`shared/federatedRoutes.ts:44`、`machineProxyRoutes.ts:190-206`） |
| GET | `/api/machines/:machineId/pi-web/status` | 远程状态代理（`shared/federatedRoutes.ts:42`） |
| GET | `/api/machines/:machineId/plugins` | 远程插件列表代理（`shared/federatedRoutes.ts:45`） |
| GET | `/api/machines/:machineId/pi-packages` | 远程 Pi 包列表代理（`shared/federatedRoutes.ts:46`） |
| POST | `/api/machines/:machineId/pi-packages/install` | 远程安装代理（`shared/federatedRoutes.ts:47`） |
| POST | `/api/machines/:machineId/pi-packages/remove` | 远程移除代理（`shared/federatedRoutes.ts:48`） |
| POST | `/api/machines/:machineId/pi-packages/update` | 远程更新代理（`shared/federatedRoutes.ts:49`） |
| GET | `/api/machines/:machineId/status` | 远程机器状态快照代理（`shared/federatedRoutes.ts:124`） |
| GET | `/api/machines/:machineId/notices` | 远程通知快照代理（`shared/federatedRoutes.ts:125`） |
| POST | `/api/machines/:machineId/notices/dismiss` | 远程通知消除代理（`shared/federatedRoutes.ts:126`） |
| POST | `/api/plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/:operation` | 插件后端 JSON 请求（本地 → sessiond）（`plugins/pluginBackendProxyRoutes.ts:20,35-80`） |
| POST | `/api/paired-plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/:operation` | 版本配对的插件后端 JSON 请求（`plugins/pluginBackendProxyRoutes.ts:25-27`） |
| POST | `/api/machines/:machineId/plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/:operation` | 同上，转发到远程机器（`shared/federatedRoutes.ts:55-62`） |
| POST | `/api/machines/:machineId/paired-plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/:operation` | 同上，转发到远程机器（`shared/federatedRoutes.ts:63-70`） |
| WS | `/api/events` | 全局实时 socket：机器状态 + 通知（`sessionProxyRoutes.ts:43-45`、`sessiond.ts:253,258`） |
| WS | `/api/paired-plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/channels/:operation` | 插件后端长连接（本地 → sessiond）（`plugins/pluginBackendChannelProxyRoutes.ts:32-67`） |
| WS | `/api/machines/:machineId/paired-plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/channels/:operation` | 同上，转发到远程机器（`machineProxyRoutes.ts:81-116`、`shared/federatedRoutes.ts:184`） |
| WS | `/api/machines/:machineId/events` | 远程机器全局实时 socket（`shared/federatedRoutes.ts:185`） |
| WS | `/api/machines/:machineId/sessions/events` | 远程机器会话列表事件（`shared/federatedRoutes.ts:186`，详见会话章节） |
| WS | `/api/machines/:machineId/sessions/:sessionId/events` | 远程机器单会话事件（`shared/federatedRoutes.ts:187`，详见会话章节） |
| GET | `/api/sessiond/health` | 代理到 sessiond `/health`（`sessionProxyRoutes.ts:32`） |
| GET | `/api/sessiond/runtime` | 代理到 sessiond `/runtime`（`sessionProxyRoutes.ts:33`） |
| GET | `/api/machines/local/sessiond/health`、`/api/machines/local/sessiond/runtime` | 上述两条的 local 别名（`app.ts:252`） |
| ANY* | `/api/status` | 代理到 sessiond `GET /status`（`sessionProxyRoutes.ts:47`） |
| ANY* | `/api/machines/local/status` | 同上 local 别名（`app.ts:252`） |
| ANY* | `/api/notices` | 代理到 sessiond `GET /notices`（`sessionProxyRoutes.ts:48`） |
| ANY* | `/api/notices/dismiss` | 代理到 sessiond `POST /notices/dismiss`（`sessionProxyRoutes.ts:49`） |
| ANY* | `/api/machines/local/notices`、`/api/machines/local/notices/dismiss` | 上述两条的 local 别名（`app.ts:252`） |
| GET | `/favicon.svg` | 部署身份资产（dev 变体替换）（`deploymentIdentityRoutes.ts:19-24`、`deploymentIdentity.ts:43-48`） |
| GET | `/apple-touch-icon.png` | 同上 |
| GET | `/pwa-icon-192.png` | 同上 |
| GET | `/pwa-icon-512.png` | 同上 |
| GET | `/manifest.webmanifest` | PWA 清单，dev 下重写 name/short_name/theme_color（`deploymentIdentityRoutes.ts:26-29`、`deploymentIdentity.ts:52-57,77-87`） |

\* `app.all()` 注册，任何方法都会转发到 sessiond；sessiond 只实现 GET/POST，别的方法得到 sessiond 的 404（`sessionProxyRoutes.ts:47-49`）。

#### 1.2 仅 sessiond 内部（不经网关；被上面部分路由代理）

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/status` | 机器状态快照（`status/machineStatusRoutes.ts:12-14`） |
| GET | `/notices` | 服务器通知快照（`notices/serverNoticeRoutes.ts:11`） |
| POST | `/notices/dismiss` | 消除一条通知（`notices/serverNoticeRoutes.ts:13-24`） |
| GET | `/health` | sessiond 健康 + 活跃会话数 + 版本（`sessiond.ts:378-390`） |
| GET | `/runtime` | sessiond 运行时组件（含 `activeAgentProfile`）（`sessiond.ts:309-316,392`） |
| POST | `/plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/:operation` | 插件后端 JSON 请求（`sessiond/pluginBackendRoutes.ts:47-49,61`） |
| POST | `/paired-plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/:operation` | 配对插件后端 JSON 请求（`sessiond/pluginBackendRoutes.ts:52-54`） |
| WS | `/paired-plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/channels/:operation` | 配对插件后端长连接（`sessiond/pluginBackendChannelRoutes.ts:69-88`） |
| DELETE | `/workspace-removals/projects/:projectId/workspaces/:workspaceId` | 主机编排的工作区删除（`sessiond/workspaceRemovalRoutes.ts:36-76`，详见「文件」章节） |

（sessiond 还注册了会话、认证、工作区目录路由，属于其他章节。）

#### 1.3 机器代理覆盖的全量 federated 路由（本节之外的仅列名）

`registerMachineProxyRoutes` 对 `FEDERATED_HTTP_ROUTES`（`shared/federatedRoutes.ts:41-181`）逐个生成
`/api/machines/:machineId<path>`，对 `FEDERATED_WEBSOCKET_ROUTES`（`shared/federatedRoutes.ts:183-188`）生成 WS 路由
（`machineProxyRoutes.ts:53-116`）。除 §1.1 已列出的条目外，其余属于会话/项目/文件章节：

`GET /projects`（:50）、`POST /projects`（:51）、`DELETE /projects/:projectId`（:52）、`GET /project-directories`（:53）、
`GET /projects/:projectId/workspaces`（:54）、`DELETE /projects/:projectId/workspaces/:workspaceId`（:71-76）、
`GET /projects/:projectId/workspaces/:workspaceId/tree`（:77-83）、`GET|PUT|DELETE /projects/:projectId/workspaces/:workspaceId/file`（:84-104）、
`POST .../file/move`（:105-111）、`GET .../file/preview`（:112-117）、`GET .../files`（:118）、
`GET /projects/trust`（:121）、`GET|PUT /projects/:projectId/workspaces/:workspaceId/trust`（:122-123）、
`/sessions...`（:127-173）、`/auth...`（:174-180）。

---

### 2. 状态 / 版本 / 运行时

#### 2.1 `GET /api/pi-web/status`

- 处理器：`app.ts:229-231`；数据由 `createPiWebStatusCache` 缓存 60 秒（`piWebStatusCache.ts:3,25-28,50-65`），配置写入会使其失效（`app.ts:146-155,241-243`）。
- Query：

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `refresh` | `"1"` 或省略 | 省略 | 等于 `"1"` 时调用 `refresh({force:true})`，强制查询 npm release（`app.ts:229-230`、`piWebStatus.ts:173`） |

- 响应：`200`，`PiWebStatusResponse`（`shared/pluginApiTypes.ts:156-175`）。字段由 `getPiWebVersionStatus` + `getPiWebStatus` 生成（`piWebStatus.ts:158-186`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `packageName` | `string` | 固定 `"@jmfederico/pi-web"`（`piWebStatus.ts:17,132,164`） |
| `generatedAt` | `string` | 本次生成的 ISO 时间（`piWebStatus.ts:133,165`） |
| `components.web` / `components.sessiond` | `PiWebComponentStatus` | 见下 |
| `release` | `PiWebReleaseStatus` | npm 最新版本检查结果（`piWebStatus.ts:404-421`） |
| `commands` | `{update?, restart?, restartWeb?, restartSessiond?, status?}` | 按安装方式推导的命令（`piWebStatus.ts:438-509`） |
| `messages` | `PiWebStatusMessage[]` | 派生告警，`id` 取值 `update-available` / `web-stale` / `sessiond-unavailable` / `sessiond-stale`（`piWebStatus.ts:597-650`） |

`PiWebComponentStatus` 字段（`shared/pluginApiTypes.ts:126-137`）：`component`（`"web"`/`"sessiond"`）、`label`、
`runtimeVersion`（进程内 package.json 版本，缺失时 `"0.0.0-dev"`，`piWebStatus.ts:19,144`）、
`installedVersion`（磁盘上安装版本，读不到则省略）、`piVersion`（`@earendil-works/pi-coding-agent` 的 `VERSION`，`piWebStatus.ts:8,151`）、
`stale`、`available`、`installation`（`PiWebInstallationInfo`：`kind` 为 `pi-package|npm-global|local|docker|unknown`，`piWebStatus.ts:232-244`）、`error`。
sessiond 不可用时 `available:false` 且带 `error`（`piWebStatus.ts:357-382,394-402`）。

```json
{
  "packageName": "@jmfederico/pi-web",
  "generatedAt": "2026-05-25T08:30:00.000Z",
  "components": {
    "web": {
      "component": "web",
      "label": "Web/UI",
      "runtimeVersion": "1.202609.0",
      "installedVersion": "1.202609.0",
      "piVersion": "0.8.0",
      "stale": false,
      "available": true,
      "installation": { "kind": "npm-global", "path": "/usr/local/lib/node_modules/@jmfederico/pi-web", "npmRoot": "/usr/local/lib/node_modules" }
    },
    "sessiond": {
      "component": "sessiond",
      "label": "Session daemon",
      "runtimeVersion": "1.202609.0",
      "installedVersion": "1.202609.0",
      "piVersion": "0.8.0",
      "stale": false,
      "available": true,
      "installation": { "kind": "npm-global", "path": "/usr/local/lib/node_modules/@jmfederico/pi-web", "npmRoot": "/usr/local/lib/node_modules" }
    }
  },
  "release": {
    "packageName": "@jmfederico/pi-web",
    "latestVersion": "1.202610.0",
    "updateAvailable": true,
    "checkedAt": "2026-05-25T08:29:59.000Z"
  },
  "commands": {
    "update": "npm install -g @jmfederico/pi-web --allow-scripts=node-pty && pi-web restart",
    "restart": "pi-web restart",
    "status": "pi-web status"
  },
  "messages": [
    {
      "id": "update-available",
      "severity": "info",
      "title": "PI WEB update available",
      "body": "PI WEB 1.202610.0 is available; installed version is 1.202609.0. Run the update command to update PI WEB and restart its services.",
      "command": "npm install -g @jmfederico/pi-web --allow-scripts=node-pty && pi-web restart"
    }
  ]
}
```

- 离线/跳过检查：设置 `PI_WEB_SKIP_VERSION_CHECK`、`PI_WEB_OFFLINE`、`PI_SKIP_VERSION_CHECK`、`PI_OFFLINE`
  任一非空时，`release` 变成 `{"packageName":"@jmfederico/pi-web","updateAvailable":false,"checkedAt":"...","skipped":true}`
  （`piWebStatus.ts:406-408,656-661`）。registry 请求失败时 `release.error` 带错误文本（`piWebStatus.ts:413-421`）。
- 状态码：正常情况下总是 `200`；内部错误（如缓存、sessiond、npm）都被编码进 payload（`piWebStatus.ts:342-382,404-436`），
  不会变成错误响应。若首次加载抛未捕获异常，Fastify 会返回其默认 500 形态 `{"statusCode":500,"error":"Internal Server Error","message":"..."}`
  <!-- TODO: 未在源码中找到 /api/pi-web/status 必然抛出未捕获异常的具体路径；仅 Fastify 默认行为 -->

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/pi-web/status"

# 强制刷新 release 检查
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/pi-web/status?refresh=1"
```

#### 2.2 `GET /api/pi-web/version`

- 处理器：`app.ts:232-235`；调 `getPiWebVersionStatus`（`piWebStatus.ts:158-168`）。
- 无参数。
- 响应：`200`，`PiWebVersionResponse`（`shared/pluginApiTypes.ts:156-163`）= §2.1 去掉 `release` / `commands` / `messages` 的形态。
- 与 §2.1 的差异：`app.ts:233-234` 会先把 active agent profile 作为 `activeAgentProfile` 传给 `getPiWebComponentStatus`，
  用于识别 `pi-package` 安装（`piWebStatus.ts:139-156,286-309`）。
- 错误：同 §2.1（内部错误被吸收）。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/pi-web/version"
```

#### 2.3 `GET /api/pi-web/runtime`

- 处理器：`app.ts:236`；调 `getPiWebRuntime`（`piWebStatus.ts:128-137`）。
- 无参数。
- 响应：`200`，`PiWebRuntimeResponse`（`shared/apiTypes.ts:1213-1221`）。

| 字段 | 类型 | 说明 |
|---|---|---|
| `packageName` | `string` | `"@jmfederico/pi-web"` |
| `generatedAt` | `string` | ISO 时间 |
| `components.web` | `PiWebRuntimeComponent` | 本进程：`capabilities` 固定为 `WEB_RUNTIME_CAPABILITIES = ["plugins.lifecycle"]`（`shared/capabilities.ts:12-14`），并带 `deprecatedAgentInputs`（配置读取失败时改为 `error` 文本，`piWebStatus.ts:117-126`） |
| `components.sessiond` | `PiWebRuntimeComponent` | 来自 sessiond `GET /runtime`；不可用时 `available:false`+`error`，无 `runtimeVersion`（`piWebStatus.ts:342-355,384-392`） |
| `capabilities` | `PiWebCapability[]` | `effectivePiWebCapabilities`：只保留**已知能力**且所需组件都 `available` 并声明了该能力；目前唯一能力 `plugins.lifecycle` 只要求 `web`（`shared/capabilities.ts:20-22,37-45`） |

`PiWebRuntimeComponent` 字段（`shared/apiTypes.ts:1198-1211`）：`component`、`label`、`runtimeVersion?`、`piVersion?`、
`available`、`capabilities`、`activeAgentProfile?`（`{schemaVersion:2, dir}`，仅 sessiond，`shared/apiTypes.ts:1192-1196`）、
`deprecatedAgentInputs?`、`error?`。

```json
{
  "packageName": "@jmfederico/pi-web",
  "generatedAt": "2026-05-25T08:30:00.000Z",
  "components": {
    "web": {
      "component": "web",
      "label": "Web/UI",
      "runtimeVersion": "1.202609.0",
      "piVersion": "0.8.0",
      "available": true,
      "capabilities": ["plugins.lifecycle"]
    },
    "sessiond": {
      "component": "sessiond",
      "label": "Session daemon",
      "runtimeVersion": "1.202609.0",
      "piVersion": "0.8.0",
      "available": true,
      "capabilities": [],
      "activeAgentProfile": { "schemaVersion": 2, "dir": "/home/alice/.pi" }
    }
  },
  "capabilities": ["plugins.lifecycle"]
}
```

sessiond 不可用时的 `components.sessiond`：

```json
{
  "component": "sessiond",
  "label": "Session daemon",
  "available": false,
  "capabilities": [],
  "error": "runtime check returned HTTP 503"
}
```

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/pi-web/runtime"
```

#### 2.4 `GET /api/sessiond/health` 与 `GET /api/sessiond/runtime`

- 注册：`sessionProxyRoutes.ts:32-33`（local 别名 `/api/machines/local/sessiond/{health,runtime}`，`app.ts:252`）。
- 它们是薄代理：把请求路径 `{prefix}/health`、`{prefix}/runtime` 转给 sessiond，保留上游 `statusCode` 与 `content-type`（`sessionProxyRoutes.ts:19-30`）。
- sessiond 侧实现：`/health` 返回 `{ok, activeSessions, checkedAt, version:{component,label,runtimeVersion?,piVersion?,stale,available}}`
  （`sessiond.ts:378-390`）；`/runtime` 返回 sessiond 的 `PiWebRuntimeComponent`（含 `activeAgentProfile`，`sessiond.ts:309-316,392`）。
- 错误：sessiond 不可达或响应非 JSON → **502** `{"error":"Session daemon unavailable: <message>"}`（`sessionProxyRoutes.ts:26-29,63-69`）。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/sessiond/health"

curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/sessiond/runtime"
```

#### 2.5 `GET /api/status` —— 机器状态快照

- 注册与实现：web 边缘用 `app.all(`${prefix}/status`)` 原样代理（`sessionProxyRoutes.ts:47`，local 别名 `app.ts:252`）；
  sessiond 的 `GET /status` 同步返回内存投影（`status/machineStatusRoutes.ts:12-14`、`status/machineStatusService.ts:75-78`）。
- 无参数、无请求体。
- 响应：`200`，`MachineStatusSnapshot`（`shared/machineStatus.ts:30-44`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `epochId` | `string` | daemon 实例标识；变化表示客户端应丢弃旧状态（`machineStatusService.ts:65`） |
| `revision` | `number` | 同一 epoch 内单调递增（`machineStatusService.ts:127`） |
| `machine` | `Record<string, boolean>` | `projects`/`workspaces`/`unattributed` 的滚动汇总（`machineStatusService.ts:159-163`） |
| `projects` | `Record<projectId, Record<string, boolean>>` | 按项目聚合 |
| `workspaces` | `Record<workspaceId, Record<string, boolean>>` | 按工作区聚合 |
| `unattributed` | `Record<string, boolean>` | 无法归属到工作区的活跃 cwd |
| `generatedAt` | `string` | ISO 时间 |

目前只会出现三个 flag（`shared/machineStatus.ts:56-63`）：`core:working`（有会话在跑）、`core:terminal`（有活终端）、
`core:unread`（有未读完成）。只有被置位的 flag 会出现（`machineStatusService.ts:175-191`）。

```json
{
  "epochId": "0f5a1d2e-6b1a-4c3d-9e77-2b8f6a1c0d42",
  "revision": 12,
  "machine": { "core:working": true, "core:unread": true },
  "projects": { "3f1c9b7e": { "core:working": true, "core:unread": true } },
  "workspaces": { "8a2d0c11": { "core:working": true } },
  "unattributed": {},
  "generatedAt": "2026-05-25T08:30:00.000Z"
}
```

- 错误：sessiond 不可达 → 502（代理层，`sessionProxyRoutes.ts:68-70`）；sessiond 关停中 → 503 `{"error":"Session daemon is shutting down"}`（`sessiond.ts:100-106`）。
- 实时等价物见 §11.1：`/api/events` 连接时会立刻收到同样结构的 `{"type":"machine.status","status":{...}}`（`sessiond.ts:258`）。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/status"
```

#### 2.6 `GET /api/machines/:machineId/health` / `/runtime`（远程探测）

- 见 §4.3。

---

### 3. 配置

#### 3.1 `GET /api/config` 与 `PUT /api/config`

- 注册：`configRoutes.ts:48-65`；`app.ts:242` 注册时使用包了缓存失效的 service（`app.ts:146-155,241`）。
- `GET`：无参数，返回 `PiWebConfigResponse`（`shared/apiTypes.ts:347-353`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `path` | `string` | 配置文件绝对路径（`configRoutes.ts:40`） |
| `exists` | `boolean` | 文件是否存在 |
| `config` | `PiWebConfigValues` | 文件里显式写下的值（`configRoutes.ts:41`） |
| `effectiveConfig` | `PiWebConfigValues` | 与环境变量合并后的生效值（`configRoutes.ts:37,42`） |
| `envOverrides` | `PiWebConfigEnvOverrides` | 哪些键被环境变量覆盖：`host,port,allowedHosts,spawnSessions,subsessions,askUser`（`configRoutes.ts:276-285`，`shared/apiTypes.ts:338-345`） |

- `PUT`：请求体类型 `{ config?: unknown } | undefined`（`configRoutes.ts:57`）。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `config` | `object`（`PiWebConfig`） | 是（缺失即报错 `"PI WEB config update must include a config object"`，`configRoutes.ts:124`） | 见下表，**整份覆盖写**（`configRoutes.ts:28-32`） |

`config` 内允许的字段（`configRoutes.ts:123-168`）：

| 字段 | 类型 | 约束/说明 |
|---|---|---|
| `host` | `string` | `configRoutes.ts:139-142` |
| `port` | `number` | `configRoutes.ts:143-146` |
| `allowedHosts` | `string[]` 或 `true` | `configRoutes.ts:147,190-196` |
| `shortcuts` | `Record<string, string \| null>` | 值必须是非空字符串或 `null`（`configRoutes.ts:148,198-204`） |
| `plugins` | `Record<pluginId, {enabled?: boolean; settings?: object; ...}>` | plugin id 必须匹配 `^[a-z][a-z0-9.-]*$`（`configRoutes.ts:149,234-245`、`shared/pluginIds.ts:1`） |
| `pathAccess` | `{allowedPaths?: string[]}` | 非空字符串数组（`configRoutes.ts:150,206-219`） |
| `uploads` | `{defaultFolder?: string}` | 由 `parseUploadsConfig` 解析（`configRoutes.ts:151`） |
| `attachments` | `{defaultFolder?: string}` | 由 `parseAttachmentsConfig` 解析（`configRoutes.ts:152`） |
| `maxUploadBytes` | `number` | 正整数（`configRoutes.ts:153,225-228`） |
| `spawnSessions` | `boolean` | `configRoutes.ts:154-157` |
| `subsessions` | `boolean` | `configRoutes.ts:158-161` |
| `askUser` | `boolean` | `configRoutes.ts:162-165` |
| `agent` | `PiWebAgentConfig` | 由 `parseAgentConfig(value,"request","current")` 解析（`configRoutes.ts:166,230-232`） |

**注意**：`PiWebConfigValues` 里还有 `environmentFacts`、`extensionDialogsTimeoutMs`（`shared/apiTypes.ts:187-199`），
但 `parseConfigRequest` 只读取上表字段，**这两个键通过 `PUT /api/config` 无法写入**（会被静默忽略）——
它们只能写配置文件。<!-- TODO: 确认是否有意为之（parseConfigRequest 不遍历未知键，也不报错） -->

- 响应（GET/PUT 相同）：`200` + `PiWebConfigResponse`。
- 错误：

| 状态 | 触发 | 响应体 |
|---|---|---|
| 400 | 校验失败（消息以 `"PI WEB config"` 开头，`configRoutes.ts:291-293`） | `{"error":"PI WEB config port must be a number"}` 等 |
| 500 | 读写文件等其他异常（`configRoutes.ts:61-62`） | `{"error":"<message>"}` |

```bash
# 读取
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/config"

# 覆盖写（示例：只改了 host/port/allowedHosts，其余键会按文件语义重建）
curl -sS -u alice:"${USER_PASSWORD}" -X PUT \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/config" \
  -d '{"config":{"host":"0.0.0.0","port":8504,"allowedHosts":true,"spawnSessions":true,"subsessions":true}}'
```

```json
{
  "path": "/home/alice/.pi-web/config.json",
  "exists": true,
  "config": { "host": "0.0.0.0", "port": 8504, "allowedHosts": true, "spawnSessions": true, "subsessions": true },
  "effectiveConfig": { "host": "0.0.0.0", "port": 8504, "allowedHosts": true, "spawnSessions": true, "subsessions": true },
  "envOverrides": { "host": false, "port": false, "allowedHosts": false, "spawnSessions": false, "subsessions": false, "askUser": false }
}
```

#### 3.2 `GET /api/machines/local/config` 与 `PUT /api/machines/local/config`

这是「选中机器」的配置视图（不是机器代理）：读时把响应裁剪到允许键，写时把 patch 合并进当前配置。

- 注册：`configRoutes.ts:67-86`。
- `GET`：返回 `selectedMachineConfigResponse(await service.read())`，即 `PiWebConfigResponse` 的 `config` 与 `effectiveConfig`
  只保留 `SELECTED_MACHINE_CONFIG_KEYS`（`configRoutes.ts:104-110,11-21`）。
- `PUT`：请求体 `{ config?: unknown } | undefined`（`configRoutes.ts:76`）。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `config` | `object` | 是（否则 400，`configRoutes.ts:88-92`） | 只允许 `SELECTED_MACHINE_CONFIG_KEYS` 中的键 |

允许的键（`configRoutes.ts:11-21`）与非法键错误：

| 允许的键 | 说明 |
|---|---|
| `plugins`, `pathAccess`, `uploads`, `attachments`, `maxUploadBytes`, `spawnSessions`, `subsessions`, `askUser`, `agent` | 不在白名单的键 → 400 `{"error":"PI WEB selected-machine config key is not allowed: <key>"}`（`configRoutes.ts:90-92`） |

- 语义：`mergeSelectedMachineConfig(current.config, patch)` = `{...current, ...patch}`（`configRoutes.ts:100-102`），
  即**局部更新**，其余全局键（`host`/`port`/`allowedHosts`/`shortcuts`）保持不动；随后调 `service.write` 落盘并返回裁剪后的响应。
- 错误：400（校验，消息前缀 `"PI WEB selected-machine config"`，`configRoutes.ts:184-188,291-293`）；500。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/machines/local/config"

curl -sS -u alice:"${USER_PASSWORD}" -X PUT \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/machines/local/config" \
  -d '{"config":{"spawnSessions":false}}'
```

```json
{
  "path": "/home/alice/.pi-web/config.json",
  "exists": true,
  "config": { "spawnSessions": false },
  "effectiveConfig": { "spawnSessions": false },
  "envOverrides": { "host": false, "port": false, "allowedHosts": false, "spawnSessions": false, "subsessions": false, "askUser": false }
}
```

---

### 4. 机器（machines）

机器数据持久化在 `machines.json`（`PI_WEB_MACHINES_FILE` 可覆盖，`machineStore.ts:23-31`），文件权限 `0600`（`machineStore.ts:21,131-134`）。
`local` 是运行时合成的机器，不可修改/删除（`machineService.ts:171-173,51,66`），其 `createdAt`/`updatedAt` 固定为
`1970-01-01T00:00:00.000Z`（`machineService.ts:24`）。

#### 4.1 `GET /api/machines` 与 `POST /api/machines`

- 注册：`machineRoutes.ts:5-13`。
- `GET`：无参数。响应 `200`：

```json
{
  "machines": [
    { "id": "local", "name": "Local", "kind": "local", "createdAt": "1970-01-01T00:00:00.000Z", "updatedAt": "1970-01-01T00:00:00.000Z" },
    { "id": "9d1f...", "name": "Remote", "kind": "remote", "baseUrl": "https://remote.example.test", "createdAt": "2026-05-25T08:00:00.000Z", "updatedAt": "2026-05-25T08:00:00.000Z" }
  ]
}
```

  `Machine` 类型见 `shared/apiTypes.ts:80-89`；`token`/`headers` **永不返回**（`machineService.ts:175-177`、`app.machines.test.ts`）。

- `POST`：请求体类型 `CreateMachineInput`（`machineService.ts:7-12`，`machineRoutes.ts:7`）：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | `string` | 是 | 去空格后不能为空，否则 400 `"Machine name is required"`（`machineService.ts:179-183`） |
| `baseUrl` | `string` | 是 | 必须是 `http:`/`https:` URL，不能带凭据、query 或 hash，末尾 `/` 会被去掉（`machineService.ts:185-198`） |
| `token` | `string` | 否 | 作为 `Authorization: Bearer <token>` 发往远端（`machineClient.ts:129`），只写不读 |
| `headers` | `Record<string,string>` | 否 | 额外请求头；`host`、`connection`、`upgrade`、`transfer-encoding`、`content-length`、`keep-alive`、`proxy-authenticate`、`proxy-authorization`、`te`、`trailer`、`authorization`、`cookie` 被禁止（`machineClient.ts:34-47,148-157`） |

- 响应 `200` + `Machine`；`id` 为 `randomUUID()`（`machineStore.ts:44`）。
- 错误：`400 {"error":"<message>"}`（`machineRoutes.ts:10-12`）。

#### 4.2 `GET|PATCH|DELETE /api/machines/:machineId`

| 方法 | 注册 | 请求体 | 成功响应 | 错误 |
|---|---|---|---|---|
| `GET` | `machineRoutes.ts:27-31` | — | `200` `Machine` | `404 {"error":"Machine not found"}` |
| `PATCH` | `machineRoutes.ts:33-41` | `UpdateMachineInput = Partial<CreateMachineInput>`（`machineService.ts:14`），字段语义同 §4.1 | `200` `Machine`（`updatedAt` 刷新，`machineStore.ts:64`） | `400`（校验失败，含 `"Local machine cannot be changed"`，`machineService.ts:51`）；`404` |
| `DELETE` | `machineRoutes.ts:43-51` | — | `200 {"deleted":true}` | `400`（`"Local machine cannot be deleted"`，`machineService.ts:66`）；`404` |

- `PATCH` 成功后清空该机器的 health/runtime 缓存（`machineService.ts:58-61`）。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/machines"

curl -sS -u alice:"${USER_PASSWORD}" -X POST \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/machines" \
  -d '{"name":"Remote","baseUrl":"https://remote.example.test","token":"secret"}'

curl -sS -u alice:"${USER_PASSWORD}" -X PATCH \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/machines/9d1f..." \
  -d '{"name":"Remote 2"}'

curl -sS -u alice:"${USER_PASSWORD}" -X DELETE -H 'Accept: application/json' \
  "${BASE}/alice/api/machines/9d1f..."
```

#### 4.3 `GET /api/machines/:machineId/health` 与 `GET /api/machines/:machineId/runtime`

- 注册：`machineRoutes.ts:15-25`。两者都支持 `machineId=local`（`machineService.ts:90,101`）。
- `health` Query：无。`runtime` Query：

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `refresh` | `"1"` 或省略 | 省略 | `"1"` 时忽略 runtime 缓存（默认缓存 TTL `DEFAULT_HEALTH_CACHE_TTL_MS` = 5 000 ms，`machineService.ts:25,96-105`） |

- `health` 响应 `200` + `MachineHealth`（`shared/apiTypes.ts:91-99`）：`machineId`、`ok`、`checkedAt`、`status`（`unknown|online|offline|error`）、
  `web?`/`sessiond?`（`PiWebComponentStatus`）、`error?`。本地判定：`localRuntime()` 成功 → `status:"online"`，异常 → `ok:false,status:"error"`
  （`machineService.ts:107-122`）；远端：`GET /api/pi-web/status` 2xx → `online`，非 2xx → `error`，异常 → `offline`
  （`machineService.ts:124-137`，超时 3 000 ms，`machineClient.ts:30`）。
- `runtime` 响应 `200` + `MachineRuntime`（`shared/apiTypes.ts:101-112`）：`machineId`、`ok`、`checkedAt`、`packageName?`、
  `generatedAt?`、`components?`、`capabilities?`、`deprecatedAgentInputs?`、`error?`。远端读 `GET /api/pi-web/runtime`（`machineService.ts:148-160`）。
- 错误：`404 {"error":"Machine not found"}`（`machineRoutes.ts:17,23`）。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/machines/local/health"

curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/machines/9d1f.../runtime?refresh=1"
```

#### 4.4 机器配置代理：`GET|PUT /api/machines/:machineId/config`

- 注册：由 `FEDERATED_HTTP_ROUTES` 生成（`shared/federatedRoutes.ts:43-44`），处理器在 `machineProxyRoutes.ts:53-79`，
  `/config` 单独走 `proxySelectedMachineConfigRequest`（`machineProxyRoutes.ts:141,190-206`）。
- `GET`：读远端 `/api/config`，解析后返回**裁剪到选中键**的 `PiWebConfigResponse`（`machineProxyRoutes.ts:191-193,212-218`）。
- `PUT`：请求体 `{config: <selected machine config patch>}`。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `config` | `object` | 是 | 取 `body.config`（`machineProxyRoutes.ts:208-210`）；按 `parseSelectedMachineConfigRequest(..., "portable")` 校验（`machineProxyRoutes.ts:196`、`configRoutes.ts:88-98`） |

  流程：先 `GET` 远端当前配置 → `mergeSelectedMachineConfig(current.config, patch)` → `PUT {config: merged}`（`machineProxyRoutes.ts:197-202`）。
- 成功响应：远端状态码 + 裁剪后的 `PiWebConfigResponse`（`machineProxyRoutes.ts:215-217`）。
- 错误：

| 状态 | 触发 | 响应体 |
|---|---|---|
| `400` | patch 校验失败（消息前缀 `"PI WEB selected-machine config"`） | `{"error":"<message>"}`（`machineProxyRoutes.ts:185`、`configRoutes.ts:184-188`） |
| `404` | 机器不存在 | `{"error":"Machine not found"}`（`machineProxyRoutes.ts:135-137`） |
| `501` | `machineId=local`（避免与静态 local 路由冲突的兜底） | `{"error":"Local machine route is not registered for this endpoint"}`（`machineProxyRoutes.ts:130-132`） |
| 远端 4xx/5xx | 原样透传（含 `machineId`/`statusCode` 兜底体） | `machineProxyRoutes.ts:198,220-224` |
| `502`/`504` | 远端不可达/超时/超长 | `{"error":"Remote machine unavailable","machineId":"...","statusCode":502,"detail":"..."}`（`machineProxyRoutes.ts:476-485`） |

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/machines/9d1f.../config"

curl -sS -u alice:"${USER_PASSWORD}" -X PUT \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/machines/9d1f.../config" \
  -d '{"config":{"spawnSessions":false}}'
```

#### 4.5 通用机器代理（federation）语义

对 §1.3 列出的每条 federated 路由，代理行为一致（`machineProxyRoutes.ts:119-188`）：

1. `machineId === "local"` → `501 {"error":"Local machine route is not registered for this endpoint"}`（`machineProxyRoutes.ts:130-132`）；
2. 未配置的机器 → `404 {"error":"Machine not found"}`（`machineProxyRoutes.ts:134-137`）；
3. 远端路径重写：去掉 `/api/machines/:machineId` 前缀后拼上 `/api`（`machineProxyRoutes.ts:306-311`）；
4. 透传状态码与安全响应头白名单：`content-type`、`content-length`、`content-disposition`、`cache-control`、
   `last-modified`、`etag`、`content-security-policy`、`x-content-type-options`（`machineProxyRoutes.ts:28-37,381-387`）；
5. 远端返回「插件后端路由不存在」的 404 时改判 **409** `{"error":"Remote machine plugin lifecycle is incompatible","code":"plugin-lifecycle-incompatible","machineId":"...","detail":"..."}`（`machineProxyRoutes.ts:166-173,349-371`）；
6. 其它失败：`502`/`504`，体为 `{"error":"Remote machine unavailable"|"Remote machine timeout","machineId":"...","statusCode":<code>,"detail":"..."}`（`machineProxyRoutes.ts:476-485`，`RemoteMachineRequestError` 定义于 `machineClient.ts:49-54`）。

WS 版代理：`local` 用 close code `1011` 关闭，未知机器用 `1008`（`machineProxyRoutes.ts:235-263`）。

---

### 5. 插件

#### 5.1 `GET /pi-web-plugins/manifest.json`

- 注册：`app.ts:213`，包在 `withProfileDependency` 中（见 §0.5）。
- 无参数。
- 响应 `200` + `PiWebPluginManifest`（`piWebPluginService.ts:31-35,97-124`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `lifecycleVersion` | `2` | `PI_WEB_PLUGIN_LIFECYCLE_VERSION`（`shared/apiTypes.ts:206`） |
| `terminalMode` | `"required"` 或 `"recovery-disabled"` | 由 sessiond 的 safe-start 快照决定（`piWebPluginLifecycle.ts:53`、`shared/requiredTerminalPlugin.ts:5-7`） |
| `plugins[]` | `PiWebPluginManifestEntry[]` | 见下 |

`PiWebPluginManifestEntry`（`piWebPluginService.ts:47-59,106-120`）：`id`、`module`（形如
`/pi-web-plugins/<id>/<path>?v=<revision>`，`piWebPluginService.ts:275-287`）、`backendRevision?`、`pairedRequestVersion?: 1`、
`pairedChannelVersion?: 1`、`source`、`scope`（`bundled|local|user|project`）、`machineSpecific`。
`terminalMode === "required"` 时 `pi-web.terminal` 必须是第一项且 `source/scope` 均为 `bundled`、`machineSpecific:true`，
并带 `backendRevision` 与两个 paired 版本（`piWebPluginService.ts:263-273`、`piWebPluginLifecycle.ts:56-64`）。

```json
{
  "lifecycleVersion": 2,
  "terminalMode": "required",
  "plugins": [
    {
      "id": "pi-web.terminal",
      "module": "/pi-web-plugins/pi-web.terminal/browser/pi-web-plugin.js?v=3f9c",
      "backendRevision": "3f9c",
      "pairedRequestVersion": 1,
      "pairedChannelVersion": 1,
      "source": "bundled",
      "scope": "bundled",
      "machineSpecific": true
    }
  ]
}
```

- 错误：`503`/`409` 见 §0.5；`plugins[].module` 指向的资产缺失时该条目被静默跳过（`piWebPluginService.ts:108-109`）。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/pi-web-plugins/manifest.json"
```

#### 5.2 `GET /pi-web-plugins/:pluginId/*?v=<revision>`

- 注册：`app.ts:215-227`。先尝试机器作用域代理（`proxyMachinePluginAsset`），再走本地（`app.ts:216-218`）。
- 路径参数：

| 参数 | 说明 |
|---|---|
| `pluginId` | 插件 id，必须匹配 `^[a-z][a-z0-9.-]*$`（`shared/pluginIds.ts:1`）；`machine.<hex(machineId)>.<pluginId>` 形态会被识别为远程机器插件（`machinePluginProxyRoutes.ts:68-72`、`shared/machinePluginIds.ts:30-45`） |
| `*` | 资产相对路径 |

- Query：`v`（可选）——浏览器插件入口的 revision；当请求的是入口文件且 `v` 与当前缓存 revision 不一致时返回 404，避免新旧字节混用（`app.ts:222`、`piWebPluginService.ts:130-141`）。
- 响应 `200`：资产本体，`content-type` 由扩展名决定——`.js/.mjs` → `application/javascript; charset=utf-8`，`.json` → `application/json; charset=utf-8`，
  `.css` → `text/css; charset=utf-8`，`.html` → `text/html; charset=utf-8`，`.svg` → `image/svg+xml`，其它 → `application/octet-stream`（`piWebPluginService.ts:311-319`）。
- 错误：

| 状态 | 触发 | 响应体 |
|---|---|---|
| `404` | 资产不在缓存/包里 | `{"error":"Plugin asset not found"}`（`app.ts:224`） |
| `404` | 机器作用域 id 指向的机器不存在 | `{"error":"Machine not found"}`（`machinePluginProxyRoutes.ts:72-76`） |
| `400` | 机器作用域资产路径非法 | `{"error":"Invalid remote PI WEB plugin asset path"}`（`machinePluginProxyRoutes.ts:78-82`） |
| `502`/`504` | 远程机器不可达 | `{"error":"Remote machine unavailable","machineId":"...","statusCode":502,"detail":"..."}`（`machinePluginProxyRoutes.ts:277-286`） |
| `503`/`409` | profile/插件运行时依赖 | §0.5 |

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/pi-web-plugins/pi-web.terminal/browser/pi-web-plugin.js?v=3f9c"
```

#### 5.3 `GET /api/plugins` 与 `GET /api/machines/local/plugins`

- 注册：`app.ts:237-238`；处理器 `piWebPlugins.plugins()`，包在 `withProfileDependency` 中。
- 无参数。响应 `200` + `PiWebPluginsResponse`（`shared/apiTypes.ts:275-280`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `lifecycleVersion` | `2` | 同 §5.1 |
| `plugins[]` | `PiWebPluginInfo[]` | 每个插件：`id`、`required?`、`module?`、`source`、`scope`、`machineSpecific`、`enabled`、`discovered`、`conflict`、`server?`（`shared/apiTypes.ts:230-246`；`server.state` 取值 `active/failed/incompatible/disabled/missing/unknown`） |
| `diagnostics[]` | `PiWebPluginDiagnostic[]` | `{kind:"conflict"\|"discovery", snapshot:"desired"\|"active", source, message, pluginId?}`（`shared/apiTypes.ts:248-254`） |
| `serverRuntime` | `PiWebPluginRuntimeInfo` | `{status:"available"\|"unavailable"\|"incompatible", terminalMode, safeStart?, desiredSafeStart?, restartRequired, message?, recovery:{showSafeStart,bundledOnly,noServerPlugins,clearSafeStart}}`（`shared/apiTypes.ts:256-273`） |

```json
{
  "lifecycleVersion": 2,
  "plugins": [
    {
      "id": "pi-web.terminal",
      "required": true,
      "module": "/pi-web-plugins/pi-web.terminal/browser/pi-web-plugin.js?v=3f9c",
      "source": "bundled",
      "scope": "bundled",
      "machineSpecific": true,
      "enabled": true,
      "discovered": true,
      "conflict": false,
      "server": { "state": "active", "activeRevision": "3f9c", "staleRevision": false, "restartRequired": false, "disableCommand": "pi-web plugins disable pi-web.terminal --restart" }
    }
  ],
  "diagnostics": [],
  "serverRuntime": {
    "status": "available",
    "terminalMode": "required",
    "restartRequired": false,
    "recovery": {
      "showSafeStart": "pi-web plugins safe-start show",
      "bundledOnly": "pi-web plugins safe-start set bundled-only --restart",
      "noServerPlugins": "pi-web plugins safe-start set none --restart",
      "clearSafeStart": "pi-web plugins safe-start clear --restart"
    }
  }
}
```

- `serverRuntime.recovery` 的四条命令是固定字符串（`shared/pluginRecoveryCommands.ts:5-11`）；`plugins[].server.disableCommand`
  形如 `pi-web plugins disable <pluginId> --restart`（`shared/pluginRecoveryCommands.ts:13-17`）。
- 错误：只有 `503`（profile 不可用）走 `withProfileDependency`（§0.5）；sessiond 不可用不会报错，而是 `serverRuntime.status:"unavailable"`。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/plugins"
```

#### 5.4 `GET /api/machines/:machineId/pi-web-plugins/manifest.json`

- 注册：`machinePluginProxyRoutes.ts:43-66`；**只用于远程机器**，`machineId=local` 直接 `400 {"error":"Local plugin manifests must use the local manifest endpoint"}`（:45-47）。
- 无参数。成功时返回**重写后**的清单：每个插件的 `module` 改成
  `../../../../pi-web-plugins/<machine.<hex>.<id>>/<assetPath><query>`（`machinePluginProxyRoutes.ts:97-110`）。
- 错误：

| 状态 | 触发 | 响应体 |
|---|---|---|
| `400` | `machineId=local` | `{"error":"Local plugin manifests must use the local manifest endpoint"}` |
| `404` | 机器不存在 | `{"error":"Machine not found"}`（:50） |
| `409` | 远端无版本化清单、lifecycle 不兼容、Terminal 缺失/顺序错误 | `{"error":"Remote machine plugin lifecycle is incompatible","code":"plugin-lifecycle-incompatible","machineId":"...","detail":"..."}`（:55,268-275,209-231） |
| 远端 4xx/5xx | 原样透传 | 远端响应体（:57） |
| `502`/`504` | 远端不可达/超时（超时 10 000 ms，:31,53） | `{"error":"Remote machine unavailable","machineId":"...","statusCode":502,"detail":"..."}`（:277-286） |

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/machines/9d1f.../pi-web-plugins/manifest.json"
```

#### 5.5 插件后端 JSON 请求（POST）

两条本地路径（web 边缘代理到 sessiond）与两条机器代理路径：

| 方法 | 路径 | 注册 |
|---|---|---|
| POST | `/api/plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/:operation` | `plugins/pluginBackendProxyRoutes.ts:20-22,29-81` |
| POST | `/api/paired-plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/:operation` | `plugins/pluginBackendProxyRoutes.ts:25-27` |
| POST | `/api/machines/:machineId/plugin-backends/...`（去掉 `/api` 前缀上送远端） | `shared/federatedRoutes.ts:55-62` |
| POST | `/api/machines/:machineId/paired-plugin-backends/...` | `shared/federatedRoutes.ts:63-70` |

- 路径参数：

| 参数 | 约束 | 来源 |
|---|---|---|
| `pluginId` | 匹配 `^[a-z][a-z0-9.-]*$`（`isPiWebPluginId`） | `sessiond/pluginBackendRoutes.ts:69`、`shared/pluginIds.ts:1` |
| `projectId` | 非空；sessiond 会用 `requireProject` 解析 | `sessiond/pluginBackendRoutes.ts:71,80` |
| `workspaceId` | 非空 | `sessiond/pluginBackendRoutes.ts:72` |
| `operation` | `^[a-z][a-z0-9.-]*$`，长度 ≤ 128（`PLUGIN_BACKEND_OPERATION_MAX_LENGTH`） | `shared/pluginBackendProtocol.ts:17,56,97-102` |

- 请求体：`PluginBackendRequestEnvelope`（`shared/pluginBackendProtocol.ts:60-63,150-156`）：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `revision` | `string` | 是 | 非空且 ≤ 512 字符（`PLUGIN_BACKEND_REVISION_MAX_LENGTH`），必须与插件当前后端 revision 一致 | 
| `input` | `JsonValue` | 是 | 纯 JSON，有限数字、无环、深度 ≤ 64，序列化 ≤ 256 KiB（`shared/pluginBackendProtocol.ts:112-123,294-318`） |

- 成功响应：`200` + `application/json; charset=utf-8`，body 是该插件操作的 JSON 结果，序列化后 ≤ 8 MiB（`sessiond/pluginBackendRoutes.ts:103-108`、`shared/pluginBackendProtocol.ts:6,10`）。
- 错误（sessiond 产生；web 代理原样透传状态码与 body，`plugins/pluginBackendProxyRoutes.ts:72-75`）：

| 状态 | 触发 | 响应体 |
|---|---|---|
| `400` | 非法 pluginId/operation/空 id/envelope 校验失败 | `{"error":"<message>","code":"invalid-request","pluginId":"...","operation":"..."}`（`sessiond/pluginBackendRoutes.ts:74-76,138-147`） |
| `404` | 项目不存在 | `{...,"code":"project-not-found"}`（:80-91） |
| `500` | 项目解析其它失败 | `{...,"code":"project-resolution-failed"}`（同上） |
| `PluginBackendRequestError.statusCode` | 后端主动返回 | `{"error":..., "code":<error.code>,"pluginId":"...","operation":"..."}`（:119-136） |
| `502` | 其它后端异常 | `{"error":"Plugin backend request failed: ...","code":"request-failed",...}`（:128-135） |
| `502` | web 边缘无法连到 sessiond | `{"error":"Session daemon unavailable: ...","code":"daemon-unavailable","pluginId":"...","operation":"..."}`（`plugins/pluginBackendProxyRoutes.ts:45-52`） |
| `502` | sessiond 返回超长/非 JSON/未注册该路由 | `{"error":"<...>","code":"daemon-protocol-error","pluginId":"...","operation":"..."}`（`plugins/pluginBackendProxyRoutes.ts:54-70,98-105,107-112`） |
| `409` | 机器代理发现远端是旧版本（404 未注册） | `{"error":"Remote machine plugin lifecycle is incompatible","code":"plugin-lifecycle-incompatible","machineId":"...","detail":"..."}`（`machineProxyRoutes.ts:166-173`） |

```bash
# Terminal 插件：列出某工作区的终端
curl -sS -u alice:"${USER_PASSWORD}" -X POST \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/paired-plugin-backends/pi-web.terminal/projects/<projectId>/workspaces/<workspaceId>/terminal.list" \
  -d '{"revision":"3f9c","input":null}'
```

#### 5.6 插件后端长连接（WebSocket-only）

| 方法 | 路径 | 注册 |
|---|---|---|
| WS | `/api/paired-plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/channels/:operation` | `plugins/pluginBackendChannelProxyRoutes.ts:26-68`（本地 → sessiond） |
| WS | `/api/machines/:machineId/paired-plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/channels/:operation` | `machineProxyRoutes.ts:81-116`（远程） |
| WS | `/paired-plugin-backends/:pluginId/projects/:projectId/workspaces/:workspaceId/channels/:operation` | `sessiond/pluginBackendChannelRoutes.ts:69-88`（sessiond 内部实现） |

- 握手：首个客户端帧必须是 `open`（`shared/pluginBackendProtocol.ts:65-70`），10 000 ms 内未收到会被以 `open-timeout`/close `1011` 关闭（`sessiond/pluginBackendChannelRoutes.ts:110-112,172-181`、`shared/pluginBackendProtocol.ts:42`）。
- 帧封装（全部为文本 JSON，`version` 必须为 `1`）：

| 方向 | 帧 | 说明 |
|---|---|---|
| 客户端 → 服务端 | `{"version":1,"kind":"open","revision":"...","input":<json>}` | input ≤ 256 KiB（含封装 ≤ 260 KiB，`shared/pluginBackendProtocol.ts:21,158-165`） |
| 客户端 → 服务端 | `{"version":1,"kind":"data","data":<json>}` | 单个 data ≤ 64 KiB（含封装 ≤ 68 KiB，`shared/pluginBackendProtocol.ts:23,25,171-177`） |
| 服务端 → 客户端 | `{"version":1,"kind":"ready"}` | 表示后端已就绪，之后的 data 才会被转发（`shared/pluginBackendProtocol.ts:72-75,167-169`） |
| 服务端 → 客户端 | `{"version":1,"kind":"data","data":<json>}` | 同上限 |
| 服务端 → 客户端 | `{"version":1,"kind":"error","code":"<code>","message":"<msg>"}` | `code` 匹配 `^[a-z][a-z0-9.-]*$` 且 ≤ 64 字符；`message` 非空且 ≤ 2 048 字节（`shared/pluginBackendProtocol.ts:50,179-183,275-288`） |

- 资源上限与关闭码（`sessiond/pluginBackendChannelRoutes.ts`、`shared/pluginBackendProtocol.ts:26-49`）：

| 项 | 值/行为 |
|---|---|
| 客户端→服务端队列 | 最多 128 帧或 1 MiB；超限 → `queue-overflow` + close `1013`（`pluginBackendChannelRoutes.ts:223-231`、`shared/pluginBackendProtocol.ts:26-28`） |
| 服务端→客户端队列 | ≤ 1 280 KiB（Terminal 全量回放 + 实时输出）；溢出 → `queue-overflow` + `1013`（`:347-352`、`shared/pluginBackendProtocol.ts:30`） |
| 二进制帧 | 拒绝 → `binary-frame` + close `1003`（`pluginBackendChannelRoutes.ts:155-160`） |
| 帧校验失败 | `invalid-frame` + close `1008`；乱序 → `unexpected-frame`/`open-required` + `1008`（:164-187） |
| 项目不存在 | `project-not-found` + `1008`（:214-219） |
| 打开失败/传输错误/关闭中 | `open-failed`/`transport-error` + `1011`；`shutdown` + `1012`（:107-131,143-145,210-219） |
| 干净关闭 | 客户端 `1000` 时先 drain（限 10 000 ms），失败改 `1011`（:296-314、`shared/pluginBackendProtocol.ts:44`） |
| socket 载荷上限 | 握手期间 `PLUGIN_BACKEND_CHANNEL_OPEN_FRAME_MAX_BYTES`（260 KiB），就绪后 `..._DATA_FRAME_MAX_BYTES`（68 KiB）（`pluginBackendChannelRoutes.ts:118,178`、`shared/pluginBackendProtocol.ts:21,25`） |

- 传输限额与鉴权：升级请求由 `markPluginBackendChannelUpgradeRequest` 标记，使 `ws` 的 `maxPayload` 只对该类升级收紧
  （`webSocketBridge.ts:68-79`、`app.ts:182-183`）；pi-web 自身不做鉴权，依赖网关的 Basic Auth（§0.1）。

```bash
# 需要支持 WebSocket 的客户端（示例用 websocat）
websocat -H="Authorization: Basic $(printf 'alice:%s' "$USER_PASSWORD" | base64)" \
  "ws://<host>:58681/alice/api/paired-plugin-backends/pi-web.terminal/projects/<projectId>/workspaces/<workspaceId>/channels/terminal.attach"
# 首帧：
# {"version":1,"kind":"open","revision":"3f9c","input":{"terminalId":"t-1","cols":120,"rows":30}}
```

---

### 6. pi-packages

包管理读写当前 **active agent profile** 的 Pi 包（`piPackageService.ts:140-143`）；
安装/移除/更新串行化，避免并发写 settings（`piPackageService.ts:129-138,187-194`）。

#### 6.1 `GET /api/pi-packages`

- 注册：`piPackageRoutes.ts:11-17`，挂载于 `/api` 与 `/api/machines/local`（`app.ts:239-240`）。
- 无参数。
- 响应 `200` + `PiPackagesResponse`（`shared/apiTypes.ts:291-301`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `packages[]` | `PiPackageInfo[]` | `{source, scope:"user"\|"project", filtered, installedPath?}`（`shared/apiTypes.ts:284-289`、`piPackageService.ts:204-211`） |
| `installableKnownPackages?` | `PiPackageInstallableSuggestion[]` | pi-web 内置、尚未配置的已知包，用于「一键安装」：`{id,label,description,source}`（`shared/apiTypes.ts:303-312`、`piPackageService.ts:90-102`）；全部已配置时省略 |

```json
{
  "packages": [
    { "source": "npm:@acme/tools", "scope": "user", "filtered": false, "installedPath": "/home/alice/.pi/packages/@acme/tools" }
  ],
  "installableKnownPackages": [
    { "id": "@jmfederico/pi-relay", "label": "Relays", "description": "Tool-agnostic Relay method, opinionated runner profile, and human-gated preparation prompts for independent Pi session chains.", "source": "/usr/lib/node_modules/@jmfederico/pi-web/dist/pi-packages/relays" }
  ]
}
```

- 错误：`503 {"error":"Active agent profile is unavailable: <reason>"}`（profile 不可用，`piPackageRoutes.ts:82-89`）；`500`（其它）。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/pi-packages"
```

#### 6.2 `POST /api/pi-packages/install` / `remove` / `update`

| 端点 | 请求体类型 | 字段 |
|---|---|---|
| `/api/pi-packages/install` | `PiPackageInstallRequest`（`shared/apiTypes.ts:314-316`） | `source: string`（非空，自动 trim，`piPackageRoutes.ts:59-62`）；带 `scope` 或 `local` 字段会直接 400 `"Pi package install scope is not supported; installs use Pi's default package location"`（`piPackageRoutes.ts:51-57`） |
| `/api/pi-packages/remove` | `PiPackageRemoveRequest`（`shared/apiTypes.ts:318-322`） | `source: string`（必填）；`scope?: "user" \| "project"`（可选，其它值 400，`piPackageRoutes.ts:71-75`） |
| `/api/pi-packages/update` | `PiPackageUpdateRequest`（`shared/apiTypes.ts:324-327`） | `source?: string`；整个 body 省略或 `source` 省略 → 更新**全部**（`piPackageRoutes.ts:64-69`、`piPackageService.ts:173-185`） |

- 成功响应：`200` + `PiPackageMutationResponse`（`shared/apiTypes.ts:331-336`）：
  `{action:"install"|"remove"|"update", source?, scope?, removed?, packages:[...], installableKnownPackages?}`
  （`piPackageService.ts:196-198`；`removed` 仅在 remove 时出现，`piPackageService.ts:163-171`）。

```json
{
  "action": "install",
  "source": "npm:@acme/new-tools",
  "packages": [
    { "source": "npm:@acme/tools", "scope": "user", "filtered": false, "installedPath": "/home/alice/.pi/packages/@acme/tools" },
    { "source": "npm:@acme/new-tools", "scope": "user", "filtered": false, "installedPath": "/home/alice/.pi/packages/@acme/new-tools" }
  ]
}
```

- 错误：

| 状态 | 触发 | 响应体 |
|---|---|---|
| `400` | 请求体非对象 / `source` 缺失或空 / install 带 scope / remove scope 非法 | `{"error":"Pi package request body must be an object"}` 等（`piPackageRoutes.ts:51-80,82-88`） |
| `503` | active agent profile 不可用（`ActiveAgentProfileAccessError`） | `{"error":"Active agent profile is unavailable: <reason>"}`（`piPackageRoutes.ts:85-86`） |
| `500` | 安装器/文件系统等其它失败 | `{"error":"<message>"}`（`piPackageRoutes.ts:87`） |

- 机器代理形态：`POST /api/machines/:machineId/pi-packages/{install,remove,update}` 透传到远端，变更类超时 5 分钟（`shared/federatedRoutes.ts:17,47-49`）。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -X POST \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/pi-packages/install" \
  -d '{"source":"npm:@acme/new-tools"}'

curl -sS -u alice:"${USER_PASSWORD}" -X POST \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/pi-packages/remove" \
  -d '{"source":"npm:@acme/new-tools","scope":"user"}'

curl -sS -u alice:"${USER_PASSWORD}" -X POST \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/pi-packages/update" -d '{}'
```

---

### 7. 部署身份资产（deployment identity）

仅在客户端静态目录存在时才注册（`app.ts:270-281`）；未注册时这些路径落到 SPA 的 notFound 处理，返回 `index.html`（`app.ts:280`）。

#### 7.1 `GET /favicon.svg`、`/apple-touch-icon.png`、`/pwa-icon-192.png`、`/pwa-icon-512.png`

- 注册：遍历 `DEPLOYMENT_IDENTITY_ASSET_PATHS`（`deploymentIdentityRoutes.ts:19-24`；常量见 `deploymentIdentity.ts:43-50`）。
- 行为：`deploymentIdentityAssetForPath(pathname, flavor)` 为 `dev` 时返回 dev 文件名（`favicon-dev.svg`、`apple-touch-icon-dev.png`、
  `pwa-icon-dev-192.png`、`pwa-icon-dev-512.png`），否则用原文件名 `reply.sendFile(...)`（`deploymentIdentity.ts:43-66`）。
- flavor 判定：安装方式为 `docker` 且 `dockerMode==="dev"`，或 `local`（源码检出）→ `dev`，其余（`npm-global`、`pi-package`）→ `stable`；
  检测失败 fail-closed 到 `stable` 并只在进程内解析一次（`deploymentIdentity.ts:12-35`、`app.ts:272-278`）。
- 响应：静态文件本体，由 `@fastify/static` 决定 `content-type`。
- 错误：文件缺失时由 `sendFile` 抛错（Fastify 默认 500 形态）。<!-- TODO: 未找到对缺失品牌资产的显式兜底；sendFile 失败进入 Fastify 错误处理 -->

```bash
curl -sS -u alice:"${USER_PASSWORD}" -o favicon.svg \
  "${BASE}/alice/favicon.svg"
```

#### 7.2 `GET /manifest.webmanifest`

- 注册：`deploymentIdentityRoutes.ts:26-29`。
- 行为：读取客户端目录下的 `manifest.webmanifest`，`content-type` 固定 `application/manifest+json`（`deploymentIdentity.ts:53`、
  `deploymentIdentityRoutes.ts:28`）；`dev` flavor 下重写 `name`=`short_name`=`"PI WEB (dev)"`、`theme_color`=`"#21132f"`，
  图标 URL 不变（`deploymentIdentity.ts:55-57,77-87`）。
- 响应 `200`，示例（stable 即原文件；dev 为下例）：

```json
{
  "name": "PI WEB (dev)",
  "short_name": "PI WEB (dev)",
  "theme_color": "#21132f",
  "icons": [
    { "src": "/pwa-icon-192.png", "sizes": "192x192", "type": "image/png" },
    { "src": "/pwa-icon-512.png", "sizes": "512x512", "type": "image/png" }
  ]
}
```

<!-- TODO: 上例的 icons 等其余字段来自客户端静态清单文件，未在 server 源码中定义 -->

```bash
curl -sS -u alice:"${USER_PASSWORD}" "${BASE}/alice/manifest.webmanifest"
```

---

### 8. 通知（notices）

通知由 sessiond 拥有，**按 daemon 实例**维护（`daemonInstanceId` 为随机 UUID，`notices/serverNoticeStore.ts:43-50`）；
无去重、无历史，只有当前未消除集合（`:34-41`）。插件来源的通知有保留上限（`:108-125`）。

#### 8.1 `GET /api/notices`

- 注册：sessiond `GET /notices`（`notices/serverNoticeRoutes.ts:11`）；web 边缘 `app.all(`${prefix}/notices`)` 代理
  （`sessionProxyRoutes.ts:48`，local 别名 `app.ts:252`）。
- 无参数。响应 `200` + `ServerNoticeSnapshot`（`shared/apiTypes.ts:459-464`）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `daemonInstanceId` | `string` | 当前 sessiond 实例 id（`serverNoticeStore.ts:50`） |
| `revision` | `number` | 当前投影的单调版本（`serverNoticeStore.ts:86,104`） |
| `notices[]` | `ServerNotice[]` | 最新在前（`serverNoticeStore.ts:96`）：`{id,severity:"info"\|"warning"\|"error",message,createdAt,source?,scope?,context?}`（`shared/apiTypes.ts:446-456`）；`id` 形如 `<daemonInstanceId>:<uuid>`（`serverNoticeStore.ts:68`）；`scope` 为 `projectId`/`workspaceId`/`sessionId` 的组合选择器（`shared/apiTypes.ts:440-443`） |

```json
{
  "daemonInstanceId": "0f5a1d2e-6b1a-4c3d-9e77-2b8f6a1c0d42",
  "revision": 3,
  "notices": [
    {
      "id": "0f5a1d2e-6b1a-4c3d-9e77-2b8f6a1c0d42:7c2b...",
      "severity": "warning",
      "message": "Terminal service restarted after an unexpected exit",
      "createdAt": "2026-05-25T08:29:10.000Z",
      "source": "plugin:pi-web.terminal",
      "scope": { "projectId": "3f1c9b7e", "workspaceId": "8a2d0c11" },
      "context": { "terminalId": "t-1" }
    }
  ]
}
```

- 错误：sessiond 不可达 → `502 {"error":"Session daemon unavailable: ..."}`；关停中 → `503 {"error":"Session daemon is shutting down"}`。
- 实时等价物：`{"type":"notices.updated","snapshot":{...}}`，走 `/api/events`（`shared/apiTypes.ts:472-475`、`serverNoticeService.ts:37-40`）。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -H 'Accept: application/json' \
  "${BASE}/alice/api/notices"
```

#### 8.2 `POST /api/notices/dismiss`

- 注册：sessiond `POST /notices/dismiss`（`notices/serverNoticeRoutes.ts:13-24`）；web 边缘代理（`sessionProxyRoutes.ts:49`）。
- 请求体（`ServerNoticeDismissRequest`，`shared/apiTypes.ts:466-469`）：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `daemonInstanceId` | `string` | 是 | 必须与快照一致；不一致**不报错**，返回未变快照（`notices/serverNoticeStore.ts:100-106`） |
| `noticeId` | `string` | 是 | 要删除的通知 id |

- 成功响应：`200` + 消除后的 `ServerNoticeSnapshot`（`serverNoticeService.ts:31-35`）。
- 错误：`400 {"error":"request body must be an object"}` / `{"error":"daemonInstanceId field must not be empty"}` / `{"error":"noticeId field must not be empty"}`
  （`notices/serverNoticeRoutes.ts:27-39`）。

```bash
curl -sS -u alice:"${USER_PASSWORD}" -X POST \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/notices/dismiss" \
  -d '{"daemonInstanceId":"0f5a1d2e-6b1a-4c3d-9e77-2b8f6a1c0d42","noticeId":"0f5a1d2e-...:7c2b..."}'
```

#### 8.3 远程机器上的通知

`GET /api/machines/:machineId/notices`、`POST /api/machines/:machineId/notices/dismiss` 走通用机器代理（§4.5），
semantics 与本地一致，只是 `daemonInstanceId` 是那台机器的 sessiond 实例（`shared/federatedRoutes.ts:125-126`）。

---

### 9. 终端（terminals）

pi-web **没有**独立的终端 HTTP 路由：终端能力由内置必需插件 `pi-web.terminal`（`dist/pi-web-plugins/terminal/package.json:1-15`，
`machineSpecific: true`）通过 **paired 插件后端**提供，因此实际调用落在 §5.5 / §5.6 的两组路径上。

- 请求型操作（走 `POST /api/paired-plugin-backends/pi-web.terminal/projects/:projectId/workspaces/:workspaceId/:operation`）：
  `terminal.list`、`terminal.create`、`terminal.close`、`terminal.continue`、`terminal.run`、`terminal.list-runs`、
  `terminal.get-run`、`terminal.cancel-run`（`dist/pi-web-plugins/terminal/server-plugin.js:48-84`）。
- **WebSocket-only** 操作：`terminal.attach`，只能走
  `WS /api/paired-plugin-backends/pi-web.terminal/projects/:projectId/workspaces/:workspaceId/channels/terminal.attach`
  （`dist/pi-web-plugins/terminal/server-plugin.js:87-92`）。
- `terminal.attach` 的 payload（包在 §5.6 的 `kind:"data"` 信封里）：

| 方向 | payload | 来源 |
|---|---|---|
| 客户端 → 服务端 | `{"type":"input","data":"<按键数据>"}` | `server-plugin.js:142-147` |
| 客户端 → 服务端 | `{"type":"resize","cols":<n>,"rows":<n>}` | `server-plugin.js:148-153` |
| 服务端 → 客户端 | `{"type":"output","data":"<PTY 输出>","replay":false}`；回放帧为 `{"type":"output","data":"...","replay":true,"replayComplete":<bool>}` | `server-plugin.js:325-329` |
| 服务端 → 客户端 | `{"type":"exit","exitCode":<number?>}` | `server-plugin.js:122-124` |

- open 信封的 `input` 需含 `terminalId`，可选 `cols`/`rows`（`server-plugin.js:92-98`）。输出帧目标大小 60 KiB（`server-plugin.js:3,303-315`）。

```bash
# 创建终端（请求型）
curl -sS -u alice:"${USER_PASSWORD}" -X POST \
  -H 'Accept: application/json' -H 'Content-Type: application/json' \
  "${BASE}/alice/api/paired-plugin-backends/pi-web.terminal/projects/<projectId>/workspaces/<workspaceId>/terminal.create" \
  -d '{"revision":"<backendRevision>","input":{"name":"build","cols":120,"rows":30}}'
```

---

### 10. 仅供 sessiond 内部的路由

这些路由只在 sessiond 的 unix socket / `PI_WEB_SESSIOND_HOST:PI_WEB_SESSIOND_PORT` 上，
不在网关路径下；外部组件应使用 §1.1 中对应的 `/api/...` 代理形态。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/status` | §2.5 |
| GET | `/notices`、POST `/notices/dismiss` | §8 |
| GET | `/health` | `{ok:true, activeSessions:<n>, checkedAt, version:{component,label,runtimeVersion?,piVersion?,stale:false,available}}`（`sessiond.ts:378-390`） |
| GET | `/runtime` | `PiWebRuntimeComponent`，含 `activeAgentProfile:{schemaVersion:2,dir}`（`sessiond.ts:309-316,392`、`shared/apiTypes.ts:1198-1211`） |
| POST | `/plugin-backends/...`、`/paired-plugin-backends/...` | §5.5 的本地实现（`sessiond/pluginBackendRoutes.ts:47-54`） |
| WS | `/paired-plugin-backends/.../channels/:operation` | §5.6 的本地实现（`sessiond/pluginBackendChannelRoutes.ts:69-88`） |
| DELETE | `/workspace-removals/projects/:projectId/workspaces/:workspaceId` | 工作区删除，请求体 4 KiB，`precondition` 必填；项目不存在 404，删除失败经 `workspaceRemovalHttpStatus` 映射（`sessiond/workspaceRemovalRoutes.ts:36-76`） |

关停行为：`sessiond.ts:99-106` 的 `onRequest` 钩子会把关停期间的所有请求变成 `503 {"error":"Session daemon is shutting down"}`。

内部错误形态示例（HTTP JSON 端点）：

```json
{ "error": "Session daemon is shutting down" }
```

---

### 11. WebSocket-only 路由汇总与用法

#### 11.1 `WS /api/events` —— 全局实时 socket

- 注册：web 边缘 `app.get(`${prefix}/events`, {websocket:true})` 桥接到 sessiond `/events`（`sessionProxyRoutes.ts:43-45`）；
  sessiond 侧由 `registerSessionRoutes` 注册（`sessiond.ts:355`、`sessionRoutes.ts:509`）。
- 用途（本节相关部分）：机器状态与服务器通知的**全量推送**，无 delta：
  - 连接成功时会立刻收到一帧当前机器状态：`{"type":"machine.status","status":<MachineStatusSnapshot>}`（`sessiond.ts:256-258`、`shared/machineStatus.ts:50-53`）；
  - 状态变化时推送同样的 `machine.status` 帧（`sessiond.ts:253`）；
  - 通知变化时推送 `{"type":"notices.updated","snapshot":<ServerNoticeSnapshot>}`（`notices/serverNoticeService.ts:37-40`、`shared/apiTypes.ts:472-475`）。
- 会话相关帧（`session.ui.*`、通知收件箱等）见「会话」章节。
- 远端形态：`WS /api/machines/:machineId/events`（`shared/federatedRoutes.ts:185`）。

```bash
# websocat 示例（Basic 认证头）
websocat -H="Authorization: Basic $(printf 'alice:%s' "$USER_PASSWORD" | base64)" \
  "ws://<host>:58681/alice/api/events"
```

#### 11.2 `WS /api/paired-plugin-backends/.../channels/:operation` —— 插件后端长连接

协议、限额、关闭码见 §5.6；WebSocket-only 的终端 attach 用法见 §9。

#### 11.3 `WS /api/machines/:machineId/{events,sessions/events,sessions/:sessionId/events}` —— 远程机器 socket

- 由 `FEDERATED_WEBSOCKET_ROUTES` 生成（`shared/federatedRoutes.ts:183-188`），代理到远端同名路径。
- `machineId` 为 `local` 时以 close code `1011` 关闭（`machineProxyRoutes.ts:247-250`）；未知机器以 `1008` 关闭（`machineProxyRoutes.ts:252-256`）。

---

### 12. 备注 / TODO

- <!-- TODO: `PUT /api/config` 无法写入 `environmentFacts` 与 `extensionDialogsTimeoutMs`（`configRoutes.ts:123-168` 不解析这两个键），但它们在 `PiWebConfigValues` 中存在（`shared/apiTypes.ts:187-199`）；未确认是有意设计还是遗漏。 -->
- <!-- TODO: `/manifest.webmanifest` 与品牌资产的其余字段/内容来自构建出的客户端静态文件，未在 server 源码中定义。 -->
- <!-- TODO: 部署身份资产路由仅在 `clientDist` 存在时注册（`app.ts:270`）；`clientDist` 不存在时这 5 个路径由 SPA notFound 处理器返回 `index.html`，行为未在测试中固定。 -->
- `GET /api/pi-web/status|version|runtime` 的设计是「永不 5xx」：内部失败都编码进 payload（`piWebStatus.ts:117-126,342-382,404-436`），
  因此客户端应检查 `components.*.available`、`release.error`、`messages[]`，而不是只看状态码。
- `/api/machines/:machineId/*` 的 `501`（local 未注册）是**兜底**：本地等价路径应走 §1.1 中的非参数化路由（`/api/...` 或 `/api/machines/local/...` 静态路由）。

---

## 8. 附录

### 8.1 术语表

| 术语 | 含义 |
|---|---|
| **Actor** | Substrate 的逻辑工作负载单元。mf-pi 中**一个用户 = 一个 Actor**，运行 `pi-web`，数据彼此隔离 |
| **atespace** | Actor 的逻辑分组/命名空间。生产 `mfpi`，测试 `mfpi-test`；同时也决定 Agent 面的 Host 后缀（`<user>.<atespace>.actors.resources.substrate.ate.dev`） |
| **WorkerPool** | 物理 worker（Kubernetes Pod）池，`replicas` 即**同时活跃（RUNNING）用户的上限**；池满时 resume 返回 503 `no free workers` |
| **Actor 状态** | `STATUS_SUSPENDED`（不占 worker）⇄ `STATUS_RUNNING`（占 worker）；过渡态 `SUSPENDING`/`RESUMING`/`PAUSING`；异常 `PAUSED`/`CRASHED` |
| **sticky 持久卷（userdata）** | 挂在 Actor 上的 5Gi 卷，映射到节点目录 `<atespace>-<actor>-userdata`，挂载进沙箱 `/data/pi-agent`；`suspend/resume` 与「删+建」刷新后数据都在 |
| **golden base 快照** | 模板首次部署时生成的基准快照，用于冷启动恢复 |
| **就绪门（gate）** | nginx 的 `auth_request /_mfpi_gate`：鉴权 + 判断 Actor 是否 `RUNNING`，未就绪时返回 403 并后台触发 resume |
| **加载页（loading page）** | Actor 未就绪时给浏览器（`Accept: text/html`）的 HTML 过渡页；API 客户端收到原始 503 |
| **共享 Skill（managed skill）** | 管理员在 `mfpi-admin` 统一上传、由所有 Actor 的拉取循环同步到 `${PI_CODING_AGENT_DIR}/skills` 的 Skill 包 |
| **专属 Key** | 管理员为单个用户设置的 DeepSeek API Key，持久化在 Secret `mfpi-user-provider-keys`，注入 Agent 后优先于环境变量 |

### 8.2 端点总览

**管理面**（`${BASE}/usermanagement`，Basic Auth `admin`）：

```
GET    /api/users
POST   /api/users
DELETE /api/users/{name}
POST   /api/users/{name}/password
POST   /api/users/{name}/apikey
DELETE /api/users/{name}/apikey
POST   /api/users/{name}/expiry        (也接受 PUT)
DELETE /api/users/{name}/expiry
POST   /api/users/{name}/tier          (也接受 PUT)
GET    /api/skills
POST   /api/skills                     (multipart)
DELETE /api/skills/{name}
POST   /api/skills/apply
GET    /healthz
```

**用户 Agent 面**（`${BASE}/{username}`，该用户 Basic Auth；下同 `/{user}` 前缀由网关剥掉）：

```
# 状态 / 元信息
GET    /api/pi-web/status              (?refresh=1 强制刷新)
GET    /api/pi-web/version
GET    /api/pi-web/runtime
GET    /api/plugins
GET    /api/machines/local/plugins
GET    /pi-web-plugins/manifest.json
GET    /pi-web-plugins/{pluginId}/*

# 项目 / 目录建议
GET    /api/projects
POST   /api/projects
DELETE /api/projects/{projectId}
GET    /api/project-directories        (?q=)
GET    /api/projects/trust             (?path=)

# 工作区目录与信任
GET    /api/projects/{projectId}/workspaces
GET    /api/projects/{projectId}/workspaces/{workspaceId}
GET    /api/projects/{projectId}/workspaces/{workspaceId}/trust
PUT    /api/projects/{projectId}/workspaces/{workspaceId}/trust
DELETE /api/projects/{projectId}/workspaces/{workspaceId}

# 工作区文件浏览器
GET    /api/projects/{projectId}/workspaces/{workspaceId}/tree
GET    /api/projects/{projectId}/workspaces/{workspaceId}/file
PUT    /api/projects/{projectId}/workspaces/{workspaceId}/file
DELETE /api/projects/{projectId}/workspaces/{workspaceId}/file
POST   /api/projects/{projectId}/workspaces/{workspaceId}/file/move
GET    /api/projects/{projectId}/workspaces/{workspaceId}/file/preview
GET    /api/projects/{projectId}/workspaces/{workspaceId}/files

# 会话（经 session daemon 代理）
GET    /api/sessions                   (?cwd= 必填)
POST   /api/sessions
GET    /api/sessions/notifications
GET    /api/sessions/unread
POST   /api/sessions/cleanup/preview
POST   /api/sessions/cleanup
POST   /api/sessions/bulk/archive
POST   /api/sessions/bulk/delete-archived
GET    /api/sessions/{sessionId}/messages
GET    /api/sessions/{sessionId}/status
GET    /api/sessions/{sessionId}/stream-snapshot
GET    /api/sessions/{sessionId}/models
GET    /api/sessions/{sessionId}/models/catalog
POST   /api/sessions/{sessionId}/models/enabled
POST   /api/sessions/{sessionId}/models/scope
POST   /api/sessions/{sessionId}/model
POST   /api/sessions/{sessionId}/model/cycle
GET    /api/sessions/{sessionId}/defaults
POST   /api/sessions/{sessionId}/defaults
GET    /api/sessions/{sessionId}/thinking-levels
POST   /api/sessions/{sessionId}/thinking-level
POST   /api/sessions/{sessionId}/thinking-level/cycle
GET    /api/sessions/{sessionId}/commands
POST   /api/sessions/{sessionId}/prompt
POST   /api/sessions/{sessionId}/queue/clear
POST   /api/sessions/{sessionId}/ask/submit
POST   /api/sessions/{sessionId}/ask/cancel
POST   /api/sessions/{sessionId}/dialogs/answer
POST   /api/sessions/{sessionId}/dialogs/cancel
POST   /api/sessions/{sessionId}/warnings/dismiss
POST   /api/sessions/{sessionId}/attachments
POST   /api/sessions/{sessionId}/shell
POST   /api/sessions/{sessionId}/commands/run
POST   /api/sessions/{sessionId}/commands/respond
POST   /api/sessions/{sessionId}/tree/navigate
POST   /api/sessions/{sessionId}/tree/fork
POST   /api/sessions/{sessionId}/abort
POST   /api/sessions/{sessionId}/stop
POST   /api/sessions/{sessionId}/archive
POST   /api/sessions/{sessionId}/archive-tree
POST   /api/sessions/{sessionId}/restore
POST   /api/sessions/{sessionId}/reload
POST   /api/sessions/{sessionId}/detach-parent
POST   /api/sessions/{sessionId}/unread/acknowledge
GET    /api/sessions/{sessionId}/notifications
POST   /api/sessions/{sessionId}/notifications/dismiss
POST   /api/sessions/{sessionId}/notifications/dismiss-all

# 认证
GET    /api/auth/providers             (?mode=login|logout&authType=oauth|api_key)
POST   /api/auth/api-key/interactive
POST   /api/auth/logout
POST   /api/auth/oauth
GET    /api/auth/oauth/{flowId}
POST   /api/auth/oauth/{flowId}/respond
POST   /api/auth/oauth/{flowId}/cancel

# 机器 / 状态 / 通知 / 配置 / 包
GET    /api/machines
POST   /api/machines
GET    /api/machines/{machineId}
DELETE /api/machines/{machineId}
GET    /api/machines/{machineId}/health
GET    /api/machines/{machineId}/runtime
GET    /api/machines/{machineId}/pi-web-plugins/manifest.json
GET    /api/status
GET    /api/notices
POST   /api/notices/dismiss
GET    /api/config
PUT    /api/config
GET    /api/machines/local/config
PUT    /api/machines/local/config
GET    /api/pi-packages
POST   /api/pi-packages/install
POST   /api/pi-packages/remove
POST   /api/pi-packages/update

# WebSocket
WS     /api/sessions/events
WS     /api/sessions/{sessionId}/events
WS     /api/events
WS     /api/machines/local/sessions/{sessionId}/events
WS     /api/machines/local/sessions/events
WS     /api/machines/local/events
```

> 除 `/api/...` 外，上述 pi-web 业务路径同时挂载在 `/api/machines/local/...` 下（`app.ts:240-264`），两者等价；本文各章节以 `/api/...` 为准并逐条标注。
>
> 另有 **sessiond 内部协议**（`/workspace-catalog/*`、`/workspace-removals/*`、daemon 根路径的 `/health`、`/runtime`）不在 `/api` 下，也不经 mf-pi 网关暴露，仅作 pi-web 内部与调试用途，外部组件不应依赖。
>
> 多机器场景下，远端机器路由形如 `/api/machines/{machineId}/...`，可用路径受 `FEDERATED_HTTP_ROUTES` 白名单约束（`src/shared/federatedRoutes.ts`）。单机 mf-pi 部署中通常只使用 `local`。

### 8.3 相关源码与文档

| 内容 | 位置 |
|---|---|
| 网关路由与鉴权 | `demos/mf-pi/nginx.conf`（生产）、`nginx-test.conf`（测试） |
| 管理面实现 | `demos/mf-pi/admin/main.go`、`apikey.go`、`expiry.go`、`idle.go`、`tier.go`、`skills.go`、`gate.go`、`loading.go` |
| 管理面路由注册 | `demos/mf-pi/admin/main.go:1421-1440` |
| 管理面单测（含响应结构断言） | `demos/mf-pi/admin/main_test.go`、`skills_test.go` |
| 用户 Agent 服务（pi-web） | 独立仓库 `cliu-pi-web`：`src/server/app.ts`（路由注册）、`src/server/sessions/*`、`src/server/sessiond.ts` |
| 部署清单 | `demos/mf-pi/mf-pi.yaml.tmpl`、`mf-pi-test.yaml.tmpl` |
| 演示总览 | `demos/mf-pi/README.md`、`demos/mf-pi/docs/mfpi.md`、`demos/mf-pi/docs/actor_inside.md` |
| Skill 分发设计 | `demos/mf-pi/docs/deploy_skill_to_actor.md` |
| 专属 Key 设计 | `demos/mf-pi/docs/injectDeepsseekKey.md` |
| 每用户持久卷设计 | `demos/mf-pi/docs/save_userdata_pv.md`、`demos/mf-pi/docs/perUserDataVolume.md` |
| 资源档位 / 有效期设计 | `demos/mf-pi/docs/resource_quota_plan.md` |
| CLI 包装脚本 | `create-user.sh`、`delete-user.sh`、`list-users.sh`、`set-user-apikey.sh`、`clear-user-apikey.sh`、`install-skill.sh`、`list-skills.sh`、`remove-skill.sh`、`apply-skills.sh`（均含 `-test` 变体） |

### 8.4 安全与部署建议

1. **管理面必须置于 Basic Auth 之后**。`mfpi-admin` 应用自身不做鉴权；只有经 nginx `/usermanagement/` 才有鉴权。**不要**把 `svc/mfpi-admin:8080` 通过 NodePort/LoadBalancer/Ingress 裸露，也不要长期保留 `kubectl port-forward` 通道。
2. **默认管理凭据是公开的**（`admin` / `mf@pass2026`，见 `run-nginx.sh:88-89`）。生产部署务必用 `ADMIN_USER` / `ADMIN_PASSWORD` 覆盖，并考虑在网关前再加一层 TLS 与访问控制。
3. **仅集群内端点**（`/internal/skills/*`、`/_mfpi_auth`、`/_mfpi_gate`、`/_mfpi_loading`）无鉴权或依赖内部头，禁止直接对外暴露。
4. **凭据传输**：网关默认是明文 HTTP。跨不可信网络请自行在入口加 TLS 终结，或改用 VPN/内网。HTTP Basic 的凭据等同明文。
5. **`/api/machines/local/config` 等配置写接口**可能改变 Agent 行为，外部组件应遵循最小权限；不要把用户密码复用于其它系统。
6. **上传**：Skill 包与文件上传会落盘；对公网入口建议额外限制请求体大小与频率。

### 8.5 版本与兼容性

| 项 | 当前值 | 说明 |
|---|---|---|
| pi-web | `1.202609.0`（`package.json`） | 版本查询：`GET /api/pi-web/version` |
| 管理面 | 随本仓库 `demos/mf-pi` 演进 | 无独立版本号；以 `GET /healthz` 探活 |
| mf-pi 生产/测试 | atespace 分别为 `mfpi` / `mfpi-test` | 两套环境互不影响 |
| 网关端口 | 生产 `58681`，测试 `59681` | 内部 port-forward：生产 58680/58682，测试 59880/59882 |

外部组件应：

- 不要把 `template`、`status` 的枚举当稳定契约之外的东西使用；`status` 取值来自 Substrate 的 Actor 状态枚举。
- 对未在本文档列出的 pi-web 端点视为**非公开接口**（可能随版本变动）；本文档覆盖的是核心稳定面。
- 对 `{"error": "..."}` 结构做通用处理，未知 `code` 字段忽略即可。
