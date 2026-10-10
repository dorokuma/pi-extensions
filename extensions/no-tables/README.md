# no-tables

## 概况
把 assistant 输出里 pi 终端不支持的 Markdown 静默改写为兼容形态，零配置、无用户交互：
1. Markdown 表格 → 项目符号列表（首列作标签，其余列作 `明细: 值`）。
2. h3–h6 标题 → 加粗行；脚注定义/引用归一；图片 → 链接。
3. Markdown 链接 → 「标签 + URL」纯文本 / 裸 URL。
围栏代码块（``` / ~~~）内的内容不处理。通过 `message_end` 钩子改写 assistant 消息。

## 权威源 / 目标 / 来源
- 权威源（本仓，唯一）：`extensions/no-tables/src/no-tables.ts`
  （绝对路径 `/root/workspace/pi-extensions/extensions/no-tables/src/no-tables.ts`）
- 目标安装路径：`/root/.pi/agent/extensions/no-tables.ts`
- 来源：自本机部署副本逐字纳管（`/root/.pi/agent/extensions/no-tables.ts`），内容一字未改。

## 纳管时的一致性记录
- 部署值（2026-10-08 rule F 修复后）：md5 `921c34be3d4f8a004a3066f3c1267b90` / `8159` 字节 / `202` 行
- 与部署副本 `cmp` 逐字节一致（2026-10-09 回仓时校验）；`bash install.sh --audit` 该项 `MATCH`。
- 历史沿革：纳管初值 `bc0c2e5da0348102f38dc6a475c6ea34` / `3430` 字节（2026-10-04 收纳）→
  2026-10-08 rule F 修复（`normalizeMisplacedFences`：贴文字的围栏行独立成行；
  `convertUnsupportedMarkdown` 前后各跑一遍 F，后者修 rule A 吞掉换行的交互）后为上值。
- **回归测试**：`extensions/no-tables/test/no-tables.test.ts`（12 用例，覆盖 rule F 的
  a–h 与幂等/边界），自部署目录 `__tests__/no-tables.test.ts` 收入本仓。
  **因本仓是分层布局（源在 `src/`）而部署是扁平布局，import 路径须从 `"../no-tables.ts"`
  改为 `"../src/no-tables.ts"`**（不改则 `ERR_MODULE_NOT_FOUND`）；用例与断言逐字相同，
  仓内副本 md5 `826f1f202c7d5d6e64615038c1642040` / 5646 字节 / 127 行。
  跑法 `node --test extensions/no-tables/test/no-tables.test.ts`（实测 12/12 pass）。

## 出处与许可
- **作者声明为自研，未经第三方独立核实。** 本仓不做独立溯源，也不代其声明任何许可证
  归属。

## 历史还原（已于 main 执行完毕）
本扩展的演化史为三段：`no-tables.ts.bak-20260902-140815`（1926 字节）→
`no-tables.ts.bak-20260902-161100`（3020 字节）→ 现值（2026-10-04 收纳时为 3430 字节，
2026-10-08 rule F 修复后为 8159 字节）。**按时间顺序的三段式历史还原已在 `main` 执行完毕**，
落在 3 个提交上（`241a969` restore v1，1926B → `33ba8e7` restore v2，3020B → `c44b86d`
current managed copy，3430B），三个 commit 均只改 `extensions/no-tables/src/no-tables.ts`
一个文件；确切命令序列见 `.agents/notes/20261004-pi-extensions-repo-init.md`，该序列从**仓内
只读副本**取源、不依赖部署目录：

```
.agents/history/no-tables/no-tables.ts.bak-20260902-140815   md5 279fd272f802613734607bc1f934c6da
.agents/history/no-tables/no-tables.ts.bak-20260902-161100   md5 b0d2175aba58bb27b8ff0f927dc5ef5e
现值 extensions/no-tables/src/no-tables.ts                  md5 921c34be3d4f8a004a3066f3c1267b90（8159 字节）
```

> 初始笔记的「【待执行】」等过程时态是 2026-10-04 的过程记录、**已被后续 3 个还原提交覆盖**
> （该笔记是历史过程记录，不改写）。该序列 commit-3 的历史期望值
> `bc0c2e5da0348102f38dc6a475c6ea34` / 3430 字节是**历史快照、本身正确，不需校正**；
> 工作区现值 `921c34be3d4f8a004a3066f3c1267b90` / 8159 字节是 2026-10-08 rule F 修复回仓后的
> **未提交改动**，只描述工作区，**不回写**该历史序列。

顺序约束（AGENTS.md 铁律⑥）：部署目录的 **8 个** `.bak-*` **在未获主代理另行书面解冻之前**
一律不得归档/清理/移动/改名（它们是还原的原始凭据）；首次入库顺序为**先 scaffold 提交、后三段式还原**。

## 改动流程（铁律）
1. 只在本仓 `extensions/no-tables/src/no-tables.ts` 改；**严禁**直接编辑部署副本。
2. `bash install.sh no-tables` 同步后 `/reload` 或新会话生效。
