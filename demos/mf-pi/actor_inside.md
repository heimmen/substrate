# Actor 内部原理（Pod / Actor / PV）

本文用最简方式说明 Substrate 在 mf-pi 演示中**三个核心概念**如何协作：

- **Pod / WorkerPool**：谁来跑沙箱、跑在哪
- **Actor**：一个用户 = 一个逻辑实体，里面跑着 pi-web
- **PV（持久卷）**：用户数据存在哪、为什么刷新不丢

---

## 1. 总览

```
                kubectl-ate (CLI) / mfpi-admin API
                          │ create / resume / suspend / delete
                          ▼
                   ate-api-server  (控制面)
        ┌──────────┬────────────┬──────────────────┐
        │ 配置卷   │ 协调 golden │ 快照 (rustfs/GCS) │
        │  (Create  │ base 快照  │ 挂起时存进程+文件系统│
        │   Volume) │            │ 恢复时原样还原     │
        └──────────┴─────┬──────┴──────────────────┘
                          │ spawn / resume
                          ▼
                     atelet  (节点上的 daemon)
                          │ runsc  (gVisor 沙箱运行时)
                          ▼
             ┌──────────────────────────────┐
             │  Actor 沙箱 (pi-web 单容器)    │
             │                              │
             │   /data/pi-agent  ◄──symlink─┼──► 节点目录:
             │      (= PV 挂载点)            │  /var/lib/ateom-gvisor/
             └──────────────────────────────┘    stickyvolumes/
                                                <atespace>-<actor>-userdata
```

一句话：**Actor 是逻辑身份，Pod/WorkerPool 是它运行所需的物理槽位，PV 是挂在 Actor 上、跨刷新存活的用户磁盘。**

---

## 2. Pod / WorkerPool

- **WorkerPool** 决定「同时能有多少个活跃 Actor」——它的 `replicas` = 并发 RUNNING 用户上限（`deploy.sh` 默认 16，测试环境默认 2）。
- 每个 **worker 是一个 k8s Pod**，里面只有一个空槽：一旦某个 Actor 被恢复（resume），atelet 在该 Pod 上用 **runsc（gVisor）** 起一个沙箱跑该 Actor。
- 没有空闲 worker 时，新用户的恢复请求会返回 `503 no free workers`；挂起（suspend）一个用户即可腾出槽位。

> 查看：`kubectl get workerpool -n ate-demo-mf-pi`；扩容：`kubectl scale workerpool mf-pi-workerpool -n ate-demo-mf-pi --replicas=8`

---

## 3. Actor

一个**用户 = 一个 Actor**（`mfpi` atespace 下一个独立实体，数据互相隔离）。

**生命周期**：`STATUS_SUSPENDED`（不占 worker）⇄ `STATUS_RUNNING`（占用一个 worker）；另有 `PAUSED`（进程留在节点 VM，快照在本地）。

**一次 resume 内部发生什么**：
1. 控制面从 **golden base 快照**（模板首次部署时生成）或上次**挂起检查点**恢复进程状态。
2. atelet 把该 Actor 的 **PV 目录 symlink** 进沙箱挂载点 `/data/pi-agent`。
3. 沙箱入口 `tini -- pi-web-bootstrap sh -c '...'` 先装内置 skills，再拉起
   `pi-web-sessiond` + `pi-web-server`，监听 80 端口。
4. supervisor 内的两个**恢复循环**（每 10s）补齐 skills 与 project 工作目录——因为
   恢复自快照时「首次启动」步骤不会重跑。

**为什么刷新（删+建）也能恢复数据**：删除 Actor 时，控制面调用 sticky 插件的
`DeleteVolume`，它**故意保留**节点上的 backing 目录；重建同名用户时
`CreateVolume` 返回同一个稳定名，新沙箱重新挂回同一目录，旧数据原样可见。

常用命令：
```bash
kubectl ate create actor alice -a mfpi --template ate-demo-mf-pi/mf-pi   # 创建（挂起态）
kubectl ate resume  actor alice -a mfpi                                   # 恢复（占用 worker）
kubectl ate suspend actor alice -a mfpi                                   # 挂起（释放 worker，进程+文件进快照）
kubectl ate get actor   alice -a mfpi                                     # 查状态
./refresh-actor.sh alice          # 测试默认 mfpi-test；等于「删+建+恢复」式镜像刷新
```

> 脚本版：`create-user.sh` / `list-users.sh` / `delete-user.sh`（生产）、`*-test.sh` 对应测试环境。

---

## 4. PV（sticky 持久卷，`userdata`）

**声明**（ActorTemplate）：`externalVolumeTemplate` 卷 `userdata`（5Gi），容器
`volumeMounts` 挂到 `/data/pi-agent`。

**稳定名与落盘**：
- 控制面按 `<atespace>-<actorName>-<volName>` 生成卷 ID（如 `mfpi-alice-userdata`）。
- sticky 插件在**节点**上维护 backing 目录：
  `/var/lib/ateom-gvisor/stickyvolumes/<卷ID>`。
- 挂载时（atelet 侧）把该目录 **symlink** 到沙箱里的 `/data/pi-agent`。
- 于是 `/data/pi-agent` **就是** PV 本身——actor 直接读写，无同步代理、无凭据。

**哪些数据自动恢复**（删除重建后）：
| 数据 | 位置 | 结果 |
|---|---|---|
| 会话历史、`auth.json`、`settings.json`、models | PV | ✅ 原样 |
| 内置 skills | 镜像 → supervisor 循环补装 | ✅ 补齐 |
| project 工作目录（如 `/dtom2`，在 PV 外） | 沙箱临时层 | ⚠️ 目录重建（历史内容不恢复） |

**什么时候回收存储（purge）**：普通「删除 actor」**不删** PV（这正是刷新能恢复的原因）。只有**删除用户**才清掉它：
```bash
# 管理流：admin 删除用户（UI「删除」/ DELETE /api/users/alice）会顺带 purge PV
# 手动：
./remove-pv.sh alice            # 生产（mfpi）
./remove-pv.sh alice --test     # 测试（mfpi-test）
kubectl ate purge volumes alice -a mfpi -t ate-demo-mf-pi/mf-pi   # 等价 CLI
```

**检查 PV**：
```bash
./list-pvs.sh [--test] [--detail] [--show-auth]   # 列出每个用户 PV 与数据概况
./exec-actor.sh alice [--test]                      # 在节点上打开该 PV 的 shell
```

---

## 5. 一张表看懂三种「重置」

| 操作 | 入口 | 沙箱 | PV 数据 | skills | 备注 |
|---|---|---|---|---|---|
| **suspend / resume** | `kubectl ate suspend/resume` | 同一实例现场还原 | 原样（同一沙箱） | 原样 | 快照恢复；WebSocket 需刷新 |
| **refresh**（删+建+恢复） | `refresh-actor.sh` | 新沙箱 | ✅ 重新挂载同一目录 | ✅ 循环补齐 | 镜像可升级，数据不丢 |
| **删除用户** | admin / `remove-pv.sh` | 销毁 | ❌ 已 purge | ❌ 已删 | 彻底清账号 |

---

## 6. 部署与访问

```bash
# 构建并推送镜像到本地仓库（kind 节点无法访问外网）
KO_DOCKER_REPO=localhost:5001 ./hack/install-ate.sh --deploy-ate-apiserver   # 控制面
cd ../pi-web && PI_WEB_IMAGE=pi-web:latest docker/scripts/build-image.sh      # pi-web 镜像
docker tag pi-web:latest localhost:5001/pi-web:latest && docker push localhost:5001/pi-web:latest

# 部署演示（解析镜像摘要、建 ns / template / admin）
cd demos/mf-pi && ./deploy.sh          # 生产（端口 58681）
                   ./deploy-test.sh     # 测试（端口 59681）

# 访问（nginx 反向代理 + 端口转发）
./run-nginx.sh          # 生产
./run-nginx-test.sh     # 测试
# 用户页：http://localhost:59681/<username>/
# 管理页：http://localhost:59681/usermanagement/  (admin / mf@pass2026)
```

更完整的端到端说明见 [README.md](./README.md)；PV 设计细节见
`save_userdata_pv.md` 与 `perUserDataVolume.md`。
