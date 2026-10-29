# mf-pi 58681 环境 Agent 压测方案

本文给出对 **58681 生产网关**（`mfpi` atespace / `ate-demo-mf-pi` namespace）下
Agent 进行压力测试的完整方案：测什么、怎么造负载、看哪些指标、按什么顺序执行、
有哪些风险。

> 结论先行：**58681 现在是真人生产环境**（25 个 RUNNING 用户，跑在同一台 24 核
> kind 单节点上，集群 CPU requests 已占 87%、Pod 槽位 107/110），直接对它做高并发
> 压测会先打满节点、把真实用户挤成 503。建议：
>
> 1. 先用 **59681 测试环境**跑通负载、校准工具；
> 2. 生产上只做**受控阶梯**（从 10 并发起、上限不超过空闲 worker 数），
>    全程盯真实用户 5xx，随时可停；
> 3. **不要**在生产上做 suspend/resume 压测 —— 当前快照恢复本身已知不可靠
>    （见 README「空闲自动挂起」警告），会把 actor 挂死。

---

## 1. 目标与判定标准

### 1.1 要回答的问题

1. **容量**：58681 当前配置下，最多能同时服务多少个「活跃浏览器 + 会话」而不劣化？
2. **瓶颈排序**：上限首先卡在哪里 —— nginx / admin 就绪门 / kubectl port-forward /
   atenet-router / worker（gVisor + 1 CPU）/ pi-web / LLM provider？
3. **失效模式**：超载时是慢、是 5xx、是 503「no free workers」、还是 actor 崩溃？
   恢复是否自动、需要多久？
4. **成本敏感点**：每用户每分钟请求数、每轮 agent turn 的耗时与 token 消耗。

### 1.2 通过标准（初版，可按业务调整）

| 维度 | 指标 | 目标 |
|---|---|---|
| 用户体验 | 首页/静态资源 p95 | < 1 s（局域网/同城） |
| 用户体验 | 只读 API（status/unread/notices）p99 | < 2 s |
| 可靠性 | 总错误率（非 401） | < 0.5% |
| 可靠性 | 真实用户 5xx | 0（压测期间不允许误伤） |
| Agent | 一轮 prompt 端到端 p95（mock LLM） | 按模型基线设定，抖动 < 20% |
| 平台 | `atenet_router_route_duration_seconds` p99 | < 500 ms（不含 LLM 时间） |
| 平台 | worker CPU throttling | 无持续 > 5% 的 throttled 周期 |
| 余量 | 压测峰值时节点 CPU | 留 ≥ 20% 空闲 |

---

## 2. 环境现状（2026-10-28 实测）

### 2.1 拓扑与关键路径

```
浏览器/压测端
  └─ :58681  mfpi-nginx（host 网络，单容器）
       ├─ auth_request → mfpi-admin:58682/_mfpi_gate
       │    ├─ 鉴权：cookie HMAC（便宜）或 Basic + PBKDF2-100k（很贵）
       │    └─ **每个请求**一次 gRPC GetActor → ate-api-server
       └─ proxy_pass → 127.0.0.1:58680（kubectl port-forward，单进程）
            └─ atenet-router（1 副本，ext_proc）
                 └─ worker Pod（gVisor，requests 250m / limits 1 CPU、1 Gi）
                      └─ pi-web web + sessiond（同容器双进程）
                           └─ LLM provider（默认 DeepSeek，外部网络）
```

几个对压测特别重要的事实：

- **就绪门是每请求同步 RPC**：`admin/gate.go` 对每个 `/<user>/...` 请求都做一次
  `GetActor`（5 s 超时，无缓存）。压测时这会先于业务先把 ate-api-server / valkey
  打成热点。
- **Basic 与 Cookie 的成本差一个数量级**：`admin/main.go` 用 PBKDF2-SHA256
  100 000 轮校验 Basic 密码；浏览器首次登录后改用 `mfpi_auth` cookie（HMAC 校验）。
  压测端如果每个请求都带 Basic，会在 admin 上烧掉大量 CPU，并得出错误结论。
  **必须分别测「纯 Basic」和「Cookie」两种模式。**
- **port-forward 是单进程**：58680 由一个 `kubectl port-forward` 进程承担，每个
  连接一条 SPDY/WebSocket 流。这是最可疑的吞吐瓶颈之一，需要用
  「绕过 nginx/PF 直连 router」的对照组来量化。
- **一个活跃用户 = 一个 worker**：WorkerPool replicas 就是最大同时活跃用户数。

### 2.2 容量与余量

| 项 | 值 | 说明 |
|---|---|---|
| 节点 | 1 × kind（24 核 / 755 Gi） | 与大量其它业务共享宿主机，**噪声很大** |
| 节点 Pod 槽位 | 107 / 110 非终止 | 只剩 ~3 个槽位 |
| 节点 CPU requests | 21.05 / 24 核（87%） | 其中 prod 15 核、test 5 核 |
| 生产 small / mid / large | 48 / 1 / 1 | 25 个用户全部 small 档 |
| 生产空闲 worker | **≈ 23** | 48 − 25；不扩容时并发用户上限就是 48 |
| 测试 small / mid / large | 8 / 1 / 1 | 入口 59681，独立 atespace/namespace |
| Prometheus | 仅抓 ate-system 组件 | 无 metrics-server；Pod 指标需读 cadvisor 原始端点 |

> 要测 >48 并发必须扩 `mf-pi-wp-small`；但节点只剩 ~3 个 Pod 槽位和 ~3 核
> requests。可选做法：回收 mf-cc 的 16 个空闲 worker Pod（0 requests）、临时缩小
> 测试环境池、或先删掉 10 个 UUID 命名的疑似遗留 actor（见 2.3）。
> 临时扩容**不要**改部署变量后重跑 `deploy.sh`（会重建镜像/滚动整个环境），
> 直接 `kubectl scale deploy`/patch WorkerPool，压测后改回。

### 2.3 现有用户

- `GET /usermanagement/api/users`：**25 个用户，全部 `STATUS_RUNNING`、全部 small**。
- 其中 10 个是 UUID 形名字（`15317133-…` 等），创建于约 4.5 小时前，疑似上一轮
  批量创建/测试的遗留；另有 15 个是真人用户名（alice、tom、zhangyy…）。
- 每个用户都已配置专属 provider key（Secret `mfpi-user-provider-keys`，DeepSeek）。
- **拿不到现有用户的明文密码**（ConfigMap 里只有 PBKDF2 哈希），所以压测**不要**
  复用现有用户；重置他们的密码会直接把人踢下线。

### 2.4 真实流量基线（来自 nginx access log）

`docker logs mfpi-nginx` 9 小时约 37 300 条请求，可用来校准流量模型：

| 指标 | 实测 |
|---|---|
| 平均速率 | 66 req/min（≈1.1 rps） |
| 峰值 | 618 req/min（≈10 rps），出现在 10:35–10:48 |
| 状态码 | 200: 66%，401: 17%，101(WS): 9%，404: 7%，5xx: 0.3% |
| 请求构成 | API 68%，WebSocket 24%，静态/插件 7% |
| Top 端点 | `.../api/machines/local/events`（WS，6768）、`.../api/users`（2319）、
`sessions/unread`（2084）、`notices`（1643）、会话级 `messages/status/stream-snapshot/notifications` |

由此可估：**一个活跃浏览器 ≈ 0.4–0.6 rps + 1 条常驻 WS**，其中大部分是 3–10 s
一次的轮询。真实部署里还有两个现成问题值得在压测中复现：某个客户端持续对
`/admin/api/machines/local/events` 发 401（占 17% 的 401），以及 pi-web
`/{u}/api/users` 偶发 500（33 次）。

---

## 3. 指标与采集

### 3.1 压测端（主）

每个场景输出：按端点类别分的 QPS、p50/p95/p99、错误码分布、WS 建连时间与断连率、
每用户请求数；输出 JSON + 控制台摘要，并暴露 `/metrics` 供 Prometheus 抓取。

### 3.2 平台侧

| 指标 | 来源 | PromQL / 采集方式 |
|---|---|---|
| router 路由耗时 | Prometheus | `histogram_quantile(0.99, sum by (le,outcome) (rate(atenet_router_route_duration_seconds_bucket[1m])))` |
| router QPS / 失败率 | Prometheus | `sum by (outcome) (rate(atenet_router_route_duration_seconds_count[1m]))` |
| worker 占用 | Prometheus | `ate_workerpool_workers{ate_workerpool_name="mf-pi-wp-small"}`（按 `ate_worker_state`） |
| 组件 CPU | Prometheus | `sum by (app) (rate(process_cpu_seconds_total[1m]))` |
| worker CPU/内存/throttle | kubelet cadvisor | `kubectl get --raw /api/v1/nodes/kind-control-plane/proxy/metrics/cadvisor`，过滤 `mf-pi-wp-small-*`，取 `container_cpu_usage_seconds_total`、`container_cpu_cfs_throttled_periods_total`、`container_memory_working_set_bytes` |
| 节点总 CPU | 宿主 | `docker stats kind-control-plane` / `/proc/stat` 采样 |
| 网关/鉴权 | nginx log + admin | 目前 admin 无 `/metrics`；见 §8 可选改造 |

> 注意：Prometheus 只抓 `atelet` / `atenet-router` / `ate-api-server`，**没有**
> cAdvisor 和 nginx。压测前建议给 `benchmarking/monitoring.yaml` 或一个独立
> DaemonSet 补上 cAdvisor 抓取；否则就在压测脚本里每 5 s 拉一次 cadvisor 原始
> 端点存 CSV（够用，且不改集群）。

---

## 4. 负载模型

用**闭合模型**（每个虚拟用户固定 think time）为主，因为 agent 负载的真实形态是
「短请求 + 长空闲 + 长连接」；再补一个**开环 RPS** 模式用来顶网关的天花板。

### Profile A：`idle-browser`（只读，最接近真实在线用户）

每个 VU 循环：

1. 首次 `GET /<u>/`（Basic → 拿 cookie；之后只用 cookie）
2. 建立 WS `/<u>/api/machines/local/events`，保持
3. 每 3–10 s 轮询：`api/machines/local/status`、`api/users`、
   `api/machines/local/sessions/unread`、`api/machines/local/notices`、`api/config`
4. 每 30–60 s 打开一次会话：`sessions?cwd=` → `status` → `messages` →
   `stream-snapshot` → 会话 WS
5. think time 抖动 ±30%

### Profile B：`agent-turn`（真实 Agent 工作）

在 A 的基础上，每 60–120 s 一轮：

1. `POST /<u>/api/sessions`（cwd 用 `/data/work/stress`）
2. `POST /<u>/api/sessions/<id>/model` 选 mock provider
3. `POST /<u>/api/sessions/<id>/prompt`（短提示）
4. 通过会话 WS 或轮询 `status` 等到 `isStreaming=false`
5. 记录一轮耗时；可选执行 tool-call（写文件/`ls`）压 actor CPU

### 到达曲线

`ramp` 模式：`0 → N` 用 60 s，`hold` 10–15 min；`spike` 模式：10 s 内 `0 → N`；
可选周期性 WS 重连（模拟浏览器断网/休眠）。

---

## 5. 场景清单

| # | 场景 | 配置 | 目的 |
|---|---|---|---|
| S0 | 空载基线 | 无流量 10 min | 记录平台背景噪声（尤其宿主上其它业务） |
| S1 | 单用户冒烟 | 1 VU × 5 min | 验证鉴权→cookie→WS→会话→prompt 全链路 |
| S2 | 网关只读阶梯 | Profile A，10/25/50/100/200 VU，各 10 min | 找网关/控制面容量拐点 |
| S3 | Agent turn 并发 | Profile B + mock LLM，25/48 VU | 找 actor/pi-web 容量拐点 |
| S4 | 突发与重连风暴 | 0→50 VU/10 s；全部 WS 同时断开重连 | 验证超时、重试、限流行为 |
| S5 | 鉴权模式对比 | 同负载：全 Basic vs 全 Cookie vs 混合 | 量化 PBKDF2 的 CPU 代价 |
| S6 | worker 耗尽 | 并发 > 空闲 worker | 验证 503 `no free workers` 与恢复 |
| S7 | Soak | 70% 容量，60–120 min | 内存/FD 泄漏、actor 重启、事件循环退化 |
| S8 | 真实 LLM 小样本 | 5 VU × 10 轮，DeepSeek | 校准 mock 与真实 provider 的差异（控成本） |
| S9 | 冷启动/恢复（**仅诊断**） | 挂起→并发唤醒 | 已知不可靠，只记录现象，不作为判定 |

S9 说明：README 明确记录「Data 快照挂起失败、Full 快照恢复报
`inconsistent private memory files on restore`」，当前唯一可靠路径是
`delete + create + resume --boot`。**不要**用 S9 去动真实用户；如需观察，只在
专用 stress 用户上做。

---

## 6. 工具方案

### 6.1 主工具：`mfpi-loadgen`（已实现）

实现位于 `tools/mfpi-loadgen/`（属主模块，`go build ./tools/mfpi-loadgen`）。
一个二进制同时充当压测端和 mock LLM，子命令：

```bash
# 建挂起 actor + 重置密码 → creds.json（不占 worker）
mfpi-loadgen users -admin-url http://127.0.0.1:59681 \
  -atespace mfpi-test -template ate-demo-mf-pi-test/mf-pi-small \
  -count 16 -creds creds-test.json

# 跑场景（idle-browser | agent-turn | admin）
mfpi-loadgen run -base-url http://127.0.0.1:59681 -creds creds-test.json \
  -scenario idle-browser -vus 16 -ramp 60s -duration 10m \
  -think 3s-10s -auth cookie -metrics-listen 127.0.0.1:9091 \
  -report idle-16.json

# agent turn（配合 §6.3 的 mock）
mfpi-loadgen run ... -scenario agent-turn -provider mock -model mock-1 \
  -turn-interval 60s -turn-timeout 120s

# 清理（admin delete 同时 purge 持久卷）
mfpi-loadgen cleanup -admin-url http://127.0.0.1:59681 -creds creds-test.json

# 分析真实流量，校准模型
docker logs mfpi-nginx --since 24h 2>&1 | mfpi-loadgen analyze-log -top 10
```

为什么不用现成工具：

- pi-web 的会话/事件是「HTTP + WebSocket + 轮询状态」的组合，现成 HTTP
  压测器（vegeta/ab）覆盖不了；
- 需要「Basic 换 cookie」、按用户路由、per-VU 状态机；
- 需要把网关、actor、LLM 三段耗时拆开统计。

实现要点（都已落地）：

- 每 VU 一个 `http.Client` + cookie jar；首次 Basic，之后复用 `mfpi_auth`；
  收到 401 自动退回 Basic。`-auth basic|cookie|mixed` 可对比 PBKDF2 的代价。
- **就绪探测走 API 而不是浏览器导航**：导航在未就绪时会被网关改写成 200 的
  提示页（`docs/user_agent_api.md` §3.3），用 `/api/machines/local/status` 才能
  拿到真实 503；`bootstrap` 的耗时单独统计（含冷启动）。
- WS 用 `github.com/gorilla/websocket`（已从 indirect 提为直接依赖）。
- 503 按退避重试到 `-ready-timeout`；统计里把 `bootstrap` 与正常请求分开。
- 退出（SIGINT / 到时）后写 JSON 报告并打印分端点 p50/p95/p99、错误码、
  WS 建连/重连、turn 成败。

详细用法见 `tools/mfpi-loadgen/README.md`。

### 6.2 对照路径（定位瓶颈，强烈建议）

同样的负载分别打三个入口，差值就是每层的开销：

| 路径 | 入口 | 去掉的层 |
|---|---|---|
| A | `http://HOST:58681/<u>/...` | 无（真实用户路径） |
| B | `http://atenet-router.ate-system.svc:80/...` + `Host: <u>.mfpi.actors…`（集群内 Job） | nginx、port-forward、admin gate |
| C | 直接 `http://<workerIP>:80/...`（集群内） | 再加 router |

如果 A/B 差距远大于 B/C，瓶颈就在网关侧（PF 或 gate），应优先做 §8 的改造。

### 6.3 Mock LLM（已实现）

`mfpi-loadgen serve-mock` 是一个极小的 OpenAI 兼容服务：

- `POST /v1/chat/completions`（`stream: true` 返回 SSE）与 `GET /v1/models`；
- 可配：首字节延迟（`-ttft`）、token 速率（`-chunk-delay`）、回复长度
  （`-chunks`）、注入失败（`-error-rate`）；
- 部署脚本同时完成「构建镜像 → apply → 注册 provider」：

```bash
cd demos/mf-pi
./stress/deploy-mock-llm.sh --test      # 59681 / ate-demo-mf-pi-test
./stress/deploy-mock-llm.sh             # 58681 / ate-demo-mf-pi
./stress/deploy-mock-llm.sh --test --delete
```

脚本用 `ko` 把 `tools/mfpi-loadgen` 打成镜像（离线基础镜像
`localhost:5001/distroless-static-debian13`），部署 `mfpi-mock-llm` 的
Deployment/Service，再通过 `POST /usermanagement/api/providers` 注册为
provider `mock` / model `mock-1`（`baseUrl` 指向集群内 Service）。

- actor 的 supervisor 每 60 s 合并 `models.json`，注册后约 1 分钟可用；
- **还要给用户存一条该 provider 的 key**（`POST /api/users/<n>/apikey`）它才会出现在
  模型选择器里；只写 `models.json` 会得到 `MODEL_UNAVAILABLE`。loadgen 的
  `-provider-api-key` 会自动注入（mock 默认 `sk-mock`）；
- 压测结束删除注册表条目（注意：已合并进 actor `/data/models.json` 的条目不会
  自动消失，可用 `exec-actor.sh` 清理，或接受它留在 stress 用户卷里）。

好处：零 API 成本、延迟稳定、可精确控制时长与失败率；真实 LLM 只留给 S8。

> 当前 mock 只返回文本，不模拟 tool_call：工具调用需要匹配的工具 schema，
> 且一个 prompt 可能演变成无界循环，暂不纳入 v1。

### 6.4 辅助

- **Playwright/Chromium，1–3 个 VU**：抓一次真实会话的 HAR 和 Performance 条目，
  用来校准 Profile A 的端点权重与 think time；也用于验证「提示页/加载体验」。
- **nginx access log**：保留，作为交叉验证；可选给 test-only conf 加上
  `$upstream_response_time`、`$request_time`（见 §8）。
- **benchmarking/locust**：已有 Python + gRPC 基础设施，但缺 WS/会话协议支持，
  不建议作为主工具；可作为「管理面/ate API」场景的补充。

---

## 7. 测试数据与隔离

1. **专用用户**：`st-01 … st-N`（DNS-1123），只压这些。
   - 用 `kubectl ate create actor st-01 -a mfpi --template ate-demo-mf-pi/mf-pi-small`
     创建（**挂起态，不占 worker**）；
   - 用 `POST /usermanagement/api/users/<name>/password` 拿一次性明文密码
     （该端点不校验用户是否存在，正好适合批量预置）；
   - 凭据写 `creds.json`，权限 600。
2. **模型**：S2–S7 用 mock provider；S8 才给少量用户注入真实 key
   （或使用 `__default__`）。
3. **清理**：`DELETE /api/users/<name>` 会删 actor 并 purge 持久卷；压测用户的数据
   不需要保留。注意别误删真人用户。
4. **不要**重置现有 25 个用户的密码，**不要**对它们做 suspend/resume。

---

## 8. 可选的平台改造（先量化再决定）

以下不是压测的前置条件，但如果目标是「生产可支撑更高并发」，大概率要做：

1. **gate 结果缓存**：`handleGate` 每请求一次 `GetActor`。可加 1–2 s 的
   per-user 状态缓存 + 失效通知，或在 nginx 侧用 `proxy_cache`/共享字典限频。
   压测 S5 会直接量化这一项。
2. **admin `/metrics`**：目前 gate / 鉴权 / GetActor 没有指标。加
   `mfpi_gate_duration_seconds`、`mfpi_getactor_duration_seconds`、
   `mfpi_auth_mode_total`（basic/cookie）三组指标，S2/S5 才能定位。
3. **去掉 port-forward**：把 `mfpi-nginx` 跑成集群内 Pod（或给 router 加
   NodePort/LoadBalancer），消除单进程 PF。对照路径 B 能证明是否值得做。
4. **nginx 日志加 upstream 时间**：`$request_time $upstream_response_time`，
   把「网关耗时」和「actor 耗时」在日志层就分开。
5. **worker 资源**：small 是 250m request / 1 CPU limit，超卖设计；压测 S3 会
   给出「多少并发 turn 时开始 throttling」的依据。

---

## 9. 执行阶段

### P0 准备（约 0.5 天）

- 跑 S0 空载基线，记录宿主噪声（本机 load average 当前 38–45，必须在报告里声明）。
- 实现 `mfpi-loadgen` 与 mock LLM；`creds.json` 预置 16 个用户。
- 在 **59681 测试环境**做 S1 冒烟，确认鉴权、cookie、WS、prompt 全通。

### P1 测试环境（约 1 天）

- 59681 上跑 S2（10→50 VU）、S3（8–16 VU，受 test 池 8 worker 限制）、S4、S5。
- 目的不是拿最终容量数字，而是**校准工具与流量模型**、暴露脚本问题。
- 需要更高并发时临时把 test 的 `mf-pi-wp-small` 从 8 扩到 16（+2 核，节点尚可）。

### P2 生产低负载（约 0.5 天）

- 先在 58681 上 **10 VU / 10 min** 的 Profile A（只读），全程盯真实用户 5xx。
- 再 25 VU（用满当前空闲 worker 的一半左右）。
- 通过后再进入 P3。

### P3 生产阶梯（约 1 天，选窗口）

- 并发上限：**≤ 空闲 worker 数**（当前 23），绝不把 `no free workers` 打给真人。
- 阶梯 10 → 25 → 40 → 48（如需 >48，先按 §2.2 释放 Pod/CPU 再扩池）。
- 每个台阶看 §3 的指标，出现「真实用户 5xx 或节点 CPU > 80%」立即降载停止。

### P4 Soak + 报告

- 70% 容量跑 1–2 h；输出容量结论、瓶颈排序、改造建议、复现命令。

---

## 10. 风险与护栏

| 风险 | 护栏 |
|---|---|
| 挤占真实用户 / 把 worker 用完 | 只用专用 `st-*` 用户；并发 ≤ 空闲 worker；实时监控真实用户 5xx |
| 触发已知的 suspend/resume 故障 | 生产禁止 suspend/resume；S9 只在 stress 用户上做 |
| LLM 费用失控 | 默认 mock；S8 限 5 VU × 10 轮，记录 token |
| 宿主噪声污染结果 | 记录同期宿主 load/其它业务；关键结论用 A/B/C 对照路径的相对差值 |
| 压测数据残留 | `cleanup` 删除 actor + purge PV；删除 mock provider 注册项 |
| 误删真人数据 | 脚本硬编码只操作 `st-` 前缀；删除前二次确认列表 |
| 集群槽位/CPU 不足 | 扩容前先确认 110 Pod / 24 核余量；优先回收 mf-cc 空闲 Pod |

---

## 11. 交付物

1. `mfpi-loadgen`（含 `users/run/cleanup` 子命令）+ mock LLM 服务；
2. 各场景的配置与一键复现脚本（`demos/mf-pi/stress/run-*.sh`）；
3. 压测报告：容量数字（最大并发/最大 RPS）、p95/p99 曲线、瓶颈排序、
   对 §8 改造项的优先级建议、原始数据（JSON/CSV/Prometheus 查询）。

---

## 12. 开工前需要确认

已确认的决策：

1. **路径**：先 59681 校准，再在 58681 做受控阶梯（并发 ≤ 空闲 worker 数）。
2. **LLM**：mock 为主 + 5 VU 真实小样本（S8）。
3. **工具**：已实现 `tools/mfpi-loadgen` 与 `demos/mf-pi/stress/deploy-mock-llm.sh`
   （见 §6.1 / §6.3）。

仍需确认：

4. **并发目标**：验证到 10 / 25 / 48，还是需要 >48？超过 48 要扩 `mf-pi-wp-small`，
   得先同意回收 mf-cc 空闲 Pod 或删除 10 个疑似遗留的 UUID actor。
5. **可否改代码/部署**：admin 加 `/metrics`、nginx 加 upstream 时间、
   nginx 改成集群内 Pod（去 port-forward），哪些可以动？
6. **时间窗口**：是否可以选低峰期并在压测前通知真实用户？

---

## 附：工具冒烟结果（2026-10-28，59681 测试环境）

已用 `tools/mfpi-loadgen` 跑通以下最小闭环（用户与 mock 均已清理）：

| 检查 | 结果 |
|---|---|
| 单用户 `idle-browser`（1 VU / 60s） | 无错误；SPA、静态资源、轮询、会话、WS 全通；WS 0 断连 |
| 冷启动 `bootstrap` | 从 SUSPENDED 到 API 200：**约 24s**（4 个用户并行冷启动 24–25s） |
| 4 VU 阶梯 ramp 20s | 4 个 actor 各自 resume；WS 4 连接、1 次异常重连后自动恢复 |
| `agent-turn` + mock（1 VU / 90s） | 4 轮全部成功，mock 收到 4 次请求；turn p50 ≈1s（ttft 100ms + 32×10ms） |
| `admin` 场景（2 VU / 20s） | `ListActors` 76 次、healthz 34 次；p99 100ms |
| `-auth basic` vs `cookie` | 两种模式均可跑；basic 每次请求都走 PBKDF2 |
| `cleanup` | 5 个用户全部删除（admin delete + purge PV） |
| mock 部署/注册/删除 | `ko` 离线构建 + 注册 provider `mock` + 清理，全流程通过 |

> 关键经验：**自定义 provider 必须先给用户存一条 key**（`POST /api/users/<n>/apikey`）
> 才会出现在 pi-web 的模型选择器里；只写 `models.json` 不够。`mfpi-loadgen` 的
> `-provider-api-key` 会在 VU 启动时自动注入（mock 默认 `sk-mock`），并重试模型
> 选择；失败时**中止该 VU**而不是回落到真实模型，避免意外产生真实费用。

---

## 附：立即可用的基线检查

```bash
cd demos/mf-pi

# 网关与用户状态
curl -s -u admin:'mf@pass2026' http://127.0.0.1:58681/usermanagement/api/users | jq '.users|length'
kubectl get workerpool -A
kubectl get pods -A --field-selector spec.nodeName=kind-control-plane --no-headers | wc -l

# 组件指标（需先把 prometheus forward 出来：kubectl port-forward -n otel-system svc/prometheus 19090:9090）
curl -s 'http://127.0.0.1:19090/api/v1/query' --data-urlencode \
  'query=sum by (outcome) (rate(atenet_router_route_duration_seconds_count[5m]))' | jq -r '.data.result[]|[.metric.outcome,.value[1]]|@tsv'
curl -s 'http://127.0.0.1:19090/api/v1/query' --data-urlencode \
  'query=ate_workerpool_workers{ate_workerpool_name=~"mf-pi-wp-.*"}' | jq -r '.data.result[]|[.metric.ate_workerpool_name,.metric.ate_worker_state,.value[1]]|@tsv'

# worker 实际 CPU / throttle（无需 metrics-server）
kubectl get --raw /api/v1/nodes/kind-control-plane/proxy/metrics/cadvisor \
  | grep -E 'container_cpu_usage_seconds_total.*mf-pi-wp-small' | head

# 真实流量画像
docker logs mfpi-nginx --since 24h 2>&1 | awk '{print $9}' | sort | uniq -c | sort -rn | head
```
