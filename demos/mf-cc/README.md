# mf-cc 演示

此目录包含将 **mf-cc**（cc-haha）Web UI 作为 Agent Substrate 上的 Actor 运行的演
示。mf-cc 是一个基于 Bun 的 HTTP + WebSocket 服务器，提供基于 React 的 AI 编码工
作台（REST API 在 `/api/*`，WebSocket 在 `/ws/*`，静态前端在 `/*`）。

将其作为 Substrate Actor 运行可获得挂起/恢复、快照和持久化存储的能力，使得会话和
配置在挂起与恢复之间能够持续存在。

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

设置 provider 环境变量（参见 cc-haha 仓库中的 `.env` 文件），然后部署。可选设置
`MFCC_WORKER_REPLICAS`（默认 `4`，决定最大同时活跃用户数）：

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

此命令将会：

- 从镜像仓库中解析 mf-cc 和 pause 镜像的摘要固定引用。
- 创建 `ate-demo-mf-cc` 命名空间。
- 创建 provider-config `Secret` 以及允许 `ate-api-server` 读取它用于环境变量解析
  的 RBAC 规则。
- 创建 `WorkerPool` 和 `ActorTemplate`。

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

Actor 初始状态为 `STATUS_SUSPENDED` — 它将在通过路由器发出第一个请求时自动恢复
（参见第 4 步）。使用以下命令检查 Actor 状态：

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

每个用户的 Actor 的 DNS 地址为
`<username>.<atespace>.actors.resources.substrate.ate.dev`，即
`alice.mfcc.actors.resources.substrate.ate.dev`。

## 如何使用（多用户）

mf-cc 现在支持**多用户**：每个用户对应一个独立 Actor，拥有独立的
`durableDir`（数据隔离），可加载各自的历史会话。

用户通过**子域名**访问：`http://<username>.localhost:58881`。有两种方式：

- **方案 A：mfcc-nginx 代理（推荐）。** 构建并运行本地 nginx 反向代理，从子域名
  自动提取用户名并设置对应 `Host` 头 — 无需编辑 `/etc/hosts`。
- **方案 B：/etc/hosts + Host 请求头。** 通过 `/etc/hosts` 或 curl 手动设置
  Host 请求头。

### 方案 A：mfcc-nginx 代理（推荐）

`mfcc-nginx` 镜像是一个轻量级的 nginx 反向代理，将所有请求转发到
atenet-router 的端口转发地址（`58880`）。它监听 `58881` 端口。

它通过正则从子域名提取用户名（`~^(?<username>[a-z0-9-]+)\.localhost$`），并设置
`Host: <username>.mfcc.actors.resources.substrate.ate.dev`，使请求路由到对应用户
的 Actor。访问示例：`http://alice.localhost:58881`、`http://bob.localhost:58881`。

无需编辑 `/etc/hosts` — 只需在浏览器中打开 `http://<username>.localhost:58881`。

> [!NOTE]
> **`*.localhost` 解析**：按 RFC 6761 / systemd-resolved，`*.localhost` 通常默认
> 解析到 `127.0.0.1`。若你的环境不解析子域，可在 `/etc/hosts` 中显式添加：
> `127.0.0.1 alice.localhost bob.localhost`。

```bash
# 构建并运行（在 demos/mf-cc 目录中执行）
./build-image.sh
./run-nginx.sh
```

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

### 方案 B：/etc/hosts + Host 请求头

每个用户的 Actor DNS 名为 `<username>.mfcc.actors.resources.substrate.ate.dev`。
如需免 nginx 直连，可手动设置 Host 头：

```bash
# 使用 curl 直接携带 Host 请求头（以 alice 为例）：
curl -H "Host: alice.mfcc.actors.resources.substrate.ate.dev" http://127.0.0.1:58880/
```

或通过 `/etc/hosts` + 浏览器访问：

```bash
# /etc/hosts（一次性操作，可加多个用户）：
echo "127.0.0.1 alice.mfcc.actors.resources.substrate.ate.dev" | sudo tee -a /etc/hosts
# 然后打开 http://alice.mfcc.actors.resources.substrate.ate.dev:58880
```

通过路由器发出的第一个请求会自动恢复 Actor。初始响应可能为 `503`，因为服务
器正在启动 — 等待几秒后重试。确认 Actor 已变为 `STATUS_RUNNING`：

```bash
kubectl ate get actor alice -a mfcc
```

健康检查（服务器启动后应返回 `200`）：

```bash
curl -H "Host: alice.mfcc.actors.resources.substrate.ate.dev" http://127.0.0.1:58880/health
```

4. Web UI 和 WebSocket（`/ws/<sessionId>` 的会话聊天）均可通过路由器工作 —
   路由器的 RouteAction `upgradeConfigs` 允许 WebSocket 升级。

### 用户管理

创建、列出、删除用户（每个用户 = 一个 Actor）：

```bash
# 创建用户 alice（幂等：若已存在则复用，不重建，历史保留）
./create-user.sh alice
./create-user.sh bob

# 列出所有用户
./list-users.sh

# 删除用户 alice（连同其 durableDir / 历史一起删除）
./delete-user.sh alice
```

> **幂等语义**：`create-user.sh` 先检查 actor 是否已存在，存在则提示"reusing
> existing session"并跳过创建。这保证了"无实例则建、有实例则复用原历史"。直接对
> 已存在的 actor 名重复 `kubectl ate create actor` 会返回 `AlreadyExists` 错误。

### 容量配置

`WorkerPool` 的副本数由环境变量 `MFCC_WORKER_REPLICAS` 控制（默认 `4`），决定
**最大同时活跃（未挂起）用户数**。部署时设置：

```bash
MFCC_WORKER_REPLICAS=8 ... ./hack/install-ate.sh --deploy-demo-mf-cc
```

### 验证持久化

会话历史与配置存储在挂载于 `/root/.claude` 的 `durableDir` 中（由
`CLAUDE_CONFIG_DIR` 显式锚定）。确认它在挂起/恢复后仍然存在：

```bash
# 在 UI 中为 alice 建立会话，然后挂起：
kubectl ate suspend actor alice -a mfcc
# 再将其恢复：
kubectl ate resume actor alice -a mfcc
```

恢复后，alice 的聊天历史应仍可加载。历史以 JSONL 保存在
`<CLAUDE_CONFIG_DIR>/projects/<sanitized-cwd>/<sessionId>.jsonl`（即
`/root/.claude/projects/.../xxx.jsonl`）。

## 如何卸载

从集群中移除 mf-cc 演示资源：

```bash
./hack/install-ate.sh --delete-demo-mf-cc
```

> [!NOTE]
> 此演示使用 `onPause: Data` / `onCommit: Data` 快照。由于 mf-cc 是一个长期运
> 行的 Web 服务器，每次挂起时进行完整的内存快照会很慢；会话数据改为通过
> `durableDir` 持久化。恢复时进程从快照重新启动，并从磁盘读取其持久化状态，因此
> 活跃的 WebSocket 连接会断开，需要刷新页面。
