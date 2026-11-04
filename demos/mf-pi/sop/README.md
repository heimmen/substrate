# mf-pi SOP · 用户连不上 Agent（值班卡片）

> **先看这一页。** 它只做三件事：判断范围 → 给可复制的命令 → 告诉你“修好了没”。
> 需要逐层深挖时，再翻 [user_cannot_connect.md](./user_cannot_connect.md)。
>
> 下面的命令默认在 `demos/mf-pi/sop/` 目录执行（`run-nginx.sh` 例外，它在上一级）。
>
> **先看全局**：`./status-all.sh` 一屏列出三套环境的用户数、RUNNING/SUSPENDED、
> 网关、worker 就绪数和“悬空 actor”数；`--detail` 看每个用户，`--watch 5` 持续刷新。

---

## 30 秒判断：一个人坏，还是一群人坏？

先问自己一句：**别的用户能打开吗？**

| 情况 | 结论 | 直接去 |
|---|---|---|
| **所有/多个**用户都卡在「Agent 正在载入」 | 整个环境的问题（网关 → router 隧道、控制面） | [A](#a-所有用户都卡在正在载入) |
| **只有这一个**用户卡，别人正常 | 这个 actor / worker 的问题 | [B](#b-只有一个用户卡在正在载入) |
| 反复弹登录框（401）、提示用户不存在、能进页面但模型报错 | 不是“连不上” | [C](#c-其他情况) |

---

## 先填变量（30 秒）

> 下面 `<...>` 都要替换成实际值；不知道值就先看紧接着的“三步找出来”。

```bash
export U=<用户名>                 # 要排查的用户
export ATESPACE=<atespace>        # 用户所在 atespace
export NS=<namespace>             # 该环境的 namespace
export PROXY=<入口>               # 例如 localhost:58681
export ROUTER=<router 本地端口>   # 例如 127.0.0.1:58680
export ADMIN=<admin 本地端口>     # 例如 127.0.0.1:58682
```

**不知道填什么？** 三步找出来：

```bash
# 1) 按用户名反查 atespace。注意：同名用户可能存在于多套环境，会输出多行。
#    从结果里选你真正要查的那一套，填到 ATESPACE。
kubectl ate get actors -A | awk -v u="$U" '$2==u{print $1}' | sort -u

ATESPACE=<从上面选一个>

# 2) namespace：actor 绑定的 pod 前缀就是 namespace
#    （actor 已 SUSPENDED 时没有 pod，需要问部署人）
POD_REF=$(kubectl ate get actor "$U" -a "$ATESPACE" | awk 'NR==2{print $5}')
NS=${POD_REF%%/*}
echo "ATESPACE=$ATESPACE NS=$NS"

# 3) 端口：本仓库的部署写在 env/*.env；其它部署看 run-nginx 脚本/部署记录
grep -H 'MFPI_ENTRY_PORT\|MFPI_ROUTER_PORT\|MFPI_ADMIN_PORT' ../env/*.env 2>/dev/null
```

> **选错 atespace 会白查**：后面所有探测都会是 `000`/`404`。如果反查结果有多行，
> 先确认用户实际在哪套环境；第 2 步的 `NS` 也必须和它对应。

> 同一台机器上有多套部署（prod/qa/test）时，**先确认用户属于哪一套**，再用那一套的
> `ATESPACE/NS/端口`；拿错环境的值排查是最常见的“查了半天没结果”。

---

## A. 所有用户都卡在「正在载入」

**先确认**（3 条命令，10 秒）：

```bash
docker ps --filter name=mfpi-nginx --format '{{.Names}}\t{{.Status}}'
curl -s -o /dev/null -w 'proxy=%{http_code}\n' "http://$PROXY/"
curl -s -o /dev/null -w 'router=%{http_code}\n' -m 5 "http://$ROUTER/"
```

| 结果 | 含义 | 动作 |
|---|---|---|
| `router=000`（或等满 5 秒） | **网关 → router 的 port-forward 断了**（最常见） | 下一步的“修复” |
| `proxy=000` | 网关容器没跑 | 下一步的“修复” |
| 都正常但用户还是卡 | 不是隧道问题 | 去看[详细手册第 1、5、8 步](./user_cannot_connect.md) |

> `router` 是 `404`/`403` 也算活着，**只有 `000` 才是隧道断了**。`docker ps`
> 会列出所有环境的网关容器，注意看你排查的那一套。

**修复**（幂等；`run-nginx.sh` 在部署目录，不在 `sop/`）：

```bash
# 如果你现在在 sop/ 目录里：
(cd .. && ./run-nginx.sh)

# 如果你已经在 demos/mf-pi：
./run-nginx.sh
```

**验证**（重新探一次，`router` 不再是 `000`）：

```bash
curl -s -o /dev/null -w 'router=%{http_code}\n' -m 5 "http://$ROUTER/"
```

> 为什么一条命令就够：`run-nginx.sh` 会探测已有隧道，健康就复用，**失效或“半死”的
> 会杀掉重建**，然后确保网关容器在跑。慢集群可加 `MFPI_PROBE_TIMEOUT=15`。

---

## B. 只有一个用户卡在「正在载入」

**先确认**（看 actor 状态、worker 是否重启过、日志是否只有几行）：

```bash
kubectl ate get actor "$U" -a "$ATESPACE"

POD=$(kubectl ate get actor "$U" -a "$ATESPACE" | awk 'NR==2{print $5}' | sed 's|.*/||')
kubectl get pod "$POD" -n "$NS" -o wide        # 重点看 RESTARTS
kubectl logs "pod/$POD" -n "$NS" --tail=10     # 重点看是不是只有几行 ateom booting
```

> 如果第一条命令显示 actor 是 `STATUS_SUSPENDED`，ATEOM POD 是空的（`POD` 也会是
> 空或 `<none>`）：**跳过 `kubectl get pod` 和 `kubectl logs`**，直接按下面表格的
> “actor 状态不是 RUNNING”一行处理。

再**绕过网关**直接问 actor（排除鉴权和页面问题）：

```bash
curl -s -o /dev/null -w 'actor=%{http_code}\n' -m 5 \
  -H "Host: $U.$ATESPACE.actors.resources.substrate.ate.dev" "http://$ROUTER/"
```

| 结果 | 含义 | 动作 |
|---|---|---|
| `actor=200` | actor/worker 是好的 | 问题在浏览器/密码 → [C](#c-其他情况) |
| `actor=503`/`000`，且 `RESTARTS>0` 或日志只有几行 | **worker 容器重启/OOM 后 actor 悬空** | 下面的“修复” |
| actor 状态不是 `STATUS_RUNNING` | actor 没起来 | `kubectl ate resume actor "$U" -a "$ATESPACE"`，等 1 分钟再探 |

**修复**（数据不丢；唤醒是冷启动，约 25–40 秒）：

```bash
# 1) 删掉这个坏 worker（每个活跃 actor 独占一个 worker，不会影响别人）
kubectl delete pod "$POD" -n "$NS" --wait=false

# 2) 等 actor 被释放成 SUSPENDED —— 关键一步，仍是 RUNNING 时 resume 会被拒绝
for i in $(seq 1 30); do
  st=$(kubectl ate get actor "$U" -a "$ATESPACE" | awk 'NR==2{print $4}')
  echo "$st"
  [ "$st" = "STATUS_SUSPENDED" ] && break
  sleep 2
done

# 3) 恢复（没有快照，会从模板冷启动）
kubectl ate resume actor "$U" -a "$ATESPACE"

# 4) 等它就绪并探活
for i in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 \
    -H "Host: $U.$ATESPACE.actors.resources.substrate.ate.dev" "http://$ROUTER/")
  echo "$code"
  [ "$code" = "200" ] && break
  sleep 2
done
```

**还不行？** 保留数据重建一次（会换一个新 actor，`/data` 不动）：

```bash
./refresh-actor.sh -y "$U"
```

**批量找出所有“状态 RUNNING 但实际打不开”的用户**：

```bash
for row in $(kubectl ate get actor -a "$ATESPACE" | awk 'NR>1 && NF>1 {print $2"@"$5}'); do
  u=${row%@*}; p=${row#*@}
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 4 \
    -H "Host: $u.$ATESPACE.actors.resources.substrate.ate.dev" "http://$ROUTER/")
  [ "$code" != "200" ] && echo "BAD $u code=$code pod=$p"
done
```

---

## C. 其他情况

| 现象 | 大概率原因 | 处理 |
|---|---|---|
| 反复弹登录框 / 401 | 密码错或没分配密码 | [详细手册第 2 步](./user_cannot_connect.md) |
| 403，载入页不消失 | actor 不是 RUNNING / 恢复失败 | 第 3、4 步 |
| 提示“用户不存在” | 建到了另一个 atespace | 用 `./create-user.sh "$U"` 在本环境重建 |
| 页面能开，但 agent 不回话 / 模型报错 | key / 模型配置，不是“连不上” | 第 9 步 |
| 管理页报 `tls: expired certificate` | 控制面（Valkey 客户端证书） | [详细手册最后一节](./user_cannot_connect.md) |

---

## 修好了吗？最终验收

**单用户**：上面的 `actor=200` 且浏览器能打开即可。

**整个环境**：

```bash
./check_agent_access.sh "$ATESPACE" "$NS"
```

期望输出：每个用户都是 `[正常]`，最后 `异常: 0`。退出码 `1` 表示还有人有问题。

---

## 全新安装：只做三件事

不需要为“防这次故障”上 Prometheus，也不需要先做容量研究。把下面三点默认化即可：

1. **worker 内存给足**：模板里 small worker 的 `limits.memory` 已经是 **2Gi**
   （原来是 1Gi；内存尖峰 OOM 是本次“单个用户悬空”的根因）。
2. **装上 watchdog**：一条命令覆盖两类故障（隧道掉线 + OOM 后 actor 悬空）：

   ```bash
   # 有 root + systemd：装成 60s 定时器，重启后仍然生效
   sudo ./install-watchdog.sh start qa

   # 普通用户：nohup 循环，重启后需要重新执行
   ./install-watchdog.sh start qa

   ./install-watchdog.sh status qa    # 看是否在跑
   ./install-watchdog.sh stop qa      # 停止
   ```

   它每分钟做两件事：探测 router/admin，挂了自动跑 `run-nginx.sh`；发现
   `RESTARTS>0` 且绕过网关探活非 200 的 RUNNING actor，自动删坏 worker、等
   `SUSPENDED`、`resume`。日志在 `/tmp/mfpi-logs/watchdog-<env>.log`。
3. **错误要可见**：nginx 已把 **502/504** 映射成“Agent 网关连接失败”错误页
   （10 秒自动重试），不再一律显示 loading；403/503（actor 正在恢复）仍走 loading。

> 平台的 syncer 修复（worker 容器重启也释放 actor）随镜像发布，不需要安装步骤。
> 如果只能做一件事：**装 watchdog**。

## 安全须知（别踩坑）

- **删 worker pod 前**确认这个 pod 上只有目标用户（活跃 actor 与 worker 是 1:1）。
- **不要**用 `delete actor` 代替恢复：先试“删 worker → 等 SUSPENDED → resume”，
  实在不行再 `refresh-actor.sh`（它才是保留数据的重建）。
- 多套部署并存时，确认 `ATESPACE/NS/端口` 是**同一套**，别拿 prod 的值查 qa。
- 用户数据在 sticky 卷上；冷启动只是慢，**数据不会丢**。

---

## 脚本清单

| 脚本 | 用途 |
|---|---|
| `status-all.sh` | **一屏看三套环境**：用户数/RUNNING/SUSPENDED、网关、worker、悬空 actor；`--detail` / `--watch N` |
| `watchdog.sh` | 每分钟自愈：隧道掉了自动重建；发现“OOM 后 actor 悬空”自动删 worker→等 SUSPENDED→resume |
| `install-watchdog.sh` | 安装/启动/停止/查看 watchdog（root+systemd 用 60s timer；否则 nohup 循环） |
| `check_agent_access.sh` | **先跑这个**：体检某 atespace 的全部用户（状态 / worker Ready / restarts / 最近活动） |
| `refresh-actor.sh` | 保留数据地 `delete + create + resume --boot`（单个/批量） |
| `recover-actors.sh` | 清 golden 快照 + 重建所有不服务的 actor（全局故障） |
| `ensure-no-golden.sh` | 只清模板的 golden 快照 |
| `exec-actor.sh` | 打开 actor 的持久卷（actor 内 `/data`）检查数据 |
| `list-pvs.sh` | 列出用户持久卷与数据概况 |
| `create-user.sh` / `list-users.sh` / `delete-user.sh` | 用户生命周期 |
| `sync-from-parent.sh` | 从 `demos/mf-pi/` 同步这些脚本副本 |

> 网关脚本（`run-nginx.sh` / `stop-nginx.sh`）依赖 `demos/mf-pi/env/` 和 `lib/`，
> 没有复制进来；请在部署目录执行（离线安装包由 `install.sh` 负责网关）。

---

## 详细手册

逐层的判定标准、错误码含义、控制面检查都在
**[user_cannot_connect.md](./user_cannot_connect.md)**。本文的 A/B 两条路径是最常见的
两类故障，覆盖了绝大多数“用户连不上”的工单。
