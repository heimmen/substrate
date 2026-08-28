# mf-cc 演示

此目录包含将 **mf-cc**（cc-haha）Web UI 作为 Agent Substrate 上的 Actor 运行的演
示。mf-cc 是一个基于 Bun 的 HTTP + WebSocket 服务器，提供基于 React 的 AI 编码工
作台（REST API 在 `/api/*`，WebSocket 在 `/ws/*`，静态前端在 `/*`）。

将其作为 Substrate Actor 运行可获得挂起/恢复与快照的能力：挂起时对进程内存和容器
文件系统做 Full 快照，恢复时原样还原，会话和配置因此在挂起与恢复之间持续存在。

## 前提条件

- 已安装 Agent Substrate 的 k8s 集群
  （`./hack/install-ate.sh --deploy-ate-system`）。
- 本地已构建的 mf-cc 镜像 `mf-cc:latest`（参见 cc-haha 仓库中的
  `docker-build.sh`）。该镜像必须能被集群节点访问。

> [!NOTE]
> **本地集群不需要真实的 GCS bucket。** 在 kind/k3s 上，快照存储在集群内部的对象
> 存储（rustfs）中；`BUCKET_NAME` 只是其中的逻辑 bucket 名称，
> `install-ate-kind.sh` 已将其设置为 `ate-snapshots`。真实的 GCS bucket
> （`gs://${BUCKET_NAME}`）仅在 GKE 上使用。

> [!IMPORTANT]
> 在本地集群上，节点无法访问外部镜像仓库，因此 mf-cc 镜像**和** pause 镜像都必
> 须推送到本地仓库（`localhost:5001`）。部署脚本在镜像就位后会自动解析其摘要引
> 用。

## 如何在 Agent Substrate 上运行

### 1. 将镜像推送到集群的镜像仓库

在 kind 集群上（`KO_DOCKER_REPO=localhost:5001`），先将镜像本地化：

```bash
# mf-cc 工作负载镜像（由 cc-haha 仓库的 docker-build.sh 构建）
docker tag mf-cc:latest localhost:5001/mf-cc:latest
docker push localhost:5001/mf-cc:latest

# pause 镜像（3.10.2；可使用任意可访问的镜像源，例如 rancher/mirrored-pause）
docker tag rancher/mirrored-pause:3.10.2 localhost:5001/pause:3.10.2
docker push localhost:5001/pause:3.10.2
```

### 2. 部署

#### 方式一：`./deploy.sh`（推荐）

在 `demos/mf-cc` 目录下直接运行。所有配置均为可选，未设置时回退到
kind/离线友好的默认值（`KO_DOCKER_REPO=localhost:5001`、
`BUCKET_NAME=ate-snapshots`、`ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic`、
`ANTHROPIC_MODEL=deepseek-v4-flash`、`MFCC_WORKER_REPLICAS=16`）。
`ANTHROPIC_AUTH_TOKEN` 未导出时会尝试从上一次部署创建的
`mf-cc-provider-config` Secret 中读取：

```bash
cd demos/mf-cc
# 首次部署（或需要更换配置时）显式设置：
ANTHROPIC_AUTH_TOKEN=<token> \
ANTHROPIC_BASE_URL=<base-url> \
ANTHROPIC_MODEL=<model> \
./deploy.sh
# 之后直接重跑即可（token 从既有 Secret 读取，配置不变）：
./deploy.sh
```

> [!NOTE]
> GKE 上请显式设置 `BUCKET_NAME=<真实的 GCS bucket>`；kind/k3s 上用默认值即可
> （集群内 rustfs 存储）。

#### 方式二：`hack/install-ate.sh`

通过安装脚本部署（`ANTHROPIC_AUTH_TOKEN`、`ANTHROPIC_BASE_URL`、
`ANTHROPIC_MODEL`、`BUCKET_NAME`、`KO_DOCKER_REPO` 必须设置；
`MFCC_WORKER_REPLICAS` 可选，默认 `4`，决定最大同时活跃用户数）：

```bash
# GKE 上（BUCKET_NAME = 真实的 GCS bucket）：
BUCKET_NAME=<bucket> \
ANTHROPIC_AUTH_TOKEN=<token> \
ANTHROPIC_BASE_URL=<base-url> \
ANTHROPIC_MODEL=<model> \
MFCC_WORKER_REPLICAS=4 \
./hack/install-ate.sh --deploy-demo-mf-cc

# kind/k3s 上（install-ate-kind.sh 会设置 KO_DOCKER_REPO=localhost:5001 和
# BUCKET_NAME=ate-snapshots 用于集群内 rustfs 存储）：
ANTHROPIC_AUTH_TOKEN=<token> \
ANTHROPIC_BASE_URL=<base-url> \
ANTHROPIC_MODEL=<model> \
MFCC_WORKER_REPLICAS=4 \
./hack/install-ate-kind.sh --deploy-demo-mf-cc
```

两种方式都会：

- 从镜像仓库中解析 mf-cc 和 pause 镜像的摘要固定引用。
- 创建 `ate-demo-mf-cc` 命名空间。
- 创建 provider-config `Secret` 以及允许 `ate-api-server` 读取它用于环境变量解析
  的 RBAC 规则。
- 创建 `WorkerPool` 和 `ActorTemplate`。
- 创建 `mfcc-admin` Deployment 与 Service（用户管理 Web UI，见下文
  [用户管理 UI（Web 界面）](#用户管理-uiweb-界面)）。

Provider 配置存储在 Secret 中，并通过 `valueFrom.secretKeyRef` 引用，因此密钥不
会出现在 git 中。

### 3. 创建用户（Actor）

Actor 存在于 **atespace** 中，在创建 Actor 之前必须先创建 atespace。为每个用户
创建一个 Actor（**一个用户 = 一个 Actor**，数据彼此隔离）。推荐使用辅助脚本：

```bash
# 如果尚未安装，将 CLI 安装为 kubectl 插件
go install ./cmd/kubectl-ate

# 创建用户 alice（脚本会幂等处理：先检查存在性，无则建、有则复用，历史保留）
./create-user.sh alice
./create-user.sh bob

# 列出 / 删除用户
./list-users.sh
./delete-user.sh alice
```

`create-user.sh` 会自动确保 atespace `mfcc` 存在。等价的手动命令：

```bash
kubectl ate create atespace mfcc
kubectl ate create actor alice -a mfcc --template ate-demo-mf-cc/mf-cc
```

脚本创建的 Actor 初始状态为 `STATUS_SUSPENDED` — 它将在通过路由器发出第一个请
求时自动恢复（参见第 4 步；管理 UI 添加的用户则会被立即恢复，见下文）。使用以下
命令检查 Actor 状态：

```bash
kubectl ate get actor alice -a mfcc
```

### 4. 端口转发路由器

通过 Substrate 路由器访问 Actor：

```bash
# 端口转发 Atenet 路由器。选择一个空闲的主机端口（8080 可能已被本地服务占用，
# 因此 58880 是一个安全的选择）。
kubectl port-forward -n ate-system svc/atenet-router 58880:80
```

> [!NOTE]
> 也可直接运行 `./run-nginx.sh`，它会自动启动上述 port-forward（以及管理 UI 的
> `58882` port-forward）并运行 nginx 代理，无需手动执行。

每个用户的 Actor 的 DNS 地址为
`<username>.<atespace>.actors.resources.substrate.ate.dev`，即
`alice.mfcc.actors.resources.substrate.ate.dev`。

## 如何使用（多用户）

mf-cc 现在支持**多用户**：每个用户对应一个独立 Actor（数据彼此隔离），
各自拥有独立的内存/文件系统状态，可加载自己的历史会话。

用户通过**路径**访问：`http://<hostname>:58881/<username>`（例如
`http://localhost:58881/alice`）。

### 方式一：mfcc-nginx 代理（推荐）

`mfcc-nginx` 镜像是一个轻量级的 nginx 反向代理，监听 `58881` 端口，将所有请求
转发到 atenet-router 的端口转发地址（`58880`）。它从 URL 路径的第一段提取用户名
并设置 `Host: <username>.mfcc.actors.resources.substrate.ate.dev`，使请求路由到对
应用户的 Actor。

无需编辑 `/etc/hosts` — 直接在浏览器打开 `http://<hostname>:58881/<username>` 即可。

```bash
# 构建并运行（在 demos/mf-cc 目录中执行）
./build-image.sh
./run-nginx.sh
```

> [!NOTE]
> `run-nginx.sh` 会自动启动两个 kubectl port-forward（`58880` → atenet-router、
> `58882` → mfcc-admin 管理 UI；已被占用的端口会跳过），再运行 nginx 容器。

> [!NOTE]
> 容器使用 `--network host` 以访问宿主机环回地址上的 kubectl 端口转发。如果使用
> Docker Desktop（macOS / Windows），请去掉 `--network host` 并将
> `nginx.conf` 中的 `127.0.0.1:58880` 替换为 `host.docker.internal:58880`
> （代理目标），然后运行：
> `docker run -d -p 58881:58881 --name mfcc-nginx mfcc-nginx`。

停止并移除容器：

```bash
docker stop mfcc-nginx && docker rm mfcc-nginx
```

#### 工作原理（cookie 路由）

mf-cc 前端对 API / WebSocket / 静态资源的请求都是**相对 origin** 发出的
（`/api/...`、`/ws/<sessionId>`、`/assets/...`），URL 里不带用户名。因此代理采用
**cookie 路由**：

1. 首次访问 `/<username>` → nginx 写入 `mfcc_user` cookie，302 跳转到
   `/<username>/`（规范化路径，保证相对资源稳定解析）。
2. `/<username>/<rest...>` → 剥离 `/<username>` 前缀，设置对应 `Host` 头转发。
3. 后续 `/api/...`、`/ws/...`、`/assets/...`、`/health` 等系统路径 → 读取
   `mfcc_user` cookie 路由回同一 actor。

> [!NOTE]
> **保留名**：`api`、`assets`、`ws`、`sdk`、`callback`、`auth`、`proxy`、
> `preview-fs`、`local-file`、`health`、`usermanagement` 为系统保留前缀，不能作为
> 用户名（路径模式下会被当作系统路径处理）。`_mfcc_auth` 为 nginx 内部鉴权
> location（`internal`），不对外提供服务。

### 方式二：免 nginx 直连（curl + Host 头）

每个用户的 Actor DNS 名为 `<username>.mfcc.actors.resources.substrate.ate.dev`。
如需免 nginx 直连，可手动设置 Host 头：

```bash
# 使用 curl 直接携带 Host 请求头（以 alice 为例）：
curl -H "Host: alice.mfcc.actors.resources.substrate.ate.dev" http://127.0.0.1:58880/
```

通过路由器发出的第一个请求会自动恢复 Actor。初始响应可能为 `503`，因为服务
器正在启动 — 等待几秒后重试。确认 Actor 已变为 `STATUS_RUNNING`：

```bash
kubectl ate get actor alice -a mfcc
```

健康检查（服务器启动后应返回 `200`）：

```bash
# 通过代理（需先访问过 /alice 以写入 cookie）。若 alice 是通过 Web UI 创建、有
# 密码的用户，需携带 Basic Auth（用户名=alice，密码=分配的一次性密码）：
curl -u alice:<password> -H "Cookie: mfcc_user=alice" http://localhost:58881/health
# 或直连（手动 Host 头；绕过 nginx 鉴权）：
curl -H "Host: alice.mfcc.actors.resources.substrate.ate.dev" http://127.0.0.1:58880/health
```

Web UI 和 WebSocket（`/ws/<sessionId>` 的会话聊天）均可通过路由器工作 —
路由器的 RouteAction `upgradeConfigs` 允许 WebSocket 升级。

### 用户管理

创建、列出、删除用户（每个用户 = 一个 Actor）：

```bash
# 创建用户 alice（幂等：若已存在则复用，不重建，历史保留）
./create-user.sh alice
./create-user.sh bob

# 列出所有用户
./list-users.sh

# 删除用户 alice（连同其快照 / 历史一起删除）
./delete-user.sh alice
```

> **幂等语义**：`create-user.sh` 先检查 actor 是否已存在，存在则提示"reusing
> existing session"并跳过创建。这保证了"无实例则建、有实例则复用原历史"。直接对
> 已存在的 actor 名重复 `kubectl ate create actor` 会返回 `AlreadyExists` 错误。

### 用户管理 UI（Web 界面）

除脚本外，还提供了一个**基于 Web 的用户管理界面**。它随演示一并部署
（`deploy.sh` / `--deploy-demo-mf-cc`，`mfcc-admin` Deployment + Service，一个
纯 Go HTTP 服务器，直接通过 gRPC 调用 ate-api-server），功能与三个脚本一致：

- **列出用户**：名称、状态、模板、ATEOM Pod、IP、版本、年龄（对应
  `list-users.sh`）；用户名渲染为链接，点击在新标签页打开对应用户的
  Agent 页面（`http://<hostname>:58881/<username>/`）
- **添加用户**：幂等，已存在则复用现有会话；创建成功后会**自动生成一次性访问
  密码**（见下文「鉴权」），并**立即恢复**该 Actor，新建用户的 Agent 页面无需
  等待懒恢复即可直接打开（对应 `create-user.sh`；若恢复失败——例如没有空闲
  worker——用户仍会创建成功，页面会在首次访问时再触发恢复）
- **删除用户**：先挂起再删除（对应 `delete-user.sh`）

访问方式：`http://<hostname>:58881/usermanagement/`（经 mfcc-nginx 代理转发到
`mfcc-admin` Service）。

```bash
# 一条命令：启动 58880 / 58882 两个 port-forward + nginx 代理
./run-nginx.sh
```

等价的手动命令：

```bash
kubectl port-forward -n ate-system svc/atenet-router 58880:80
kubectl port-forward -n ate-demo-mf-cc svc/mfcc-admin 58882:8080
# 再构建并运行 nginx 代理：
./build-image.sh && docker run -d -p 58881:58881 --name mfcc-nginx --network host mfcc-nginx
```

> [!NOTE]
> `/usermanagement/` 是保留路径，**不能**作为用户名使用。

### 鉴权

mfcc-nginx 为多用户 Web UI 提供了两层鉴权：

- **管理页（`/usermanagement/`）**：固定 HTTP Basic Auth。默认账号密码为
  `admin` / `mf@pass2026`，可在运行 `run-nginx.sh` 时通过环境变量覆盖：
  `ADMIN_USER=... ADMIN_PASSWORD=... ./run-nginx.sh`。
- **用户 agent 页（`/<username>/...`）**：通过 Web UI 添加用户时，系统会**自动
  生成一个一次性密码**（在 UI 中只显示一次，请立即复制并告知该用户）。用户访问
  `http://<hostname>:58881/<username>/` 时，浏览器会弹出 Basic Auth 提示，填入
  **用户名 = `<username>`，密码 = 分配的一次性密码** 即可进入。

> [!NOTE]
> **所有用户的 agent 页都需要密码**。未分配密码的用户（例如通过 `create-user.sh`
> 脚本创建、或在鉴权功能上线前创建的旧用户）会被拒绝访问，需先在管理页该用户所在
> 行点击「**重置密码**」按钮生成密码后，才能进入。

如果某个用户的一次性密码丢失，可在管理页该用户所在行点击「**重置密码**」按钮，
生成一个新的密码（旧密码立即失效，新密码同样只显示一次）。

### 容量配置

`WorkerPool` 的副本数由环境变量 `MFCC_WORKER_REPLICAS` 控制，决定
**最大同时活跃（未挂起）用户数**：`./deploy.sh` 默认 `16`，
`hack/install-ate.sh` 默认 `4`。部署时设置：

```bash
MFCC_WORKER_REPLICAS=8 ./deploy.sh
# 或
MFCC_WORKER_REPLICAS=8 ... ./hack/install-ate.sh --deploy-demo-mf-cc
```

### 验证持久化

本演示不再挂载 `durableDir` 卷，会话历史与配置随容器的文件系统一起保存在
Full 快照中（`CLAUDE_CONFIG_DIR` 将其锚定在 `/root/.claude`）。确认它在
挂起/恢复后仍然存在：

```bash
# 在 UI 中为 alice 建立会话，然后挂起：
kubectl ate suspend actor alice -a mfcc
# 再将其恢复：
kubectl ate resume actor alice -a mfcc
```

恢复后，alice 的聊天历史应仍可加载。历史以 JSONL 保存在
`<CLAUDE_CONFIG_DIR>/projects/<sanitized-cwd>/<sessionId>.jsonl`（即
`/root/.claude/projects/.../xxx.jsonl`）。

## 测试环境（Test Environment）

除了上面的生产环境外，还提供了一套与生产**完全隔离**的测试环境，入口端口为
`59881`（生产 `58881`）。两者可同时运行在同一台机器 / 同一个集群上，互不冲突。

### 隔离概览

| 项 | 生产 | 测试 |
|---|---|---|
| 入口端口 | `58881` | `59881` |
| Namespace | `ate-demo-mf-cc` | `ate-demo-mf-cc-test` |
| Atespace | `mfcc` | `mfcc-test` |
| Router port-forward | `58880` | `59880` |
| Admin port-forward | `58882` | `59882` |
| Cookie 名 | `mfcc_user` | `mfcc_user_test` |
| nginx 容器名 | `mfcc-nginx` | `mfcc-nginx-test` |
| 工作负载标签 | `workload: mf-cc` | `workload: mf-cc-test` |
| 快照路径 | `gs://${BUCKET_NAME}/ate-demo-mf-cc/` | `gs://${BUCKET_NAME}/ate-demo-mf-cc-test/` |
| `MFCC_WORKER_REPLICAS` 默认 | `16`（deploy.sh）/ `4`（install-ate） | `2` |

> [!NOTE]
> **为什么需要这些隔离**：Atespace 是集群级资源；nginx cookie 忽略端口（同一宿主
> 上 `58881` 与 `59881` 共享 `mfcc_user` cookie）；worker 按标签调度且匹配范围是整
> 个集群；路由器按 Host 头的 atespace 路由。因此测试环境必须使用不同的 atespace、
> cookie 名、worker 标签和快照路径，才能与生产互不干扰。

### 部署

```bash
cd demos/mf-cc
ANTHROPIC_AUTH_TOKEN=<token> \
ANTHROPIC_BASE_URL=<base-url> \
ANTHROPIC_MODEL=<model> \
./deploy-test.sh
```

与 `deploy.sh` 一样，所有配置均为可选，默认值与生产相同
（`KO_DOCKER_REPO=localhost:5001`、`BUCKET_NAME=ate-snapshots`、
`ANTHROPIC_BASE_URL` / `ANTHROPIC_MODEL` 等），仅 `MFCC_WORKER_REPLICAS` 默认为
`2`。也可通过 install-ate harness 部署 / 卸载：

```bash
./hack/install-ate.sh --deploy-demo-mf-cc-test   # 部署
./hack/install-ate.sh --delete-demo-mf-cc-test   # 卸载
```

### 访问

```bash
cd demos/mf-cc
./run-nginx-test.sh
```

它会自动启动两个 kubectl port-forward（`59880` → atenet-router、`59882` →
`ate-demo-mf-cc-test` namespace 的 `mfcc-admin`），并运行 `mfcc-nginx-test` 容器
（监听 `59881`）。该容器复用同一个 `mfcc-nginx` 镜像，但通过 bind-mount
`nginx-test.conf` 覆盖镜像内 bake 的生产配置，并使用独立的 htpasswd 文件
（`/tmp/mfcc-admin-test.htpasswd`）。

- 用户 Agent 页：`http://localhost:59881/<username>`（cookie 为 `mfcc_user_test`）
- 管理 UI：`http://localhost:59881/usermanagement/`（账号密码同生产，默认
  `admin` / `mf@pass2026`，可用 `ADMIN_USER` / `ADMIN_PASSWORD` 覆盖）
- 免 nginx 直连：`curl -H "Host: <username>.mfcc-test.actors.resources.substrate.ate.dev" http://127.0.0.1:59880/`

> [!NOTE]
> 测试 nginx 的 Host 头使用 `mfcc-test` atespace、cookie 使用 `mfcc_user_test`。
> 若直接复用生产配置去访问测试端口，请求会被路由到生产 atespace 或读到生产 cookie，
> 因此**必须**使用 `nginx-test.conf`。

### 用户管理脚本

```bash
./create-user-test.sh alice   # 在 mfcc-test 建用户（模板 ate-demo-mf-cc-test/mf-cc）
./list-users-test.sh          # 列出测试用户
./delete-user-test.sh alice   # 删除测试用户
```

### 故障排查差异

- 测试环境的 WorkerPool / Deployment / Service 等资源都在 `ate-demo-mf-cc-test`
  namespace 中；排查命令只需把 namespace 换成 `ate-demo-mf-cc-test`。
- 清理遗留的测试 port-forward：`pkill -f "port-forward.*5988[02]"`（与生产的
  `5888x` 不冲突）。

## 故障排查

### 访问用户时返回 503：`no free workers available`

**现象**：浏览器打开 `http://<hostname>:58881/<username>`（或直连
`curl -H "Host: <username>.mfcc.actors.resources.substrate.ate.dev" http://127.0.0.1:58880/`）
返回 `503`，响应体为：

```
actor "<username>" unavailable: no free workers available
```

**原因**：WorkerPool 的 `replicas` 数量小于同时活跃（未挂起）的用户数。每个并发
活跃用户占用一个 worker；当所有 worker 都被占满时，新用户无法被恢复。

**排查**：

```bash
# 1) 查看 WorkerPool 副本数（DESIRED / REPLICAS）
kubectl get workerpool -n ate-demo-mf-cc

# 2) 查看当前有多少用户处于 STATUS_RUNNING（每个占用一个 worker）
./list-users.sh
```

如果 `REPLICAS` 小于（或等于）处于 `STATUS_RUNNING` 的用户数，就会出现该错误。

**解决**：

```bash
# 方式一：临时扩容（仅对当前集群生效；重新部署会被覆盖）
kubectl scale workerpool mf-cc-workerpool -n ate-demo-mf-cc --replicas=4

# 方式二：重新部署时设置 MFCC_WORKER_REPLICAS（持久化）
MFCC_WORKER_REPLICAS=4 ./hack/install-ate-kind.sh --deploy-demo-mf-cc
```

扩容后再次访问该用户即可触发恢复。

> [!NOTE]
> 旧集群可能是在 `MFCC_WORKER_REPLICAS` 默认值（4）生效之前部署的，其
> WorkerPool 可能仍是 `replicas: 1`。如果升级了 `install-demo-mf-cc.sh` 但未重新
> 部署，请用上面的方式一直接扩容，或用方式二重新部署。

### 首次访问返回 503（服务正在启动）

脚本创建的 Actor 初始为 `STATUS_SUSPENDED`，通过路由器发出第一个请求时才自动恢
复。首次请求可能返回 `503`（如 `upstream connect error ... connection failure`），
这是服务器仍在启动，**等待几秒后重试**即可。可通过 `./list-users.sh` 确认状态已
变为 `STATUS_RUNNING`。通过管理 UI 添加的用户会被立即恢复，通常不会遇到此情况。

### Actor 卡在 `STATUS_RESUMING`（gVisor sandbox 启动失败）

**现象**：Actor 一直处于 `STATUS_RESUMING`，无法恢复为 `STATUS_RUNNING`，且 worker
pod 日志反复出现：

```
FATAL ERROR: creating container: cannot create sandbox: cannot read client sync file: waiting for sandbox to start: EOF
```

**原因**：`ateom-gvisor` 工作负载镜像被错误地构建在过小的基础镜像（如
`pause`/scratch）之上，导致 gVisor sandbox 无法在该 rootfs 上初始化。这通常发生在
离线环境（无法访问 `gcr.io`）下，用 `KO_DEFAULTBASEIMAGE` 覆盖基础镜像时——该变量
会作用于**所有** ko 构建的镜像（包括 `ateom-gvisor`），从而把 worker 镜像也重建到
了错误的 base 上。

**解决**：用正确的基础镜像重建 `ateom-gvisor`，并把 WorkerPool 指回新镜像：

```bash
# 1) 把本地已缓存的基础镜像推入本地仓库（离线环境下 gcr.io 不可达）
docker tag gcr.io/distroless/static-debian13:latest localhost:5001/distroless-static-debian13:latest
docker push localhost:5001/distroless-static-debian13:latest

# 2) 用正确的 base 重建 ateom-gvisor（记下输出的 @sha256:... digest）
KO_DEFAULTBASEIMAGE=localhost:5001/distroless-static-debian13:latest \
  KO_DOCKER_REPO=localhost:5001 \
  ko build ./cmd/ateom-gvisor --platform=linux/amd64

# 3) 把 WorkerPool 指回新镜像
kubectl patch workerpool mf-cc-workerpool -n ate-demo-mf-cc --type=merge \
  -p '{"spec":{"ateomImage":"localhost:5001/ateom-gvisor-...@sha256:<digest>"}}'

# 4) 等待滚动更新完成，然后访问该用户触发恢复
kubectl rollout status deployment mf-cc-workerpool -n ate-demo-mf-cc --timeout=120s
```

> [!NOTE]
> 部署时若需覆盖基础镜像，务必指向**完整**的运行时（如
> `distroless/static-debian13`），而不是 `pause` 之类的极简镜像。覆盖纯静态 Go 二
> 进制（如管理 UI）的 base 是安全的，但覆盖 `ateom-gvisor` 会破坏 gVisor。

### `/usermanagement/` 打不开（404 或被当作用户路径路由）

**现象**：`http://<hostname>:58881/usermanagement/` 无法打开。

**原因**：`mfcc-nginx` 镜像是根据 `nginx.conf` 构建的。修改 `nginx.conf`（例如新增
`/usermanagement/` 路由）后**必须重新构建并重启该容器**，否则容器内仍是旧配置，
`/usermanagement/` 会落入用户路径正则而被路由到错误的 actor。

**解决**：

```bash
cd demos/mf-cc
./build-image.sh        # 重建 mfcc-nginx 镜像
docker rm -f mfcc-nginx # 移除旧容器
./run-nginx.sh          # 重新运行（会自动启动两个 port-forward）
```

同时确认管理 UI 的 port-forward 存在：

```bash
kubectl port-forward -n ate-demo-mf-cc svc/mfcc-admin 58882:8080
```

> [!NOTE]
> `/usermanagement/` 由 nginx 前缀 location 路由到 `127.0.0.1:58882`（管理 UI 的
> port-forward），它不属于任何 actor，也**不能**作为用户名。

## 如何卸载

从集群中移除 mf-cc 演示资源（包括 `mfcc-admin` 用户管理 UI 的
Deployment / Service / RBAC）：

```bash
./hack/install-ate.sh --delete-demo-mf-cc
```

> [!NOTE]
> 此演示使用 `onPause: Full` / `onCommit: Full` 快照（进程内存 + 文件系统差量，
> 不再挂载 `durableDir`）。mf-cc 是长期运行的 Web 服务器，完整的内存快照在挂起
> 时较慢，但无需额外的持久卷。恢复时进程从快照原样还原，但活跃的 WebSocket
> 连接会断开，需要刷新页面。
