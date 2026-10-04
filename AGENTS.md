# AGENTS.md — pi-extensions（联邦式 Pi 扩展分发中枢）

本仓是全机 Pi 扩展的**唯一权威源/分发中枢**（架构：联邦式）。任何人/任何 agent 在本仓
工作前，先读这份铁律。

## 铁律（不可违背）

1. **严禁直接编辑部署目录下的 `.ts`**：`/root/.pi/agent/extensions/*.ts` 一律只读。
   不要在那里改代码、不要手动 `cp` 覆盖、不要删/改/命名其下任何文件（含 6 个 `.bak-*`
   历史备份，其归档/清理属后续阶段）。部署副本只能由本仓 `install.sh` 依 md5 幂等同步。

2. **内生（hosted）扩展改动只在本仓做**：`auto-continue` / `no-tables` / `prism` /
   `herdsman-bridge` / `ctx-orchestrate` 的源码修改必须落在 `extensions/<name>/src/`，**类型检查通过后**再
   `bash install.sh <name>` 同步到部署目录（`/reload` 或新会话生效）。不要在部署目录就地改。

3. **外生（federated）扩展改动回宿主仓**：`codegraph-go` / `ctxmode` / `pi-cache-guardian`
   的源**留在各自宿主仓**（`/root/workspace/codegraph-go`、`/root/workspace/ctxmode`、
   `/root/workspace/pi-cache-guardian`），本仓只以子进程级联调用它们各自的
   `integrations/pi/install.sh`。要改它们，去对应宿主仓走其既有流程，**不要**在本仓副本改。

4. **重大变更与踩坑必须落笔记**：结构/契约/口径变更、双审事项、踩坑复盘，写入
   `.agents/notes/YYYYMMDD-<slug>.md`（带 front-matter：`status` 等）。见本仓初始笔记
   `20261004-pi-extensions-repo-init.md`。注意：`.agents/notes/` 属**本地过程记录、
   gitignore 不入库**（远端读者看不到，这是有意的——过程资产不是交付物），因此铁律④
   不产生悬空引用：需要入库的结论必须同步落到 `README.md` / 各扩展 `README.md`
   的对应小节，而不是依赖 notes 的远端可见性。

5. **任何脚本/流程都绝不写 `herdr-agent-state.ts`**：该文件由 **herdr 二进制托管**，本仓
   只在其旁挂自定义钩子。`install.sh` 对它是硬编码禁写白名单（`NEVERWRITE`）+
   `external` 类别剔除，双重防护；`--audit` 只对它只读巡检。**禁止**通过本仓任何脚本
   写入、覆盖、删除它。

6. **`.bak-*` 归档/清理的前置条件（顺序钉死）**：**no-tables 三段式历史还原完成之前，部署
   目录 `/root/.pi/agent/extensions/` 下的 6 个 `.bak-*` 一律不得归档/清理/移动/改名**——它们是
   还原的原始凭据（本仓 `.agents/history/no-tables/` 只存了两份 no-tables 历史备份的只读副本）。
   同理，**首次入库顺序钉死为「先 scaffold 提交（骨架 + 4 个 hosted 副本 + install.sh +
   registry + 历史副本），后 no-tables 三段式还原」**，不得出现「无骨架却有内容」的历史。

## 架构：联邦三层

| 类别 | 扩展 | 权威源 | 本仓职责 |
|---|---|---|---|
| hosted（内生） | auto-continue / no-tables / prism / herdsman-bridge / ctx-orchestrate | 本仓 `extensions/<name>/src/` | 唯一权威源，逐字节同步 |
| federated（外生） | codegraph-go / ctxmode / pi-cache-guardian | 各自宿主仓 | 仅级联调用其 `install.sh` |
| external（外部） | herdr-agent-state | herdr 二进制（带外） | 只读巡检，绝不写 |

`herdr` 二进制还托管着 `herdr-agent-state.ts`；本仓与其只读共存。详见 `README.md` 的全机
9 扩展矩阵与 `federated/registry.json`。

## 命令入口

```bash
# 依赖安装（本机根目录有 /pnpm-workspace.yaml，勿改它，用 --ignore-workspace 摆脱干扰）
pnpm setup                       # = pnpm install --ignore-workspace
pnpm check                       # = tsc --noEmit（见 README『类型检查』的范围与局限）

bash install.sh --list           # 列出 9 扩展：名称/类别/权威源/目标/当前状态
bash install.sh --audit          # 只读巡检 9 扩展：md5/权限/外生脚本是否存在/herdr 只读态
bash install.sh --dry-run <name> # 只演练，不写任何文件
bash install.sh <name...>        # 安装指定扩展（hosted 直拷；federated 级联宿主 install.sh）
bash install.sh --all            # 安装全部 hosted+federated（绝不写 herdr-agent-state.ts）
bash install.sh --dest-dir <DIR> # 覆盖目标目录；默认 $PI_EXT_DEST_DIR 或 ~/.pi/agent/extensions
```

数据源唯一：`federated/registry.json`。`install.sh` 契约与
`/root/workspace/codegraph-go/integrations/pi/install.sh` 对齐（`set -euo pipefail`、
`md5sum` 预检、md5 幂等、源缺失即失败、目标父目录 `mkdir -p`、`install -m 644`、DEST
非绝对/为目录显式 FAILED 非零退出、改动后提示 `/reload`、外生失败在最后一行重打 WARN 汇总）。

## 部署副本一致性
hosted 扩展仓内副本与部署副本在收纳时已 `cmp` 逐字节一致；md5/字节数记录在各自
`extensions/<name>/README.md`。`install.sh` 保证此后同步幂等（md5 同则 `unchanged`，
不动 mtime）。
