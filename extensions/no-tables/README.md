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
- md5：`bc0c2e5da0348102f38dc6a475c6ea34`
- 字节数：`3430`
- 与部署副本 `cmp` 逐字节一致（2026-10-04 收纳时校验）。

## 出处与许可
- **作者声明为自研，未经第三方独立核实。** 本仓不做独立溯源，也不代其声明任何许可证
  归属。

## 历史还原（后续阶段，尚未执行）
本扩展的演化史为三段：`no-tables.ts.bak-20260902-140815`（1926 字节）→
`no-tables.ts.bak-20260902-161100`（3020 字节）→ 现值（3430 字节）。**按时间顺序的三段式
历史还原属后续阶段，确切命令序列见 `.agents/notes/20261004-pi-extensions-repo-init.md`**，
该序列已从**仓内只读副本**取源、不再依赖部署目录：

```
.agents/history/no-tables/no-tables.ts.bak-20260902-140815   md5 279fd272f802613734607bc1f934c6da
.agents/history/no-tables/no-tables.ts.bak-20260902-161100   md5 b0d2175aba58bb27b8ff0f927dc5ef5e
现值 extensions/no-tables/src/no-tables.ts                  md5 bc0c2e5da0348102f38dc6a475c6ea34
```

顺序约束（AGENTS.md 铁律⑥）：**三段式历史还原完成之前，部署目录的 6 个 `.bak-*` 一律不得
归档/清理**；首次入库顺序为**先 scaffold 提交、后三段式还原**。

## 改动流程（铁律）
1. 只在本仓 `extensions/no-tables/src/no-tables.ts` 改；**严禁**直接编辑部署副本。
2. `bash install.sh no-tables` 同步后 `/reload` 或新会话生效。
