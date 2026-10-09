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
- 本地已构建的 pi-web 镜像 `mf-agent:latest`（由 pi-web 仓库
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
docker tag mf-agent:latest localhost:5001/mf-agent:latest
docker push localhost:5001/mf-agent:latest

# pause 镜像（3.10.2；可使用任意可访问的镜像源，例如 rancher/mirrored-pause）
docker tag rancher/mirrored-pause:3.10.2 localhost:5001/pause:3.10.2
docker push localhost:5001/pause:3.10.2
```

> [!IMPORTANT]
> **重命名 Actor 容器/镜像会破坏既有快照。** Actor 挂起时把整个容器的运行状态
> 存成快照，快照里记录了容器**名**（例如 `pi-web`）与其 spec。如果你改了
> `ActorTemplate` 里的容器名（例如改成 `mf-agent`）或镜像，那么**在重命名前挂起的
> 所有 Actor** 恢复时都会失败：
> `checkpoint image does not contain spec for container:"<新名>"`（`runsc restore`
> exit 128），并会一直卡在 `STATUS_RESUMING`。这与镜像内容无关（改名后 digest 相同），
> 纯粹是**容器名与快照 spec 不匹配**。见
> [故障排查：Actor 无法恢复（卡在 STATUS_RESUMING）](#actor-无法恢复卡在-status_resuming)。

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
  sessiond+web supervisor 并监听 80 端口）。ActorTemplate 声明一个 sticky 的
  **userdata 外部卷**（`externalVolumeTemplate`）挂载到 `/data/pi-agent`，用于
  用户数据持久化（见下文
  [用户数据持久化](#用户数据持久化重置后自动恢复)）。
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
> `58682` → mfpi-admin 管理 UI；已被占用且健康的端口会跳过，若隧道已失效——接受
> TCP 但始终无响应——则会自动替换为新隧道）并运行 nginx 容器。

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
- **删除用户**：先挂起再删除（对应 `delete-user.sh`），并**同时 purge 该用户的
  持久卷**（见下文「用户数据持久化 → 彻底删除（purge）」）
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
重建后 key 仍在 Secret 中，重新创建用户时会自动重放注入。此外 `mfpi-admin` 内有
一个每 30s 的 reconcile 循环：为已 `RUNNING` 但凭据尚未注入的 actor 重放 stored
key，因此任何 resume 路径（包括 `refresh-actor.sh` 的删除重建）都会自动找回 key；
删除用户时该 key 一并清除。

**语义**：

- 设置 = **先写 Secret，再**（必要时恢复并）注入；注入失败返回 502 并说明「key 已
  存储」，可重试或等下次恢复时自动应用（提交记录优先）。
- 清除 = **先**驱动 actor 退出 DeepSeek 登录（回退 env），**成功后才**删除 Secret
  条目；logout 失败返回 502 且保留条目；actor 已删除时直接清空 Secret 条目（幂
  等）。
- 设置 / 清除都会在需要时自动恢复 `SUSPENDED` 的 actor。挂起 / 恢复（Full 快照）
  保留 `auth.json`，因此已设的 key 在挂起 / 恢复后依然生效。

### 统一安装 Skill（管理员分发到所有用户）

`mfpi-admin` 是**唯一权威源**：管理员在 `/usermanagement/` 上传 / 卸载 skill，
平台自动把它分发到**所有用户（每个用户 = 一个 pi-web Actor）**，安装到每个
actor 的 `/data/pi-agent/skills`，新会话自动生效，已打开的会话可用「**立即应
用**」触发 reload 热加载。完整设计与实现进度见 `deploy_skill_to_actor.md`。

**分发机制（Actor 自拉）**：admin 把 skill 目录存到 PVC（`mfpi-shared-skills`，
挂 `$SKILLS_DIR=/var/lib/mfpi-skills`，`replicas: 1` 固化），暴露两个只读端点
`/internal/skills/manifest` 与 `/internal/skills/<name>.tgz`；每个 actor 的
supervisor 每 10s 用 `If-None-Match` 条件拉取 manifest，按 `sha256` 增量覆盖安
装到 `skills/`。manifest 中消失且曾被托管的 skill 会被移除；**从未托管的用户自
建 skill 一律不动**（统一安装不破坏用户私有数据）。网络 / 解析失败静默跳过，
绝不阻塞 web/sessiond 主进程。

**立即应用（可选加速）**：对每个 `RUNNING` 用户经 router 扇出 pi-web 的
session reload（`GET /api/projects` → `GET /api/sessions?cwd=` →
`POST /api/sessions/:id/reload`），使已打开会话无需重启即加载新版。这是尽力而
为：有活跃工作的会话会在下次开会话时生效。

**界面（管理 UI）**：`/usermanagement/` 顶部新增「**共享 Skills**」卡片——列
表（名称 / 大小 / 更新时间 / 删除）、上传（`.tgz` / `.zip` 或目录，自动打包）、
「**立即应用**」按钮（应用后显示「已应用到 N/M 个在线用户」）。

**CLI**（经临时 port-forward 调 mfpi-admin REST `api/skills`）：

```bash
./install-skill.sh mofang-form ./mofang-form.tgz   # 或 .zip / 目录；自动打包
./list-skills.sh                                    # 列出已托管 skill
./remove-skill.sh mofang-form                       # 卸载（仅删托管副本）
./apply-skills.sh                                   # 立即应用（扇出 reload）
# 测试环境（atespace mfpi-test）：
./install-skill-test.sh mofang-form ./mofang-form.tgz
./list-skills-test.sh
./remove-skill-test.sh mofang-form
./apply-skills-test.sh
```

> [!TIP]
> **完整端到端验证**可直接运行
> `./test-skill-distribution.sh`（测试环境）：自动创建测试用户并跑通「上传新
> skill → actor 10s 拉取安装 → 增量更新 v2 → 卸载 + 用户自建 skill 隔离保护 →
> 立即应用（扇出 reload）」全流程并逐项断言，无需手动操作。

**安全与边界**：上传校验——名称必须为 DNS-1123 slug，包内必须含 `SKILL.md`，
tar/zip 解包拒绝 `../` 与绝对路径条目（zip-slip），单文件大小上限 32 MiB、整包
64 MiB。管理端 `GET /internal/skills/*` 不做鉴权（actor 侧 env 冻结进 golden 快
照，token 轮换对已恢复的 actor 不生效），如需要可用 NetworkPolicy 限制来源。

### 用户数据持久化（重置后自动恢复）

每个用户的数据（`/data/pi-agent` 下的 `auth.json`、skills、`sessions/`、
`models.json`、`settings.json`）保存在一个 **sticky 的 per-actor 持久卷**上
（ActorTemplate 声明的 `externalVolumeTemplate` 卷 `userdata`，挂载到
`/data/pi-agent`），使「删除并重建」式的实例重置（例如刷新 actor 镜像后重新部
署）**不再丢失用户数据**。

#### PV 如何挂载到 actor

- `ActorTemplate` 声明 `externalVolumeTemplate` 卷 `userdata`（5Gi，
  `storageClassName: standard`），容器的 `volumeMounts` 把它挂到
  `/data/pi-agent`。
- 控制面按 **actor 稳定名**生成卷 ID：`<atespace>-<actorName>-<volName>`
  （例如 `mfpi-alice-userdata`）。sticky 卷插件的 backing 目录就是 worker 节点上
  的 `/var/lib/ateom-gvisor/stickyvolumes/<卷ID>`；`CreateVolume` 幂等（同名卷
  直接复用），`DeleteVolume` 刻意保留目录（回收存储见下文「彻底删除（purge）」）。
- actor 启动 / 恢复时，节点侧（ateom）把 backing 目录 symlink 到沙箱挂载点。
  因此 `/data/pi-agent` **就是**持久卷本身：actor 直接读写，无需 supervisor 同
  步、broker 或 token，actor 内不持有任何存储凭据。

#### 哪些数据会自动恢复

| 数据 | 存放位置 | 删除重建（refresh）后 |
|---|---|---|
| 会话历史、`auth.json`、`settings.json`、models 等 | PV（`/data/pi-agent`） | ✅ 原样恢复 |
| skills（镜像内置） | 镜像 `/opt/pi-web/skills` → supervisor 恢复循环补装 | ✅ 自动补齐 |
| project 工作目录（PV 外，如 `/dtom2`） | 沙箱临时层 | ⚠️ 目录自动重建（历史内容不恢复） |

后两项由 actor supervisor 内的两个**后台恢复循环**（每 10s）处理：

- **skills 恢复**：镜像内置 skills 由 `pi-web-bootstrap` 只在**首次启动**时装
  入；而新用户的沙箱是从 golden base 快照**恢复**的（bootstrap 已在 golden
  actor 里跑过、装到了 golden 卷上），不会再执行，新用户会「没有 skill」。
  supervisor 循环按 bootstrap 相同的**不覆盖**语义，把缺失的 skill 从镜像补装
  到用户 PV；用户已有的 skill（可能被修改）一律不动。
- **project 目录恢复**：会话文件（PV 上）记录了工作目录（如 `/dtom2`），而该目
  录本身若建在 PV 外，刷新后即消失，pi agent 会拒绝打开会话（"Stored session
  working directory does not exist"）。supervisor 循环读取 PV 上持久化的
  `projects.json`，为每个 project 路径 `mkdir -p` 重建空目录。

> [!NOTE]
> 为什么必须是 supervisor 内的**后台循环**而不是一次性启动步骤：refresh 后沙箱
> 是从快照（golden base 或挂起 checkpoint）**恢复**的，恢复的进程从快照断点继
> 续运行，启动期的一次性步骤永远不会重跑；循环进程被恢复后会继续运行，每次
> resume 后数秒内即完成重建。

#### 彻底删除（purge）

删除 actor **刻意不删**持久卷数据——这正是「重置 = 删除 + 重建」后数据能自动恢
复的原因（`delete-user.sh` 同样只删 actor、保留数据）。只有**删除用户**才回收
存储，两条途径：

```bash
# 1) 管理流程：admin 删除用户（管理 UI「删除」按钮 / DELETE /api/users/<name>）
#    会在删除 actor 后自动 purge 其持久卷；purge 失败会留下孤儿卷，可再用方式 2 清理

# 2) 手动 purge（按模板推导卷 ID，actor 记录可已不存在；RUNNING 的 actor 会被拒绝）
./remove-pv.sh tom3 --test        # 测试环境（mfpi-test）
./remove-pv.sh alice              # 生产环境（mfpi）
# 等价 CLI：
kubectl ate purge volumes <user> -a <atespace> -t <templateNS>/<templateName>
```

#### 检查与验证工具

- `./exec-actor.sh <user> [--test]`：在节点上打开该用户 PV 的 shell（以
  `data/pi-agent/` 视图呈现，即 actor 内 `/data/pi-agent` 的内容）。
- `./list-pvs.sh [--test] [--detail] [--show-auth]`：列出所有用户 PV 及数据概
  况（actor 状态、大小、mtime、文件数、顶层内容；`--show-auth` 会输出
  `auth.json`，含 API key，谨慎使用）。
- `./test-userdata-persistence.sh`：端到端验证——写入哨兵文件 → refresh → 读
  回，并用 `runsc exec` 校验刷新后 actor 内 `/data/pi-agent` 仍挂载且哨兵可读。

**重置一个用户并验证自动恢复**：

```bash
./delete-user.sh alice        # 删除 Actor（持久卷数据保留）
./create-user.sh alice        # 重建同名 Actor（挂起态）
kubectl ate resume actor alice -a mfpi   # 恢复 → 自动挂回同一持久卷
```

恢复后，alice 的专属 DeepSeek key（`auth.json`）、已安装的 skills 与会话历史会自动
出现，**无需**重新驱动注入。完整设计见 `save_userdata_pv.md` 与
`perUserDataVolume.md`。

### 容量配置

`WorkerPool` 的副本数由环境变量 `MFPI_WORKER_REPLICAS` 控制，决定
**最大同时活跃（未挂起）用户数**：`./deploy.sh` 默认 `16`，
`hack/install-ate.sh` 默认 `4`。部署时设置：

```bash
MFPI_WORKER_REPLICAS=8 ./deploy.sh
# 或
MFPI_WORKER_REPLICAS=8 ... ./hack/install-ate-kind.sh --deploy-demo-mf-pi
```

### 空闲自动挂起（释放资源）

为**mid / small 资源档位**的用户优化资源利用率：当这类 Actor **超过
`IDLE_TIMEOUT`（默认 `90m`）没有任何输入**时，mfpi-admin 后台 reconciler（每
30s）会自动 `SuspendActor` 释放其 worker/Pod 资源（快照与用户数据保留）；用户
下次访问时由路由器**懒恢复**重新载入，该次访问同时重新计时。

- **「输入」信号**：nginx 对每个 `/<username>/` 请求转发前都会执行
  `auth_request /_mfpi_auth`，因此 admin 服务**成功鉴权**即是一次可靠输入
  （含 API 轮询与 WebSocket 升级）。
- **范围**：仅 `IDLE_TIERS`（默认 `small,mid`）内的档位生效；**large 档位
  Actor 永不因空闲被自动挂起**（设计上常驻）。
- 被挂起的用户下次访问时会先看到「**Agent 正在载入**」提示页（见
  [Agent 载入中的提示页](#agent-载入中的提示页)），恢复完成后自动进入界面。
- **配置**：可在 `mf-pi.yaml.tmpl` / `mf-pi-test.yaml.tmpl` 的 mfpi-admin env
  中调整 `IDLE_TIMEOUT` 与 `IDLE_TIERS`（也可直接用 `kubectl set env` 覆盖）。
- 与「激活有效期」的区别：有效期是管理员设定的**绝对到期**；空闲挂起是**相对
  最近一次输入**的自动回收，属于平台级的资源复用策略。

```bash
kubectl -n ate-demo-mf-pi set env deploy/mfpi-admin IDLE_TIMEOUT=45m IDLE_TIERS=small,mid
```

### Agent 载入中的提示页

访问一个**尚未就绪**的 Agent（已挂起需要唤醒，或正在启动、Web 服务还没起来）
时，用户会立刻看到一个「Agent 正在载入」的提示页，而不是白屏或裸错误页：

- 显示旋转指示器与**基于实时 Actor 状态**的文案：
  - `RESUMING` / `SUSPENDED` → 「正在唤醒 Agent…（从快照恢复）」
  - `RUNNING` → 「Agent 正在启动…（Web 服务即将就绪）」
  - `SUSPENDING` / `PAUSING` / `PAUSED` → 「正在切换状态…」
  - `CRASHED` → 「正在重启…」
  - 用户不存在 → 「用户不存在」（不再自动刷新）
- **每 3 秒自动重试**，显示已等待秒数；等待超过 60 秒会提示可能是**没有空闲
  worker** 或恢复较慢，并给出「立即重试」按钮与用户管理入口。
- 页面由 admin 服务根据 Actor 状态动态渲染，纯内联 HTML/CSS/JS，不依赖任何外部
  资源（Actor 未就绪时也无从加载资源）。页面带稳定的 `mfpi-page=loading` 标记，
  供 E2E 测试识别。

#### 为什么需要「就绪门」（readiness gate）

访问已挂起的 Actor 时，**路由器不会返回 5xx**：它会在 `ext_proc` 里阻塞重试
（最多 15s）直到恢复完成，然后直接把请求代理过去。健康的挂起 Actor 因此表现为
「白屏约 4 秒后突然出现真实页面」——nginx 没有任何错误可拦截，提示页也就无从触
发。为了**立刻**给出反馈，nginx 对用户路径改用 `auth_request /_mfpi_gate`：

| gate 返回 | 含义 | nginx 行为 |
|---|---|---|
| `401` | 缺少/错误凭据 | 浏览器弹出 Basic Auth 提示 |
| `403` | 凭据正确但 Actor 未 `RUNNING` | 浏览器 → 提示页；API/WS → 原始 `503` |
| `200` | Actor 已 `RUNNING` | 正常代理到 Actor |

`/_mfpi_gate` 在返回 `403` 的同时**在后台触发恢复**（去重，避免 3 秒自动刷新堆叠
恢复请求），因此提示页刷新几次后 Actor 就绪，自动进入真实界面。对于「已
`RUNNING` 但 pi-web 还在启动」的窗口，`proxy_intercept_errors` 把上游
`502/503/504` 也映射到同一个提示页。

**只对浏览器导航返回 HTML**：`/_mfpi_loading` 内部 location 仅在 `Accept` 含
`text/html` 时返回提示页；API（`Accept: application/json`）、静态资源与
WebSocket 升级请求仍然收到原始 `503`，因此 SPA 自身的重试/报错逻辑不受影响，也
不会把 HTML 误当作 JSON。

#### 端到端验证

```bash
cd demos/mf-pi
# 1) 让 admin 与 nginx 都跑上当前代码（gate 是新增端点）
./deploy-test.sh          # 重新构建并部署测试环境 admin（ko apply）
./run-nginx-test.sh       # 用当前 nginx-test.conf 重建测试 nginx 容器
# 2) 跑 E2E：新建用户 → 挂起 → 首次访问应立刻拿到提示页 → 自动变为真实页面
./test-loading-page.sh
```

`test-loading-page.sh` 断言：首次导航在 3 秒内返回提示页（含 `mfpi-page=loading`
标记）并带自动刷新；随后自动变为真实 pi-web 页面（`PI WEB`）；未就绪时 API 请求
收到原始 `503` 而非 HTML；`/_mfpi_gate`、`/_mfpi_loading` 从外部不可达（404）。

> [!IMPORTANT]
> 提示页需要**两侧都更新**，缺一会看到旧行为（白屏数秒后出现真实页面）：
> * **nginx**：拦截规则在 `nginx.conf` / `nginx-test.conf` 内，**改后必须重建
>   `mfpi-nginx` 镜像并重启容器**（`./build-image.sh && docker rm -f mfpi-nginx &&
>   ./run-nginx.sh`；测试环境用 `./run-nginx-test.sh`，它 bind-mount 配置文件，
>   重建容器即可）。
> * **mfpi-admin**：`/_mfpi_gate`、`/_mfpi_loading` 是新端点，旧镜像会 404，需
>   重新部署（`./deploy.sh` / `./deploy-test.sh`）。

也可以手动验证生产环境（挂起后打开页面，应立刻看到「正在唤醒 Agent…」）：

```bash
kubectl ate suspend actor alice -a mfpi
# 浏览器打开 http://<hostname>:58681/alice/（需该用户访问密码）
# 或 curl（带上 Basic Auth 密码）确认返回的是提示页：
curl -s -u alice:<用户密码> -H 'Accept: text/html' http://localhost:58681/alice/ | head -5
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

> [!TIP]
> 「删除并重建」式持久化的完整端到端验证可直接运行
> `./test-userdata-persistence.sh`（测试环境）：自动创建测试用户、写入哨兵文件、
> refresh、读回并断言，另用 `runsc exec` 校验刷新后 actor 内 `/data/pi-agent`
> 仍挂载且哨兵可读，无需手动操作。

> [!NOTE]
> 挂起 / 恢复（Full 快照）只能保住**同一实例**的会话。若想验证「**删除并重建**
> 同一用户」后数据仍在，见上文
> [用户数据持久化](#用户数据持久化重置后自动恢复)：数据保存在 sticky 持久卷上，
> 重建后自动重新挂载。

## 开发：运行 admin 服务单元测试

`mfpi-admin`（用户管理后端，`demos/mf-pi/admin/`）的单元测试全部位于
`main_test.go`（以及 `skills_test.go`），覆盖用户增删查、鉴权、激活有效期/资源
档位、空闲自动挂起、每用户 DeepSeek Key 注入与 skill 分发等逻辑。它们**不需要
集群或 gRPC 服务**——控制面通过内存 fake 客户端模拟，直接本地运行：

```bash
cd demos/mf-pi/admin

# 运行全部单元测试（main_test.go + skills_test.go）
go test ./...

# 详细输出每个用例
go test -v .

# 运行某个具体用例 / 名称前缀（-run 为正则，匹配用例名）
go test -run 'ReconcileIdle' -v .

# 运行并查看覆盖
go test -cover ./...
```

> [!NOTE]
> 请勿使用 `go test main_test.go` 这种「只编译该文件」的写法：`main_test.go` 与
> 实现文件同属 `package main`，单独编译会因找不到 `server`、`newFake` 等符号而
> 构建失败；`main_test.go` 与 `skills_test.go` 也无法按文件分别运行。始终用
> `go test .`（或 `./...`）让整个包一起编译，再用 `-run <正则>` 选取具体用例。

用例速查：

| 用例 | 覆盖内容 |
|---|---|
| `TestHandleAuth*` | nginx `/_mfpi_auth` 鉴权与输入记录（`s.idle.touch`） |
| `TestReconcileExpire*` | 激活有效期到期自动挂起 |
| `TestReconcileIdle*` | 空闲自动挂起（仅 mid/small、久闲、非活跃跳过等） |
| `TestHandleSetTier*` / `TestHandleCreateUserUsesTierTemplate` | 资源档位设置与模板选择 |
| `TestHandleSetAPIKey*` / `TestReconcileInjectsKey*` | 每用户 DeepSeek Key 注入 |
| `TestSkill*` / `TestInternalManifestETag` / `TestApplySkills*` | skill 分发与「立即应用」扇出 |
| `TestHandleListUsers*` / `TestHandleDeleteUser*` | 列表汇总与删除清理 |

> [!NOTE]
> 若本地 `GOFLAGS`/GOPATH 缓存权限受限导致构建报错，可指定一个可写的
> `GOCACHE` 后重试：`GOCACHE=/tmp/mfpi-gocache go test ./...`。

## 测试环境（Test Environment）

除了上面的生产环境外，还提供了一套与生产**完全隔离**的测试环境，入口端口为
`59681`（生产 `58681`）。两者可同时运行在同一台机器 / 同一个集群上，互不冲突。

### 隔离概览

| 项 | 生产 | 测试 |
|---|---|---|
| 入口端口 | `58681` | `59681` |
| Namespace | `ate-demo-mf-pi` | `ate-demo-mf-pi-test` |
| Atespace | `mfpi` | `mfpi-test` |
| Router port-forward | `58680` | `59880` |
| Admin port-forward | `58682` | `59882` |
| Cookie 名 | `mfpi_user` | `mfpi_user_test` |
| nginx 容器名 | `mfpi-nginx` | `mfpi-nginx-test` |
| 工作负载标签 | `workload: mf-pi` | `workload: mf-pi-test` |
| 快照路径 | `gs://${BUCKET_NAME}/ate-demo-mf-pi/` | `gs://${BUCKET_NAME}/ate-demo-mf-pi-test/` |
| userdata 持久卷 | sticky per-actor 卷（随 ActorTemplate） | 同左（test ns 内） |
| `MFPI_WORKER_REPLICAS` 默认 | `16`（deploy.sh）/ `4`（install-ate） | `2` |

> [!NOTE]
> **为什么需要这些隔离**：Atespace 是集群级资源；nginx cookie 忽略端口（同一宿主
> 上 `58681` 与 `59681` 共享 `mfpi_user` cookie）；worker 按标签调度且匹配范围是整
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
（监听 `59681`）。该容器复用同一个 `mfpi-nginx` 镜像，但通过 bind-mount
`nginx-test.conf` 覆盖镜像内 bake 的生产配置，并使用独立的 htpasswd 文件
（`/tmp/mfpi-admin-test.htpasswd`）。

- 用户 Agent 页：`http://localhost:59681/<username>`（cookie 为 `mfpi_user_test`）
- 管理 UI：`http://localhost:59681/usermanagement/`（账号密码同生产，默认
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

### 端到端测试脚本

```bash
./test-loading-page.sh                # 「Agent 正在载入」提示页 + 就绪门（见上文）
./test-skill-distribution.sh          # 管理员统一安装 Skill 的分发全流程
./test-userdata-persistence.sh        # 用户数据持久化（删除重建后自动恢复）
```

> [!NOTE]
> `test-loading-page.sh` 依赖 **当前版本** 的 mfpi-admin（`/_mfpi_gate`）与
> `nginx-test.conf`：请先 `./deploy-test.sh` 与 `./run-nginx-test.sh`，否则脚本会
> 明确报错/跳过而不是给出误导性的通过。

## 故障排查

### 访问用户时返回 503：`no free workers available`

**原因**：WorkerPool 的 `replicas` 数量小于同时活跃（未挂起）的用户数。每个并发
活跃用户占用一个 worker；当所有 worker 都被占满时，新用户无法被恢复。

在浏览器里这表现为「**Agent 正在载入**」提示页长时间不消失（等待超过 60 秒时会
提示可能没有空闲 worker）；用 `curl`/API 直接访问仍会看到原始
`actor "..." unavailable: no free workers available` 的 503。

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
sessiond、后 web）。

在浏览器里**不会再看到裸 503**：nginx 会改用「**Agent 正在载入**」提示页（见
[Agent 载入中的提示页](#agent-载入中的提示页)），页面每 3 秒自动重试，Actor 就绪
后自动进入正常界面。若通过 API / `curl` 访问，仍会收到原始 `503`，**等待几秒后
重试**即可。可通过 `./list-users.sh` 确认状态已变为 `STATUS_RUNNING`。通过管理
UI 添加的用户会被立即恢复，通常不会遇到此情况。

### Actor 无法恢复（卡在 STATUS_RESUMING）

**现象**：某个 Actor 一直处于 `STATUS_RESUMING`，浏览器一直看到「Agent 正在载
入」或直接报错；`kubectl ate get actor <u>` 不掉出 `STATUS_RESUMING`；atelet 日志
出现：

```
failed to validate restore spec: checkpoint image does not contain spec for container: "<name>"
while running `runsc restore`: exit status 128
```

**原因**：该 Actor 挂起时的**快照是按旧容器名/旧模板生成的**，而当前
`ActorTemplate` 的容器名或镜像与之不匹配（最常见是把容器名 `pi-web` 改成
`mf-agent`、或换了镜像/改动了容器 spec）。恢复时 runsc 按当前模板的容器名去快照里
找 spec，找不到就 exit 128。恢复失败不是「崩溃」（atelet 只对带 `actorCrashed`
标记的错误才置 CRASHED），所以恢复工作流会**无限重试**，Actor 卡死在
`STATUS_RESUMING`；`delete`/`suspend` 也会因「不是 SUSPENDED」被拒绝。

**解决（迁移到一个新的、不带旧快照的 Actor，保留用户数据）**：

关键：被删除 Worker 后，恢复工作流发现「已分配的 worker 不存在」，会把 Actor 释放
回 `STATUS_SUSPENDED`，从而允许 `delete`。若只删一个 worker，池里的其他 worker
会立刻把 Actor 重新拾起继续重试，所以要先把该池 `replicas` 缩到 **0**（清空该池
worker，确保没有 worker 能再拾起它）。

```bash
# 0) 该 Actor 属于哪个 WorkerPool？例如 mf-pi-workerpool / mf-pi-wp-small
kubectl ate get actor <u> -a <atespace>

# 1) 把该池缩到 0，等 Actor 回到 SUSPENDED（这也会释放 CPU：kind 单节点容易
#    CPU 打满导致新 worker Pending）
kubectl patch workerpool <pool> -n <ns> --type merge -p '{"spec":{"replicas":0}}'
until kubectl ate get actor <u> -a <atespace> -o json \
  | jq -r '.actors[0].status // .status' | grep -q SUSPENDED; do sleep 2; done

# 2) 删除并重建（当前模板 → 无旧快照，冷启动；sticky 用户数据卷会原样挂回）
kubectl ate delete actor <u> -a <atespace>
kubectl ate create actor <u> -a <atespace> --template <template>

# 3) 把池扩回原副本数
kubectl patch workerpool <pool> -n <ns> --type merge -p '{"spec":{"replicas":2}}'

# 4) 恢复（等有 worker 再 resume；冷启动后会生成新的、匹配当前模板的快照）
kubectl ate resume actor <u> -a <atespace>
```

**注意**：
- 一个池里可能有多个 Actor（例如 `mf-pi-wp-small` 同时有 alice、bob）。把池缩到 0
  会把同池**所有** RUNNING/RESUMING 的 Actor 都释放为 SUSPENDED；要在第 2 步把同池
  所有需要迁移的 Actor 一起删建，再在第 3 步扩回并逐个恢复。
- 重命名容器后，**所有**在重命名前挂起过的 Actor 都要这样迁移（它们都带着旧名快
  照）；否则下次自动挂起/恢复又会卡死。
- 迁移会中断在线用户（其进行一次冷启动），但用户数据（`/data/pi-agent` 等）在
  sticky 卷上**不丢失**。
- 若只是 CPU 不足（新 worker 卡 Pending、`Insufficient cpu`）而没有任何 Actor 卡
  死不散，把没有用户使用的 mid/large 池缩到 0 即可释放 CPU，不必动 Actor。

### 上传大文件返回 413（`Request Entity Too Large`）

**原因**：nginx 的 `client_max_body_size` 内置默认是 **1m**；超过就会被 nginx 直接
以 `413` 拒绝，请求根本到不了 pi-web / mfpi-admin。

**当前配置**：`nginx.conf` 与 `nginx-test.conf` 的 server 块都显式设为
**`client_max_body_size 512m;`**（覆盖 pi-web 上传与管理 UI 的 skill 包；mfpi-admin
自身对 skill 包的限额是 64MiB/包、32MiB/文件）。要调整上限就改这两处，然后重建
nginx 容器/镜像。

**排查**：

```bash
# 看运行的容器里生效值（应为 512m；若为 1m 或缺失，说明容器是旧配置）
docker exec mfpi-nginx nginx -T | grep client_max_body_size
# 重建生效（生产）：
./build-image.sh && docker rm -f mfpi-nginx && ./run-nginx.sh
# 测试环境（bind-mount nginx-test.conf，重建容器即可）：
docker rm -f mfpi-nginx-test && ./run-nginx-test.sh
```

> [!NOTE]
> 管理 UI（`/usermanagement/`）的 skill 上传**经过 nginx**，受此限制；CLI
> `./install-skill.sh` 走临时 port-forward 直连 mfpi-admin，**不经过 nginx**，因此
> 不受该限制。

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

> [!NOTE]
> 卸载（`--delete-demo-mf-pi` / `--delete-demo-mf-pi-test`）会删除 Actor 与
> ActorTemplate 等资源；用户数据保存在 worker 节点上的 sticky 卷目录
> （`/var/lib/ateom-gvisor/stickyvolumes/`）中，该目录不随命名空间删除。若需在
> 卸载后彻底清除用户数据，可在卸载前对每个用户运行 `./remove-pv.sh <user>
> [--test]` 逐个回收，或到对应 worker 节点删除该目录。
