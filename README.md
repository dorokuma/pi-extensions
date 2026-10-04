# pi-extensions — 联邦式 Pi 扩展分发中枢

本仓是全机 Pi 扩展的**唯一权威源 / 分发中枢**，架构为**联邦式**：

- **hosted（内生，4 个）**：本仓是其唯一权威源，逐字节同步到部署目录。
- **federated（外生，3 个）**：源**留在各自宿主仓**，本仓仅以子进程级联调用它们各自的
  `install.sh`；改动回宿主仓走既有流程。
- **external（外部，1 个）**：`herdr-agent-state.ts` 由 **herdr 二进制托管**，本仓只读
  巡检、**绝不写它**。

数据源唯一：`federated/registry.json`。请先读 `AGENTS.md` 的 6 条铁律。

## 全机 8 扩展矩阵

| 名称 | 类别 | 权威源（绝对路径） | 目标安装路径 |
|---|---|---|---|
| auto-continue | hosted | `/root/workspace/pi-extensions/extensions/auto-continue/src/auto-continue.ts` | `/root/.pi/agent/extensions/auto-continue.ts` |
| no-tables | hosted | `/root/workspace/pi-extensions/extensions/no-tables/src/no-tables.ts` | `/root/.pi/agent/extensions/no-tables.ts` |
| prism | hosted | `/root/workspace/pi-extensions/extensions/prism/src/prism.ts` | `/root/.pi/agent/extensions/prism.ts` |
| herdsman-bridge | hosted | `/root/workspace/pi-extensions/extensions/herdsman-bridge/src/herdsman-bridge.ts`（运行时再导出 `/root/workspace/herdsman/packages/herdsman-pi/src/index.ts`） | `/root/.pi/agent/extensions/herdsman-pi.ts` |
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

bash install.sh --list     # 列出 8 扩展：名称/类别/权威源/目标/当前状态
bash install.sh --audit    # 只读巡检 8 扩展：md5/权限/外生脚本是否存在/herdr 只读态
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
细节与实测见 `.agents/notes/20261004-pi-extensions-repo-init.md`。

## 已知事项（如实披露）

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
extensions/<name>/src/<file>.ts     # 4 个内生扩展源码（唯一权威副本）
extensions/<name>/README.md          # 各扩展：权威源/目标/来源/出处/一致性记录
federated/registry.json              # 8 扩展的类别与绝对权威路径（install.sh 数据源）
install.sh                           # 统一安装/巡检入口
.agents/notes/                       # 重大变更与踩坑笔记
.agents/history/no-tables/           # no-tables 两份历史备份的只读副本（三段式还原输入源）
```

## 类型检查

- 主路径：`pnpm setup`（= `pnpm install --ignore-workspace`）+ `tsc --noEmit`，覆盖 3 个
  自包含内生扩展 `auto-continue` / `no-tables` / `prism`（实测通过，exit 0）。
- `herdsman-bridge` 是一个跨仓再导出壳，目标源码在 `/root/workspace/herdsman`（铁律禁改，
  由其自身 tsconfig + `typecheck` 脚本负责）；bundler 解析下会触发它的 `.ts` 绝对导入
  `TS5097` 并拖入 herdsman 自身源码的既有告警，故**不纳入**本仓 tsc 图（见 tsconfig
  `include`/`exclude` 与该扩展 README）。
- 退化方案与其局限详见初始笔记；未伪造任何「检查通过」。

## 状态
- 阶段 0（工程底座）+ 阶段 1（逐字纳管 4 内生扩展）+ 阶段 2（install.sh + registry.json）已完成，
  双审（reviewer / oracle）均判「可放行」；3 条 should-fix 已在**首次入库前的修复轮**全部落地
  （install.sh herdsman 防护 fail-closed + settings 解析优先级、installable_names 死代码修正、
  no-tables 三段式还原输入源与部署目录解耦），观察项已登记遗留清单，详见初始笔记。
  **仍是零 commit**（main 分支无任何提交）。
- 首次入库顺序已钉死：**先 scaffold 提交（骨架 + 4 个副本 + install.sh + registry +
  `.agents/history/no-tables/` 两份只读历史副本），后 no-tables 三段式还原**；三段式还原完成前，
  部署目录 6 个 `.bak-*` 一律不得归档/清理（AGENTS.md 铁律⑥）。
- 部署目录 6 个 `.bak-*` 的归档/清理、no-tables 三段式历史还原的**执行**，均属后续阶段，
  待 scaffold 提交后按铁律⑥顺序进行。
