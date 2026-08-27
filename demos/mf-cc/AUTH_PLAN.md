# mf-cc Web UI 鉴权方案

## TODO

- [x] `admin/main.go`：新增 `passwordStore` 接口 + `configMapPasswordStore`（client-go 读写 ConfigMap）
- [x] `admin/main.go`：新增 `generatePassword` / `hashPassword` / `checkPassword`
- [x] `admin/main.go`：`handleCreateUser` 生成密码并返回明文；`handleDeleteUser` 清理存储
- [x] `admin/main.go`：新增 `handleResetPassword`（`POST /api/users/{name}/password`）
- [x] `admin/main.go`：新增 `handleAuth`（`/_mfcc_auth`）；`main()` 接入 client-go、注册路由
- [x] `admin/index.html`：创建成功后展示一次性密码；新增「重置密码」按钮
- [x] `admin/main_test.go`：补充 auth / create / reset / hash 单测
- [x] `nginx.conf`：`/usermanagement/` 加 `auth_basic`；新增 `/_mfcc_auth` 内部 location；三个 proxy 类 location 加 `auth_request`
- [x] `mfcc-admin.htpasswd`：新增默认 admin 凭据文件
- [x] `Dockerfile`：`COPY mfcc-admin.htpasswd`
- [x] `run-nginx.sh`：从 `ADMIN_USER`/`ADMIN_PASSWORD` 生成 htpasswd 并挂载
- [x] `mf-cc.yaml.tmpl`：新增 ServiceAccount / Role / RoleBinding；Deployment 加 `serviceAccountName` 与 env
- [x] `README.md`：补充鉴权章节、更新 health-check 示例与保留名
- [x] 运行 `go test ./demos/mf-cc/admin/` 并手动冒烟验证

## 目标

给 `demos/mf-cc` 的多用户 Web UI 增加两层鉴权：

1. **管理页（usermanagement）**：固定用户名/密码即可进入 → 用 **nginx Basic Auth**。
2. **每个用户的 agent 页**：Web UI 创建用户时自动生成一个密码，用户只能用该密码进入
   对应 agent 页（`/<username>/...`）→ 用 **nginx `auth_request`** 转发到 Go admin
   服务校验。

## 已确认的决策

- 管理页鉴权：**nginx Basic Auth**（固定账号密码）。
- 每用户密码存储：**Kubernetes ConfigMap**（`ate-demo-mf-cc/mfcc-user-passwords`），
  重启 admin Pod 后密码依然有效。
- **所有用户的 agent 页都需要密码**：未分配密码的用户（`create-user.sh` 创建、
  或鉴权上线前的旧用户）会被拒绝访问，需在管理页用「重置密码」生成密码后才能进入。

## 关键设计

- **管理页**：`nginx.conf` 的 `/usermanagement/` location 加 `auth_basic` +
  `auth_basic_user_file`。凭据由 `run-nginx.sh` 从 `ADMIN_USER`/`ADMIN_PASSWORD`
  （默认 `admin`/`mf@pass2026`）用 `openssl passwd -apr1` 生成 htpasswd 并挂载进容器；
  Dockerfile 内烘焙一份默认 htpasswd 作为兜底。
- **每用户密码**：admin Go 服务创建用户时用 `crypto/rand` 生成随机密码，哈希后写入
  ConfigMap，并在创建响应里返回明文（UI 只显示一次）。
- **agent 页鉴权**：nginx 用 `auth_request /_mfcc_auth;` 在转发前校验。`/_mfcc_auth`
  是 `internal` location，把原始 URI 与 `mfcc_user` cookie 传给 Go 服务的
  `/_mfcc_auth` 端点；Go 服务解析目标用户名（优先取路径首段，其次取 cookie），
  若无存储哈希→放行（脚本创建的用户）；有哈希→校验 Basic Auth（用户名须等于目标
  用户，密码须匹配）。
- **密码哈希**：stdlib 实现（`crypto/rand` 盐 + `crypto/sha256`/`crypto/hmac` 迭代），
  格式 `盐$迭代次数$哈希`。避免引入 `golang.org/x/crypto/bcrypt`（未 vendor）。
  demo 级别够用，README 注明可换 bcrypt。
- **ConfigMap 读写**：用已 vendored 的 `k8s.io/client-go`（`rest.InClusterConfig` +
  `kubernetes.Clientset`）。admin Pod 用专用 SA `mfcc-admin`（默认 token 供 k8s API，
  已有的 projected `ateapi-token` 仍供 ateapi，互不影响）。

### nginx 相位注意点（重要）

`auth_request` 属于 **access 相位**，而 `return`/`rewrite` 属于 **rewrite 相位**
（更早），因此 **`return 302` 会跳过 `auth_request`**。所以：

- `/<username>`（无斜杠）入口 location **保持不变**（仍 `return 302`），不加
  `auth_request`——它只做跳转，真正的内容由 `<username>/<rest>` location 承载并受
  auth_request 保护；未认证访问 `/alice` 只会得到 302→401，不泄露内容。
- 在**使用 `proxy_pass` 的 location**（`/<username>/<rest>`、系统路径、`/` 兜底）上
  加 `auth_request /_mfcc_auth;`。

## 修改/新增文件

### 1. `demos/mf-cc/nginx.conf`

- `/usermanagement/` location 增加：
  ```nginx
  auth_basic "mfcc admin";
  auth_basic_user_file /etc/nginx/mfcc-admin.htpasswd;
  ```
- 新增内部鉴权 location（放在用户正则 location 之前更清晰）：
  ```nginx
  location = /_mfcc_auth {
      internal;
      proxy_pass http://127.0.0.1:58882/_mfcc_auth;
      proxy_pass_request_body off;
      proxy_set_header Content-Length "";
      proxy_set_header X-Original-URI $request_uri;
      proxy_set_header X-Mfcc-User $cookie_mfcc_user;
      proxy_set_header Authorization $http_authorization;
  }
  ```
- 在以下三个 location 加 `auth_request /_mfcc_auth;`：
  - `location ~ ^/(?<username>[a-z0-9-]+)/(?<rest>.*)$`（用户作用域）
  - `location ~ ^/(api|assets|ws|sdk|callback|auth|proxy|preview-fs|local-file|health)(/|$)`（系统路径）
  - `location /`（兜底）
- 入口 `location ~ ^/(?<username>[a-z0-9-]+)$` **不改**。
- 顶部注释更新：说明鉴权模型。

### 2. `demos/mf-cc/admin/main.go`

- 新增常量/配置 env：`PASSWORDS_CONFIGMAP`(默认 `mfcc-user-passwords`)、
  `PASSWORDS_NAMESPACE`(默认 `ate-demo-mf-cc`)。
- 新增 `passwordStore` 接口（`Get/Set/Delete`）+ `configMapPasswordStore` 实现
  （内存 map + `sync.RWMutex` + client-go 读写 ConfigMap），便于单测用 fake。
- 新增 `generatePassword()`、`hashPassword()`、`checkPassword()`（stdlib 盐+迭代哈希）。
- `handleCreateUser`：新创建（非复用）时生成密码→`Set(name, hash)`→响应加
  `"password": "<明文>"`；复用已有用户时不返回密码。
- `handleDeleteUser`：删除成功后 `Delete(name)`（幂等）。
- 新增 `handleResetPassword`：`POST /api/users/{name}/password` → 生成新密码、
  更新存储、返回明文（供 UI「重置密码」按钮，避免一次性密码丢失后永久锁死）。
- 新增 `handleAuth`：`/_mfcc_auth` —— 解析目标用户名（`X-Original-URI` 路径首段，
  否则 `X-Mfcc-User` cookie）；无用户名→401；`Get(name)` 无哈希→200（放行）；
  有哈希→解析 `Authorization: Basic`，用户名须等于目标用户且 `checkPassword` 通过，
  否则 401。
- `main()`：`rest.InClusterConfig()` → `kubernetes.NewForConfig` → 构造 store 并
  `load`，注册 `/api/users/{name}/password`、`/_mfcc_auth` 路由。
- `controlClient` 接口不变；`server` 结构体增加 `passwords passwordStore` 字段。

### 3. `demos/mf-cc/admin/index.html`

- `createUser` 成功回调：若响应含 `j.password`，醒目展示「访问密码：xxx（仅显示一次）」。
- 用户列表「操作」列新增「重置密码」按钮 → `POST api/users/<name>/password`，
  展示新密码。

### 4. `demos/mf-cc/admin/main_test.go`

- 新增 fake `passwordStore`。
- 测试：`handleAuth`（无哈希→200；正确 Basic→200；错密码→401；缺 Authorization→401；
  用户名不匹配→401）；`handleCreateUser`（返回 password 字段并写入 store）；
  `handleResetPassword`（返回新密码、旧密码失效）；`hashPassword/checkPassword` 往返。

### 5. `demos/mf-cc/mfcc-admin.htpasswd`（新增）

默认 `admin:<openssl passwd -apr1 "mf@pass2026" 的哈希>`，作为镜像内兜底。

### 6. `demos/mf-cc/Dockerfile`

`COPY mfcc-admin.htpasswd /etc/nginx/mfcc-admin.htpasswd`。

### 7. `demos/mf-cc/run-nginx.sh`

- 从 `ADMIN_USER`(默认 `admin`)/`ADMIN_PASSWORD`(默认 `mf@pass2026`) 用
  `openssl passwd -apr1` 生成临时 htpasswd（`mktemp` + `trap` 清理）。
- `docker run` 增加 `-v "$HTPASSWD:/etc/nginx/mfcc-admin.htpasswd:ro"`。

### 8. `demos/mf-cc/mf-cc.yaml.tmpl`

- 新增 `ServiceAccount mfcc-admin`（`ate-demo-mf-cc`）。
- 新增 `Role mfcc-admin-passwords`（`ate-demo-mf-cc`）：对 `configmaps` 资源名
  `mfcc-user-passwords` 授予 `get/create/update`。
- 新增 `RoleBinding`：绑定 `mfcc-admin` SA → 上述 Role。
- `Deployment mfcc-admin`：加 `serviceAccountName: mfcc-admin`；env 追加
  `PASSWORDS_CONFIGMAP=mfcc-user-passwords`、`PASSWORDS_NAMESPACE=ate-demo-mf-cc`。

### 9. `demos/mf-cc/README.md`

- 新增「鉴权」章节：管理页固定账号密码（默认 `admin`/`mf@pass2026`，`run-nginx.sh` 可用
  `ADMIN_USER`/`ADMIN_PASSWORD` 覆盖）；Web UI 创建用户时生成一次性密码，用户访问
  agent 页时用 Basic Auth（用户名=`<username>`，密码=分配密码）；「重置密码」功能；
  脚本创建的用户无密码、agent 页直接可进。
- 更新 health-check curl 示例（需带 Basic Auth 或使用脚本创建的无密码用户）。
- 保留名列表补充 `_mfcc_auth` 说明（内部使用，不对外）。

## 测试

- `go test ./demos/mf-cc/admin/`（新增鉴权/密码相关单测）。
- 手动冒烟：
  1. `./build-image.sh && ./run-nginx.sh`（或显式设 `ADMIN_USER`/`ADMIN_PASSWORD`）。
  2. 打开 `http://localhost:58881/usermanagement/` → 触发 Basic Auth 提示。
  3. UI 添加用户 → 复制一次性密码。
  4. 无认证打开 `http://localhost:58881/<username>/` → 401 提示；输入用户名+密码 → 进入。
  5. 用 `create-user.sh` 建的用户直接可进（无密码）。

## 注意事项 / 取舍

- 密码哈希用 stdlib 盐+迭代哈希（demo 级），README 注明可换 bcrypt（需 `go mod vendor`）。
- 未分配密码的用户（脚本/旧用户）会被拒绝访问（401），需在管理页「重置密码」生成
  密码后才能进入。
- `delete-user.sh` 删除 actor 不会清理 ConfigMap 中的哈希（幂等无害；仅残留条目，
  actor 已不存在时请求自然 404/503）。
- 修改 `nginx.conf` 后必须 `./build-image.sh` 重建 `mfcc-nginx` 镜像再重启容器
  （README 已有该说明，此处沿用）。
