# 通过 Web UI 安装用户自己的 Skill（最小方案）

## 目标
在用户自己的 agent 页面（pi-web）的 Settings 中新增一个独立的 **「Skills」面板**，让用户直接从 git URL / npm 包安装 skill。**复用现有 `/api/pi-packages/install` 后端与客户端 API，不新增后端接口、不列出/卸载 skill**（仅“安装入口”，最小范围）。

## 背景 / 关键事实（已调研确认）
- pi-web 已有 **Settings → “Pi packages”** 面板：`POST /api/pi-packages/install`（body `{source}`，source = git URL / `npm:@scope/package` / 本地路径），等价于 `pi install <source>`，调用 coding-agent 的 `DefaultPackageManager.installAndPersist`。
- 包内的 skill 是**运行时按需发现**的（`<packageRoot>/skills/<name>/SKILL.md` 或 `pi.skills` in package.json），不是拷贝进 `<agentDir>/skills`。装完在会话里 `/reload` 即可生效。
- 目前没有专门的 Skills 设置面板；“skills”只作为 Pi packages 面板里的一句文案出现。用户感觉“pi-web 没有直接做到”是因为该入口被“Pi packages”掩盖，不直观。
- 环境：真实部署是 mf-pi demo，pi-web 镜像由本仓库（/home/liuchong/git/pi-web）构建。**本次改动只在 pi-web 客户端**，无需改 substrate/mf-pi。

## 改动清单（都在 /home/liuchong/git/pi-web）

### 1. `src/client/src/settingsRoute.ts`
- `SettingsSection` 联合类型（第 1 行）追加 `"skills"`。
- `parseSettingsSection`（第 18-25 行）追加：`if (value === "skills") return "skills";`（URL token = `?settings=skills`）。

### 2. 新建 `src/client/src/components/settings/SettingsSkillsPanel.ts`
新面板 `@customElement("settings-skills-panel")`，模仿 `SettingsPackagesPanel` 但**最小化**（只有安装表单，无列表/卸载/更新/刷新）：
- 用 `<settings-panel-frame heading="Skills" .description=... .notices=...>` 包裹。
  - description：说明 skill 来自安装的 Pi 包、装后 `/reload` 生效。
- 表单：`Skill source` 输入框（placeholder `git URL or npm:@scope/package`，绑定 `installSource`，`installing` 时禁用）+ `Install` 按钮（installing 时显示 “Installing…”）。
- 校验错误区（复用 `SettingsPackagesPanel.ts:104` 的 `field-error` 模式）。
- 引导 `<small>`：合法 skill 来源 = `skills/<name>/SKILL.md` 的 git 仓库、暴露 `pi.skills` 的 npm 包、或本地路径；等价 `pi install <source>`。
- 回调属性 `onInstallSkill`（面板**不做**网络请求，由父组件注入回调；与 `SettingsPackagesPanel` 的 `onInstallPackage` 模式一致）。提交处理器：校验 → `await this.onInstallSkill?.(source)` → 成功清空输入；失败由父组件呈现（见下）。
- 样式复用 `SettingsPackagesPanel` 的 visual（`.install-card` / `.install-row` / `.field-error` / `input` / `button`）。

### 3. `src/client/src/components/SettingsDialog.ts`（唯一需要改动多处的文件）
- **import**：`import "./settings/SettingsSkillsPanel";`（第 7-11 行面板 import 处）。
- **导航**（第 108-112 行）：在 packages 按钮后加 `renderNavButton("skills", "Skills", "Selected machine")`。
- **`renderActiveSection()`**：新增 `case "skills"`，最小绑定并**直接复用现有包安装路径**：
  ```ts
  if (this.section === "skills") {
    return html`
      <settings-skills-panel
        .target=${this.packageTarget()}
        .installing=${this.packageOperation !== undefined}
        .error=${this.packageError}
        .successMessage=${this.packageMessage}
        .onInstallSkill=${(source: string) => this.installPiPackage(source)}
      ></settings-skills-panel>
    `;
  }
  ```
- **`SettingsPanelTag` 联合**（第 623-628 行）追加 `| "settings-skills-panel"`。
- **`activeSettingsPanelTag()`**（第 638-648 行）追加 `case "skills": return "settings-skills-panel";`。

### 4. 复用现有安装路径（关键，零后端改动）
- `SettingsPanel.onInstallSkill` → `SettingsDialog.installPiPackage`（第 432-435 行）→ `runPiPackageMutation(..., () => piPackagesApi.install(source, target.id))`。
- 即复用现有 `packageOperation` / `packageError` / `packageMessage` / `saving` 状态与 `friendlyPiPackageErrorMessage` / `piPackageMutationFollowUpMessage`（后者文案已含“skills…/reload”），无需新增状态；`.installing` 绑定 `packageOperation !== undefined` 即可得到“Installing…”按钮态。
- 面板作用域即“当前选中机器/用户”，与 Pi packages 面板一致——正好符合“装在用户自己的 agent 页”。

### 5. 校验/归一化辅助（二选一，推荐 A 最简）
- **A（推荐，最少改面）**：直接复用 `piPackageSettings.ts` 的 `normalizePiPackageSource` / `piPackageSourceValidationMessage`，**不新增** `skillsSettings.ts`；只需在面板里用自定义 `<small>` 引导文案提供 skill 化措辞。
- B（可选）：新增很小的 `settings/skillsSettings.ts` 提供 skill 措辞的 `normalizeSkillSource` / `skillSourceValidationMessage` 与 `skillsDescription`。若不想别扭的“Pi package source”校验文案，选 B。

### 6. 测试更新
- `src/client/src/settingsRoute.test.ts`：`expect(parseSettingsSection("skills")).toBe("skills");`
- `SettingsDialog.general.test.ts`：`activeSettingsPanelTag("skills")` 断言追加到 `expect(activeSettingsPanelTag("skills")).toBe("settings-skills-panel")`。
- 新建 `components/settings/SettingsSkillsPanel.test.ts`：heading “Skills”、输入框 label “Skill source”、空 source 触发校验消息、有效 source 调用 `onInstallSkill`（trim 后）、`installing` 渲染 “Installing…”/禁用按钮。
- `SettingsDialog.packages.test.ts`（或新建）追加父侧接线用例：spy `piPackagesApi.install`，`installPiPackage("npm:@acme/skills")` 应落到 `piPackagesApi.install`。

## 验证
在 `/home/liuchong/git/pi-web`：
- `npm run typecheck`（强制 `SettingsPanelTag`/`activeSettingsPanelTag` 穷尽 + 新联合成员）
- `npm run lint`
- 单测（vitest）：
  - `npx vitest run --config vitest.config.ts src/client/src/settingsRoute.test.ts`
  - `npx vitest run --config vitest.config.ts src/client/src/components/settings/SettingsSkillsPanel.test.ts`
  - `npx vitest run --config vitest.config.ts src/client/src/components/SettingsDialog.general.test.ts src/client/src/components/SettingsDialog.packages.test.ts`
- `npm run verify`（typecheck + lint + knip + test）
- `npm run build` 确认新 custom element 编进客户端 bundle。

手动验证（dev stack）：
1. 打开 web UI 用户 agent 页 Settings，左侧栏出现 “Skills”。
2. 点 “Skills”，见 heading/输入框/Install/引导文案。
3. 输入 git URL（含 `skills/<name>/SKILL.md`）或 `npm:@scope/package`，点 Install；网络面板见 POST `api/pi-packages/install`，出现成功提示与“/reload”，期间按钮 “Installing…”。
4. 留空点 Install → 显示校验消息且不发请求。
5. 深链 `?settings=skills` 直达本面板。

## 关键文件
- `src/client/src/components/SettingsDialog.ts`（导航、renderActiveSection、SettingsPanelTag、activeSettingsPanelTag、复用 installPiPackage/runPiPackageMutation）
- `src/client/src/settingsRoute.ts`
- 新建 `src/client/src/components/settings/SettingsSkillsPanel.ts`（+ `.test.ts`）
- 参考样板：`src/client/src/components/settings/SettingsPackagesPanel.ts`、`src/client/src/components/settings/piPackageSettings.ts`

## 明确不做（范围外）
- 不新增后端/服务端接口；不列出已装 skill；不做卸载/更新；不做压缩包上传（用户已选“复用 Pi Packages + 仅安装入口”）。
- 不改 substrate/mf-pi demo（仅需用新代码重建 pi-web 镜像即可在部署中体现）。

---

## Todolist

- [ ] 1. `settingsRoute.ts`：`SettingsSection` 联合追加 `"skills"`；`parseSettingsSection` 支持 `?settings=skills`。
- [ ] 2. 新建 `settings/SettingsSkillsPanel.ts`：`@customElement("settings-skills-panel")`，最小安装表单（source 输入 + Install 按钮 + 校验 + 引导文案 + `onInstallSkill` 回调）。
- [ ] 3. `SettingsDialog.ts`：import 面板；导航加 `renderNavButton("skills", "Skills", ...)`；`renderActiveSection()` 加 `case "skills"`；`SettingsPanelTag` 加 `settings-skills-panel`；`activeSettingsPanelTag()` 加 `case "skills"`。
- [ ] 4. 接线复用：新面板 `onInstallSkill` → `SettingsDialog.installPiPackage`（复用 `runPiPackageMutation` / `piPackagesApi.install`，零后端改动）。
- [ ] 5. 校验/归一化：按“改动清单第 5 节”选定方案 A（复用 `piPackageSettings.ts` 现有 helper）并落实。
- [ ] 6. 测试：`settingsRoute.test.ts`、`SettingsDialog.general.test.ts`、新建 `SettingsSkillsPanel.test.ts`、`SettingsDialog.packages.test.ts` 父侧接线用例。
- [ ] 7. 验证：`npm run typecheck` / `lint` / vitest / `npm run verify` / `npm run build`；dev stack 手动验证“Skills”面板安装流程。
