# mf-pi 演示

此目录包含将 **mf-pi**（pi-web）Web UI 作为 Agent Substrate 上的 Actor 运行的演
示。pi-web 是一个 Node.js 的 AI 编码工作台，由 `pi-web-sessiond`（会话守护进程，
持有 agent 运行时与 unix socket）和 `pi-web-server`（Web UI / API，监听
`PI_WEB_PORT`）组成；两者共享 `/data` 持久状态。本演示在**单个 Actor 容器**内用
supervisor 同时运行两者（与 pi-web 仓库 `docker/scripts/run-container.sh` 相同的
形态），模型接入 DeepSeek（`DEEPSEEK_API_KEY`）。

将其作为 Substrate Actor 运行可获得挂起/恢复与快照的能力：挂起时对进程内存和容
器文件系统做 Full 快照，恢复时原样还原，会话、skills 与配置因此在挂起与恢复之间
持续存在。

## 前提条件

- 已安装 Agent Substrate 的 k8s 集群
  （`./hack/install-ate.sh --deploy-ate-system`）。
- 本地已构建的 pi-web 镜像 `pi-web:latest`（由 pi-web 仓库
  `docker/scripts/build-image.sh` 构建，本地 tag 也可用 `pi-web:local`）。该镜像
  必须能被集群节点访问。

> [!NOTE]
> **本地集群不需要真实的 GCS bucket。** 在 kind/k3s 上，快照存储在集群内部的对象
> 存储（rustfs）中；`BUCKET_NAME` 只是其中的逻辑 bucket 名称，
> `install-ate-kind.sh` 已将其设置为 `ate-snapshots`。真实的 GCS bucket
> （`gs://${BUCKET_NAME}`）仅在 GKE 上使用。

> [!IMPORTANT]
> 在本地集群上，节点无法访问外部镜像仓库，因此 pi-web 镜像**和** pause 镜像都必
> 须推送到本地仓库（`localhost:5001`）。部署脚本在镜像就位后会自动解析其摘要引
> 用。

### 1. 将镜像推送到集群的镜像仓库

在 kind 集群上（`KO_DOCKER_REPO=localhost:5001`），先将镜像本地化：

```bash
# pi-web 工作负载镜像（由 pi-web 仓库的 docker/scripts/build-image.sh 构建）
docker tag pi-web:latest localhost:5001/pi-web:latest
docker push localhost:5001/pi-web:latest

# pause 镜像（3.10.2；可使用任意可访问的镜像源，例如 rancher/mirrored-pause）
docker tag rancher/mirrored-pause:3.10.2 localhost:5001/pause:3.10.2
docker push localhost:5001/pause:3.10.2
```

### 2. 部署

在 `demos/mf-pi` 目录下运行 `./deploy.sh`。所有配置均为可选，未设置时回退到
kind/离线友好的默认值（`KO_DOCKER_REPO=localhost:5001`、
`BUCKET_NAME=ate-snapshots`、`MFPI_WORKER_REPLICAS=16`）。`DEEPSEEK_API_KEY` 未
导出时会尝试从上一次部署创建的 `mf-pi-provider-config` Secret 中读取：

```bash
cd demos/mf-pi
# 首次部署（或需要更换配置时）显式设置：
DEEPSEEK_API_KEY=<key> ./deploy.sh
# 之后直接重跑即可（key 从既有 Secret 读取，配置不变）：
./deploy.sh
```

也可通过 install-ate harness 部署（`DEEPSEEK_API_KEY`、`BUCKET_NAME`、
`KO_DOCKER_REPO` 必须设置；`MFPI_WORKER_REPLICAS` 可选，默认 `4`，决定最大同时
活跃用户数）：

```bash
DEEPSEEK_API_KEY=<key> \
BUCKET_NAME=ate-snapshots \
KO_DOCKER_REPO=localhost:5001 \
MFPI_WORKER_REPLICAS=4 \
./hack/install-ate-kind.sh --deploy-demo-mf-pi
```

两种方式都会：

- 从镜像仓库中解析 pi-web 和 pause 镜像的摘要固定引用。
- 创建 `ate-demo-mf-pi` 命名空间。
- 创建 provider-config `Secret` 以及允许 `ate-api-server` 读取它用于环境变量解
  析的 RBAC 规则。
- 创建 `WorkerPool` 和 `ActorTemplate`（Actor 容器只设 `args`，保留镜像
  ENTRYPOINT `tini -- pi-web-bootstrap`：首启安装内置 skills，随后 exec
  sessiond+web supervisor 并监听 80 端口）。
- 创建 `mfpi-admin` Deployment 与 Service（用户管理 Web UI，见下文
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

`create-user.sh` 会自动确保 atespace `mfpi` 存在。等价的手动命令：

```bash
kubectl ate create atespace mfpi
kubectl ate create actor alice -a mfpi --template ate-demo-mf-pi/mf-pi
```

脚本创建的 Actor 初始状态为 `STATUS_SUSPENDED` — 它将在通过路由器发出第一个请
求时自动恢复（参见第 4 步；管理 UI 添加的用户则会被立即恢复，见下文）。使用以下
命令检查 Actor 状态：

```bash
kubectl ate get actor alice -a mfpi
```

### 4. 端口转发路由器

通过 Substrate 路由器访问 Actor：

```bash
# 端口转发 Atenet 路由器。
kubectl port-forward -n ate-system svc/atenet-router 58680:80
```

> [!NOTE]
> 也可直接运行 `./run-nginx.sh`，它会自动启动上述 port-forward（以及管理 UI 的
> `58682` port-forward）并运行 nginx 代理，无需手动执行。

每个用户的 Actor 的 DNS 地址为
`<username>.<atespace>.actors.resources.substrate.ate.dev`，即
`alice.mfpi.actors.resources.substrate.ate.dev`。

## 如何使用（多用户）

mf-pi 支持**多用户**：每个用户对应一个独立 Actor（数据彼此隔离），各自拥有独立
的内存/文件系统状态，可加载自己的历史会话。

用户通过**路径**访问：`http://<hostname>:58681/<username>`（例如
`http://localhost:58681/alice`）。

### mfpi-nginx 代理

`mfpi-nginx` 镜像是一个轻量级的 nginx 反向代理，监听 `58681` 端口，将所有请求
转发到 atenet-router 的端口转发地址（`58680`）。它从 URL 路径的第一段提取用户名
并设置 `Host: <username>.mfpi.actors.resources.substrate.ate.dev`，使请求路由到对
应用户的 Actor。

无需编辑 `/etc/hosts` — 直接在浏览器打开 `http://<hostname>:58681/<username>` 即可。

```bash
# 构建并运行（在 demos/mf-pi 目录中执行）
./build-image.sh
./run-nginx.sh
```

> [!NOTE]
> `run-nginx.sh` 会自动启动两个 kubectl port-forward（`58680` → atenet-router、
> `58682` → mfpi-admin 管理 UI；已被占用的端口会跳过），再运行 nginx 容器。

> [!NOTE]
> 容器使用 `--network host` 以访问宿主机环回地址上的 kubectl 端口转发。

停止并移除容器：

```bash
docker stop mfpi-nginx && docker rm mfpi-nginx
```

#### 工作原理（路径路由 + cookie 兜底）

pi-web 前端所有请求（API `/api/...`、插件资源 `/pi-web-plugins/...`、WebSocket）
都是**相对 SPA 基址**发出的（vite `base: "./"` + `resolveAppUrl`），因此几乎全部
流量都天然带有 `/<username>/` 前缀：

1. 首次访问 `/<username>` → nginx 写入 `mfpi_user` cookie，302 跳转到
   `/<username>/`（规范化路径）。
2. `/<username>/<rest...>` → 剥离 `/<username>` 前缀，设置对应 `Host` 头转发。
   SPA、静态资源、API 与 WebSocket 全部走这一条路径（支持 Upgrade）。
3. 极少数 origin 绝对路径（favicon 等）→ `location /` fallback 按 `mfpi_user`
   cookie 路由回同一 actor。

> [!NOTE]
> **保留名**：`usermanagement` 为系统保留前缀，不能作为用户名（路径模式下会被
> 当作系统路径处理）。`_mfpi_auth` 为 nginx 内部鉴权 location（`internal`），不对
> 外提供服务。

### 用户管理 UI（Web 界面）

提供了一个**基于 Web 的用户管理界面**。它随演示一并部署（`deploy.sh` /
`--deploy-demo-mf-pi`，`mfpi-admin` Deployment + Service，一个纯 Go HTTP 服务
器，直接通过 gRPC 调用 ate-api-server），功能与三个脚本一致：

- **列出用户**：名称、状态、模板、ATEOM Pod、IP、版本、年龄（对应
  `list-users.sh`）；用户名渲染为链接，点击在新标签页打开对应用户的
  Agent 页面（`http://<hostname>:58681/<username>/`）
- **添加用户**：幂等，已存在则复用现有会话；创建成功后会**自动生成一次性访问
  密码**（见下文「鉴权」），并**立即恢复**该 Actor，新建用户的 Agent 页面无需
  等待懒恢复即可直接打开（若恢复失败——例如没有空闲 worker——用户仍会创建成
  功，页面会在首次访问时再触发恢复）
- **删除用户**：先挂起再删除（对应 `delete-user.sh`）
- **DeepSeek Key**：每行显示该用户是否已设专属 DeepSeek API Key（「已设置 /
  未设置」徽标），并提供「**设置 Key**」/「**清除**」按钮，为指定用户动态注入或
  退出其专属 key（详见下文「每用户专属 DeepSeek API Key」）

访问方式：`http://<hostname>:58681/usermanagement/`（经 mfpi-nginx 代理转发到
`mfpi-admin` Service）。

```bash
# 一条命令：启动 58680 / 58682 两个 port-forward + nginx 代理
./run-nginx.sh
```

等价的手动命令：

```bash
kubectl port-forward -n ate-system svc/atenet-router 58680:80
kubectl port-forward -n ate-demo-mf-pi svc/mfpi-admin 58682:8080
# 再构建并运行 nginx 代理：
./build-image.sh && docker run -d -p 58681:58681 --name mfpi-nginx --network host mfpi-nginx
```

> [!NOTE]
> `/usermanagement/` 是保留路径，**不能**作为用户名使用。

### 鉴权

mfpi-nginx 为多用户 Web UI 提供了两层鉴权：

- **管理页（`/usermanagement/`）**：固定 HTTP Basic Auth。默认账号密码为
  `admin` / `mf@pass2026`，可在运行 `run-nginx.sh` 时通过环境变量覆盖：
  `ADMIN_USER=... ADMIN_PASSWORD=... ./run-nginx.sh`。
- **用户 agent 页（`/<username>/...`）**：通过 Web UI 添加用户时，系统会**自动
  生成一个一次性密码**（在 UI 中只显示一次，请立即复制并告知该用户）。用户访问
  `http://<hostname>:58681/<username>/` 时，浏览器会弹出 Basic Auth 提示，填入
  **用户名 = `<username>`，密码 = 分配的一次性密码** 即可进入。

> [!NOTE]
> **所有用户的 agent 页都需要密码**。未分配密码的用户（例如通过 `create-user.sh`
> 脚本创建的用户）会被拒绝访问，需先在管理页该用户所在行点击「**重置密码**」按
> 钮生成密码后，才能进入。

如果某个用户的一次性密码丢失，可在管理页该用户所在行点击「**重置密码**」按钮，
生成一个新的密码（旧密码立即失效，新密码同样只显示一次）。

### DeepSeek 模型

pi-web 内建 `deepseek` provider（OpenAI 兼容，`https://api.deepseek.com`），无需
改代码。部署时把 `DEEPSEEK_API_KEY` 注入 Actor 即可；用户在模型选择器中选择
`deepseek/deepseek-chat` 或 `deepseek/deepseek-reasoner`。

### 每用户专属 DeepSeek API Key

`DEEPSEEK_API_KEY` env 是**共享**的（来自 ActorTemplate 上固定的
`secretKeyRef`），且平台层无法在运行期给某个 Actor 单独注入 env/Secret：env 冻结
在 Full 快照里、恢复时原样还原，也没有外部途径写入 Actor 文件系统。因此本演示为
**单个用户动态设置 / 清除其专属 DeepSeek API key** 的方式是：经路由器驱动该用户
Actor 内 pi-web 自身的 api-key 登录流程，把凭据写进该 Actor 的 `auth.json`
（`/data/pi-agent/auth.json`）。一个用户 == 一个 Actor，正好构成 per-user 的 key
面。完整设计见 `injectDeepsseekKey.md`。

pi-web 每次模型调用都会重读该凭据文件，**已存储的凭据优先于** `DEEPSEEK_API_KEY`
env（无需重启）；清除后该用户回退到 env key。

**界面（管理 UI）**：`/usermanagement/` 每行显示「DeepSeek Key」徽标（已设置 /
未设置），并可用「**设置 Key**」（弹出输入 `sk-...`）或「**清除**」按钮操作。

**CLI**（经临时 port-forward 调 mfpi-admin REST `api/users/<name>/apikey`）：

```bash
./set-user-apikey.sh alice sk-...    # 设置（覆盖）；actor 挂起时自动先恢复
./clear-user-apikey.sh alice         # 清除（幂等）；agent 回退到 env key
# 测试环境（atespace mfpi-test）：
./set-user-apikey-test.sh alice sk-...
./clear-user-apikey-test.sh alice
```

**持久化**：key 同时写入预创建的 Secret `mfpi-user-provider-keys`（username →
key，见 `mf-pi.yaml.tmpl` 与 `mf-pi-test.yaml.tmpl`）。UI 据此显示徽标；Actor 删除
重建后 key 仍在 Secret 中，重新创建用户时会自动重放注入；删除用户时该 key 一并清
除。

**语义**：

- 设置 = **先写 Secret，再**（必要时恢复并）注入；注入失败返回 502 并说明「key 已
  存储」，可重试或等下次恢复时自动应用（提交记录优先）。
- 清除 = **先**驱动 actor 退出 DeepSeek 登录（回退 env），**成功后才**删除 Secret
  条目；logout 失败返回 502 且保留条目；actor 已删除时直接清空 Secret 条目（幂
  等）。
- 设置 / 清除都会在需要时自动恢复 `SUSPENDED` 的 actor。挂起 / 恢复（Full 快照）
  保留 `auth.json`，因此已设的 key 在挂起 / 恢复后依然生效。

### 容量配置

`WorkerPool` 的副本数由环境变量 `MFPI_WORKER_REPLICAS` 控制，决定
**最大同时活跃（未挂起）用户数**：`./deploy.sh` 默认 `16`，
`hack/install-ate.sh` 默认 `4`。部署时设置：

```bash
MFPI_WORKER_REPLICAS=8 ./deploy.sh
# 或
MFPI_WORKER_REPLICAS=8 ... ./hack/install-ate-kind.sh --deploy-demo-mf-pi
```

### 验证持久化

本演示不挂载 `durableDir` 卷，会话历史与 skills 随容器的文件系统一起保存在
Full 快照中（session JSONL 位于 `/data/pi-agent/sessions/`）。确认它在挂起/恢复
后仍然存在：

```bash
# 在 UI 中为 alice 建立会话，然后挂起：
kubectl ate suspend actor alice -a mfpi
# 再将其恢复：
kubectl ate resume actor alice -a mfpi
```

恢复后，alice 的聊天历史应仍可加载（sessiond 的 unix socket 在恢复后需重新建
立，web 可能短暂 503，稍候重试即可）。

## 测试环境（Test Environment）

除了上面的生产环境外，还提供了一套与生产**完全隔离**的测试环境，入口端口为
`59881`（生产 `58681`）。两者可同时运行在同一台机器 / 同一个集群上，互不冲突。

### 隔离概览

| 项 | 生产 | 测试 |
|---|---|---|
| 入口端口 | `58681` | `59881` |
| Namespace | `ate-demo-mf-pi` | `ate-demo-mf-pi-test` |
| Atespace | `mfpi` | `mfpi-test` |
| Router port-forward | `58680` | `59880` |
| Admin port-forward | `58682` | `59882` |
| Cookie 名 | `mfpi_user` | `mfpi_user_test` |
| nginx 容器名 | `mfpi-nginx` | `mfpi-nginx-test` |
| 工作负载标签 | `workload: mf-pi` | `workload: mf-pi-test` |
| 快照路径 | `gs://${BUCKET_NAME}/ate-demo-mf-pi/` | `gs://${BUCKET_NAME}/ate-demo-mf-pi-test/` |
| `MFPI_WORKER_REPLICAS` 默认 | `16`（deploy.sh）/ `4`（install-ate） | `2` |

> [!NOTE]
> **为什么需要这些隔离**：Atespace 是集群级资源；nginx cookie 忽略端口（同一宿主
> 上 `58681` 与 `59881` 共享 `mfpi_user` cookie）；worker 按标签调度且匹配范围是整
> 个集群；路由器按 Host 头的 atespace 路由。因此测试环境必须使用不同的 atespace、
> cookie 名、worker 标签和快照路径，才能与生产互不干扰。

### 部署

```bash
cd demos/mf-pi
DEEPSEEK_API_KEY=<key> ./deploy-test.sh
# 或
./hack/install-ate.sh --deploy-demo-mf-pi-test   # 部署
./hack/install-ate.sh --delete-demo-mf-pi-test   # 卸载
```

### 访问

```bash
cd demos/mf-pi
./run-nginx-test.sh
```

它会自动启动两个 kubectl port-forward（`59880` → atenet-router、`59882` →
`ate-demo-mf-pi-test` namespace 的 `mfpi-admin`），并运行 `mfpi-nginx-test` 容器
（监听 `59881`）。该容器复用同一个 `mfpi-nginx` 镜像，但通过 bind-mount
`nginx-test.conf` 覆盖镜像内 bake 的生产配置，并使用独立的 htpasswd 文件
（`/tmp/mfpi-admin-test.htpasswd`）。

- 用户 Agent 页：`http://localhost:59881/<username>`（cookie 为 `mfpi_user_test`）
- 管理 UI：`http://localhost:59881/usermanagement/`（账号密码同生产，默认
  `admin` / `mf@pass2026`，可用 `ADMIN_USER` / `ADMIN_PASSWORD` 覆盖）
- 免 nginx 直连：`curl -H "Host: <username>.mfpi-test.actors.resources.substrate.ate.dev" http://127.0.0.1:59880/`

> [!NOTE]
> 测试 nginx 的 Host 头使用 `mfpi-test` atespace、cookie 使用 `mfpi_user_test`。
> 若直接复用生产配置去访问测试端口，请求会被路由到生产 atespace 或读到生产
> cookie，因此**必须**使用 `nginx-test.conf`。

### 用户管理脚本

```bash
./create-user-test.sh alice           # 在 mfpi-test 建用户（模板 ate-demo-mf-pi-test/mf-pi）
./list-users-test.sh                  # 列出测试用户
./delete-user-test.sh alice           # 删除测试用户
./set-user-apikey-test.sh alice sk-...  # 设置测试用户专属 DeepSeek Key
./clear-user-apikey-test.sh alice     # 清除测试用户专属 DeepSeek Key
```

## 故障排查

### 访问用户时返回 503：`no free workers available`

**原因**：WorkerPool 的 `replicas` 数量小于同时活跃（未挂起）的用户数。每个并发
活跃用户占用一个 worker；当所有 worker 都被占满时，新用户无法被恢复。

**排查**：

```bash
kubectl get workerpool -n ate-demo-mf-pi
./list-users.sh
```

**解决**：

```bash
# 方式一：临时扩容（仅对当前集群生效；重新部署会被覆盖）
kubectl scale workerpool mf-pi-workerpool -n ate-demo-mf-pi --replicas=4

# 方式二：重新部署时设置 MFPI_WORKER_REPLICAS（持久化）
MFPI_WORKER_REPLICAS=4 ./hack/install-ate-kind.sh --deploy-demo-mf-pi
```

### 首次访问返回 503（服务正在启动）

脚本创建的 Actor 初始为 `STATUS_SUSPENDED`，通过路由器发出第一个请求时才自动恢
复。首次请求可能返回 `503`——sessiond 启动与 web 监听需要数秒（单容器内先
sessiond、后 web），**等待几秒后重试**即可。可通过 `./list-users.sh` 确认状态已
变为 `STATUS_RUNNING`。通过管理 UI 添加的用户会被立即恢复，通常不会遇到此情况。

### `/usermanagement/` 打不开（404 或被当作用户路径路由）

**原因**：`mfpi-nginx` 镜像是根据 `nginx.conf` 构建的。修改 `nginx.conf` 后**必须
重新构建并重启该容器**，否则容器内仍是旧配置。

**解决**：

```bash
cd demos/mf-pi
./build-image.sh        # 重建 mfpi-nginx 镜像
docker rm -f mfpi-nginx # 移除旧容器
./run-nginx.sh          # 重新运行（会自动启动两个 port-forward）
```

同时确认管理 UI 的 port-forward 存在：

```bash
kubectl port-forward -n ate-demo-mf-pi svc/mfpi-admin 58682:8080
```

## 如何卸载

从集群中移除 mf-pi 演示资源（包括 `mfpi-admin` 用户管理 UI 的
Deployment / Service / RBAC）：

```bash
./hack/install-ate.sh --delete-demo-mf-pi
# 测试环境：
./hack/install-ate.sh --delete-demo-mf-pi-test
```

> [!NOTE]
> 此演示使用 `onPause: Full` / `onCommit: Full` 快照（进程内存 + 文件系统差量，
> 不挂载 `durableDir`）。pi-web 是长期运行的 Web 服务器，完整的内存快照在挂起
> 时较慢，但无需额外的持久卷。恢复时进程从快照原样还原，但活跃的 WebSocket
> 连接会断开，需要刷新页面。
