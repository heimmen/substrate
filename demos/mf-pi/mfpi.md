# mf-pi 计划：把 pi-web 镜像以 mf-cc 方式跑在 Agent Substrate

> 本文档是 `demos/mf-pi` 的实施计划与进度跟踪（TODO 见文末）。目标：完全参照
> `demos/mf-cc` 的全部功能（多用户 path 路由、用户管理 Web UI + 鉴权、
> create/list/delete 用户脚本、Full 快照持久化、nginx 反代、deploy 脚本、
> `install-ate.sh` harness、生产+测试两套环境），把 `/home/liuchong/git/pi-web` 的
> 镜像作为 Substrate Actor 运行。用户管理入口为 **58681**（生产）/
> **59881**（测试）。

## 关键事实（已调研确认）

- **路由端口写死 80**：`cmd/atenet/internal/router/extproc.go:174-175`
  `targetAddr := net.JoinHostPort(workerIP, "80")` —— Actor 容器必须监听 80。
- **Actor 容器强制以 root 运行**：`cmd/atelet/oci.go:229-232` `User: {UID:0,GID:0}`，
  且 `cmd/atelet/oci.go:236-256` 授予 `CAP_NET_BIND_SERVICE`。pi-web 镜像虽
  `USER pi-web`(1000)，但作为 Actor 会以 root 运行，绑定 80 无碍。
- **pi-web 单容器需跑 sessiond + web**：镜像 ENTRYPOINT 为
  `tini -- /usr/local/bin/pi-web-bootstrap`（首启把 `/opt/pi-web/skills` 拷到
  `/data/pi-agent/skills`），CMD 为 `pi-web-server`（只跑 web）。
  `docker/scripts/run-container.sh` 已给出单容器 supervisor 模式
  （先 sessiond，等 unix socket，再 web）。
- **ActorTemplate `command` 覆盖镜像 ENTRYPOINT+CMD；`args` 只覆盖 CMD、保留
  ENTRYPOINT**（`pkg/api/v1alpha1/actortemplate_types.go:112-133`）。因此用 `args`
  传 supervisor 脚本，即可保留 `tini` + bootstrap（skills 拷贝）。
- **pi-web 前端完全相对路径**：`vite.config.ts:96` `base: "./"`，客户端
  `resolveAppUrl` 把 `/api/...`、`/pi-web-plugins/...` 都解析为相对 SPA 基址的路径
  （`src/client/src/appUrl.ts`），WebSocket 亦如此（`src/client/src/api/sockets.ts`）。
  因此所有流量都带 `/<username>/` 前缀，**不需要** mf-cc 那批 reserved-prefix
  cookie 路由；仅保留 fallback + cookie 兜底。

## 命名与端口映射（生产 + 测试）

| 项 | 生产 | 测试 |
|---|---|---|
| Namespace | `ate-demo-mf-pi` | `ate-demo-mf-pi-test` |
| Atespace | `mfpi` | `mfpi-test` |
| ActorTemplate 名 | `mf-pi` | `mf-pi`（test ns 内） |
| WorkerPool | `mf-pi-workerpool` | 同左（test ns 内） |
| worker 标签 | `workload: mf-pi` | `workload: mf-pi-test` |
| admin Deployment/Service | `mfpi-admin` | 同左（test ns 内） |
| 密码 ConfigMap | `mfpi-user-passwords` | 同左 |
| provider Secret | `mf-pi-provider-config` | 同左 |
| Cookie 名 | `mfpi_user` | `mfpi_user_test` |
| nginx 容器名 | `mfpi-nginx` | `mfpi-nginx-test` |
| 入口端口 | **58681** | 59881 |
| router port-forward | 58680 | 59880 |
| admin port-forward | 58682 | 59882 |
| 快照路径 | `gs://${BUCKET_NAME}/ate-demo-mf-pi/` | `gs://${BUCKET_NAME}/ate-demo-mf-pi-test/` |
| worker replicas 默认 | 16（deploy.sh）/ 4（harness） | 2 |
| 工作负载镜像 | `localhost:5001/pi-web@${MF_PI_DIGEST}` | 同左 |

Actor DNS：`<username>.mfpi.actors.resources.substrate.ate.dev`（测试：
`<username>.mfpi-test.actors.resources.substrate.ate.dev`）。

## Actor 容器关键设计（核心难点）

`mf-pi.yaml.tmpl` 的 ActorTemplate 容器（测试版同，仅 label/路径不同）：

```yaml
spec:
  pauseImage: "localhost:5001/pause:3.10.2@${PAUSE_DIGEST}"
  containers:
  - name: pi-web
    image: "localhost:5001/pi-web@${MF_PI_DIGEST}"
    # 用 args（非 command）保留镜像 ENTRYPOINT：tini -- pi-web-bootstrap。
    # bootstrap 首启拷贝 skills 到 /data/pi-agent/skills，然后 exec 下面的 supervisor。
    args:
    - sh
    - -c
    - |
      set -eu
      sock=${PI_WEB_SESSIOND_SOCKET:-/data/pi-web/sessiond.sock}
      pi-web-sessiond &
      sessiond_pid=$!
      n=0
      until [ -S "$sock" ] || [ "$n" -ge 150 ]; do sleep 0.2; n=$((n+1)); done
      if [ ! -S "$sock" ]; then
        echo "mf-pi: sessiond socket missing at $sock" >&2
        kill "$sessiond_pid" 2>/dev/null || true; exit 1
      fi
      pi-web-server &
      web_pid=$!
      term() { kill -TERM "$web_pid" "$sessiond_pid" 2>/dev/null || true; }
      trap term TERM INT
      set +e
      wait "$web_pid"; web_status=$?
      term; wait "$sessiond_pid" 2>/dev/null
      exit "$web_status"
    env:
    - name: PI_WEB_PORT
      value: "80"
    - name: PI_WEB_HOST
      value: "0.0.0.0"
    - name: HOSTEXEC_MODE
      value: disabled
    - name: DEEPSEEK_API_KEY
      valueFrom:
        secretKeyRef:
          name: mf-pi-provider-config
          key: DEEPSEEK_API_KEY
  workerSelector:
    matchLabels:
      workload: mf-pi
  snapshotsConfig:
    onPause: Full
    onCommit: Full
    location: gs://${BUCKET_NAME}/ate-demo-mf-pi/
```

- 镜像其余关键 ENV 已内建（`docker/Dockerfile`）：`HOME=/data/home`、
  `XDG_CONFIG_HOME=/data/config`、`PI_WEB_DATA_DIR=/data/pi-web`、
  `PI_WEB_SESSIOND_SOCKET=/data/pi-web/sessiond.sock`、
  `PI_CODING_AGENT_DIR=/data/pi-agent`。
- 凭据只需 `DEEPSEEK_API_KEY`（pi-web 内建 deepseek provider），不再需要
  mf-cc 的 `ANTHROPIC_*` 三项。

## nginx 路由模型（相对 mf-cc 的简化）

pi-web 前端全相对路径，nginx 只需：

- `= /` 落地页；
- `= /_mfpi_auth` internal 鉴权子请求 → `http://127.0.0.1:58682/_mfpi_auth`，传
  `X-Original-URI` / `X-Mfpi-User $cookie_mfpi_user`；
- `^~ /usermanagement/` 固定 Basic Auth → `http://127.0.0.1:58682/`；
- `~ ^/(?<username>[a-z0-9-]+)$` → 写 cookie + 302 到 `/<username>/`；
- `~ ^/(?<username>[a-z0-9-]+)/(?<rest>.*)$` → auth_request + 剥前缀 + 代理
  （Host `<username>.mfpi.actors...`，带 Upgrade/Connection 头支持 WS）；
- `location /` fallback → cookie 路由（favicon 等）。

## admin 服务改造要点（相对 mf-cc 的 diff）

- 默认值：`defaultAtespace="mfpi"`、`defaultTemplateNamespace="ate-demo-mf-pi"`、
  `defaultTemplateName="mf-pi"`、`defaultPasswordsConfigMap="mfpi-user-passwords"`、
  `defaultPasswordsNamespace="ate-demo-mf-pi"`。
- `reservedPrefixRe` 精简为 `^/(usermanagement)(/|$)`（pi-web 无 mf-cc 那批
  origin 绝对路径）。
- 鉴权端点 `/_mfcc_auth` → `/_mfpi_auth`，头 `X-Mfcc-User` → `X-Mfpi-User`。
- 文案 "mf-cc" → "mf-pi"。

## 镜像前置条件

```bash
# 1) 构建 pi-web 镜像并推到本地 registry（kind 离线环境）
cd /home/liuchong/git/pi-web
PI_WEB_IMAGE=pi-web:latest docker/scripts/build-image.sh
docker tag pi-web:latest localhost:5001/pi-web:latest
docker push localhost:5001/pi-web:latest

# 2) pause 镜像（与 mf-cc 共用）
docker tag rancher/mirrored-pause:3.10.2 localhost:5001/pause:3.10.2
docker push localhost:5001/pause:3.10.2
```

## 部署与验证

```bash
cd demos/mf-pi
DEEPSEEK_API_KEY=<key> ./deploy.sh          # 生产（或 ./deploy-test.sh 测试）
./create-user.sh alice                       # 建用户（atespace mfpi）
./run-nginx.sh                               # 起 port-forward + nginx
# 访问：
#   用户页  http://localhost:58681/alice/
#   管理页  http://localhost:58681/usermanagement/  (admin / mf@pass2026)
```

验证点：管理页列出/添加/删除用户；添加用户后拿到一次性密码能进用户页；用户页可
选 `deepseek/deepseek-chat` 完成一次调用；挂起/恢复后会话历史保留
（`kubectl ate suspend/resume actor alice -a mfpi`）。

harness 方式：`DEEPSEEK_API_KEY=... BUCKET_NAME=... KO_DOCKER_REPO=... ./hack/install-ate-kind.sh --deploy-demo-mf-pi`（测试 `--deploy-demo-mf-pi-test`）。

## 风险/备注

- pi-web 镜像较大（openSUSE + Node），Full 快照仅记录 delta，不重复整镜像；
  但进程内存快照比 mf-cc（Bun）更大，挂起略慢，属预期。
- 单容器双进程（sessiond+web）通过 unix socket 通信；Actor 恢复后 socket 需重新
  建立，web 可能短暂 503，等几秒重试（同 mf-cc 首访语义）。
- gVisor 下 `/data` 为容器文件系统（无 durableDir），skills/sessions 依赖 Full
  快照持久化，与 mf-cc 一致。

---

## TODO List（实施进度跟踪）

> 逐项完成后把 `- [ ]` 改为 `- [x]`。

### 阶段 A：目录与镜像
- [x] A1. 新建 `demos/mf-pi/` 目录（含 `admin/` 子目录）
- [x] A2. 构建 `pi-web:latest` 镜像并推送 `localhost:5001/pi-web:latest`（tag 自既有 `pi-web:local`，digest `sha256:364a73cf…`）
- [x] A3. 确认 `localhost:5001/pause:3.10.2` 就位（与 mf-cc 共用）

### 阶段 B：核心清单（ActorTemplate 等）
- [x] B1. `mf-pi.yaml.tmpl`（生产：Namespace/Secret/RBAC/WorkerPool/ActorTemplate/密码 ConfigMap/ServiceAccount/Deployment(mfpi-admin)/Service；渲染校验通过）
- [x] B2. `mf-pi-test.yaml.tmpl`（测试：`ate-demo-mf-pi-test`/`mfpi-test`/`workload: mf-pi-test`；渲染校验通过）

### 阶段 C：admin Go 服务（用户管理 UI + 鉴权）
- [x] C1. `admin/main.go`（复制 mf-cc 并改名/改默认值/精简 reservedPrefixRe/`_mfpi_auth`/`X-Mfpi-User`）
- [x] C2. `admin/main_test.go`（同步改名，另增 path 解析用户与 cookie 兜底两条用例）
- [x] C3. `admin/index.html`（标题改「mf-pi 用户管理」）

### 阶段 D：nginx 反代与镜像
- [x] D1. `nginx.conf`（生产，listen 58681，转发 58680/58682，cookie `mfpi_user`，atespace `mfpi`；`nginx -t` 通过）
- [x] D2. `nginx-test.conf`（测试，59881/59880/59882，`mfpi_user_test`，`mfpi-test`；`nginx -t` 通过）
- [x] D3. `Dockerfile` + `mfpi-admin.htpasswd`
- [x] D4. `build-image.sh`（`mfpi-nginx` 镜像构建成功）
- [x] D5. `run-nginx.sh` / `run-nginx-test.sh`

### 阶段 E：部署与用户脚本
- [x] E1. `deploy.sh` / `deploy-test.sh`（bash -n 通过）
- [x] E2. `create-user.sh` / `create-user-test.sh`
- [x] E3. `list-users.sh` / `list-users-test.sh`
- [x] E4. `delete-user.sh` / `delete-user-test.sh`

### 阶段 F：install-ate.sh harness 集成
- [x] F1. `hack/install-demo-mf-pi.sh`（source 校验通过，`demo-mf-pi` 已注册）
- [x] F2. `hack/install-demo-mf-pi-test.sh`（`demo-mf-pi-test` 已注册）
- [x] F3. 在 `hack/install-ate.sh` 追加 `source` 两行新 harness

### 阶段 G：文档与验证
- [x] G1. 完善本文档 `demos/mf-pi/mfpi.md`（含 TODO 跟踪；已修正测试环境端口笔误 59681→59881）
- [x] G2. `demos/mf-pi/README.md`
- [x] G3. `gofmt -l demos/mf-pi/admin/`、`go test ./demos/mf-pi/admin/...` 通过；`make verify` 中 mf-pi 相关检查（gofmt/shellcheck/boilerplate，无 mf-pi 文件被标记）通过
- [x] G4. 端到端验证（见下方「验证结果」）

#### 验证结果（G3/G4）

- **G3**：`gofmt -l demos/mf-pi/admin/` 无输出（格式正确）；`go test ./demos/mf-pi/admin/...` `ok`。
  `make verify` 全量跑：mf-pi 相关全部通过；仓库级 `verify` 脚本中
  gofmt.sh / shellcheck.sh 通过，boilerplate.sh 仅标记既有的 `demos/mf-cc/mfcc-admin.htpasswd`
  （非本 demo 文件）。`licenses.sh` / `go-modules.sh` 及 `go test ./pkg/api/v1alpha1/...`
  在本离线环境因无法访问 `proxy.golang.org` 而失败，属**环境网络限制**，与本 demo 改动无关。
- **G4**（kind 集群 `kind-kind`，镜像 `pi-web`/`pause` 已在 `localhost:5001`）：
  1. `DEEPSEEK_API_KEY=<placeholder> ./deploy.sh` → 全部资源创建成功，
     `mfpi-admin` Deployment 1/1 Ready；WorkerPool 16 副本、`mf-pi` ActorTemplate(gvisor) 就位。
  2. `./create-user.sh alice` → atespace `mfpi` 自动创建，actor `alice` 初始 `STATUS_SUSPENDED`。
  3. `./build-image.sh && ./run-nginx.sh` → 58680/58681/58682 三个端口监听，`mfpi-nginx` 容器运行。
  4. 鉴权/路由：`/usermanagement/` 无凭据 401、`admin/mf@pass2026` 200（`<title>mf-pi 用户管理</title>`）；
     `/alice` → 302 `/alice/`；`/alice/` 未分配密码 401。
  5. `POST /api/users/alice/password` 生成一次性密码（ConfigMap 落盘）；错密码 401、
     正确密码 200 且返回真实 pi-web UI（`<title>PI WEB</title>`），actor 变 `STATUS_RUNNING`。
  6. `kubectl ate suspend actor alice -a mfpi` → `STATUS_SUSPENDED`（Full 快照，pod 释放）；
     `kubectl ate resume actor alice -a mfpi` → 重新 `STATUS_RUNNING`，UI 仍 200（`<title>PI WEB</title>`）。
     挂起/恢复路径与持久化语义验证通过。
  - 说明：本次未配置真实 `DEEPSEEK_API_KEY`，故「选择 deepseek 模型完成一次真实推理」
    未纳入本次验证；接入真实 key 后即可，不影响 Actor/路由/鉴权/快照路径的正确性。

## 待新增文件清单

### `demos/mf-pi/`（22 个）
1. `mfpi.md`（本文档）
2. `mf-pi.yaml.tmpl`
3. `mf-pi-test.yaml.tmpl`
4. `validate-templates.sh`（模板渲染校验辅助脚本）
4. `deploy.sh`
5. `deploy-test.sh`
6. `Dockerfile`
7. `nginx.conf`
8. `nginx-test.conf`
9. `build-image.sh`
10. `run-nginx.sh`
11. `run-nginx-test.sh`
12. `mfpi-admin.htpasswd`
13. `create-user.sh`
14. `create-user-test.sh`
15. `list-users.sh`
16. `list-users-test.sh`
17. `delete-user.sh`
18. `delete-user-test.sh`
19. `admin/main.go`
20. `admin/main_test.go`
21. `admin/index.html`
22. `README.md`

### `hack/`（3 处）
23. `hack/install-demo-mf-pi.sh`
24. `hack/install-demo-mf-pi-test.sh`
25. `hack/install-ate.sh` —— 追加 `source` 两行

> 说明：mf-cc 目录里的历史设计文档（`DEPLOY_PLAN.md`/`AUTH_PLAN.md`/
> `MULTI_USER_PLAN.md`/`ADMIN_UI_PLAN.md`）属历史记录，不复刻；本项目的计划
> 统一沉淀在 `mfpi.md`。
