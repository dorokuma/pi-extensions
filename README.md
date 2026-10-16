# pi-extensions — 联邦式 Pi 扩展分发中枢

本仓是全机 Pi 扩展的**唯一权威源 / 分发中枢**，架构为**联邦式**：

- **hosted（内生，7 个）**：本仓是其唯一权威源，逐字节同步到部署目录。
- **federated（外生，3 个）**：源**留在各自宿主仓**，本仓仅以子进程级联调用它们各自的
  `install.sh`；改动回宿主仓走既有流程。
- **external（外部，1 个）**：`herdr-agent-state.ts` 由 **herdr 二进制托管**，本仓只读
  巡检、**绝不写它**。

数据源唯一：`federated/registry.json`。请先读 `AGENTS.md` 的 6 条铁律。

## 全机 11 扩展矩阵

| 名称 | 类别 | 权威源（绝对路径） | 目标安装路径 |
|---|---|---|---|
| auto-continue | hosted | `/root/workspace/pi-extensions/extensions/auto-continue/src/auto-continue.ts` | `/root/.pi/agent/extensions/auto-continue.ts` |
| no-tables | hosted | `/root/workspace/pi-extensions/extensions/no-tables/src/no-tables.ts` | `/root/.pi/agent/extensions/no-tables.ts` |
| ctx-orchestrate | hosted | `/root/workspace/pi-extensions/extensions/ctx-orchestrate/src/ctx-orchestrate.ts` | `/root/.pi/agent/extensions/ctx-orchestrate.ts` |
| herdsman-bridge | hosted | `/root/workspace/pi-extensions/extensions/herdsman-bridge/src/herdsman-bridge.ts`（运行时再导出 `/root/workspace/herdsman/packages/herdsman-pi/src/index.ts`） | `/root/.pi/agent/extensions/herdsman-pi.ts` |
| jev-gate | hosted | `/root/workspace/pi-extensions/extensions/jev-gate/src/jev-gate.ts` | `/root/.pi/agent/extensions/jev-gate.ts` |
| exclude-tools-guard | hosted | `/root/workspace/pi-extensions/extensions/exclude-tools-guard/src/exclude-tools-guard.ts` | `/root/.pi/agent/extensions/exclude-tools-guard.ts` |
| magpie-image | hosted | `/root/workspace/pi-extensions/extensions/magpie-image/src/magpie-image.ts` | `/root/.pi/agent/extensions/magpie-image.ts` |
| codegraph-go | federated | `/root/workspace/codegraph-go/integrations/pi/codegraph-go.ts`（宿主仓 `integrations/pi/install.sh`） | `/root/.pi/agent/extensions/codegraph-go.ts` |
| ctxmode | federated | `/root/workspace/ctxmode/integrations/pi/ctxmode.ts`（宿主仓 `integrations/pi/install.sh`） | `/root/.pi/agent/extensions/ctxmode.ts` |
| pi-cache-guardian | federated | `/root/workspace/pi-cache-guardian/extensions/cache-guardian.ts`（宿主仓 `integrations/pi/install.sh`） | `/root/.pi/agent/extensions/cache-guardian.ts` |
| herdr-agent-state | external | herdr 二进制托管（带外，无本仓/宿主仓权威源） | `/root/.pi/agent/extensions/herdr-agent-state.ts`（只读，绝不写） |

> herdsman-bridge 仓内文件名是 `herdsman-bridge.ts`，安装后部署文件名是 `herdsman-pi.ts`，
> 内容逐字一致。

## 命令用法

```bash
# 依赖安装：本机根目录存在 /pnpm-workspace.yaml（勿改它），用 --ignore-workspace 摆脱工作区探测
pnpm setup                 # = pnpm install --ignore-workspace
pnpm check                 # = tsc --noEmit

bash install.sh --list     # 列出 11 扩展：名称/类别/权威源/目标/当前状态
bash install.sh --audit    # 只读巡检 11 扩展（md5/权限/外生脚本/herdr）+ dest 目录未登记条目差集告警
bash install.sh --dry-run <name...>   # 只演练，不写任何文件
bash install.sh <name...>  # 安装指定扩展（hosted 直拷；federated 级联宿主 install.sh）
bash install.sh --all      # 安装全部 hosted+federated（绝不写 herdr-agent-state.ts）
bash install.sh --dest-dir <DIR> ...   # 覆盖目标目录；默认 $PI_EXT_DEST_DIR 或 ~/.pi/agent/extensions
```

## 环境变量

| 变量 | 作用 | 优先级 / 备注 |
|---|---|---|
| `PI_EXT_DEST_DIR` | 目标部署目录 | CLI `--dest-dir` > `$PI_EXT_DEST_DIR` > `~/.pi/agent/extensions`；须为绝对路径 |
| `PI_EXT_SETTINGS_JSON` | herdsman 双加载防护读取的 `settings.json` 覆盖点（**测试/演练用**） | 高于 registry `doubleLoadGuard.settingsFile`，高于 `dirname($DEST_DIR)/settings.json` 回落；正常安装**无需设置**（留空即走 registry 配置的真实 `/root/.pi/agent/settings.json`）。登记于此以消除「隐藏后门 + 死代码」——它不是后门，是已登记的显式覆盖点 |

两条均可通过 `bash install.sh --help` 的 `Env:` 段自查。

`install.sh` 契约与 `/root/workspace/codegraph-go/integrations/pi/install.sh` 对齐：
`set -euo pipefail`、`md5sum` 预检、md5 幂等（同则 `unchanged`、不动 mtime）、源缺失即失败、
目标父目录 `mkdir -p` + `install -m 644`、DEST 非绝对/为目录显式 `FAILED` 非零退出、实质改动
后提示 `/reload` 或新会话生效、外生脚本缺失/失败容忍并在最后一行重打 `WARN` 汇总。
`--audit` 保持严格只读，在巡检完 11 项已登记扩展后，额外对目标目录（`DEST_DIR`）执行顶层差集比对与子目录递归扫描（`find -P -mindepth 2` 无限深度，不跟随符号链接、不进入符号链接目录）：
顶层条目与 `federated/registry.json` 的 `.extensions[].destFile` 做差集，子目录递归收集 `.ts` 文件（非 `.ts` 嵌套文件不逐条列，仅在汇总行计入 `nested non-.ts not listed`），
在明细表中按 8 类 `KIND` 标注（`other *.ts` 顶层独立或残留扩展、`nested *.ts` 子目录未登记 `.ts`、`__tests__ file` `__tests__/` 内合法冻结测试文件、`historical backup` 冻结历史备份［含 `*.bak-*` 与 `*.bak`］、`__tests__ dir` / `dir` 目录、`other-file` / `other`），
打印分类计数（汇总行 `dir [incl. __tests__/]: N` 含 `__tests__ dir`）与未登记 `*.ts`（含顶层 `other *.ts` 与子目录 `nested *.ts`，不含 `__tests__ file`）专项 `WARN` 告警；
差集 `WARN` 直接输出到 **stdout**（非 `warn()` 的 `stderr` 通道），**不改变既有退出码语义（保持 `exit 0`）**。
细节与实测见 `.agents/notes/20261004-pi-extensions-repo-init.md`。

## 已知事项（如实披露）

- **`install.sh --audit` 的 dest 目录差集告警、子目录扫描与退出码语义**：`--audit` 除巡检 `federated/registry.json`
  已登记的 11 项外，会枚举 `DEST_DIR` 顶层条目并按 `.extensions[].destFile` 口径（兼容 `herdsman-bridge` →
  `herdsman-pi.ts`、`pi-cache-guardian` → `cache-guardian.ts` 的异名映射）列出全部未登记顶层条目，同时以
  `find -P -mindepth 2`（无限深度，不跟随符号链接、不进入符号链接目录）递归收集子目录内的 `.ts` 文件：
  - 明细表 `KIND` 共 8 类：`other *.ts`（顶层未登记 `.ts`）、`nested *.ts`（非 `__tests__/` 子目录内的未登记 `.ts`）、
    `__tests__ file`（`__tests__/` 目录内的 `.ts` 测试文件，合法冻结）、`historical backup`（匹配 `*.bak-*` 与 `*.bak` 的历史备份）、
    `__tests__ dir`、`dir`、`other-file`、`other`；非 `.ts` 的嵌套文件不逐条列出，仅在汇总行给计数（`nested non-.ts not listed: N`）；
    汇总行 `dir [incl. __tests__/]: N` 显式包含 `dir` 与 `__tests__ dir`。
  - 专项点名告警 `install.sh WARN: unregistered *.ts in dest-dir: ...` 覆盖顶层 `other *.ts` 与子目录 `nested *.ts`，
    **不点名**合法冻结的 `__tests__ file`。
  - **输出通道说明**：`--audit` 差集段的 `install.sh WARN:` 行直接通过 `echo` 输出到 **stdout**（与巡检表同流，便于单流检索），
    **不走**安装模式 `warn()` 的 `stderr` 通道与脚本末尾 `WARNS` 汇总。
  - **退出码语义与后续可选 `--strict`（未实现）**：即使存在未登记 `.ts` 扩展（如真实部署目录中独立存在的 `gemini-safety.ts`，
    或扩展从 registry 移除后尚未清理的残留副本），`--audit` 默认仍保持 `exit 0`（仅输出 `WARN`）——原因是真实部署目录长期合法共存
    未入 registry 的独立脚本、受铁律⑥ 冻结保护的 8 份 `.bak-*` 以及 `__tests__/` 目录与测试文件，若默认升级为非零退出码会导致日常只读巡检恒常失败、
    破坏既有调用方语义。可选严格模式 `--strict`（在检测到未登记 `other *.ts` / `nested *.ts` 或 `DRIFT` 时返回非零退出码，供强门禁 CI 按需显式开启）
    **当前未实现**，作为后续可选演进项登记于此。
- **`pnpm setup` 返回 exit 1 属已知告警**：pnpm 的 approve-builds 机制会以
  `ERR_PNPM_IGNORED_BUILDS`（`Ignored build scripts: @google/genai / esbuild / protobufjs`）
  令 `pnpm install --ignore-workspace` 整体 exit 1。此时依赖**已装全**（`Already up to date`），
  `pnpm check` / `tsc --noEmit` 实测可用（exit 0）；需要时再 `pnpm approve-builds` 显式挑选用
  哪些依赖的构建脚本。这不是离线退化，也不影响本仓类型检查结论。
- **herdsman 双加载防护的实际保护范围**：仅在默认部署目录下读**真实**
  `/root/.pi/agent/settings.json`（registry `doubleLoadGuard.settingsFile`，实测该文件
  `packages` 无 `herdsman-pi` npm 包）；显式 `--dest-dir <DIR>` 时回落到
  `dirname(<DIR>)/settings.json`，演练场景下该文件通常**不存在** → 打印 note 后放行
  （理由：没有 settings 文件，就不可能装同名 npm 包）。`settings.json` **存在**但非法 JSON 或
  `packages` 非数组时 **fail-closed**：显式 `FAILED`、exit 1，宁可拦住也不放行。读取路径可用
  `PI_EXT_SETTINGS_JSON` 覆盖（见环境变量表；测试/演练用）。
- **`--dest-dir` 对 federated 只是尽力传递、不构成隔离保证**：本仓只按各宿主仓约定的 dest
  环境变量（`PI_EXT_DEST` / `PI_CTXMODE_EXT`）把目标路径传给他们各自的 `install.sh`；宿主脚本
  内部逻辑不透明，理论上可忽略该变量写到别处，本仓**不校验**其写后落点。冷目录演练实测
  3 个 federated 均落在指定目录，但这是宿主脚本的当前行为，不是契约保证。

## 目录布局

```
extensions/<name>/src/<file>.ts     # 7 个内生扩展源码（唯一权威副本）
extensions/<name>/README.md          # 各扩展：权威源/目标/来源/出处/一致性记录
federated/registry.json              # 11 扩展的类别与绝对权威路径（install.sh 数据源）
install.sh                           # 统一安装/巡检入口
.agents/notes/                       # 重大变更与踩坑笔记
.agents/history/no-tables/           # no-tables 两份历史备份的只读副本（三段式还原输入源）
```

## 类型检查

- 主路径：`pnpm setup`（= `pnpm install --ignore-workspace`）+ `tsc --noEmit`，覆盖 6 个
  自包含内生扩展 `auto-continue` / `ctx-orchestrate` / `exclude-tools-guard` / `jev-gate` /
  `magpie-image` / `no-tables`（实测通过，exit 0）。
- **2026-10-09 补 `exclude-tools-guard`**：此前 `extensions/exclude-tools-guard/src/**` 不在
  tsconfig `include`（AGENTS.md 铁律②与矩阵都列了该扩展、类型检查却不覆盖它）→ 已按字母序补入
  `include`，当时本仓 tsc 图由 6 个增至 7 个（2026-10-16 移除已归档扩展后现为 6 个），与 `include` 逐项一致。
- **2026-10-06 修正两处既有缺口**：① `extensions/ctx-orchestrate/src/**` 与新增的
  `extensions/jev-gate/src/**` 此前不在 tsconfig `include`（旧图只列 3 个扩展，导致
  「检查通过」为假绿）；② 源码 `import { Type } from "typebox"` 所需依赖此前仅存在于
  pnpm store（pi 的传递依赖），故补声明 `devDependencies.typebox: 1.3.27`（与 pi 侧同版本）。
- **2026-10-09 补 `magpie-image` 与其依赖**：新增 `extensions/magpie-image/src/**` 进
  `include` 后，`tsc --noEmit` 报 `TS2307: Cannot find module '@earendil-works/pi-ai'`
  （该扩展的 image 相关类型来自此包）→ 依授权补声明
  `devDependencies.@earendil-works/pi-ai: ^1.0.0`（本就是 `pi-coding-agent` 的传递依赖，
  锁文件里已有 1.0.0，未新增任何其它依赖）。补后 exit 0。
- `herdsman-bridge` 是一个跨仓再导出壳，目标源码在 `/root/workspace/herdsman`（铁律禁改，
  由其自身 tsconfig + `typecheck` 脚本负责）；bundler 解析下会触发它的 `.ts` 绝对导入
  `TS5097` 并拖入 herdsman 自身源码的既有告警，故**不纳入**本仓 tsc 图（见 tsconfig
  `include`/`exclude` 与该扩展 README）。
- 退化方案与其局限详见初始笔记；未伪造任何「检查通过」。

## 状态

- **已入库（历史快照：截至 `50ee528`；后续已由 `ab50f9d` 入库同步并经 `77885dd` 合入 `main`）**：截至 `50ee528` 时 `main` 上有 **8 个 commit**（`fc1176a` scaffold → `b9d5331` 第 5 个 hosted 入库 →
  `50ee528`），当时 `origin/main` 与 `main` 同指 `50ee528`、工作分支 `chore/catchup-magpie-image`
  自 `50ee528` 开出且改动尚未提交；**后续已由 `ab50f9d` 入库同步并经 `77885dd` 合入 `main`**（当前 `main` 与 `origin/main` 同指 `77885dd`，共 **10 个 commit**）。阶段 0（工程底座）+ 阶段 1（hosted 逐字纳管）+ 阶段 2
  （`install.sh` + `registry.json`）均已完成并通过双审（reviewer / oracle），3 条 should-fix 已在
  首次入库前的修复轮落地（install.sh herdsman 防护 fail-closed + settings 解析优先级、
  installable_names 死代码修正、no-tables 三段式还原输入源与部署目录解耦），详见初始笔记。
- **计数现状**：hosted **7 个**（auto-continue / no-tables / ctx-orchestrate /
  herdsman-bridge / jev-gate / exclude-tools-guard / magpie-image），全机 **11 个**
  （7 hosted + 3 federated + 1 external）。
- **本轮收尾内容**（历史快照：截至 `50ee528` 时为分支 `chore/catchup-magpie-image` 未提交改动；后续已由 `ab50f9d` 入库同步并经 `77885dd` 合入 `main`）：三处「部署比仓内新」的漂移整文件
  回仓（jev-gate / exclude-tools-guard / no-tables 的 rule F 修复）；当时第 8 个 hosted 扩展
  `magpie-image` 纳管（2026-10-16 移除已归档扩展后现为 7 个 hosted）；no-tables rule F 回归测试收入本仓；计数同步（`registry.json` /
  `install.sh` / `AGENTS.md` / `tsconfig.json`）；`.gitignore` 补 `*.bak-*`（保留
  `.agents/history/no-tables/` 两份只读副本的既有反选例外）。
- **注记 ①（no-tables 三段式历史还原）**：该还原**已在 `main` 执行完毕**，落在 3 个提交上
  （`241a969` restore v1，1926B → `33ba8e7` restore v2，3020B → `c44b86d` current managed copy，
  3430B），三个 commit 均只改 `extensions/no-tables/src/no-tables.ts` 一个文件；其输入源是**仓内只读
  副本** `.agents/history/no-tables/no-tables.ts.bak-20260902-140815`（`279fd272…` / 1926 字节）与
  `…-161100`（`b0d2175a…` / 3020 字节），不依赖部署目录。初始笔记
  `.agents/notes/20261004-pi-extensions-repo-init.md` 中该序列的「【待执行】」等过程时态是
  2026-10-04 的过程记录、**已被后续 3 个还原提交覆盖**（该笔记是
  历史过程记录，不改写）。该序列
  commit-3 的历史期望值 `bc0c2e5da0348102f38dc6a475c6ea34` / 3430 字节是**历史快照、本身正确，
  不需校正**；现值 `921c34be3d4f8a004a3066f3c1267b90` / 8159 字节是 2026-10-08 rule F 修复
  回仓后的版本（历史快照：截至 `50ee528` 时为工作区未提交改动；后续已由 `ab50f9d` 入库同步），**不回写**该历史序列。
- **强制约束 ②（原注记 ②：`.bak-*` 冻结线，现行有效）**：AGENTS.md 铁律⑥ 关于部署目录 `.bak-*` 的冻结线**仍然有效**——
  三段式还原虽已在 `main` 执行完毕，但部署目录的 8 个 `.bak-*`（它们是还原的原始凭据）
  **在未获主代理另行书面解冻之前**，一律不得归档/清理/移动/改名；解冻与否只取决于主代理的书面指令，
  **不以「还原是否完成并复核」为停止条件**。计数口径已统一为 **8 个**（cache-guardian 4 份 + no-tables 2 份
  + jev-gate / exclude-tools-guard 各 1 份），铁律①/⑥ 与 no-tables README 同值。
