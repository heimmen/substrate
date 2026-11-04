# mf-pi SOP：用户连不上 Agent

本目录把「用户连不上 Agent」的诊断文档和脚本放在一起，便于值班同学单独取用。
完整步骤见 **[user_cannot_connect.md](./user_cannot_connect.md)**。

## 先按现象分流

| 现象 | 去哪 |
|---|---|
| **多个/所有**用户同时卡在「Agent 正在载入」，actor 却都是 `STATUS_RUNNING` | 网关→router 的 port-forward 掉了：runbook 第 1.5 步；在部署目录跑 `./run-nginx.sh` |
| **单个**用户卡在「Agent 正在载入」，actor 显示 `STATUS_RUNNING`，worker `RESTARTS>0` | worker 容器重启/OOM 后 actor 悬空：runbook 第 5.5 步 |
| 401 / 403 / 「用户不存在」/ 页面能开但模型报错 | runbook 第 0 步的分流表 |

## 环境变量（按你的部署填写）

```bash
export U=<用户名>
export ATESPACE=mfpi NS=ate-demo-mf-pi
export PROXY=localhost:58681 ADMIN=127.0.0.1:58682 ROUTER=127.0.0.1:58680
export NGINX=mfpi-nginx NGINX_SCRIPT=run-nginx.sh
```

> 上面的默认值只是本仓库 demo 的常见部署；换环境就换这几个值。

## 脚本

| 脚本 | 用途 |
|---|---|
| `check_agent_access.sh` | **先跑这个**：体检某 atespace 的全部用户（状态 / worker Ready / restarts / 最近活动），退出码 1 表示有异常 |
| `refresh-actor.sh` | 保留数据地 `delete + create + resume --boot`（支持单个/批量） |
| `recover-actors.sh` | 清 golden 快照 + 重建所有不服务的 actor（全局故障） |
| `ensure-no-golden.sh` | 只清模板的 `goldenStatus`/golden 快照 |
| `exec-actor.sh` | 打开 actor 的持久卷（actor 内 `/data`）检查数据是否还在 |
| `list-pvs.sh` | 列出用户持久卷与数据概况 |
| `create-user.sh` / `list-users.sh` / `delete-user.sh` | 用户生命周期 |

典型用法：

```bash
./check_agent_access.sh "$ATESPACE" "$NS"
./refresh-actor.sh -y "$U"
./recover-actors.sh --dry-run && ./recover-actors.sh -y
```

这些脚本是从 `demos/mf-pi/` 下的同名文件**复制**来的，方便单独分发；需要同步时运行：

```bash
./sync-from-parent.sh
```

> 网关脚本（`run-nginx.sh` / `stop-nginx.sh`）依赖 `demos/mf-pi/env/` 和 `lib/`，所以
> 没有复制进来；请在部署目录执行（离线安装包由 `install.sh` 负责网关）。
