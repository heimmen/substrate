# 将 cc-haha (mf-cc) Web UI 作为 actor 部署到 Substrate

## Context

将 `/home/liuchong/git/cc-haha-mf_cc_skill_webui` 构建出的 Docker 镜像 `mf-cc:latest`（一个 Bun HTTP+WebSocket 的 AI 编码 Web UI）作为 actor 跑在 Substrate 集群上，复用 Substrate 的暂停/恢复、快照、持久化能力。

本环境是 **kind 本地集群**（context `kind-kind`），配置了 `kind-registry` 映射到 `localhost:5001`。**重要限制**：集群节点无法访问外网 registry（之前 `--delete-demo-counter` 失败根因就是节点拉取 `registry.k8s.io/pause:3.10.2@sha256:f548...` 超时），所以**所有镜像（含 pause）必须推送到 localhost:5001**。

## 已确认的决策

| 决策 | 选择 | 依据 |
|---|---|---|
| 集成方式 | 集成到 install-ate.sh | 符合现有 demo 模式 |
| API key | Kubernetes Secret + secretKeyRef | 密钥不入库，照 claude-code-multiplex 模式 |
| 持久化 | durableDir 卷挂 `/root/.claude` | 保存 settings/projects/skills |

## 关键技术约束

1. **atenet-router 转发到 actor 的 80 端口**（HTTP/1.1，`cmd/atenet/internal/router/xds.go:363`），所以容器必须 `SERVER_PORT=80` 监听。
2. **ActorTemplate 镜像必须带 `@sha256` digest**，spec 创建后不可变。
3. **pause 镜像必须本地化**：kind 节点无法从外网拉取 pause:3.10.2，且 containerd 里只有 kind 自带的 pause:3.10。需把 pause 推送到 localhost:5001 并在模板里引用。
4. **actor DNS 名**：`<actor-name>.<atespace>.actors.resources.substrate.ate.dev`（`internal/resources/actor.go` 的 `ActorDNSName`）。外部访问需要 port-forward router + /etc/hosts。
5. mf-cc 容器持久化数据在 `/root/.claude`（settings.json、projects/、skills/），需 durableDir 挂载。
6. 快照 location 用 `gs://${BUCKET_NAME}/ate-demo-mf-cc/`，本集群 `BUCKET_NAME=ate-snapshots`。

## 实施步骤

### 1. 本地化镜像（pause + mf-cc）到 localhost:5001

```bash
# pause 镜像 —— 必须本地化，否则节点拉取超时
docker pull registry.k8s.io/pause:3.10.2
docker tag registry.k8s.io/pause:3.10.2 localhost:5001/pause:3.10.2
docker push localhost:5001/pause:3.10.2
PAUSE_DIGEST=$(docker inspect localhost:5001/pause:3.10.2 --format='{{index .RepoDigests 0}}' | cut -d@ -f2)

# mf-cc 镜像
docker tag mf-cc:latest localhost:5001/mf-cc:latest
docker push localhost:5001/mf-cc:latest
MFCC_DIGEST=$(docker inspect localhost:5001/mf-cc:latest --format='{{index .RepoDigests 0}}' | cut -d@ -f2)
```

如果本地 `docker pull registry.k8s.io/pause:3.10.2` 也超时，改用已存在于 kind 节点上的 pause:3.10 的 digest，或从其他可达源获取。

### 2. 新建 `demos/mf-cc/mf-cc.yaml.tmpl`

结构（照 `demos/claude-code-multiplex/claude-code-multiplex.yaml.tmpl` + `demos/counter/counter.yaml.tmpl`）：

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: ate-demo-mf-cc
---
apiVersion: v1
kind: Secret
metadata:
  name: mf-cc-provider-config
  namespace: ate-demo-mf-cc
type: Opaque
stringData:
  ANTHROPIC_AUTH_TOKEN: "${ANTHROPIC_AUTH_TOKEN}"
  ANTHROPIC_BASE_URL: "${ANTHROPIC_BASE_URL}"
  ANTHROPIC_MODEL: "${ANTHROPIC_MODEL}"
---
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: mf-cc-workerpool
  namespace: ate-demo-mf-cc
  labels:
    workload: mf-cc
spec:
  replicas: 1
  ateomImage: ko://github.com/agent-substrate/substrate/cmd/ateom-gvisor
---
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: mf-cc
  namespace: ate-demo-mf-cc
spec:
  pauseImage: "localhost:5001/pause:3.10.2@${PAUSE_DIGEST}"
  containers:
  - name: mf-cc
    image: "localhost:5001/mf-cc@${MFCC_DIGEST}"
    env:
    - name: SERVER_PORT
      value: "80"          # router 固定转发到 80
    - name: SERVER_HOST
      value: "0.0.0.0"
    - name: CLAUDE_H5_ALLOW_ALL_ORIGINS
      value: "1"
    - name: ANTHROPIC_AUTH_TOKEN
      valueFrom:
        secretKeyRef:
          name: mf-cc-provider-config
          key: ANTHROPIC_AUTH_TOKEN
    - name: ANTHROPIC_BASE_URL
      valueFrom:
        secretKeyRef:
          name: mf-cc-provider-config
          key: ANTHROPIC_BASE_URL
    - name: ANTHROPIC_MODEL
      valueFrom:
        secretKeyRef:
          name: mf-cc-provider-config
          key: ANTHROPIC_MODEL
    volumeMounts:
    - name: cc-data
      mountPath: /root/.claude
  workerSelector:
    matchLabels:
      workload: mf-cc
  snapshotsConfig:
    onPause: Data        # 长时间 web server 避免全量内存快照
    onCommit: Data
    location: gs://${BUCKET_NAME}/ate-demo-mf-cc/
  volumes:
  - name: cc-data
    durableDir: {}
```

注意：`run_ko apply` 只负责把模板里 `ko://` 前缀的镜像（ateomImage）解析推送；模板里 `localhost:5001/...@digest` 的普通镜像字符串直接透传给 kubectl。

### 3. 新建 `hack/install-demo-mf-cc.sh`

照 `hack/install-demo-claude-code-multiplex.sh` 的模式：

```bash
ATE_DEMOS+=(demo-mf-cc)

demo-mf-cc_cmdline() {
  case "${1}" in
    --deploy-demo-mf-cc) demo-mf-cc_deploy ;;
    --delete-demo-mf-cc) demo-mf-cc_delete ;;
    *) return 1 ;;
  esac
  return 0
}

demo-mf-cc_build_and_push_images() {
  # 1) pause 镜像
  docker pull registry.k8s.io/pause:3.10.2 >&2
  docker tag registry.k8s.io/pause:3.10.2 localhost:5001/pause:3.10.2 >&2
  docker push localhost:5001/pause:3.10.2 >&2
  PAUSE_DIGEST=$(docker inspect localhost:5001/pause:3.10.2 --format='{{index .RepoDigests 0}}' | cut -d@ -f2)
  # 2) mf-cc 镜像
  docker tag mf-cc:latest localhost:5001/mf-cc:latest >&2
  docker push localhost:5001/mf-cc:latest >&2
  MFCC_DIGEST=$(docker inspect localhost:5001/mf-cc:latest --format='{{index .RepoDigests 0}}' | cut -d@ -f2)
  MFCC_IMAGE="localhost:5001/mf-cc@${MFCC_DIGEST}"
  PAUSE_IMAGE="localhost:5001/pause:3.10.2@${PAUSE_DIGEST}"
}

demo-mf-cc_deploy() {
  log_step "demo-mf-cc_deploy"
  # 校验 env 存在
  for v in ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL ANTHROPIC_MODEL BUCKET_NAME; do
    [[ -n "${!v:-}" ]] || { echo "$v must be set" >&2; return 1; }
  done
  demo-mf-cc_build_and_push_images
  sed -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
      -e "s|\${ANTHROPIC_AUTH_TOKEN}|${ANTHROPIC_AUTH_TOKEN}|g" \
      -e "s|\${ANTHROPIC_BASE_URL}|${ANTHROPIC_BASE_URL}|g" \
      -e "s|\${ANTHROPIC_MODEL}|${ANTHROPIC_MODEL}|g" \
      -e "s|\${MFCC_DIGEST}|${MFCC_DIGEST}|g" \
      -e "s|\${PAUSE_DIGEST}|${PAUSE_DIGEST}|g" \
      demos/mf-cc/mf-cc.yaml.tmpl \
    | run_ko apply -f -
}

demo-mf-cc_delete() {
  log_step "demo-mf-cc_delete"
  delete_demo_actors ate-demo-mf-cc mf-cc
  sed -e "s|\${BUCKET_NAME}|${BUCKET_NAME:-placeholder}|g" \
      -e "s|\${ANTHROPIC_AUTH_TOKEN}|placeholder|g" \
      -e "s|\${ANTHROPIC_BASE_URL}|placeholder|g" \
      -e "s|\${ANTHROPIC_MODEL}|placeholder|g" \
      -e "s|\${MFCC_DIGEST}|placeholder|g" \
      -e "s|\${PAUSE_DIGEST}|placeholder|g" \
      demos/mf-cc/mf-cc.yaml.tmpl \
    | run_kubectl delete --ignore-not-found -f -
}

demo-mf-cc_usage() {
  echo ""
  echo "  Required env: ANTHROPIC_AUTH_TOKEN, ANTHROPIC_BASE_URL, ANTHROPIC_MODEL, BUCKET_NAME"
}
```

说明：`run_kubectl delete` 只按 metadata 匹配删除，k8s 不校验容器镜像（claude-code-multiplex 注释已说明这点），所以 delete 时占位符不会报错。

### 4. 在 `hack/install-ate.sh` 注册

在 `hack/install-ate.sh` 的 `install-demo-multi-template.sh` source 之后（约 46 行）加一行：

```bash
source "${ROOT}"/hack/install-demo-mf-cc.sh
```

### 5. 部署

```bash
BUCKET_NAME=ate-snapshots \
ANTHROPIC_AUTH_TOKEN=<deepseek-token> \
ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic \
ANTHROPIC_MODEL=deepseek-v4-flash \
./hack/install-ate.sh --deploy-demo-mf-cc
```

### 6. 创建 actor 并访问

```bash
# 创建 actor（golden actor 由模板自动创建，但需一个命名 actor 供访问）
go run ./cmd/kubectl-ate create actor mfcc -t ate-demo-mf-cc/mf-cc -a mfcc
go run ./cmd/kubectl-ate get actor mfcc -a mfcc

# port-forward router
kubectl port-forward -n ate-system svc/atenet-router 8080:80 &

# /etc/hosts 加一行（actor DNS 名 → 127.0.0.1）
echo "127.0.0.1 mfcc.mfcc.actors.resources.substrate.ate.dev" | sudo tee -a /etc/hosts

# 浏览器访问
open http://mfcc.mfcc.actors.resources.substrate.ate.dev:8080
```

（actor 名称、atespace 可调整；DNS 名用 `internal/resources/actor.go` 的 `ActorDNSName` 拼接。）

## 验证

1. **部署成功**：`--deploy-demo-mf-cc` 无报错，`kubectl get workerpool -A`、`kubectl get actortemplate -A` 显示新资源，golden actor `STATUS_RUNNING`。
2. **pod 起来**：`kubectl get pods -A | grep ate-demo-mf-cc`，ateom pod Running，容器 ready。
3. **镜像确实本地化**：`docker exec kind-control-plane crictl images | grep -E "mf-cc|pause"` 应显示 `localhost:5001/mf-cc` 与 `localhost:5001/pause:3.10.2`，避免外网拉取超时。
4. **HTTP 可访问**：port-forward + /etc/hosts 后 `curl http://mfcc.mfcc.actors.resources.substrate.ate.dev:8080/health` 返回 200（mf-cc 的 /health 端点）。
5. **Web UI 加载**：浏览器打开，页面渲染、登录/会话功能可用。
6. **WebSocket**：验证 `/ws/*` 升级成功（Envoy HCM 默认允许 upgrade；如失败需在 xds 加 upgradeConfigs 或调容器配置）。
7. **持久化**：在 Web UI 里创建配置 → `kubectl-ate pause`/`resume` actor → 确认 `/root/.claude` 数据仍在。

## 风险与注意

- **WebSocket 走 Envoy**：HCM 未显式配置 `upgradeConfigs`，Envoy HTTP/1.1 默认放行 upgrade，但 ext_proc filter 与 DFP 组合需实测；若失败需在 `cmd/atenet/internal/router/xds.go` 增加 WebSocket upgrade 配置。
- **actor 暂停/恢复**：mf-cc 是长驻 web server，`onPause: Data` 只快照 durableDir，恢复后进程重启（内存不保留），WebSocket 连接会断，需前端重连或刷新页面。这是可接受的行为。
- **镜像大小**：mf-cc 1.67GB，push/拉取较慢属正常。
- **durableDir 数据宿主路径**：恢复后数据在节点本地盘，若节点重建会丢；生产需用 externalVolumeTemplate（本环境 kind 用 durableDir 足够）。
