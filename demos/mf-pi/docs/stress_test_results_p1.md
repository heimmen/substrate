# mf-pi 压测 · P1 测试环境（59681）校准结果

执行日期：2026-10-29 · 工具：`tools/mfpi-loadgen`（`idle-browser` 场景，
`-auth cookie`，`-think 3s-8s`）· 环境：`ate-demo-mf-pi-test` / `mfpi-test`。

> 方法见 [`stress_test_plan.md`](./stress_test_plan.md) §5/§9。本阶段目的是「校准
> 工具与流量模型、确认测试环境可安全并发」，**不是**产能定标——测试环境 small 池
> 只有 8 个 worker，且与本机其它业务（含另一个 atespace 的真实流量）共用同一台
> 单节点。

## 参数与资源

- 测试 small pool：`mf-pi-wp-small` = 8 副本；压测期间另有 2 个测试用户
  （`mark`、`tom2`）已 RUNNING，故实际可并发 = 6，阶梯取 1 → 3 → 6。
- 节点：单节点 kind 24 核；全程受宿主其它业务干扰（节点容器 CPU 在 48%–162% 波动）。
- 每次运行：ramp 后等**所有 VU 就绪**再开始 `-duration` 计时（冷启动单独计）。

## 结果汇总

| 指标 | S0 基线 | S1 =1 VU | S2a =3 VU | S2b =6 VU |
|---|---|---|---|---|
| loadgen 端 `bootstrap` 冷启动 (max) | — | 23.7s | 24.0s | 24.2s |
| loadgen 端 `poll.*` p99（跨未读/状态等） | — | ≤2.5s | ≤2.5s | ≤2.5s（未读 500ms，status 2500ms） |
| loadgen 端 transport 错误 / 5xx | — | 0 | 0 | 0 |
| router 端 QPS（`route_duration…count`，全 atespace 合计） | 0.575 | 0.624 | 1.776 | 2.812 |
| router 端 p99（ok） | 118ms | 68.6ms | 71.7ms | 96.0ms |
| ate-api-server CPU（核） | 0.0065 | 0.0061 | 0.010 | 0.0153 |
| atenet-router CPU（核） | 0.0073 | 0.0079 | 0.0099 | 0.0116 |
| 节点容器 CPU | 131% | 99% | 48% | 162% |
| WS 异常断开 1006 / reconnects | — | 2 | 6 | 12 |

原始报告（JSON）：`/tmp/mfpi-p1/{s1-1vu,s2-3vu,s2-6vu}.json`（会话外）。

## 观察与结论

1. **工具与流程验证通过**：`users → run(ladder) → cleanup` 全程稳定；冷启动约 24s，
   所有 HTTP/WS 端点无错误、无 5xx。
2. **6 并发远未到瓶颈**：router/ate-api-server/worker CPU 都还很低，
   router p99 只从 69ms 小幅升到 96ms。测试环境 8 worker 就封顶，梯度只能到 6；
   更高档位（10/25/48/…）必须到生产或扩 worker 才能测。
3. **WS events 通道反复 1006 异常断开**，且随并发线性增加（2 → 6 → 12 次重连）。
   这是阶段内唯一稳定的异常信号，疑似服务端侧（sessiond/event WebSocket 或
   网关对长连接的处理），建议用 S4（WS 重连风暴）与 S7（soak）单独定性。
4. **指标是粗粒度**：router/ate-api-server 的 Prometheus 指标含所有 atespace +
   天然噪声（本机其它业务），且单节点 CPU 波动很大。最可信的是 loadgen 端的分
   端点延迟与错误计数。生产定标时建议按 `stress_test_plan.md` §6.2 打 A/B/C 对照
   路径，把网关、port-forward、router 逐层剥开。
5. **测试环境干净**：6 个 `st-*` 用户已全部删除（admin delete + purge PV），未触碰
   其它用户；未部署 mock（本阶段只跑只读 idle）。

## 遗留 / 下一步

- 定位 WS 1006 来源（服务端 vs 网关），再进入上门槛的 S2/S3。
- P2（58681 受控）：并发 ≤ 空闲 worker、选低峰窗口，先 10 VU 跑 S2 只读，再视
  情况补 S3（需部署 mock）。
- 若要做 >48 并发，需先释放 Pod/CPU 槽位（回收 mf-cc 空闲 worker 或清理遗留
  UUID actor），仅限正式窗口。
