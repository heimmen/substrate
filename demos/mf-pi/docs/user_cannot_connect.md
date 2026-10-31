# 单个用户连不上 Agent：一步一诊断

适用场景：**装完环境后，其他用户都正常，只有某一个用户（系统账号）打不开自己的
Agent**。本文按「从外到内」的顺序给出可复制命令，每一步都有判定标准和下一步指向；
不要跳步，前一步的结论决定后一步是否有意义。

> 如果是**所有用户**都连不上，先看
> [README → 故障排查](../README.md#故障排查)（golden base 快照损坏等）和下文
> **第 1.5 步**（网关→router 隧道掉线，常见的全局故障）。本文主要处理“单点用户”。

## 先固定环境变量（每台机器只做一次）

本手册以 58681 环境为准；排查前先确认你面对的是 `mfpi` 这个 atespace，别对着别的
atespace 查（用户建错 atespace 是“某个用户莫名连不上”的常见原因）。

```bash
cd demos/mf-pi
export U=<用户名>

export ATESPACE=mfpi NS=ate-demo-mf-pi
export PROXY=localhost:58681 ADMIN=127.0.0.1:58682 ROUTER=127.0.0.1:58680

echo "user=$U atespace=$ATESPACE ns=$NS"
```

## 第 0 步：按现象选入口

| 浏览器/客户端现象 | 先做 |
|---|---|
| 反复弹出登录框、401 | 第 2 步（鉴权/密码） |
| 一直停在“Agent 正在载入”、403 | 第 3、4 步（actor 不存在/没恢复） |
| 502 / 503 / 504 / 空白页 / connection refused | 第 1、5、6 步 |
| **多个/所有用户同时**卡在载入页或 502/504，但 actor 全是 `STATUS_RUNNING` | 第 1.5 步（网关→router 隧道掉线） |
| 页面能打开，但 agent 不回话/模型报错 | 第 9 步（这是 key/模型问题，不是“连不上”） |
| 提示“用户不存在” | 第 3 步 |

---

## 第 1 步：网关是否活着

```bash
docker ps --filter name=mfpi-nginx --format '{{.Names}}\t{{.Status}}'
curl -s -o /dev/null -w 'proxy=%{http_code}\n'  "http://$PROXY/"
curl -s -o /dev/null -w 'admin=%{http_code}\n'  "http://$ADMIN/healthz"
curl -s -o /dev/null -w 'router=%{http_code}\n' "http://$ROUTER/"
```

**判定**：`proxy=200`、`admin=200`、`router` 不是 `000`（4xx/200 都算活着）。

- `proxy=000` 或容器不在：`./run-nginx.sh` 重新拉起。
- `admin=000`：58682 port-forward 掉了，同样重跑 `run-nginx.sh`。
- `router=000`（或 curl 等满 5 秒才返回 000）：58680 port-forward 掉了或处于“半死”
  状态。先按第 1.5 步处理。

## 第 1.5 步：网关 → router 的 port-forward 隧道掉线（高频）

**现象**：**多个用户（甚至全部）同时**卡在“Agent 正在载入”，或 API 客户端收到
502/504；但 `kubectl ate get actors -a "$ATESPACE"` 里 actor 全是 `STATUS_RUNNING`。
浏览器通常看不到裸 502，因为 nginx 把 502/503/504 映射成了载入页（见
[nginx-qa.conf](../nginx-qa.conf) 的 `error_page 403 502 503 504 = /_mfpi_loading`）。

**原因**：mfpi-nginx 并不直连 router，而是经 `127.0.0.1:58680` 的
`kubectl port-forward` 连 `svc/atenet-router:80`。router 滚动/重启、节点网络抖动、
或 port-forward 进程自己退出后，这条隧道就断了；**它不会自动重连**。两种形态：

- **进程没了**：`curl` 立即返回 `000`（connection refused）。
- **半死**：本地端口仍在 listen、TCP 能连上，但 HTTP 请求超时/被 reset；`curl`
  同样是 `000`，只是会等满超时。这种最容易被误判成“actor 慢/系统卡”。

**判定**：

```bash
# 1) 隧道是否可用：000 或超时 = 隧道问题
curl -s -o /dev/null -w 'tunnel=%{http_code}\n' -m 5 "http://$ROUTER/"

# 2) 有没有 actor 真的不在 RUNNING（排除 actor 侧问题）
kubectl ate get actors -a "$ATESPACE" | awk 'NR>1 && $4!="STATUS_RUNNING"' | head

# 3) router pod 本身是否正常
kubectl -n ate-system get pods -l app=atenet-router
```

第 1 步 `router=000`、而 router pod 是 Running/Ready → 就是隧道掉线。再看 nginx 侧
证据（官方 nginx 镜像把 error.log 指到 stderr，所以用 `docker logs`）：

```bash
docker logs mfpi-nginx --since 10m 2>&1 \
  | grep -E '127\.0\.0\.1:58680|upstream' | tail
# 典型：connect() failed (111: Connection refused) while connecting to upstream,
#       ... upstream: "http://127.0.0.1:58680/..."
```

**修复（一条命令，幂等）**：

```bash
./run-nginx.sh
```

`run-nginx.sh` 会先探测已有隧道（5s 内是否有 HTTP 响应）：健康则复用，**失效或半死
则杀掉旧 port-forward 并重建**，然后确保 nginx 容器在跑。慢集群可放宽探测超时：
`MFPI_PROBE_TIMEOUT=15 ./run-nginx.sh`。

只想手工重建 router 这一条隧道时：

```bash
kubectl port-forward --address 127.0.0.1 \
  -n ate-system svc/atenet-router 58680:80
```

> 先停掉占用 58680 的旧进程；`run-nginx.sh` 会自动做这件事，手工重建则要注意
> `--address 127.0.0.1`（nginx 只连 IPv4 loopback，漏掉它可能出现间歇 502）。

**验证**：

```bash
curl -s -o /dev/null -w 'tunnel=%{http_code}\n' -m 5 "http://$ROUTER/"
curl -s -o /dev/null -w 'user=%{http_code} bytes=%{size_download}\n' -m 20 \
  -H "Host: $U.$ATESPACE.actors.resources.substrate.ate.dev" "http://$ROUTER/"
```

期望：`tunnel` 非 `000`，`user=200` 且 `bytes>100`。

**预防**：port-forward 没有守护进程，挂了不会自愈。每次 router 发布/重启后都要重跑
`run-nginx.sh`；长期运行建议用 systemd/supervisor 守护该命令，或生产环境改用
Ingress/NodePort 暴露 router，不要让网关依赖本机的 kubectl 隧道。

## 第 2 步：鉴权是否通过

用错误密码探一下，确认 401 链路正常：

```bash
curl -s -o /dev/null -w '%{http_code}\n' -u "$U:definitely-wrong" "http://$PROXY/$U/"
```

- 返回 **401**：鉴权链路正常，说明问题不在密码。继续第 3 步。
- 返回 **403**：密码是对的，但 actor 没 RUNNING → 第 4 步。
- 返回 **200**：密码对且 actor 正常；若浏览器仍连不上，多半是 cookie/浏览器缓存，
  先清 `mfpi_user`、`mfpi_auth` cookie 再试。

**密码确实不对/用户没有密码**：重置（返回值里有新密码，只显示一次）：

```bash
curl -s -X POST "http://$ADMIN/api/users/$U/password" | jq -r '.password'
```

> `$ADMIN` 是 admin 的本地 port-forward，本身不做 Basic Auth（nginx 才做），
> 所以可以直接调用。

## 第 3 步：用户/actor 是否存在，在哪个 atespace

```bash
kubectl ate get actor "$U" -a "$ATESPACE" -o json \
  | jq -r '(.actors[0] // .).status'
./list-users.sh | grep -w "$U" || echo "NOT in atespace=$ATESPACE"
```

**判定**：

- 两条都空/NotFound → **最常见**：用户建错了 atespace。确认后在本环境重新创建：
  `./create-user.sh "$U"`。
- `STATUS_SUSPENDED` → 正常（首次访问才懒恢复），继续第 4 步。
- `STATUS_RESUMING` → 恢复卡住，继续第 4 步。
- `STATUS_RUNNING` → 继续第 5 步。

## 第 4 步：Actor 能否恢复（SUSPENDED / RESUMING）

主动恢复一次并观察：

```bash
kubectl ate resume actor "$U" -a "$ATESPACE" --boot
for i in $(seq 1 30); do
  kubectl ate get actor "$U" -a "$ATESPACE" -o json \
    | jq -r '(.actors[0] // .).status'
  sleep 2
done
```

同时看控制面为什么没恢复：

```bash
kubectl -n ate-system logs deploy/ate-controller --tail=200 \
  | grep -iE "no free workers|resum|snapshot|$U" | tail -20
```

**判定与处理**：

- 变成 `STATUS_RUNNING` → 第 5 步。
- 日志有 `no free workers available` → worker 池被占满。先看该用户的档位
  （`kubectl ate get actors -a "$ATESPACE" | awk -v u="$U" '$2==u {print "template="$3}'`
  或管理 UI），再扩容对应池：

  ```bash
  kubectl get workerpool -n "$NS"
  kubectl scale workerpool mf-pi-wp-small -n "$NS" --replicas=<N>   # small/mid/large 按档位选
  ```

  （`mf-pi-workerpool` 是遗留的基础池，通常为 0，不要当成实际池。）
- 卡在 `STATUS_RESUMING`，atelet/controller 日志出现
  `checkpoint image does not contain spec for container` → 模板/容器名与旧快照不匹配：
  用 `./refresh-actor.sh -y "$U"` 做保留数据的 delete+create+resume。
- 日志出现 `failed to walk ... 9p ... no such file` → golden base 快照问题：
  `./recover-actors.sh --dry-run` 后 `./recover-actors.sh -y`。

## 第 5 步：绑定的 worker pod 是否健康

```bash
kubectl ate get actors -a "$ATESPACE" | awk -v u="$U" '$2==u {print "status="$4, "pod="$5}'

# ATEOM POD 列带 namespace 前缀，去掉它再操作
POD=$(kubectl ate get actors -a "$ATESPACE" | awk -v u="$U" '$2==u {print $5}' | sed 's|.*/||')
echo "POD=$POD"
kubectl get pod "$POD" -n "$NS"
kubectl logs "pod/$POD" -n "$NS" --tail=50
```

**判定**（与 `check_agent_access.sh` 的判定一致）：

- `Running`、`1/1`、`RESTARTS=0`，且日志**超过 3 行** → worker 正常，继续第 6 步。
- 日志只有 ≤3 行 → “ateom 卡 boot”，mf-agent 没起来：删掉这个坏 worker，让它回到
  SUSPENDED 再恢复：

  ```bash
  kubectl delete pod "$POD" -n "$NS"
  kubectl ate resume actor "$U" -a "$ATESPACE"
  ```

- `CrashLoopBackOff`/`ImagePullBackOff`/OOMKilled → 看 `kubectl describe pod "$POD" -n "$NS"`
  的 Events；镜像问题需要回滚/修镜像，不是单个用户问题。

也可以一次性体检全部用户（退出码 1 表示有异常，并打印修复提示）：

```bash
./check_agent_access.sh "$ATESPACE" "$NS"
```

## 第 6 步：绕过 nginx 直连 router（区分网关问题和平台问题）

```bash
curl -s -o /dev/null -w 'code=%{http_code} bytes=%{size_download}\n' -m 20 \
  -H "Host: $U.$ATESPACE.actors.resources.substrate.ate.dev" \
  "http://$ROUTER/"
```

**判定**：

- `code=200` 且 `bytes>100` → actor 真的在服务，问题在 nginx 鉴权/路由 → 回第 2 步；
  顺带确认 nginx 容器里的 Host 模板与该 atespace 一致（生产 conf 固定 `mfpi`）。
- `503` → router 连不上 actor → 回第 4、5 步。
- `504`/超时 → actor 在启动或卡住 → 第 5、7 步。
- `404` → 用户不在此 atespace（Host 拼错或串环境）→ 回第 3 步。

## 第 7 步：Actor 内部是否真的起了 pi-web / sessiond

worker 是 gVisor sandbox + distroless，**没有可 exec 的 shell**（`kubectl exec` 会失败）。
只能看日志和持久卷：

```bash
kubectl logs "pod/$POD" -n "$NS" --tail=300
```

关注：sessiond 是否启动、web 是否监听、是否有 panic/exit。

检查 actor 的持久 `/data`（会话、`auth.json`、skills 等）：

```bash
./exec-actor.sh "$U"          # 交互式查看
./exec-actor.sh "$U" --print  # 只打印节点上的 PV 路径
```

> `exec-actor.sh` 打开的是 kind 节点上的 sticky 卷（actor 的 `/data`），不是活进程；
> 用来确认“数据还在不在”，不能用来重启进程。

数据完好但进程没起 → 走第 5 步的“删坏 worker + resume”，仍不行再用
`./refresh-actor.sh -y "$U"`。

## 第 8 步：控制面（ate-api / valkey / 证书）

单用户异常也可能是控制面坏了（例如 valkey 证书过期时，admin 的 ListActors 会报
`remote error: tls: expired certificate`）。

```bash
# admin 能否列用户（500 说明控制面/数据库有问题）
curl -s "http://$ADMIN/api/users" | head -c 300; echo

kubectl -n ate-system get pods
kubectl -n ate-system logs deploy/ate-api-server --tail=100 \
  | grep -iE "error|expired certificate|valkey|tls|CLUSTERDOWN" | tail -20
kubectl -n ate-system get pods -l app=valkey-cluster
```

**常见判定**：

- 日志有 `remote error: tls: expired certificate` → ate-api 的 valkey 客户端证书过期
  （旧镜像启动时把证书钉死了）。临时缓解：
  `kubectl rollout restart deployment/ate-api-server -n ate-system`；永久修复需部署
  带 `credbundle.ClientLoader` 的新镜像（commit `15071aa2`）。
- `CLUSTERDOWN` / `no such host` → valkey 集群没起来/在滚动，先看
  `kubectl -n ate-system get pods -l app=valkey-cluster`。
- `ate-controller` 持续 `Reconciler error ... no free workers` → 回第 4 步。

## 第 9 步：能连上但 agent 不工作（不要误判为“连不上”）

页面能打开、能发消息，但模型报错/一直转，通常是 **provider key / 模型配置**问题：

```bash
curl -s "http://$ADMIN/api/users" \
  | jq -r '.users[] | select(.name=="'"$U"'") | [.status,.keySource,.hasPersonalKey,.personalKeyProvider] | @tsv'
```

`keySource` 期望是 `personal` 或 `default`；`none` 表示没有任何可用 key。

- 同步重放已存的 key：

  ```bash
  curl -s -X POST "http://$ADMIN/api/users/$U/reinject" | jq .
  ```

  （该接口来自 commit `f6227eb8`；旧镜像返回 404 时，等 30s 后台 reconciler 兜底，
  或在管理 UI 重新保存一次 key。）

- 检查 provider 端点/模型是否可用：管理 UI →「模型设置」；或看 worker 日志里的
  provider 报错（超时、401 invalid api key、404 model not found）。

## 第 10 步：收集信息并升级

一条命令打包（贴给平台同学）：

```bash
TS=$(date +%Y%m%d-%H%M%S); D=/tmp/mfpi-diag-$U-$TS; mkdir -p "$D"
{
  echo "## time"; date -Is
  echo "## env"; echo "U=$U ATESPACE=$ATESPACE NS=$NS"
  echo "## actor"; kubectl ate get actor "$U" -a "$ATESPACE" -o json
  echo "## actors"; kubectl ate get actors -a "$ATESPACE"
  echo "## pods"; kubectl get pods -n "$NS" -o wide
  echo "## admin users"; curl -s "http://$ADMIN/api/users"
} >"$D/summary.txt" 2>&1
kubectl -n "$NS" logs deploy/mfpi-admin --tail=500 >"$D/mfpi-admin.log" 2>&1
kubectl -n ate-system logs deploy/ate-api-server --tail=500 >"$D/ate-api.log" 2>&1
kubectl -n ate-system logs deploy/ate-controller --tail=500 >"$D/ate-controller.log" 2>&1
kubectl -n "$NS" describe pod "$POD" >"$D/worker-pod.txt" 2>&1
kubectl -n "$NS" logs "pod/$POD" --tail=1000 >"$D/worker.log" 2>&1
tar czf "$D.tgz" -C /tmp "$(basename "$D")"
echo "打包完成：$D.tgz"
```

## 常见根因速查

| 现象 | 最可能原因 | 一条命令修复 |
|---|---|---|
| 用户不存在 / 404 | 建到了另一套 atespace | `./create-user.sh "$U"`（对应当前环境） |
| 401 反复 | 密码错/没密码 | `curl -X POST http://$ADMIN/api/users/$U/password` |
| 403 + 载入页不消失 | actor 未 RUNNING / 恢复失败 | `kubectl ate resume actor "$U" -a "$ATESPACE" --boot` |
| 503 `no free workers available` | 对应档位的 worker 池满 | `kubectl scale workerpool mf-pi-wp-small -n "$NS" --replicas=<N>`（按档位选池） |
| 多个用户同时卡载入页/502/504，actor 都 RUNNING | 网关→router 的 port-forward 隧道掉线 | `./run-nginx.sh`（幂等，会替换半死隧道） |
| 卡 `STATUS_RESUMING` + 容器 spec 不匹配 | 旧快照 vs 新模板 | `./refresh-actor.sh -y "$U"` |
| 所有用户都挂 + `failed to walk 9p` | golden base 快照损坏 | `./recover-actors.sh -y` |
| RUNNING 但 worker 日志 ≤3 行 | ateom 卡 boot | 删 worker pod 后 resume（第 5 步） |
| admin 报 `tls: expired certificate` | ate-api 客户端证书过期 | `kubectl rollout restart deployment/ate-api-server -n ate-system`，并升级镜像 |
| 页面能开、模型报错 | key/模型配置 | `curl -X POST http://$ADMIN/api/users/$U/reinject` |

---

## 管理页报 Valkey TLS `expired certificate`（控制面，不是用户 Actor）

**现象**：`/usermanagement/` 列出用户失败：

```
列出用户失败: rpc error: code = Internal desc = internal server error:
while listing actors in db: while scanning shard
valkey-cluster-5.valkey-cluster-service.ate-system.svc:6379:
remote error: tls: expired certificate
```

同时 `valkey-cluster-5` 日志反复出现
`Error accepting a client connection: ... certificate verify failed (addr=<ate-api-server IP>)`。

**原因**：`ate-api-server` 旧版本在**启动时**把 Valkey 客户端证书加载进
`tls.Config.Certificates` 并一直复用。kubelet 会每天轮换 projected
`podCertificate`（`/run/podidentity.../credential-bundle.pem`）；旧进程继续出示
**已过期**的客户端证书，Valkey（`tls-auth-clients yes`）拒绝它，Go 侧就报
`remote error: tls: expired certificate`。与用户 Actor、nginx、atespace 无关。

**修复**（代码修复已包含 `15071aa2 fix(ateapi): reload the Valkey client certificate on rotation`）：

```bash
# 临时止血：重启后进程会重新读取当前有效证书
kubectl rollout restart deployment/ate-api-server -n ate-system

# 根治：用包含上述提交的 HEAD 重建并滚动 ate-api-server
KO_DOCKER_REPO=localhost:5001 \
KO_DEFAULTBASEIMAGE=localhost:5001/distroless-static-debian13 \
  ko apply -f manifests/ate-install/ate-api-server.yaml
```

**验证**：

```bash
kubectl ate get actors -A                      # 全 shard 可列出，无 TLS 错误
kubectl logs -n ate-system valkey-cluster-5 --since=3m | grep -c "certificate verify failed"  # 0
curl -u admin:'mf@pass2026' http://127.0.0.1:57681/usermanagement/api/users  # 200
```

> 只重启不重建只能撑到下一次证书轮换（约每天一次）；必须升级到含
> `15071aa2` 的镜像。
