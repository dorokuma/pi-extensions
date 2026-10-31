# pi-extensions — 联邦式 Pi 扩展分发中枢

本仓是全机 Pi 扩展的**唯一权威源 / 分发中枢**，架构为**联邦式**：

- **hosted（内生，6 个）**：本仓是其唯一权威源，逐字节同步到部署目录。
- **federated（外生，3 个）**：源**留在各自宿主仓**，本仓仅以子进程级联调用它们各自的
  `install.sh`；改动回宿主仓走既有流程。
- **external（外部，1 个）**：`herdr-agent-state.ts` 由 **herdr 二进制托管**，本仓只读
  巡检、**绝不写它**。

数据源唯一：`federated/registry.json`。请先读 `AGENTS.md` 的 6 条铁律。

## 全机 10 扩展矩阵

| 名称 | 类别 | 权威源（绝对路径） | 目标安装路径 |
|---|---|---|---|
| auto-continue | hosted | `/root/workspace/pi-extensions/extensions/auto-continue/src/auto-continue.ts` | `/root/.pi/agent/extensions/auto-continue.ts` |
| no-tables | hosted | `/root/workspace/pi-extensions/extensions/no-tables/src/no-tables.ts` | `/root/.pi/agent/extensions/no-tables.ts` |
| ctx-orchestrate | hosted | `/root/workspace/pi-extensions/extensions/ctx-orchestrate/src/ctx-orchestrate.ts` | `/root/.pi/agent/extensions/ctx-orchestrate.ts` |
| jev-gate | hosted | `/root/workspace/pi-extensions/extensions/jev-gate/src/jev-gate.ts` | `/root/.pi/agent/extensions/jev-gate.ts` |
| exclude-tools-guard | hosted | `/root/workspace/pi-extensions/extensions/exclude-tools-guard/src/exclude-tools-guard.ts` | `/root/.pi/agent/extensions/exclude-tools-guard.ts` |
| magpie-image | hosted | `/root/workspace/pi-extensions/extensions/magpie-image/src/magpie-image.ts` | `/root/.pi/agent/extensions/magpie-image.ts` |
| codegraph-go | federated | `/root/workspace/codegraph-go/integrations/pi/codegraph-go.ts`（宿主仓 `integrations/pi/install.sh`） | `/root/.pi/agent/extensions/codegraph-go.ts` |
| ctxmode | federated | `/root/workspace/ctxmode/integrations/pi/ctxmode.ts`（宿主仓 `integrations/pi/install.sh`） | `/root/.pi/agent/extensions/ctxmode.ts` |
| pi-cache-stats | federated | `/root/workspace/pi-cache-stats/extensions/cache-stats.ts`（宿主仓 `integrations/pi/install.sh`） | `/root/.pi/agent/extensions/cache-stats.ts` |
| herdr-agent-state | external | herdr 二进制托管（带外，无本仓/宿主仓权威源） | `/root/.pi/agent/extensions/herdr-agent-state.ts`（只读，绝不写） |

## 命令用法

```bash
# 依赖安装：本机根目录存在 /pnpm-workspace.yaml（勿改它），用 --ignore-workspace 摆脱工作区探测
pnpm setup                 # = pnpm install --ignore-workspace
pnpm check                 # = tsc --noEmit

bash install.sh --list     # 列出 10 扩展：名称/类别/权威源/目标/当前状态
bash install.sh --audit    # 只读巡检 10 扩展（md5/权限/外生脚本/herdr）+ dest 目录未登记条目差集告警
bash install.sh --dry-run <name...>   # 只演练，不写任何文件
bash install.sh <name...>  # 安装指定扩展（hosted 直拷；federated 级联宿主 install.sh）
bash install.sh --all      # 安装全部 hosted+federated（绝不写 herdr-agent-state.ts）
bash install.sh --dest-dir <DIR> ...   # 覆盖目标目录；默认 $PI_EXT_DEST_DIR 或 ~/.pi/agent/extensions
```

## 环境变量

| 变量 | 作用 | 优先级 / 备注 |
|---|---|---|
| `PI_EXT_DEST_DIR` | 目标部署目录 | CLI `--dest-dir` > `$PI_EXT_DEST_DIR` > `~/.pi/agent/extensions`；须为绝对路径 |

可通过 `bash install.sh --help` 的 `Env:` 段自查。

`install.sh` 契约与 `/root/workspace/codegraph-go/integrations/pi/install.sh` 对齐：
`set -euo pipefail`、`md5sum` 预检、md5 幂等（同则 `unchanged`、不动 mtime）、源缺失即失败、
目标父目录 `mkdir -p` + `install -m 644`、DEST 非绝对/为目录显式 `FAILED` 非零退出、实质改动
后提示 `/reload` 或新会话生效、外生脚本缺失/失败容忍并在最后一行重打 `WARN` 汇总。
`--audit` 保持严格只读，在巡检完 10 项已登记扩展后，额外对目标目录（`DEST_DIR`）执行顶层差集比对与子目录递归扫描（`find -P -mindepth 2` 无限深度，不跟随符号链接、不进入符号链接目录）：
顶层条目与 `federated/registry.json` 的 `.extensions[].destFile` 做差集，子目录**大小写不敏感**递归收集 `.ts` 文件（覆盖 `.ts` / `.TS` / `.Ts` / `.tS`；非 `.ts` 嵌套文件不逐条列，仅在汇总行计入 `nested non-.ts not listed`），
在明细表中按 8 类 `KIND` 标注（`other *.ts` 顶层独立或残留 `.ts`［大小写不敏感］、`nested *.ts` 非 `__tests__/` 子目录内未登记 `.ts`［大小写不敏感］、`__tests__ file` 任意层级 `__tests__/` 目录内［匹配 `__tests__/*` 或 `*/__tests__/*`］的合法冻结测试文件、`historical backup` 冻结历史备份［含 `*.bak-*` 与 `*.bak`，大小写敏感］、`__tests__ dir` / `dir` 目录、`other-file` / `other`），
对非普通文件（fifo / socket / device / 目录 / 符号链接）一律**跳过 `md5sum` 内容校验**（`DEST_MD5` 显示 `----` 并在 `AUDIT` 列注明 `content check skipped`）以防 fifo 阻塞挂死，
表头与汇总的递归 `.ts` 扫描计数统一标注为 `nested .ts scanned`（与 `KIND` 名 `nested *.ts` 区分），汇总行将合并桶显式分解为 `dir [merged: dir=A + __tests__ dir=B]: C` 与 `other [merged: other-file=D + other=E]: F`（使明细行数与汇总数字可双向推导），并输出未登记 `*.ts`（含顶层 `other *.ts` 与子目录 `nested *.ts`，不含 `__tests__ file`）专项 `WARN` 告警；
同时显式检查 `find` / `sort` 可用性并捕获退出码与 `stderr`，若递归扫描失败/受阻绝不静默，而是在表头/范围行标注 `INCOMPLETE` 并输出 `install.sh WARN: recursive subdirectory scan INCOMPLETE ... — nested entries may be under-reported`；
差集 `WARN` 直接输出到 **stdout**（非 `warn()` 的 `stderr` 通道），**不改变既有退出码语义（保持 `exit 0`）**；差集明细表仅供人工可读巡检，自动化检索应消费单行 `install.sh WARN:` 汇总（换行/空白/控制字符文件名的边界见已知限制第 8 条）。
设计取舍与已知边界详见下文「已知事项（如实披露）」小节。

## 已知事项（如实披露）

- **`install.sh --audit` 的 dest 目录差集告警、子目录扫描与退出码语义**：`--audit` 除巡检 `federated/registry.json`
  已登记的 10 项外，会枚举 `DEST_DIR` 顶层条目并按 `.extensions[].destFile` 口径（兼容
  `pi-cache-stats` → `cache-stats.ts` 的异名映射）列出全部未登记顶层条目，同时以
  `find -P -mindepth 2`（无限深度，不跟随符号链接、不进入符号链接目录）递归收集子目录内的 `.ts` 文件：
  - **`.ts` 后缀大小写不敏感**：顶层 `other *.ts` 与子目录递归收集均先取小写副本再匹配 `*.ts`（覆盖 `.ts` / `.TS` / `.Ts` / `.tS`），明细表与专项点名保留原始文件名大小写；`historical backup`（`*.bak-*` / `*.bak`）与 `__tests__` 目录名保持大小写敏感，不改变其它类别语义。
  - **任意层级 `__tests__/` 归类契约**：子目录 `.ts` 只要位于任意层级的 `__tests__/` 目录内（相对路径匹配 `__tests__/*` 或 `*/__tests__/*`，如 `__tests__/no-tables.test.ts`、`__tests__/nested/z.ts`、`x/__tests__/y.ts`），均归为 `__tests__ file`（进明细表披露，**不进**专项 `WARN` 点名）；其余子目录 `.ts` 归为 `nested *.ts`（进明细表且与顶层 `other *.ts` 一并进 `install.sh WARN: unregistered *.ts in dest-dir: ...` 专项点名）。
  - **表头与汇总标签区分及可分解计数**：明细表 `KIND` 共 8 类（`other *.ts`、`nested *.ts`、`__tests__ file`、`historical backup`、`__tests__ dir`、`dir`、`other-file`、`other`）；非 `.ts` 嵌套文件不逐条列出，仅计入 `nested non-.ts (count-only)` / `nested non-.ts not listed`。表头与汇总中的递归 `.ts` 命中总数统一用 `nested .ts scanned`（等于 `nested *.ts` + `__tests__ file`），与 `KIND` 标签 `nested *.ts` 严格区分；汇总行合并桶显式写出可分解细项 `dir [merged: dir=A + __tests__ dir=B]: C` 与 `other [merged: other-file=D + other=E]: F`，保证「明细表行数 ↔ 汇总数字」可双向推导。
  - **特殊文件防挂死（跳过内容校验）**：`md5_of`、`status_of` 与 `audit_unregistered_dest` 对所有非普通文件（fifo / socket / device / 目录 / 符号链接，即不满足 `[ ! -L ] && [ -f ]` 者）一律跳过 `md5sum` 内容校验，`DEST_MD5` 输出 `----` 并在 `AUDIT` 列说明 `content check skipped`，确保含 `mkfifo` 管道等特殊文件时 `--audit` 永不阻塞挂死。
  - **`find` / `sort` 失败不静默**：递归扫描前显式预检 `command -v find` 与 `command -v sort`，执行时通过 `set -o pipefail` 与 FD 3 捕获 `find ... | sort -z` 的退出码与 `stderr`。一旦 `find` / `sort` 缺失或子树遍历报错（非零退出），绝不输出「全部已登记 / 完整 recursive scan」的虚假结论，而是在表头标注 `[INCOMPLETE: exit <rc>]`、在 `scan scope:` 与末尾显式打印 `install.sh WARN: recursive subdirectory scan INCOMPLETE in <ddir> (exit <rc>: <reason>) — nested entries may be under-reported (read-only audit; exit code unchanged).`（之所以选打印明确 `WARN` 而非 `fail` 非零退出，是为了在部分子树权限受限或受限环境缺 `find` 时仍完整输出 11 项已登记扩展与顶层差集的只读巡检结果，并维持 `--audit` 默认 `exit 0` 的只读契约一致）。
  - **输出通道说明**：`--audit` 差集段的 `install.sh WARN:` 行直接通过 `echo` 输出到 **stdout**（与巡检表同流，便于单流检索），
    **不走**安装模式 `warn()` 的 `stderr` 通道与脚本末尾 `WARNS` 汇总。
  - **退出码语义与后续可选 `--strict`（未实现）**：即使存在未登记 `.ts` 扩展（如真实部署目录中独立存在的 `gemini-safety.ts`，
    或扩展从 registry 移除后尚未清理的残留副本），`--audit` 默认仍保持 `exit 0`（仅输出 `WARN`）——原因是真实部署目录长期合法共存
    未入 registry 的独立脚本、受铁律⑥ 冻结保护的 8 份 `.bak-*` 以及 `__tests__/` 目录与测试文件，若默认升级为非零退出码会导致日常只读巡检恒常失败、
    破坏既有调用方语义。可选严格模式 `--strict`（在检测到未登记 `other *.ts` / `nested *.ts` 或 `DRIFT` 时返回非零退出码，供强门禁 CI 按需显式开启）
    **当前未实现**，作为后续可选演进项登记于此。
  - **`install.sh --audit` 差集巡检的 8 项已知限制与边界（受控落盘）**：
    1. **稀疏或超大普通 `.ts` 文件不被跳过**：**现象**：普通稀疏文件仍全量计算哈希（不被非普通文件跳过逻辑覆盖），每文件调用两次全量 `md5sum`（`install.sh:411,456`）；8 GiB 稀疏普通文件多次实测单次全量 `md5sum` 约 9–11s、单文件两轮（探测 + 取值）纯双次 `md5sum` 墙钟约 18–22s、整次 `--audit`（`timeout 60`）墙钟约 18–22s（均随 CPU 负载波动）；`timeout 20` 处于临界，随 CPU 负载波动可能得到 `RC=0` 也可能得到 `RC=124`；**触发条件**：`DEST_DIR` 顶层或子目录内存在稀疏或超大**普通** `.ts` 文件（如 `truncate -s 8G "$D/a.ts"`，`blocks=0` 仍满足 `[ ! -L ] && [ -f ]` 普通文件判据，不属特殊文件跳过范围）；**可复现命令**（建议改用 `timeout 60` 稳定观察完整耗时；`timeout 20` 处于临界，可能得到 `RC=0` 也可能得到 `RC=124`）：`D="$(mktemp -d)"; truncate -s 8G "$D/a.ts"; timeout 60 bash install.sh --dest-dir "$D" --audit; echo "RC=$?"; rm -rf "$D"`；**影响与是否影响当前生产**：极端超大或稀疏普通 `.ts` 会拖慢 `--audit`（在 `timeout 20` 等临界短超时下可能触发 `RC=124`），**不影响当前生产**（真实生产 dest 最大文件 83803 字节，多次实测整次审计墙钟约 0.3–0.6s）；**是否为文档明示取舍**：属仅跳过非普通文件（fifo/socket/device/目录/软链）的已知边界，**已排期 R4 做功能加固（见下文「后续计划（R4）」小节：`stat` 大小/块数预检或流式取样），当前仅作文档登记**。
    2. **差集表不是稳定 TSV**：**现象**：明细表以 `printf '%-42s %-22s %-34s %-5s %s\n'`（`install.sh:386,444,479`）定宽空格对齐而非 TSV，路径超过 42 列时 `KIND` 会粘到路径列（仅隔单空格，存在示例：下述复现命令打出的明细行长为 192–199 字符 / 194–201 字节［随路径长度变化：嵌套 `.ts` 行 192 字符 / 194 字节、目录行 199 字符 / 201 字节；`AUDIT` 列中 UTF-8 破折号 `—` 占 1 字符 / 3 字节］），含换行的文件名会拆成多行；**触发条件**：未登记条目相对路径长度 > 42 列，或文件名含换行符（`$'\n'`）；**可复现命令**（复现命令需在 bash 下执行，`$'…'` 为 bash ANSI-C 引用，`/bin/sh`［dash］下不展开）：`D="$(mktemp -d)"; mkdir -p "$D/a_very_long_subdirectory_name_exceeding_42_columns"; touch "$D/a_very_long_subdirectory_name_exceeding_42_columns/ext.ts" "$D/"$'line1\nline2.ts'; bash install.sh --dest-dir "$D" --audit; rm -rf "$D"`；**影响与是否影响当前生产**：按固定列宽或空白分列解析明细表的自动化门禁会误分类或漏报（且文件名含换行符、制表符或首尾空白时专项 `WARN` 亦会被拆行或破坏空白分列，详见第 8 条），**不影响当前生产**（真实生产 dest 最长未登记路径为 42 列［`exclude-tools-guard.ts.bak-20261008-122514`］，恰好等于 `printf '%-42s'` 的列宽阈值、处于零裕度边界，当前不粘连仅因 8 份 `.bak-*` 受铁律⑥冻结、长度不再变化；且生产无换行或特殊空白文件名）；**是否为文档明示取舍**：**是**，上文 `--audit` 契约段已同步明示明细表仅供人工可读巡检，自动化检索应消费单行 `install.sh WARN:` 汇总（换行/空白/控制字符文件名的边界见第 8 条）。
    3. **`find -P` 对软链目录静默**：**现象**：被 `find -P`（`install.sh:328`）跳过的符号链接目录内部内容既不进 `nested non-.ts` 内部文件计数也不进 `.ts` 专项点名，仅在 `scan scope:` 行总述 `symlinks not followed (find -P / lstat)`；**触发条件**：`DEST_DIR` 顶层或子目录内存在指向目录（含其下有 `.ts` 或非 `.ts` 文件）的符号链接；**可复现命令**：`D="$(mktemp -d)"; T="$(mktemp -d)"; touch "$T/hidden.ts" "$T/note.txt"; mkdir -p "$D/sub"; ln -s "$T" "$D/sub/symdir"; bash install.sh --dest-dir "$D" --audit; rm -rf "$D" "$T"`；**影响与是否影响当前生产**：经软链目录挂载的子树内容不会被递归展开巡检，**不影响当前生产**（真实生产 dest 无符号链接目录）；**是否为文档明示取舍**：**是**，属防软链环路与防越界读取的文档明示安全取舍。
    4. **`INCOMPLETE` 场景退出码仍为 `0`**：**现象**：递归扫描受阻时虽显式输出 `[INCOMPLETE: exit <rc>]` + `install.sh WARN: recursive subdirectory scan INCOMPLETE ...` 且绝不假报「全部已登记」，但整体仍返回 `exit 0`（`install.sh:383,599`）；**触发条件**：`PATH` 缺 `find` / `sort`（`exit 127`）或子目录无读权限（如 `chmod 000` 触发 `find` `exit 1`；注：在 root 下直接运行不触发，因 root 无视 `000`、`find` `rc=0`、无 `INCOMPLETE`，需非 root 或降权运行［如 `runuser -u nobody -- env LC_ALL=C PATH=/usr/bin:/bin /bin/bash …`］才触发；降权执行时输出 `INCOMPLETE: exit 1`、`权限不够`［或 `Permission denied`］、整体退出码仍为 `0`）；**可复现命令**：`D="$(mktemp -d)"; mkdir -p "$D/locked"; chmod 000 "$D/locked"; bash install.sh --dest-dir "$D" --audit; echo "RC=$?"; chmod 700 "$D/locked"; rm -rf "$D"`（需非 root 或降权运行，如 `runuser -u nobody -- env LC_ALL=C PATH=/usr/bin:/bin /bin/bash …` 才触发；降权执行时输出 `INCOMPLETE: exit 1`、`权限不够`［或 `Permission denied`］、整体退出码仍为 `0`）；**影响与是否影响当前生产**：只看退出码而不检查 stdout `WARN` / `INCOMPLETE` 的门禁会放行，**不影响当前生产**（真实生产 dest 权限完整、`find`/`sort` 可用）；**是否为文档明示取舍**：**是**，属 README 明示的只读契约取舍（强门禁非零退出留待可选 `--strict`）。
    5. **消费端异常早退导致 `find` 子进程 `SIGPIPE` 时保守报 `INCOMPLETE`**：**现象**：若本脚本消费循环（`install.sh:335-352`）未读完管道即异常早退关闭 FD，子进程管道触发 `SIGPIPE`（或非零退出），`wait "$find_pid"`（`install.sh:354-358`）会捕获非零 `sub_rc` 并并入 `scan_rc`，从而打印保守误报的 `INCOMPLETE`；**触发条件**：主脚本读取循环在 `find ... | sort -z` 输出耗尽前因异常或信号提前中断；**可复现命令**：`bash -c 'exec {fd}< <(set -o pipefail; yes "sub/a.ts" | head -n 50000 | tr "\n" "\0" | LC_ALL=C sort -z); pid=$!; IFS= read -r -d "" _ <&"$fd"; exec {fd}<&-; rc=0; wait "$pid" || rc=$?; echo "wait_rc=$rc"'`；**影响与是否影响当前生产**：异常早退时倾向于保守报 `INCOMPLETE` 而非静默假绿，方向正确、无需改动，**不影响当前生产**（真实生产 dest 正常全量消费无早退）；**是否为文档明示取舍**：**是**，属 fail-safe 保守告警设计取舍，无需改动。
    6. **`md5_of` 每文件调用两次（探测 + 取值）**：**现象**：`status_of`（`install.sh:230-235`）与 `audit_unregistered_dest`（`install.sh:411,456`）对每个普通文件先执行 `if md5_of "$f" >/dev/null 2>&1` 探测再执行 `$(md5_of "$f")` 取值，导致每文件派生 2 次 `md5sum`；**触发条件**：`--list` / `--audit` 校验任意普通文件（已登记源/目标 + 未登记普通文件）时均触发；**可复现命令**：`BASH_XTRACEFD=7 bash -x install.sh --audit 7>&1 >/dev/null 2>/dev/null | grep -c 'md5sum --'`（前提：registry 中 10 个非 `external` 扩展的权威源 `authority.sourcePath` 全部满足 `[ ! -L "$src" ] && [ -f "$src" ]` 普通文件判据［`install.sh:230`］；真实生产 dest 普通文件 21 个 + 满足该判据的权威源普通文件 10 个 = 31 个，每文件 2 次共调用 62 次）；**影响与是否影响当前生产**：目录文件多或体积大时有双倍 `md5sum` 开销，**不影响当前生产**（真实生产 dest 普通文件 21 个、最大文件 83803 字节，连同满足普通文件判据的权威源 10 个共 62 次调用，多次实测整次审计墙钟约 0.3–0.6s）；**是否为文档明示取舍**：属既有实现已知开销，**已排期在 R4 与第 1 项一并优化（见下文「后续计划（R4）」小节：合并 `md5_of` 的单文件双次调用），当前仅作文档登记**。
    7. **顶层与子目录（含 `__tests__/`）内 fifo 命名的 `.ts` 分类与专项点名三分支非对称**：**现象**：顶层 `[ -f ]` 判定在 `install.sh:410`、顶层 `.ts` 收集在 `install.sh:423-428`（`423` 为 `*.ts)` 分支头、`424-428` 为分支体，`429` 行起为 `*)` 的 `other-file` 分支），故顶层名为 `x.ts` 的 fifo 落 `other` 分支（`install.sh:438-443` 赋 `kind="other"` 与 `content check skipped`；明细行 `install.sh:444` 的 `DEST_MD5=----` 来自顶层循环默认赋值 `install.sh:391`）且**不**出现在 `unregistered *.ts in dest-dir:` 点名行；而子目录在 `find -P`（`install.sh:328`）收集阶段按后缀入列（`install.sh:344-346`）时不区分文件类型，随后按相对路径分两支（非普通文件在明细行 `install.sh:479` 的 `DEST_MD5=----` 来自子目录循环默认赋值 `install.sh:451`）——非 `__tests__/` 子目录内的 `sub/y.ts` fifo 归为 `nested *.ts`（`install.sh:471`、`ts_list` 入列在 `470`）并进入 `unregistered *.ts in dest-dir:` 专项点名 `echo`（`install.sh:484`），而任意层级 `__tests__/` 下的 `__tests__/t.ts`、`sub/__tests__/u.ts` fifo 则命中 `__tests__/*|*/__tests__/*` 分支归为 `__tests__ file`（`install.sh:459-467`，`kind` 赋值在 `461`）、`DEST_MD5=----`（`install.sh:451` 默认值）、**不进** `unregistered *.ts in dest-dir:` 点名行；**触发条件**：`DEST_DIR` 下存在以 `.ts` 结尾的 fifo（或 socket/device 等非普通文件），按位置分三分支：① 顶层（`[ -f ]` 为假 → 归 `other`、**不点名**）；② 非 `__tests__/` 子目录（归 `nested *.ts`、**会点名**）；③ 任意层级 `__tests__/` 下（归 `__tests__ file`、**不点名**）；**可复现命令**：`D="$(mktemp -d)"; mkfifo "$D/x.ts"; mkdir -p "$D/sub" "$D/__tests__" "$D/sub/__tests__"; mkfifo "$D/sub/y.ts" "$D/__tests__/t.ts" "$D/sub/__tests__/u.ts"; bash install.sh --dest-dir "$D" --audit; rm -rf "$D"`；**影响与是否影响当前生产**：三类位置的 `.ts` 命名特殊文件在 `KIND` 分类（`other` / `nested *.ts` / `__tests__ file`）及是否进入 `unregistered *.ts in dest-dir:` 专项点名上存在非对称（三者均跳过 `md5sum` 不挂死），**不影响当前生产**（真实生产 dest 无 fifo/socket/device 等特殊文件）；**是否为文档明示取舍**：属顶层先按文件类型分流、子目录先按后缀收集再按 `__tests__/` 路径分流的实现路径差异所致已知非对称边界，如实记录于此。
    8. **换行、空白与反斜杠文件名的拆行、漏分类、空白分列破坏及 `DEST_MD5` 前导 `\`**：**现象**：除顶层换行 `.ts` 文件名（如 `$'line1\nline2.ts'`）既拆明细表（见第 2 条，`install.sh:444`）又将末尾专项点名 `echo "$PROG WARN: unregistered *.ts in dest-dir: ${ts_list[*]} ..."`（`install.sh:484`；`483` 为 `if`、`485` 为 `fi`）拆成两行外，同类控制字符/空白/反斜杠文件名还存在以下边界：① **嵌套换行 `.ts` 同样拆专项 `WARN`，且含 `\n` 或 `\` 的文件名会给 `DEST_MD5` 加前导 `\`**：如 `sub/$'nline1\nnline2.ts'` 既拆明细表嵌套行（`install.sh:479`）又拆 `install.sh:484` 专项 `WARN`；同时凡文件名含换行符（`$'\n'`）或反斜杠（`\`，如 `back\slash.ts`）的普通文件，GNU `md5sum -- "$1"`（`install.sh:79`）会在输出行首加转义前缀 `\`，令 `${out%% *}`（`install.sh:82`）截出的 `DEST_MD5` 带前导 `\`（如 `\d41d8cd98f00b204e9800998ecf8427e`）；② **尾空格文件名（`trail.ts `）不进 `*.ts` 分类，且在表上显示成 `trail.ts`**：顶层 `trail.ts `（末尾为空格）在 `case "$base_lc" in *.ts)`（`install.sh:422-423`）不匹配 `*.ts` 而落入 `other-file`（`install.sh:429-434`），且因 `printf '%-42s'`（`install.sh:444`）以空格右填充会吃掉尾空格，**在表上显示成 `trail.ts`**、看起来像普通 `.ts` 被错分成 `other-file`；嵌套 `sub/trail.ts ` 在 `case "$rel_lc" in *.ts)`（`install.sh:344-345`）不匹配而落入 `nested non-.ts (count-only)`（`install.sh:348-350`），二者均**不进** `other *.ts` / `nested *.ts` 与专项 `WARN` 点名；③ **首空格、内部空格或制表符（`tab\t.ts`）破坏按空白分列**：顶层 ` .ts`、嵌套 `sub/ .ts` 及含制表符的 `$'tab\t.ts'`（或 `sub/$'ntab\t.ts'`）虽仍归 `other *.ts` / `nested *.ts`（`$'tab\t.ts'` 的 `DEST_MD5` 无前导 `\`）且不拆行，但在明细表（`install.sh:444,479`）与 `install.sh:484` 以空格拼接的 `${ts_list[*]}` 中，空格与 `\t` 均会破坏按空白分列（`\t` 还会因制表位展开破坏明细表列对齐）；**触发条件**：`DEST_DIR` 顶层或子目录内存在含换行符（`$'\n'`）、制表符（`$'\t'`）、反斜杠（`\`）、尾随空格或前导/内部空格的文件名；**可复现命令**（复现命令需在 bash 下执行，`$'…'` 为 bash ANSI-C 引用，`/bin/sh`［dash］下不展开；单测顶层换行：`D="$(mktemp -d)"; touch "$D/"$'line1\nline2.ts'; bash install.sh --dest-dir "$D" --audit; rm -rf "$D"`；六类同测）：`D="$(mktemp -d)"; mkdir -p "$D/sub"; touch "$D/"$'line1\nline2.ts' "$D/sub/"$'nline1\nnline2.ts' "$D/trail.ts " "$D/ .ts" "$D/"$'tab\t.ts' "$D/"$'back\\slash.ts'; bash install.sh --dest-dir "$D" --audit; rm -rf "$D"`；**影响与是否影响当前生产**：第 2 条所述「自动化应消费单行 `WARN`」在顶层/嵌套换行、尾空格漏分类及空格/`\t` 破坏空白分列场景下**同样受限或失效**（含 `\` 或 `\n` 时 `DEST_MD5` 列还多出前导 `\`），**不影响当前生产**（真实生产 dest 无含换行符、制表符、反斜杠或首尾空白的文件名）；**是否为文档明示取舍**：属直接拼接原始文件名与 `md5sum` 首字段、未对控制字符/空白/反斜杠做转义或归一化的已知边界（与第 2 条交叉关联），如实记录于此。
- **`pnpm setup` 返回 exit 1 属已知告警**：pnpm 的 approve-builds 机制会以
  `ERR_PNPM_IGNORED_BUILDS`（`Ignored build scripts: @google/genai / esbuild / protobufjs`）
  令 `pnpm install --ignore-workspace` 整体 exit 1。此时依赖**已装全**（`Already up to date`），
  `pnpm check` / `tsc --noEmit` 实测可用（exit 0）；需要时再 `pnpm approve-builds` 显式挑选用
  哪些依赖的构建脚本。这不是离线退化，也不影响本仓类型检查结论。
- **`--dest-dir` 对 federated 只是尽力传递、不构成隔离保证**：本仓只按各宿主仓约定的 dest
  环境变量（`PI_EXT_DEST` / `PI_CTXMODE_EXT`）把目标路径传给他们各自的 `install.sh`；宿主脚本
  内部逻辑不透明，理论上可忽略该变量写到别处，本仓**不校验**其写后落点。冷目录演练实测
  3 个 federated 均落在指定目录，但这是宿主脚本的当前行为，不是契约保证。

## 目录布局

```
extensions/<name>/src/<file>.ts     # 6 个内生扩展源码（唯一权威副本）
extensions/<name>/README.md          # 各扩展：权威源/目标/来源/出处/一致性记录
federated/registry.json              # 10 扩展的类别与绝对权威路径（install.sh 数据源）
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
- 原 `herdsman` TS 桥已归档至 `archive/`（不再纳入本仓 tsc 图与安装面）。
- 退化方案与其局限详见初始笔记；未伪造任何「检查通过」。

## 状态

- **已入库（历史快照：截至 `50ee528`；后续已由 `ab50f9d` 入库同步并经 `77885dd` 合入 `main`）**：截至 `50ee528` 时 `main` 上有 **8 个 commit**（`fc1176a` scaffold → `b9d5331` 第 5 个 hosted 入库 →
  `50ee528`），当时 `origin/main` 与 `main` 同指 `50ee528`、工作分支 `chore/catchup-magpie-image`
  自 `50ee528` 开出且改动尚未提交；**后续已由 `ab50f9d` 入库同步并经 `77885dd` 合入 `main`**（当前 `main` 与 `origin/main` 同指 `77885dd`，共 **10 个 commit**）。阶段 0（工程底座）+ 阶段 1（hosted 逐字纳管）+ 阶段 2
  （`install.sh` + `registry.json`）均已完成并通过双审（reviewer / oracle），3 条 should-fix 已在
  首次入库前的修复轮落地（installable_names 死代码修正、no-tables 三段式还原输入源与部署目录解耦），详见初始笔记。
- **计数现状**：hosted **6 个**（auto-continue / no-tables / ctx-orchestrate /
  jev-gate / exclude-tools-guard / magpie-image），全机 **10 个**
  （6 hosted + 3 federated + 1 external）。
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

## 后续计划（R4）

以下为已登记的 R4 排期范围（两项），作为上文「`install.sh --audit` 差集巡检的 8 项已知限制与边界」第 1、6 条的仓库内可追溯锚点（当前仅作文档登记，不修改 `install.sh`）：

1. **稀疏/超大普通文件按 `stat` 大小/块数预检或流式取样（对应已知限制第 1 条）**：对满足 `[ ! -L ] && [ -f ]` 的**普通**文件，在调用全量 `md5sum` 前按 `stat` 文件大小/块数做预检或改用有界流式取样，避免稀疏或超大普通 `.ts` 文件将只读审计拖长。
2. **合并 `md5_of` 的单文件双次调用（对应已知限制第 6 条）**：将 `status_of`（`install.sh:230-235`）与 `audit_unregistered_dest`（`install.sh:411,456`）中「先 `if md5_of "$f" >/dev/null 2>&1` 探测、再 `$(md5_of "$f")` 取值」的双次调用合并为单次调用，消除每文件双倍 `md5sum` 调用开销。
