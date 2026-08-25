# mf-cc 用户管理 Web UI 方案

## 目标

在 `demos/mf-cc/` 下新增一个用户管理 Web 应用：
- 3 个 REST API：列出用户、创建用户、删除用户。
- 中文 Web UI（列表 / 添加 / 删除），展示 actor 状态。
- 应用以 Pod 形式运行，用户通过 `http://<hostname>:58881/usermanagement/` 访问（经由现有 mfcc-nginx 代理）。

## 已确认的决策

- **API 后端**：通过 `ateapipb.ControlClient` 直接调用 ate-api-server gRPC（**不**在 Pod 内执行脚本），行为与 `create-user.sh` / `delete-user.sh` / `list-users.sh` 保持一致。
- **UI 语言**：中文。

## 关键发现（集群内认证模式）

集群内组件（如 `ate-controller`、`atenet-router`）通过以下方式认证访问 ateapi gRPC 服务：
- 投射的 `serviceAccountToken` 卷，`audience: api.ate-system.svc`
- 投射的 `clusterTrustBundle` 卷，`signerName: servicedns.podcert.ate.dev/identity`
- 通过 `internal/ateapiauth.DialOptions` 拨号（`UseTokenAuth=true`，`CAFile` = 挂载的信任包，`ServerName=api.ate-system.svc`）

因此管理应用**无需额外 RBAC**，且是纯 Go 二进制 → 用 **ko**（`ko://...`）构建，无需 Dockerfile，镜像内无需 kubectl。参考：`manifests/ate-install/token-client/patches.yaml`、`cmd/atecontroller/main.go:58`。

## 新增文件

### `demos/mf-cc/admin/main.go`（package main）

基于标准库的 HTTP 服务器，监听 `:8080`，拨号 ateapi 并提供：

- `GET  /api/users` → `ListActors(atespace)`；返回 `{"users":[{name, template, status, ateomPod, ip, version, age}], "atespace": "mfcc"}`（按名称排序）。对应 `list-users.sh`。
- `POST /api/users` body `{"name":"alice"}` → 对应 `create-user.sh`：
  1. 校验名称是否符合 DNS-1123（`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`），不合法返回 400
  2. `GetAtespace`；NotFound 则 `CreateAtespace`（自动创建，同脚本）
  3. `GetActor`；已存在则返回 200 + 消息 `用户已存在，复用现有会话`（幂等，同脚本）
  4. `CreateActor`（template ns/name、atespace、name）
  5. 返回 200 + 创建的 actor 摘要
- `DELETE /api/users/{name}` → 对应修复后的 `delete-user.sh`：
  1. 校验名称；`GetActor` 不存在返回 404
  2. `SuspendActor`（幂等：已 SUSPENDED 则为 no-op）
  3. `DeleteActor`
  4. 返回 200 + 消息
- `GET  /` → 提供内嵌的 `index.html`（`//go:embed index.html`）。

环境变量配置（含集群内默认值）：
`PORT`(8080)、`ATESPACE`(mfcc)、`ACTOR_TEMPLATE_NAMESPACE`(ate-demo-mf-cc)、`ACTOR_TEMPLATE_NAME`(mf-cc)、
`ATEAPI_ADDR`(`dns:///api.ate-system.svc:443`)、`ATEAPI_CA_FILE`(`/run/servicedns-ca/trust-bundle.pem`)、`ATEAPI_TOKEN_FILE`(`/run/ateapi-token/token`)。

结构：定义小型 `controlClient` 接口，包装所用到的 `ateapipb.ControlClient` 方法（ListActors/GetActor/CreateActor/DeleteActor/SuspendActor/GetAtespace/CreateAtespace），便于用 fake 做单元测试。年龄渲染复用 `k8s.io/apimachinery/pkg/util/duration`。导入：`internal/ateapiauth`、`pkg/proto/ateapipb`、`google.golang.org/grpc`。

### `demos/mf-cc/admin/index.html`

自包含单页中文 UI（内联 CSS/JS，无构建步骤），通过 `go:embed` 内嵌。
- 使用**相对路径** fetch（`fetch("api/users")`、`fetch("api/users/"+name, {method:"DELETE"})`），以便在 `/usermanagement/` nginx 前缀下工作。
- 功能：添加用户表单（DNS-1123 校验）、用户列表表格（名称/状态/模板/ATEOM Pod/IP/版本/年龄/操作）、彩色状态徽章（RUNNING 绿 / SUSPENDED 灰 / SUSPENDING·PAUSING 蓝 / CRASHED 红）、删除按钮带 `confirm()`、消息/错误区域、刷新按钮 + 每 5 秒自动刷新。

### `demos/mf-cc/admin/main_test.go`

用 fake `controlClient` 做单元测试：
- list handler：解析/排序/JSON 结构
- create handler：非法名称 → 400；自动创建 atespace；重复 → 200「已存在」
- delete handler：actor 不存在 → 404；suspend+delete 正常路径

## 修改文件

### `demos/mf-cc/mf-cc.yaml.tmpl` — 追加 Deployment + Service

追加两个资源（无需 RBAC）：
- `Deployment mfcc-admin`（ns `ate-demo-mf-cc`）：
  - `image: ko://github.com/agent-substrate/substrate/demos/mf-cc/admin`
  - env：ATESPACE、ACTOR_TEMPLATE_NAMESPACE、ACTOR_TEMPLATE_NAME、ATEAPI_ADDR、ATEAPI_CA_FILE、ATEAPI_TOKEN_FILE
  - `volumeMounts` + `volumes`：`ateapi-token`（projected serviceAccountToken，audience `api.ate-system.svc`，path `token`）和 `servicedns-ca`（projected clusterTrustBundle，signer `servicedns.podcert.ate.dev/identity`，label `podcert.ate.dev/canarying: live`，path `trust-bundle.pem`）——拷贝 `manifests/ate-install/token-client/patches.yaml` 的形状。
  - `containerPort: 8080`
- `Service mfcc-admin`（ns `ate-demo-mf-cc`）→ selector `app: mfcc-admin`，端口 8080→8080。

`install-demo-mf-cc.sh` 中的 `run_ko apply` 会自动构建/推送 ko 镜像；无需 digest 替换。

### `demos/mf-cc/nginx.conf` — 将 `/usermanagement/` 路由到管理应用

增加一个前缀 location（优先于正则用户路径 location）：
```nginx
location ^~ /usermanagement/ {
    proxy_pass http://127.0.0.1:58882/;
    proxy_set_header Host $host;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```
（`proxy_pass` 带尾部 `/` 会去掉 `/usermanagement/` 前缀，因此 `/usermanagement/api/users` → 应用 `/api/users`。）同时在注释中把 `usermanagement` 列为保留名。

### `demos/mf-cc/run-nginx.sh` — 启动管理应用的 port-forward

启动两个后台 port-forward（容忍已存在），再运行 nginx；`trap` 在退出时清理：
- `kubectl port-forward -n ate-system svc/atenet-router 58880:80`
- `kubectl port-forward -n ate-demo-mf-cc svc/mfcc-admin 58882:8080`

### `demos/mf-cc/README.md` — 文档化管理 UI

- 新增章节：访问 `http://<hostname>:58881/usermanagement/`（需已部署 demo + `run-nginx.sh` 或手动两个 port-forward）。
- 注明 `usermanagement` 是保留路径，不能作为用户名。
- 在部署/卸载章节提及 `mfcc-admin` Deployment/Service 属于 `--deploy-demo-mf-cc` / `--delete-demo-mf-cc`。

### `hack/install-demo-mf-cc.sh` — 小改动

- `demo-mf-cc_deploy` 中 `run_ko apply` 后追加 `run_kubectl rollout status deployment/mfcc-admin -n ate-demo-mf-cc --timeout=120s`。
- 更新 `demo-mf-cc_usage` 提及管理 UI。（删除路径已通过模板删除 Deployment/Service。）

## 部署与访问流程

1. `MFCC_WORKER_REPLICAS=4 ... ./hack/install-ate-kind.sh --deploy-demo-mf-cc` → ko 构建 `demos/mf-cc/admin`，应用 Deployment+Service。
2. `kubectl port-forward -n ate-system svc/atenet-router 58880:80` 与 `kubectl port-forward -n ate-demo-mf-cc svc/mfcc-admin 58882:8080`（或 `./run-nginx.sh` 同时启动两个 + nginx）。
3. 打开 `http://localhost:58881/usermanagement/`。

## 测试

- `go test ./demos/mf-cc/admin/`（带 fake client 的单元测试）。
- 手动冒烟：部署 demo，打开 UI，列出用户（应显示现有 mfcc/alice actor），添加用户，删除用户，确认状态流转。

## 注意事项

- 管理 UI 仅操作**配置的 atespace**（默认 `mfcc`）。管理其他 atespace 需修改 Deployment 中的 `ATESPACE` 环境变量。
- 创建是幂等的（复用已有 actor 及其历史），与 `create-user.sh` 一致；UI 显示「已存在，复用」消息。
- 删除先挂起（suspend）再删除，与修复后的 `delete-user.sh` 一致（API 拒绝删除 RUNNING 状态的 actor）。
