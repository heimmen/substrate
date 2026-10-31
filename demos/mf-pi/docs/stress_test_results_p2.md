# mf-pi 压测 · P2 58681（现 qa）32 并发 Agent 实测

> 环境映射：58681 现在是 **qa**（历史 `mfpi` / `ate-demo-mf-pi`），新生产是
> **57681**（`mfpi-prod` / `ate-demo-mf-pi-prod`）。本文件的容量结论来自 58681
> 这套资源，**prod 上线后需在 57681 复测**；两者 worker 默认值不同
> （qa 24 / prod 24 / test 8，见 `docs/environments.md`）。

执行日期：2026-10-29 · 工具：`tools/mfpi-loadgen`（`agent-turn` 场景，`-auth cookie`，
`-turn-interval 30s`，mock model）· 环境：`ate-demo-mf-pi` / `mfpi`（生产网关 58681）。

> 目标：**32 个用户同时做真实的 agent turn**（发 prompt + 模型生成），测生产容量与
> 瓶颈。模型用部署在生产 namespace 的 mock LLM（零成本、延迟稳定）。压测前生产 48
> 个 small worker 中仅 3 个 RUNNING，空闲 45，故 32 并发不挤占真实用户。

## 执行

1. 部署 mock LLM 到 `ate-demo-mf-pi` 并注册 provider `mock/mock-1`。
2. 建 32 个 `st-*` 用户（挂起态，不占 worker；`record` 密码）。
3. 8 VU 冒烟通过后，跑 **32 VU / ramp 60s / hold 5min / turn-interval 30s**。
4. 跑完删除 32 用户 + 卸载 mock；生产复原（25 actor，3 RUNNING + 22 SUSPENDED）。

## 结果

| 指标（loadgen 端） | 值 | 说明 |
|---|---|---|
| VU 就绪 | 32 / 32 | 8 个 warm + 24 个冷启动 |
| 冷启动（并发排队） | 最高 ~67s（p99 67.7s） | 32 个同时冷启动互相争 CPU |
| agent turn 成功 | **320 / 321** | 仅 1 失败（一个 VU 曾 bootstrap 500） |
| turn 延迟 | p50 1.0s / p95 2.5s / **p99 5.0s** | 并发下明显爬升 |
| prompt / model.set / status p99 | 2.5s / **5.0s** / 2.5s | 均较 8 VU 恶化 |
| WebSocket | 30 197 条消息，**0 次 1006 重连** | 生产未复现 P1 测试环境的 1006 |

| 指标（平台，最近 7min） | 值 | 对比基线 |
|---|---|---|
| router 端 QPS | 5.81 rps | 基线 0.58 |
| **router 端 p99（ok）** | **1100 ms** | 基线 96 ms（**≈11×**） |
| router 端 cancelled p99 | **9950 ms** | 出现过载/超时取消 |
| router 端 error | 0.01 rps | 基线 0 |
| ate-api-server CPU | 0.034 核 | 基线 0.006（**控制面未饱和**） |
| atenet-router CPU | 0.019 核 | 基线 0.007 |
| 节点 CPU（kind 容器） | **807%**（~8 核） | 平静期 ~1–3 核 |
| 节点 load average | ~49 / 24 核 | —— |

## 结论

1. **32 个 agent 同时工作已经把单节点 CPU 打满**：kind 容器 807% CPU、load ~49。
   这直接反映到用户体验 —— router 前端 p99 从 ~100ms 劣化到 ~1100ms，并出现
   ~10s 的 cancelled/超时。**这就是当前架构的容量拐点（至少在 32 并发、turn 间隔
   30s 时已经触发）。**
2. **瓶颈不在控制面**：ate-api-server（gate 的 GetActor 路径）CPU 只到 0.034 核，
   router 也只 0.019 核。真正的瓶颈是**执行侧**：gVisor sandbox + agent（prompt、
   会话处理）在 24 核对节点上的 CPU 争抢。
3. **worker 超卖设计生效但开始劣化**：48 个 worker（各 limit 1 核、request 250m）
   在 32 并发 agent 时已扛不住；「一个活跃 agent 独占一 worker + 1 核 limit」的
   模型在真实计算负载下，可同时「好好工作」的 agent 数远小于 48。
4. **生产 WS 无 1006**：32 并发、30k 条消息、0 重连 —— P1 测试环境偶发的 WS 1006
   未在生产复现，需要 S7 soak 才能定性，暂不作为生产问题。
5. **真实用户零影响**：压测期间 3 个 RUNNING 真实用户正常，22 个挂起用户未受影响
   （worker 充足，未出现 `no free workers`）。

## 建议

- 生产要支撑「更多并发同时工作」或更低延迟，**先解决执行侧 CPU**：
  - 降低 worker CPU 超卖（提高 small 的 request 或减 replicas）；
  - 或把 agent 工作负载放到 CPU 更强/更多的节点；
  - 或按 turn 并发而不是「活跃用户数」来规划容量（因为真算力上限在 CPU，不在
    worker 数）。
- 下一轮可加：不同 turn 间隔（30s/60s/120s）看容量随「有效并发」的变化；以及在
  压测中采集每个 worker 的 cAdvisor CPU throttling，量化「超卖」到底超到多少。
- 生产上这类高并发 mock 压测建议在低峰 + 可随时 kill 的窗口内做，且 mock 链路
  已验证安全（零 API 成本、可一键清理）。
