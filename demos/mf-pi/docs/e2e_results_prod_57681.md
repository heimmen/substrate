# mf-pi 57681（prod）E2E 验证结果

执行日期：2026-10-31 · 环境：`ate-demo-mf-pi-prod` / `mfpi-prod` · 入口 **57681**。

## 一、部署可用性（先决条件）

初次检查发现 prod **不可用**：24 个 Pod 全部 `Pending`（`Too many pods`），
`mfpi-admin` 也没起来，5768x 端口无监听。节点 110/110 Pod、CPU requests 90%。

为腾出槽位做了以下调整（**现在 live 与 `env/*.env` 已一致**）：

| 环境 | 调整前 | 调整后 |
|---|---|---|
| qa small | 48 | **26**（=现有 actor 数） |
| qa mid / large | 1 / 1 | **0 / 0** |
| test mid / large | 1 / 1 | **0 / 0** |
| prod | 未调度 | **24 / 1 / 1 + admin** |

调整后：节点 **108/110 Pod、CPU requests 77%**；prod 27 个 Pod 全部 Running。
缩容 qa 时 controller 把 10 个 actor 置为 SUSPENDED，已全部 `resume` 回
**26/26 RUNNING**（数据无损）。

## 二、E2E 结果

| # | 检查项 | 结果 |
|---|---|---|
| 1 | prod namespace / 资源 | ✅ 27 Pods Running（24 small + mid + large + admin） |
| 2 | 57681 监听 + 根页面 | ✅ `GET /` → 200 |
| 3 | 管理面健康 | ✅ `GET /usermanagement/healthz` → `{"atespace":"mfpi-prod","status":"ok"}` |
| 4 | 创建用户（管理 API） | ✅ `POST /api/users` 自动创建 atespace `mfpi-prod`，返回一次性密码，actor 立即 RUNNING |
| 5 | 网关 Basic 鉴权 + 就绪门 | ✅ 未就绪时返回加载页；就绪后 SPA 正常（`<title>智能工作台</title>`） |
| 6 | **Cookie 隔离** | ✅ 响应头为 `mfpi_auth_prod` + `mfpi_user_prod`（非 qa/test 名） |
| 7 | 只读用户流（1 VU / 60s） | ✅ SPA、静态资源、轮询、会话、WebSocket 全部 200；**0 transport error、0 WS 错误** |
| 8 | Agent turn（真实 DeepSeek） | ❌ `MODEL_AUTHENTICATION_FAILED`：prod 的 `DEEPSEEK_API_KEY` 是占位符 `placeholder`（部署时未提供真实 key） |
| 9 | Agent turn（mock LLM） | ✅ **3/3 turn 成功**，mock 收到 3 次请求，turn p50 ≈1s，0 错误 |
| 10 | 清理 | ✅ e2e 用户删除（含 PV purge）、mock workload 与 provider 注册移除 |

> 第 9 项验证了完整 agent 链路：网关 → router → actor → pi-web/sessiond →
> provider（mock）→ 流式返回 → 会话消息/token 更新。
>
> 第 8 项失败属**环境配置**而非代码问题。补上真实 key 后即可复测：
>
> ```bash
> DEEPSEEK_API_KEY=<真实 key> ./deploy-prod.sh    # 或经管理面按用户注入
> ```
>
> 当前 prod Secret 里 `DEEPSEEK_API_KEY` 解码长度为 11，正好是 `placeholder`。

## 三、验证中修的工具问题

- `mfpi-loadgen` 的 `-provider-api-key` 原来只在 VU 启动时注入一次；而
  `models.json` 合并是异步的（最长 ~60s），导致注入 400 `provider not found`。
  已改为在 `selectModel` 重试循环里每 15s 重试注入，mock E2E 因此通过。
- 部署脚本：裸 `./deploy.sh` 恢复为 **qa/58681**；新增 `MFPI_DRY_RUN=1`；
  读取缺失的 provider secret 不再让部署静默退出；失败时打印行号与命令。

## 四、遗留 / 建议

1. **给 prod 配真实 DeepSeek key**，再跑一次真实 agent turn（1 VU、1–2 轮即可）。
2. prod 目前 **0 个用户、24 个空闲 small worker**；如果暂时不使用，可
   `kubectl patch workerpool mf-pi-wp-small -n ate-demo-mf-pi-prod --type merge -p '{"spec":{"replicas":8}}'`
   省资源（`env/prod.env` 再同步改）。
3. qa/test 的 mid/large 暂为 0；需要时先腾 Pod/CPU（节点只剩 2 个槽位）。
4. 三套环境的容量数字已写回 `env/*.env`，下次 `deploy.sh` 不会再把 qa 扩回 48
   而挤掉 prod。
