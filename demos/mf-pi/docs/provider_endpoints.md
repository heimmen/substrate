# 自定义 OpenAI 兼容端点（admin 管理的 provider 注册表）

本文说明 mf-pi 如何把**任意 OpenAI 兼容端点**加入部署、让**所有用户**的 pi-web 模型选择器
都能选到它，以及为什么端点是部署级、只有凭据是每用户。实现见 `admin/providermodels.go`、
`admin/providers.go`，以及两个 `mf-pi*.yaml.tmpl` 中的 `mfpi-provider-models` ConfigMap、
`mfpi-admin-providers` RBAC 与 Actor supervisor 脚本。

## 1. 目标与边界

- **端点（provider）= 部署级**：管理员新增一个端点（显示名 + `baseUrl` + 模型名列表）后，
  它对**所有**用户立即可选；端点本身**不携带凭据**。
- **凭据 = 每用户**：每个用户仍各自为自己的 provider 存一把 Key
  （`POST /api/users/{name}/apikey`，见 `docs/injectDeepsseekKey.md`）。端点共享，Key 不共享。

## 2. 为什么端点是部署级（平台约束）

三条平台事实决定了无法做 per-user 端点，只能把端点做成部署级、把凭据留成每用户：

1. **ConfigMap / Secret 无法按文件挂进 Actor**：ActorTemplate 的 `volumes[].volumeSource` 只
   支持 `durableDir` 与 `externalVolumeTemplate`
   （`pkg/api/v1alpha1/actortemplate_types.go:53-65`），且 `VolumeMount` 只有 `name` /
   `mountPath`、**没有 `subPath`**
   （`pkg/api/v1alpha1/actortemplate_types.go:80-95`）——单个文件无法挂到
   `<agentDir>/models.json` 而不覆盖该目录下已有的用户数据。
2. **Actor 无法自证身份**：ActorTemplate 的 env 只支持字面量与 `secretKeyRef`（没有 downward
   API），沙箱 hostname 固定为 `runsc`，且 worker 多路复用——同一 Pod IP 上跑多个 Actor。
   Actor 侧因此无法知道「我是谁」以领取属于自己的那份端点配置。
3. 结论：端点写进所有 Actor 都能拉到的**部署级注册表**（与托管 Skill 同构，见
   `docs/deploy_skill_to_actor.md`）；per-user 的部分只能是凭据（`admin/apikey.go`）。

## 3. 存储：ConfigMap + admin 环境变量

- 注册表存放在 ConfigMap `mfpi-provider-models` 的 `provider-models.json` 键，内容是一份
  `models.json` 文档（`{"providers":{"<id>":{...}}}`），可直接喂给 pi-web。
- 两个部署清单会**预创建**该 ConfigMap，并以 **SiliconFlow（硅基流动）** 作为 seed
  （id `siliconflow`，`baseUrl` `https://api.siliconflow.cn/v1`，3 个模型）。
- `mfpi-admin` 经 **Kubernetes API server** 读写该 ConfigMap。

环境变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PROVIDER_MODELS_CONFIGMAP` | `mfpi-provider-models` | 注册表 ConfigMap 名 |
| `PROVIDER_MODELS_NAMESPACE` | `ate-demo-mf-pi` | 注册表 ConfigMap 所在 namespace |

- RBAC 由 `mfpi-admin-providers` Role / RoleBinding 授予该 ConfigMap 的 `get` / `update`；
  因清单已预创建，不需要 `create`。
- **已移除**：旧的 `PROVIDER_MODELS_FILE` 环境变量与 admin Pod 上的 ConfigMap 卷挂载都不再
  存在——admin 直接和 API server 通信，不再读本地文件。

> ⚠️ **redeploy 会重置注册表**：清单里的 ConfigMap 是声明式的，重新 apply 部署清单会把注册表
> 恢复成 seed（只剩 SiliconFlow）。通过 API 添加的端点在 redeploy 后**丢失**；要长期保留，
> 须把端点写回清单的 ConfigMap，或另行备份该 ConfigMap 的内容。

## 4. 管理 API（查 / 增 / 删）

经 nginx 均在 `/usermanagement` 下，走 HTTP Basic（`admin`）；直连 `mfpi-admin:8080` 时去掉
前缀且无鉴权。

**管理面 UI**：`/usermanagement/` 顶部的「**模型端点**」按钮打开一个模态框，列出注册表中的
自定义端点（`custom:true`），每行可**删除**；「**添加端点**」表单包含显示名 / Base URL /
模型名（逗号或换行分隔）/ API 类型 / 可选 id。增删后前端会重新拉取 provider 列表，因此
「**设置 Key**」下拉立即能选到新端点（自定义项标注 `…（自定义）`）。以下 API 即该 UI 的后端。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/api/providers` | 列出可选 provider：内置列表，后接注册表中的自定义端点（后者带 `"custom":true`，并附 `baseUrl` / `models` 供管理页渲染） |
| POST | `/api/providers` | 新增 / 覆盖一个自定义端点 |
| DELETE | `/api/providers/{id}` | 从注册表移除一个自定义端点 |

**POST** body：`{"id"?:string,"name":string,"baseUrl":string,"api"?:string,"models":string[]}`

- `id` 可省略，由 `name` 推导：转小写；`-` `_` `.` 与空格折叠为 `-`（去掉首尾），**其它非字母
  数字字符直接丢弃**（例：`Z.AI  GLM` → `z-ai-glm`）；推不出任何字符时回退为 `custom`。显式 id 须匹配
  `^[a-z0-9][a-z0-9._-]*$`，且不得与内置 provider 冲突。
- `name` 可省略（缺省即 id）；`api` 缺省 `openai-completions`；`models` 至少一个非空项，
  重名去重，每个模型的显示名缺省即其 id。
- `400` 错误（`{"error":...}`）：
  - `请求体不是合法 JSON`
  - `provider id "X" 非法：需匹配 ^[a-z0-9][a-z0-9._-]*$`
  - `provider id "openai" 与内置 provider 冲突，请换一个`
  - `baseUrl 不能为空`
  - `baseUrl "x" 非法：需要 http:// 或 https:// 开头的完整地址`
  - `至少要指定一个模型名`
- `500`：`{"error":"保存 provider 失败: ..."}`（写 ConfigMap 失败）。

```bash
curl -sS -u admin:'mf@pass2026' -X POST "${BASE}/usermanagement/api/providers" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Internal Gateway","baseUrl":"https://llm.internal.example/v1","models":["qwen3-32b"]}'
# 200 {"provider":{"id":"internal-gateway","name":"Internal Gateway","custom":true,
#              "baseUrl":"https://llm.internal.example/v1","models":["qwen3-32b"]},
#      "message":"Internal Gateway 已保存；该端点会在 60s 内出现在各用户的模型列表中"}
```

**DELETE**：成功返回 `200`，body 为 `{"message":"<id> 已从注册表移除；已拉取该端点的 actor 仍保留本地 models.json 中的定义","provider":"<id>"}`；不存在返回 `404 {"error":"provider 不存在: <id>"}`。

```bash
curl -sS -u admin:'mf@pass2026' -X DELETE \
  "${BASE}/usermanagement/api/providers/internal-gateway"
```

## 5. 分发机制：admin 暴露 + Actor 拉取合并

1. admin 把注册表渲染成 `models.json` 文档，经 `GET /internal/providers/models.json` 提供
   （`Content-Type: application/json`、`Cache-Control: no-store`；仅接受 GET）。
2. 每个 ActorTemplate 的 supervisor 每 60s 拉取一次
   `PI_WEB_MODELS_URL=http://mfpi-admin.<ns>.svc.cluster.local:8080/internal/providers/models.json`，
   把返回文档的 `providers` **合并**进 `$PI_CODING_AGENT_DIR/models.json`
   （默认 `/data/pi-agent/models.json`）。
3. pi-web 每次请求都重读 `models.json`，因此端点改动**无需重启 sessiond/web、无需重建镜像**，
   ~60s 内出现在各用户的模型列表中。

**合并语义**：只覆盖 `providers` 中**同名**的托管项；文件原有的其它 provider、其它顶层键
一律保留。拉取失败、非 2xx、响应体无法解析或缺少 `providers` 时**保持现有文件不变**；只有
合并结果确实变化时才写盘。admin 侧每 30s 的 reconcile 循环也会重读 ConfigMap，使
`kubectl edit` 的改动同样生效。

## 6. 移除是「只增不删」（Actor 侧）

从注册表删除端点后，管理面与 `GET /api/providers` 立即不再提供它，但**已经拉取过它的 Actor
仍保留**本地 `models.json` 中的定义——合并只写不删，而该文件位于用户持久卷 `/data/pi-agent`，
跨 Actor 重建保留。要彻底下架，需在 Actor 内手动删除对应条目（可用 `exec-actor.sh`）。
因此**删除端点不是安全边界**：其模型仍可能留在老用户的选择器里，只是不再有新用户拿到。

## 7. KEY_PROVIDERS 只管理内置列表

`KEY_PROVIDERS`（`admin/providers.go`）只用来**覆盖内置精选列表**；`GET /api/providers` 的最终
列表 = 内置列表（可被 `KEY_PROVIDERS` 覆盖）**并上**注册表中的自定义端点。自定义端点**不需要**
写进 `KEY_PROVIDERS`——旧「注册表 provider 必须同时出现在 KEY_PROVIDERS」的不变式已从
`validate-templates.sh` 移除。该脚本现在断言：注册表 ConfigMap 可解析、每个 provider 都有
`baseUrl` / `api` / `models[].id`、admin env 与 `mfpi-admin-providers` RBAC 正确、ConfigMap 未
被挂进 admin Pod。

## 8. 安全

`GET /internal/providers/models.json` 与 `/internal/skills/*` 一样**无鉴权**，仅供集群内 Actor
经 Service DNS 访问。nginx 只对外暴露 `/usermanagement/` 与 `/<user>/`，因此**绝不能**把该端点
发布到集群外（例如给 admin Service 加 NodePort / Ingress）。管理 API 本身依赖 nginx 的 Basic
Auth——`mfpi-admin` 应用自身不做鉴权。

## 9. 运维注意：ActorTemplate spec 变更会重建模板

本特性要求在 ActorTemplate 中新增 `PI_WEB_MODELS_URL` env 与 supervisor 脚本逻辑。**修改
ActorTemplate spec 会重建模板并重新生成它的 golden base 快照**，已有 Actor 需要刷新 / 重建后
才会带上拉取循环（参见 `refresh-actor.sh` 与 `docs/integration-scenario.md`）。之后只改注册表
是纯运行时操作，不触碰模板；但注意 §3 的 redeploy 重置行为。

## 10. 验证记录

`./validate-templates.sh` 对两个模板均通过；`bash -n` 通过渲染后的 Actor 脚本，`node --check`
通过内嵌拉取循环载荷。合并循环已实机验证（加入 `siliconflow`、保留用户 provider 与无关顶层
键、载荷畸形时文件不变）；端到端以真实 SiliconFlow key 跑通凭据存储（`source=stored`）、
`pi auth check`（`{"status":"ready",...}`）与一次真实推理。
