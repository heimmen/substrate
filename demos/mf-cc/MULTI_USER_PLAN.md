# mf-cc 多用户支持方案

## 目标

让 mf-cc 支持多个相互独立、数据隔离的用户。每个用户拥有独立的 mf-cc 会话。

## 核心思路：利用 Substrate 原生功能（不修改任何 Go 代码）

**一个用户 = 一个 Actor**（与 `demos/agent-secret` 创建 23 个 session actor 的模式一致）。Substrate 天然支持：

- 多个 Actor 从同一个模板创建，彼此完全隔离：
  - 独立沙箱 / worker / 快照存储
  - 独立 `durableDir`（挂载 `/root/.claude`）→ **数据隔离的关键**
- 路由器按 `Host` 头解析 `<actor>.<atespace>.actors.resources.substrate.ate.dev`，每个用户有自己的 DNS 名，如 `alice.mfcc.actors.resources.substrate.ate.dev`
- Actor 首次请求自动恢复（`STATUS_SUSPENDED` → 自动 resume），空闲挂起释放 worker

用户访问方式（已确认）：**子域名 per 用户**。`alice` 访问 `http://alice.localhost:58881`，nginx 用正则提取子域名 → 设置对应 `Host` 头 → 转发到 `127.0.0.1:58880`（kubectl port-forward）。

## 变更清单

### 1. `demos/mf-cc/nginx.conf` — 子域名路由

重写为两个 server 块：

- **用户路由块**：`server_name ~^(?<username>[a-z0-9-]+)\.localhost$`
  - `proxy_set_header Host $username.mfcc.actors.resources.substrate.ate.dev;`（atespace `mfcc` 用 `set` 变量保留，便于修改）
  - `proxy_pass http://127.0.0.1:58880;`（静态 IP，无 DNS 解析问题）
  - 保留 WebSocket 升级头（`proxy_http_version 1.1` + `Upgrade`/`Connection: upgrade`）
- **默认/落地块**：`server_name localhost 127.0.0.1;` + `default_server`
  - 返回纯文本提示页：说明访问方式 `http://<username>.localhost:58881` 及对应关系

### 2. `demos/mf-cc/mf-cc.yaml.tmpl` — WorkerPool 容量配置化 + 历史目录显式锚定

- `spec.replicas` 从 `1` 改为 `${MFCC_WORKER_REPLICAS}`。
- 在 mf-cc 容器 `env` 中显式新增 `CLAUDE_CONFIG_DIR`，把历史/配置锚定到 durableDir 挂载点：
  ```yaml
  env:
  - name: CLAUDE_CONFIG_DIR
    value: /root/.claude
  ```

> **为什么必须设 `CLAUDE_CONFIG_DIR`（历史会话持久化的关键）**：cc-haha（Claude Code）的
> 所有持久化数据根目录由 `CLAUDE_CONFIG_DIR` 决定（默认 `~/.claude`，见
> `src/utils/envUtils.ts` 的 `getClaudeConfigHomeDir`）。其中**聊天历史**以 JSONL 形式落盘在
> `<CLAUDE_CONFIG_DIR>/projects/<sanitized-cwd>/<sessionId>.jsonl`
> （见 `src/utils/sessionStorage.ts` 的 `getProjectsDir` / `getTranscriptPath`），恢复时
> `adoptResumedSessionFile` 据此加载历史。
>
> 虽然容器用户是 root（`~` == `/root`），默认恰好命中 `/root/.claude` 挂载点，但显式设置
> `CLAUDE_CONFIG_DIR` 可消除对 `HOME` 的隐式依赖，保证行为可预测、可移植。

### 3. `hack/install-demo-mf-cc.sh` — 部署脚本支持新变量

- 在 `demo-mf-cc_deploy()` 中新增：`MFCC_WORKER_REPLICAS="${MFCC_WORKER_REPLICAS:-4}"`（默认 4，非必填）
- 在 deploy 和 delete 的 `sed` 替换中加入 `-e "s|\${MFCC_WORKER_REPLICAS}|${MFCC_WORKER_REPLICAS}|g"`（delete 时用 `placeholder` 或 `1` 兜底，因为 delete 只是按 metadata 删除）
- 更新 `demo-mf-cc_usage()` 提示

### 4. 新增用户管理脚本（`demos/mf-cc/` 下）

- **`create-user.sh <username>`**（**幂等**：无实例则建，有实例则复用原历史）：
  - 校验用户名符合 DNS-1123（`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`）
  - 确保 atespace `mfcc` 存在（`kubectl ate get atespace mfcc` 失败则 `kubectl ate create atespace mfcc`）
  - **存在性检查**：`kubectl ate get actor <username> -a mfcc` 若成功，说明实例已存在，
    直接提示复用、不再创建（历史/配置保存在该 actor 的 durableDir 中，天然保留）；
    仅当 actor 不存在时才 `kubectl ate create actor <username> -a mfcc --template ate-demo-mf-cc/mf-cc`。
  - 核心逻辑：
    ```bash
    if kubectl ate get actor "$username" -a mfcc >/dev/null 2>&1; then
        echo "Actor $username already exists; reusing existing session (history preserved)."
    else
        kubectl ate create actor "$username" -a mfcc --template ate-demo-mf-cc/mf-cc
    fi
    ```
  - > **为什么必须幂等**：控制面 `CreateActor` 对已存在的 actor 名会返回
    > `AlreadyExists`（"Actor id1 already exists"，见 `cmd/ateapi/internal/controlapi/create_actor.go`
    > 及 `functional_test.go` 的 `TestCreateActor_Duplicate`）。不做存在性检查则重复调用直接报错，
    > 无法满足"有就复用"。
- **`list-users.sh`**：`kubectl ate get actors -a mfcc`
- **`delete-user.sh <username>`**：`kubectl ate delete actor <username> -a mfcc`

三个脚本都加 `set -euo pipefail`，参考现有 `build-image.sh`/`run-nginx.sh` 的 `cd "$(dirname "$0")"` 惯例。

### 5. `demos/mf-cc/README.md` — 文档更新（中文）

- "如何使用"章节改为多用户模式：`http://<username>.localhost:58881`
- 说明 `*.localhost` 在 systemd-resolved / RFC 6761 下默认解析到 127.0.0.1；若无效则回退到 `/etc/hosts`（`127.0.0.1 alice.localhost`）
- 新增用户管理章节：`./create-user.sh alice`、`./list-users.sh`、`./delete-user.sh alice`
- 部署章节注明 `MFCC_WORKER_REPLICAS`（默认 4，决定最大同时在线用户数）
- 保留 "Option B: /etc/hosts + Host 头" 作为免 nginx 的直连方式（`alice.mfcc.actors.resources.substrate.ate.dev`）

## 不需要改动的部分

- `demos/mf-cc/Dockerfile`、`build-image.sh`、`run-nginx.sh`（原样，镜像只拷 nginx.conf）
- 任何 Substrate Go 源码（路由、调度、存储均原生支持一用户一 Actor）

## 验证

```bash
# 1. 部署（在 hack/ 下）
MFCC_WORKER_REPLICAS=2 ANTHROPIC_AUTH_TOKEN=... ANTHROPIC_BASE_URL=... ANTHROPIC_MODEL=... \
  ./install-ate-kind.sh --deploy-demo-mf-cc

# 2. 端口转发
kubectl port-forward -n ate-system svc/atenet-router 58880:80

# 3. 创建用户
cd demos/mf-cc
./create-user.sh alice
./create-user.sh bob

# 4. 构建并启动 nginx 代理
./build-image.sh && ./run-nginx.sh

# 5. 验证路由（*.localhost → 127.0.0.1）
curl -s -o /dev/null -w "%{http_code}\n" http://alice.localhost:58881/          # 首次可能 503（actor 恢复中），重试应为 200
curl -s -o /dev/null -w "%{http_code}\n" http://alice.localhost:58881/health    # 200
curl -s -o /dev/null -w "%{http_code}\n" http://bob.localhost:58881/health      # 200（不同 Actor）

# 6. 验证数据隔离：alice 与 bob 的 /root/.claude 各自独立（各自有独立 durableDir）
#    可在 UI 中为 alice 建立一次会话，确认 durableDir 中生成 JSONL：
#    <CLAUDE_CONFIG_DIR>/projects/<sanitized-cwd>/<sessionId>.jsonl

# 7. 验证幂等复用：重复 create 应复用而非报错/重建
./create-user.sh alice     # 应提示 "already exists; reusing"
./create-user.sh bob       # 应提示 "already exists; reusing"

# 8. 验证历史会话加载：挂起/恢复后，alice 的会话历史仍在
kubectl ate suspend actor alice -a mfcc
kubectl ate resume actor alice -a mfcc
#   重新打开 http://alice.localhost:58881，确认上次会话/历史仍可恢复

# 9. 验证管理脚本
./list-users.sh    # 应列出 alice、bob
./delete-user.sh alice
./list-users.sh    # 应只剩 bob
```

## 进度清单

- [x] 重写 nginx.conf 支持子域名路由（*.localhost → 对应 Actor）
- [x] mf-cc.yaml.tmpl：WorkerPool replicas 改为 ${MFCC_WORKER_REPLICAS}（默认 4）
- [x] mf-cc.yaml.tmpl：容器 env 显式新增 CLAUDE_CONFIG_DIR=/root/.claude（锚定历史会话到 durableDir）
- [x] install-demo-mf-cc.sh 支持 MFCC_WORKER_REPLICAS 变量替换
- [x] 新增用户管理脚本 create-user.sh（幂等：先查后建）/ list-users.sh / delete-user.sh
- [x] 更新 README.md 文档（多用户访问、用户管理、容量配置、历史持久化说明）
- [~] 端到端验证（已完成静态/渲染/容器校验；集群流程待执行）
  - [x] 静态：三个用户脚本 + install-demo-mf-cc.sh 通过 `bash -n`
  - [x] 静态：模板 sed 渲染后 6 个 YAML 文档有效；WorkerPool.replicas 注入正确；CLAUDE_CONFIG_DIR=/root/.claude 与 mountPath 一致
  - [x] 容器：nginx.conf 挂载为 /etc/nginx/conf.d/default.conf（同 Dockerfile）在 nginx:latest 镜像内 `nginx -t` 通过
  - [ ] 集群：部署 `--deploy-demo-mf-cc`、创建用户、子域名路由、数据隔离、幂等复用、历史挂起/恢复后仍可加载、管理脚本（需 kind 集群 + 镜像 + kubectl-ate）
