# mf-pi 用户 Agent 集成场景文档

> 面向**把 mf-pi 用户 Agent 集成进另一个应用**的开发者（第三方服务、独立后台、
> 自动化脚本、CI/CD 等）。本文只回答一个场景：**自动连接 mf-pi 的 API → 创建用户 →
> 打开该用户的 Agent 供用户直接操作**，给出端到端的调用顺序与最小可运行示例。

---

## 目录

- [1. 场景与目标](#1-场景与目标)
- [2. 需要理解的两个 API 平面](#2-需要理解的两个-api-平面)
- [3. 环境与地址前提](#3-环境与地址前提)
- [4. 端到端流程总览](#4-端到端流程总览)
- [5. Step 1 — 连接并探活](#5-step-1--连接并探活)
- [6. Step 2 — 创建（或复用）用户](#6-step-2--创建或复用用户)
- [7. Step 3 — 在应用中打开用户 Agent](#7-step-3--在应用中打开用户-agent)
- [8. Step 4 —（可选）以用户身份调用 Agent 业务 API](#8-step-4可选以用户身份调用-agent-业务-api)
- [9. 错误处理与重试策略](#9-错误处理与重试策略)
- [10. 示例代码](#10-示例代码)
- [11. 常见问题与边界](#11-常见问题与边界)

---

## 1. 场景与目标

外部应用需要为**每个终端用户**提供一个独立的 AI Agent 工作台。当用户在产品里点击
「打开助手」时，应用在自己的界面里**弹出一个窗口 / 新标签页**，地址指向该用户的
mf-pi Agent；**用户直接在弹窗的 UI 里操作**（建项目、开会话、发消息等），外部应用
负责把它「带起来」：

1. 连接到 mf-pi 的接入点（网关）。
2. 为某用户创建（或复用已有的）Agent 实例。
3. 在应用中弹出该用户的 Agent 窗口，用户在弹窗的 UI 里直接操作。
4. （可选）以该用户身份用 API 做自动化增强（如 CI、脚本）。

mf-pi 的模型是 **每个用户独立一个 Agent 实例**，数据互相隔离。外部应用**不需要处理
任何底层基础设施**——集成方的完整工作就是三个动作：
**探活 → 建用户（拿密码）→ 打开 Agent 弹窗**。

> 用户在弹窗里花的都是 **UI 操作**（浏览器加载 Agent 的 Web 界面），因此
> **「打开」是浏览器导航，不是 API 调用**。外部应用的核心职责是准备好「用户 + 密码 +
> 弹窗地址」，把窗口弹出来，剩下的交互全部发生在弹窗内。

---

## 2. 需要理解的两个 API 平面

mf-pi 经同一个**网关**暴露两套独立的 HTTP 服务，靠路径前缀区分：

| 平面 | 服务 | 干什么 | 路径前缀 | 鉴权 |
|---|---|---|---|---|
| **平台管理面** | `mfpi-admin` | 用户的增删查、访问密码、每用户 DeepSeek Key、有效期、资源档位、Skill 分发 | `/usermanagement/` | HTTP Basic，**固定管理员账号**（默认 `admin` / `mf@pass2026`） |
| **用户 Agent 面** | `pi-web`（每用户一个实例） | 提供给这个用户操作的 Agent **Web UI** 及其 API | `/<username>/` | HTTP Basic，**用户名必须等于 URL 中的 `<username>`**，密码是**该用户**的一次性密码 |

衔接两者的关键是**一次性访问密码**：它由管理面在创建/重置用户时生成并**只返回一次**，
之后无论是弹窗里的浏览器、还是外部的 API 调用，都用「用户名 + 这个密码」去访问
用户 Agent 面。这是最容易搞混的地方。

### 用浏览器进入管理页面

除了用 API 做自动化，集成方也常常需要**在浏览器里打开管理页面**做人工操作
（查看用户状态、手动建用户、重置密码、配置 DeepSeek Key 等）。管理页面就是
管理面的 Web UI，地址：

```
${GATEWAY}/usermanagement/
```

浏览器访问后，会弹出一个 **HTTP Basic 登录框**，输入**管理员账号密码**（默认
`admin` / `mf@pass2026`）即进入嵌入式管理界面，可在里面完成上面提到的全部管理操作。

> 它和用户 Agent 用的鉴权不同：管理页面用的是**管理员账号**；而用户 Agent 弹窗用的是
> **该用户的用户名 + 一次性密码**。两者入口不同（`/usermanagement/` vs `/${USER}/`），
> 凭据也不同，不要混用。

---

## 3. 环境与地址前提

| 项 | 生产（默认） | 测试 |
|---|---|---|
| 网关地址 | `http://<host>:58681` | `http://<host>:59681` |

- `<host>` 是网关所在机器的地址。
- 管理面 Base：`${GATEWAY}/usermanagement`（供后端脚本/服务调用）
- 管理面 UI：`${GATEWAY}/usermanagement/`（**浏览器**打开，见 §2）
- 用户 Agent 入口：`${GATEWAY}/${USER}/`（浏览器弹窗导航到这里）

下文统一用占位符：

```bash
GATEWAY=http://<host>:58681                 # 测试环境改成 http://<host>:59681
ADMIN_USER=admin                            # 管理面 Basic Auth 账号（部署时确定）
ADMIN_PASSWORD='mf@pass2026'                # 管理面 Basic Auth 密码（部署时确定）
NEWUSER=alice                               # 要创建的用户名（== 网关 URL 段的用户名）
USER_PASSWORD='<创建/重置时返回的一次性密码>'
```

由于弹窗里的浏览器无法替你带着后端的密码，**集成方需要自己决定如何把「用户 + 密码」
传给浏览器**。常见两种做法（见 §7）：在弹窗 URL 里内嵌凭据；或由后端先以该用户身份
访问并让浏览器保存凭据/会话。具体取决于你的前端实现。

---

## 4. 端到端流程总览

```
① 探活                GET /usermanagement/healthz            → 200 {"status":"ok",...}
② 创建/复用用户        POST /usermanagement/api/users {"name": USER}
                      → 200 { "user": {...}, "password": "一次性密码" }   ☆密码只返回一次
③ 打开 Agent 弹窗      前端 window.open( `${GATEWAY}/${USER}/` )，浏览器带该用户凭据导航
                      → 未就绪时浏览器自动显示加载页并等待；就绪后进入 Agent Web UI
                      → 用户直接在弹窗里操作
④ (可选) 编程调用      以 ${USER}:${USER_PASSWORD} 调 /${USER}/api/...（自动化/后端增强）
```

> ② 里管理面创建用户时默认会**让它立即可用**，因此新用户弹窗通常无需长等；
> 若创建时没有空闲资源，弹窗首次打开时会先见「Agent 正在载入」的加载页，就绪后
> 自动进入界面。

---

## 5. Step 1 — 连接并探活

确认网关与 `mfpi-admin` 可达、atespace 正确：

```bash
curl -sS -u "$ADMIN_USER:$ADMIN_PASSWORD" \
  "${GATEWAY}/usermanagement/healthz"
```

成功响应 `200`：

```json
{ "status": "ok", "atespace": "mfpi" }
```

- `atespace` 字段用于确认连接的是生产（`mfpi`）还是测试（`mfpi-test`）。
- 连接失败 / 非 `200` → 主机地址或端口不对、网关未启动、或管理面部署异常，
  先解决接入问题再继续。

---

## 6. Step 2 — 创建（或复用）用户

**端点**：`POST /usermanagement/api/users`（经网关前缀 `/usermanagement`）

**请求体**：仅需 `name`，必须匹配 DNS-1123：

```json
{ "name": "alice" }
```

**命令**：

```bash
curl -sS -u "$ADMIN_USER:$ADMIN_PASSWORD" \
  -X POST "${GATEWAY}/usermanagement/api/users" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"$NEWUSER\"}"
```

**成功响应 `200`（新建成功）**：

```json
{
  "message": "创建成功",
  "user": {
    "name": "alice",
    "template": "ate-demo-mf-pi/mf-pi-small",
    "status": "STATUS_RUNNING",
    "tier": "small"
  },
  "password": "k7Qm2f..."
}
```

要点：

- **`password` 只在此响应中一次性返回**，请立即安全保存。它是「用户 + 密码 + 弹窗地址」
  链条里唯一的动态凭据；丢失后只能重置（见下）。
- **幂等**：同 `name` 再次调用不会重置历史数据，而是**复用**现有会话并返回：
  `{"message":"用户已存在，复用现有会话","name":"alice"}`（此时**没有** `password`）。
  因此：若你本地没有保存该用户的密码，不能靠重复「创建」拿回旧密码，必须走重置。
- 创建成功但未能立即可用（如没有空闲资源）会返回：
  `{"message":"创建成功，但立即恢复失败（可稍后打开页面触发恢复）：...","user":{...},"password":"..."}`，
  用户仍已创建，弹窗首次打开时会触发启动。

**若密码丢失**：`POST /usermanagement/api/users/<name>/password` 重置并返回新的一次
性密码（旧密码立即失效）：

```bash
curl -sS -u "$ADMIN_USER:$ADMIN_PASSWORD" \
  -X POST "${GATEWAY}/usermanagement/api/users/$NEWUSER/password"
# → 200 {"message":"密码已重置","name":"alice","password":"Zq8x1..."}
```

> 也可用 `GET /usermanagement/api/users` 查询某用户是否存在及其 `status`。

---

## 7. Step 3 — 在应用中打开用户 Agent

**「打开」= 在应用中弹出指向该用户 Agent 的窗口 / 新标签页，让用户在界面里操作。
它不是 API 调用。**

**弹窗地址**：`${GATEWAY}/${USER}/`

浏览器导航到该地址后：若 Agent 已就绪，加载 pi-web 的 Web 界面，用户直接使用；
若尚未就绪，网关会返回一个**自动刷新的「Agent 正在载入」加载页**，就绪后进入界面。
整个过程都是浏览器行为，外部应用**不需要**轮询或解析状态。

**唯一要做对的是鉴权**。弹窗里的浏览器访问 `${GATEWAY}/${USER}/` 时，网关要求
HTTP Basic 认证，其中：**用户名必须等于 URL 中的 `<username>`（即 `alice`），密码是
该用户的一次性密码**。你需要让弹窗的浏览器带着这份凭据访问，常见两种做法：

1. **URL 内嵌凭据**（简单，适合弹窗/新标签页，见下方示例）——把用户名和密码拼进
   主机名部分，浏览器会自动把它转成 `Authorization: Basic ...` 头发出去。
2. **后端预认证后交给浏览器**（适合不想在 URL 里出现明文密码的部署）——由你的后端
   先以该用户身份完成访问并在浏览器侧种下会话凭据，再让弹窗命中同一域名。

> 凭据的注入方式取决于你的前端架构，本文不做强约束；只要最终让浏览器带着正确的
> Basic 凭据访问弹窗地址即可。

**参考：前端弹窗片段（做法 1）**

```js
// 打开用户 Agent 弹窗（URL 内嵌凭据）
const agentUsername = "alice";              // 由 Step 2 决定
const agentPassword = "k7Qm2f...";          // Step 2 返回的一次性密码，后端传给前端
const url = `http://${encodeURIComponent(agentUsername)}:${encodeURIComponent(agentPassword)}` +
  `@${HOST}:58681/${agentUsername}/`;
window.open(url, "_blank", "width=1200,height=800");
```

> 若你的浏览器/安全策略禁止 URL 内嵌凭据（部分浏览器会拦截 `user:pass@` 形式），
> 请改用做法 2，由后端完成预认证。

**为什么「打开」不做成 API 探测**：由于用户在弹窗里走的是浏览器 UI（`Accept:
text/html`），未就绪时浏览器拿到的是加载页（200），它会自刷新，**不需要**你写轮询。
只有当你**用程序**去访问该用户 Agent 的接口时才需要按 §8/§9 处理 503——那属于
可选的后端自动化，不是「打开」本身。

---

## 8. Step 4 —（可选）以用户身份调用 Agent 业务 API

用户在弹窗里操作时，外部应用通常**不需要**直接调用这些接口。仅在你需要**编程式
增强**（CI、脚本、后台批量任务）时才需要。此时所有能力都以 `/${USER}/api/...`
前缀暴露，统一用 `${USER}:${USER_PASSWORD}` 做 Basic Auth：

| 用途 | 请求 | 说明 |
|---|---|---|
| 版本 | `GET /api/pi-web/version` | 无需 `cwd`，可作就绪探针 |
| 项目列表 | `GET /api/projects` | 需要 `Accept: application/json` |
| 会话列表 | `GET /api/sessions` | — |
| 创建会话 | `POST /api/sessions` | 需 `cwd` 指向项目根绝对路径 |
| 发消息 | `POST /api/sessions/:sessionId/prompt` | — |

```bash
curl -sS -u "$NEWUSER:$USER_PASSWORD" \
  -H 'Accept: application/json' \
  "${GATEWAY}/$NEWUSER/api/projects"
```

> 说明：`/api/...` 路径同时等价挂在 `/api/machines/local/...` 下，行为一致，通常用
> `/api/...`。调用这些 API 时必须设置 `Accept: application/json`，以拿到真实的 `503`
> 而非 HTML 加载页（见 §9）。

---

## 9. 错误处理与重试策略

**弹窗场景（浏览器 UI）**：用户在弹窗里看到的「未就绪」会被网关渲染成自动刷新的
加载页，**通常不需要外部应用处理**。外部应用要处理的，主要是后端调用管理面 API
和（可选）编程式调用 Agent API 时的错误。

| HTTP 码 | 含义 | 处理建议 |
|---|---|---|
| `200` | 成功 | —— |
| `400` | 参数非法（用户名非 DNS-1123、缺 `cwd` 等） | 修正参数，**不重试** |
| `401` | 鉴权失败（管理面密码错；Agent 面用户名 ≠ URL / 密码错 / 未分配密码） | **不重试**，检查/重置凭据 |
| `403` | 已认证但用户不可用（经非 HTML `Accept` 已变成 `503`） | 按 503 处理 |
| `404` | 用户 / 资源不存在 | 确认用户是否已创建（管理面 `GET /api/users`） |
| `500` | 管理面内部错误 | 稍后重试，必要时查 admin 日志 |
| `502` | 管理面下游失败 / 上游 pi-web 不可达 | 稍后重试 |
| `503` | **无空闲资源** 或 Web 服务启动中 | **指数退避重试**（见下） |
| `504` | 上游超时 | 稍后重试 |

**未就绪（503）退避**（仅编程式调用时需要）：首次访问触发 Agent 启动、或创建时无
空闲资源时，常见 `503`。推荐：

```bash
for i in 1 2 3 4 5 6; do
  code=$(curl -sS -o /tmp/out.json -w '%{http_code}' \
    -u "$NEWUSER:$USER_PASSWORD" -H 'Accept: application/json' \
    "${GATEWAY}/$NEWUSER/api/projects")
  [ "$code" = "200" ] && break
  sleep $((2**i))     # 2s, 4s, 8s, ...
done
```

要点：

- 总预算建议 60–120s；期间**不要并发发起大量请求**。
- `401` **永远不重试**——修正或重置凭据（`POST /usermanagement/api/users/<name>/password`）。
- 幂等写操作（创建用户、清除 Key/有效期、删除 Skill）可安全重试；`POST /api/users`
  重复调用不会重置已有用户数据。

---

## 10. 示例代码

**后端：探活 + 建用户（拿到一次性密码）**，这段跑在你的服务端：

```bash
#!/usr/bin/env bash
set -euo pipefail
GATEWAY=${GATEWAY:-http://<host>:58681}
ADMIN_USER=${ADMIN_USER:-admin}
ADMIN_PASSWORD=${ADMIN_PASSWORD:-'mf@pass2026'}
NEWUSER=${NEWUSER:-alice}
AUTH="${ADMIN_USER}:${ADMIN_PASSWORD}"

# ① 探活
curl -sS -u "$AUTH" "${GATEWAY}/usermanagement/healthz" >/dev/null

# ② 创建/复用用户，取出一次性密码
resp=$(curl -sS -u "$AUTH" -X POST "${GATEWAY}/usermanagement/api/users" \
  -H 'Content-Type: application/json' -d "{\"name\":\"$NEWUSER\"}")
pw=$(printf '%s' "$resp" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("password",""))')
echo "user=$NEWUSER password=$pw"
# 把 pw 交给前端用于拼弹窗地址；务必安全传输与保存
```

**前端：弹出用户 Agent 窗口**（做法 1，URL 内嵌凭据）：

```js
const url = `http://${encodeURIComponent(user)}:${encodeURIComponent(pw)}` +
  `@${HOST}:58681/${user}/`;
window.open(url, "agent", "width=1200,height=800");
```

思路（适用任何语言）：

```text
health = POST  {GATEWAY}/usermanagement/api/users      # Basic(admin, pw)
                 body {"name": <user>}
                 → password = user.password            # 只返回一次，务必保存
open   = 前端   window.open(GATEWAY + "/" + user + "/"，带该用户 Basic 凭据)
                 # 浏览器进入 Agent Web UI，用户直接操作
use    = (可选) GET {GATEWAY}/<user>/api/projects      # Basic(user, password)
                 # 仅编程式调用时需要
```

如果密码已丢失，先：

```text
POST {GATEWAY}/usermanagement/api/users/<user>/password  # 重置，拿新 password
```

---

## 11. 常见问题与边界

| 现象 | 原因与处理 |
|---|---|
| 弹窗一直停在「Agent 正在载入」 | Agent 尚未就绪，页面自刷新等待；若长期卡住，用管理面 `GET /api/users` 查该用户 `status`，必要时申请扩容 |
| 弹窗弹出浏览器登录框 / 反复 401 | 没带对凭据：Basic 用户名必须 == URL 段的用户名，密码必须是该用户密码；确认内嵌凭据或预认证方式正确，否则重置密码 |
| 浏览器拦截 `user:pass@` 形式的 URL | 部分浏览器安全策略禁止 URL 内嵌凭据；改用「后端预认证 + 会话」方式 |
| 一直 `401` | Agent 面 Basic 用户名≠URL 段、密码错、或该用户未分配过密码；管理面重置密码 |
| 一直 `503` 超时（编程式） | 并发活跃用户超出可容纳数量，或服务仍在启动；用管理面 `GET /api/users` 查该用户 `status`，必要时申请扩容 |
| 想清掉一个用户 | `DELETE /usermanagement/api/users/<name>`（删除时**同时清除其数据**，不可恢复） |
| 想给某用户专属模型 Key | `POST /usermanagement/api/users/<name>/apikey`（`{"apiKey":"sk-..."}`） |
| 保留名 | `usermanagement`、`_mfpi_auth` 等系统保留前缀**不能**用作 `name` |
