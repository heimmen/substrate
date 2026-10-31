# mf-pi 三环境（prod / qa / test）

mf-pi 现在有**三套完全隔离的环境**。每套都有独立的 namespace、atespace、
workload 标签、入口端口、网关容器和 cookie 名；启动 / 部署 / 清理脚本互不
影响。

## 环境矩阵

| | **prod**（新） | **qa**（原 58681） | **test** |
|---|---|---|---|
| 入口端口 | **57681** | **58681** | **59681** |
| router port-forward | 57680 | 58680 | 59880 |
| admin port-forward | 57682 | 58682 | 59882 |
| CI/CD 页面后端 | 57690 | 58690 | 59690 |
| namespace | `ate-demo-mf-pi-prod` | `ate-demo-mf-pi` | `ate-demo-mf-pi-test` |
| atespace | `mfpi-prod` | `mfpi` | `mfpi-test` |
| workload 标签 | `mf-pi-prod` | `mf-pi` | `mf-pi-test` |
| 网关容器 | `mfpi-nginx-prod` | `mfpi-nginx-qa` | `mfpi-nginx-test` |
| nginx 配置 | `nginx-prod.conf` | `nginx-qa.conf` | `nginx-test.conf` |
| 用户 cookie | `mfpi_user_prod` | `mfpi_user_qa` | `mfpi_user_test` |
| 会话 cookie | `mfpi_auth_prod` | `mfpi_auth_qa` | `mfpi_auth_test` |
| small worker 默认 | 24 | 26（=现有 actor 数） | 8 |
| mid / large 默认 | 1 / 1 | **0 / 0**\* | **0 / 0**\* |

> **为什么 qa 保留 `mfpi` / `ate-demo-mf-pi` 这些老名字**：atespace、actor 名和
> sticky 卷 ID（`<atespace>-<actor>-userdata`）都带名字，原地改名会让已有用户
> 的会话/数据丢失。所以历史资源整体成为 **qa**，**prod** 是全新的
> `mfpi-prod` / `ate-demo-mf-pi-prod`（初始为空）。

> **cookie 为什么要分名字**：浏览器 cookie 不区分端口。同一个 host 上如果三套
> 环境都叫 `mfpi_user` / `mfpi_auth`，在 qa 登录后浏览器会把 qa 的 cookie 发给
> prod/test，导致串环境或反复弹密码。三套环境使用不同 cookie 名（`AUTH_COOKIE_NAME`
> 由 admin 部署的 env 控制）。

## 单一事实来源

所有环境常量都在 `demos/mf-pi/env/<env>.env`：

```bash
demos/mf-pi/env/prod.env
demos/mf-pi/env/qa.env
demos/mf-pi/env/test.env
```

`demos/mf-pi/lib/env.sh` 负责加载并校验；任何脚本都不应再硬编码端口/namespace。

清单只有一个参数化模板 `demos/mf-pi/mf-pi.yaml.tmpl`，由部署脚本按 env 渲染：

| 占位符 | 含义 |
|---|---|
| `${MFPI_NAMESPACE}` | namespace（同时用作快照路径段） |
| `${MFPI_ATESPACE}` | mfpi-admin 管理的 atespace |
| `${MFPI_WORKLOAD_LABEL}` | 集群级 workload 标签（另加 `-small/-mid/-large`） |
| `${MFPI_AUTH_COOKIE}` | 会话 cookie 名 |

## 部署：三个独立入口脚本

行为和以前一样，每个环境一个脚本：

| 脚本 | 环境 | 入口 |
|---|---|---|
| `./deploy.sh` | **qa**（历史默认） | 58681 |
| `./deploy-test.sh` | **test** | 59681 |
| `./deploy-prod.sh` | **prod**（新增） | 57681 |

```bash
cd demos/mf-pi
./deploy.sh          # 58681（qa）—— 和改造前完全一样
./deploy-test.sh     # 59681（test）
./deploy-prod.sh     # 57681（prod，首次部署前 prod 资源还是空的）

# 也可以显式指定环境（等价，便于脚本化）：
./deploy.sh qa | ./deploy.sh test | ./deploy.sh prod
MFPI_ENV=qa ./deploy.sh

# 只渲染、不碰集群（新增，用于排查/CI）：
MFPI_DRY_RUN=1 ./deploy.sh
```

> 裸 `./deploy.sh` 默认仍然是 **58681（qa）**，不会误部署到新生产。生产必须用
> `./deploy-prod.sh`（或 `./deploy.sh prod`）显式选择。

也可以走 install harness（`hack/install-ate.sh` / `install-ate-kind.sh`）：

```bash
DEEPSEEK_API_KEY=<key> BUCKET_NAME=ate-snapshots KO_DOCKER_REPO=localhost:5001 \
  ./hack/install-ate-kind.sh --deploy-demo-mf-pi        # prod（57681）
  ./hack/install-ate-kind.sh --deploy-demo-mf-pi-qa     # qa（58681）
  ./hack/install-ate-kind.sh --deploy-demo-mf-pi-test   # test（59681）
```

对应卸载 flag：`--delete-demo-mf-pi[-qa|-test]`。

## 启动 / 停止网关

| 脚本 | 环境 |
|---|---|
| `./run-nginx.sh` | **qa**（58681，历史默认） |
| `./run-nginx-test.sh` | **test**（59681） |
| `./run-nginx-prod.sh` | **prod**（57681） |

```bash
./run-nginx.sh            # 58681（qa）
./run-nginx-test.sh       # 59681（test）
./run-nginx-prod.sh       # 57681（prod）
./stop-nginx.sh           # 停 qa
./stop-nginx-test.sh      # 停 test
./stop-nginx-prod.sh      # 停 prod
```

每个环境只操作自己的端口（router/admin/entry）、容器名、nginx conf 和
htpasswd 路径。网关镜像 `mfpi-nginx` 是**环境无关**的：运行脚本把对应
`nginx-<env>.conf` 和 htpasswd bind-mount 进容器；改配置只需重启该环境容器，
不需要重建镜像。

## CI/CD 页面

```bash
./cicd-server.sh qa start      # 或 ./cicd-server-qa.sh start（端口 58690）
./cicd-server.sh prod start    # 57690，部署 deploy-prod.sh
./cicd-server.sh test start    # 59690，部署 deploy-test.sh
```

三个实例可同时运行：pidfile / 日志 / 端口均按环境隔离。

## 用户 / Skill 等运维脚本

新增 `-qa` / `-prod` 薄包装（`-test` 保留）。包装只导出环境变量后 exec 基础
脚本，例如：

```bash
./create-user-prod.sh alice      # 等价 MFPI_ENV=prod ./create-user.sh alice
./list-users-qa.sh
./set-user-apikey-prod.sh alice sk-...
./refresh-actor-qa.sh alice
./install-skill-test.sh ...
```

可用包装：`create-user`、`list-users`、`delete-user`、`set-user-apikey`、
`clear-user-apikey`、`install-skill`、`list-skills`、`remove-skill`、
`apply-skills`、`refresh-actor`、`release-small-agents`、`list-pvs`、
`exec-actor`、`remove-pv`。

> 基础脚本（不带后缀）仍然默认操作 qa 的历史名字（`mfpi` /
> `ate-demo-mf-pi`）。需要 prod/test 时用 `-prod` / `-test` 包装或显式
> `MFPI_ENV=`。

## 首次部署 prod（57681）前

**prod 目前还没有部署**（只有脚本；集群里只有 qa/test）。真正拉起前先过一遍资源
检查，否则会大面积 Pending：

```bash
cd demos/mf-pi

# 1) 预检：只渲染、不碰集群（会解析镜像 digest）
MFPI_DRY_RUN=1 ./deploy-prod.sh

# 2) 腾资源（当前节点约 107/110 Pod、21/24 核 requests；prod 约需 27 个 Pod）
#    方式 A：把 qa 缩到现有 actor 数附近（qa 现有 26 个 actor，不可低于它）
MFPI_WORKER_REPLICAS_SMALL=26 ./deploy.sh
#    方式 B：回收其它 demo 的空闲 worker，或把 qa/test 的 mid+large 调 0

# 3) 部署（首次会创建 ate-demo-mf-pi-prod namespace）
DEEPSEEK_API_KEY=<key> ./deploy-prod.sh
#    空间仍紧时先起最小集，之后再扩：
MFPI_WORKER_REPLICAS_SMALL=8 MFPI_WORKER_REPLICAS_MID=0 MFPI_WORKER_REPLICAS_LARGE=0 \
  DEEPSEEK_API_KEY=<key> ./deploy-prod.sh

# 4) 启动 57681 网关
./run-nginx-prod.sh
```

> \* 三套环境共享单节点，prod 上线后 qa/test 的 mid/large 没有余量，故默认 0；
> 需要时先腾出 Pod/CPU 再调大。
>
> prod 的 `mid/large` 默认各 1（额外 3 核 requests）。若节点吃紧，首次部署建议
> `MFPI_WORKER_REPLICAS_MID=0 MFPI_WORKER_REPLICAS_LARGE=0`，先只开 small 档
> （用户默认就是 small）。

## 资源与容量提醒

当前单节点 24 核。三套同时在线时：qa 26 + prod 24 + test 8 = 58 个 small
worker（14.5 核 requests）+ prod 的 mid/large 3 核 ≈ 18 核，留出 kube/其它组件
余量；qa/test 的 mid/large 为 0。若需要恢复 mid/large，先腾资源。
加上三套 mid/large（各 1 核 + 2 核）共 9 核，再加 kube-system/otel 等，节点
CPU requests 会接近或略超 24 核。**真正部署前**建议：

- 若节点吃紧，把非 prod 环境的 `MFPI_WORKER_REPLICAS_MID` / `_LARGE` 调 0；
- 或先部署 prod，再按需起 qa/test；
- Pod 槽位也有限（当前 107/110），三套 worker + admin 需要约 +3 个 pod。

worker 数可在部署时覆盖（env 文件用 `:=`，显式 env 优先）：

```bash
MFPI_WORKER_REPLICAS_SMALL=8 ./deploy.sh qa
```
