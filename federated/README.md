# federated — 联邦式分发与 `install.sh` 部署保护契约

本目录是全机 Pi 扩展的联邦式分发中枢。`install.sh` 是唯一安装/巡检入口，
`federated/registry.json` 是其唯一数据源。本文记录 `install.sh` 的部署保护契约
与 `--audit` 巡检边界，所有行为均以脚本源码为准。

## 数据源：`federated/registry.json`

`registry.json` 是 install.sh 的**唯一数据源**，登记本仓托管的 10 个 TS 扩展条目
（`.extensions[].category`）、权威路径（`.authority`）、部署文件名（`.destFile`），
以及 14 个真实加载入口（13 个 `category=native` 的 PiG 原生 Go 扩展 + 1 个
`category=external` 的 `moshi-hooks.ts`），另含三项全局字段：

- `defaultDestDir` — 真实 pig 扩展部署目录锚点（`~/.pig/agent/extensions`），
  zero-node 保护的第二锚点（见下文）。
- `neverWrite` — 禁写白名单（当前仅 `herdr-agent-state.ts`），与 `install.sh`
  内硬编码的 `NEVERWRITE` 数组双保险一致。
- `settingsJson` — pig settings.json 路径（登记信息，install.sh 不直接使用）。

预检要求：`md5sum`、`jq`、`realpath` 必须在 PATH（缺失即显式 FAILED），
registry 必须存在且为合法 JSON。

## 四类条目

| 类别 | 权威源 | install.sh 行为 |
|---|---|---|
| **hosted**（6 个） | 本仓 `extensions/<name>/src/` | 逐字节直拷部署目录：`md5` 幂等（同则 `unchanged`、不动 mtime），父目录 `mkdir -p`，`install -m 644` |
| **federated**（3 个） | 各自宿主仓 | 以子进程级联调用宿主仓 `install.sh`：`env <installDestEnv>=<dest> bash <installScript>`；上游脚本缺失或失败只记 WARN 不中止 |
| **external**（2 个） | 带外托管 | `herdr-agent-state.ts`（herdr 二进制托管）与 `moshi-hooks.ts`（外部 moshi-hook 工具生成）只读巡检，任何路径绝不写 |
| **native**（14 个） | 本仓 `go/<name>` | PiG 原生 Go 扩展：pig 直接加载 `<destDir>/<destName>/go.mod` 目录；install.sh 不做 TS 安装，仅 `--list`/`--audit` 按 `destName` 检测目录 `go.mod` 在位性（`native Go active`/`absent`/`DRIFT`） |

hosted：auto-continue / no-tables / ctx-orchestrate / jev-gate /
exclude-tools-guard / magpie-image（registry 在册，默认未部署，需显式安装）。
federated：codegraph-go / ctxmode / pi-cache-stats（注意其 `destFile` 为
`cache-stats.ts`，与 `name` 异名）。external：herdr-agent-state / moshi-hooks。
native：cache-stats / codegraph-go-ext / ctxmode-ext / ctx-orchestrate-go /
exclude-tools-guard-go / jev-gate-go / no-tables-go / auto-continue-go /
pi-emphasis / pi-env / pi-my-compact-ui / pi-tinyfish / pi-vcc /
rpiv-ask-user-question（`destName` 为部署目录名，其中 13 个在部署目录有位、
`auto-continue-go` 登记为源存在但未部署）。

目标目录解析顺序：CLI `--dest-dir` > `$PI_EXT_DEST_DIR` > `$HOME/.pig/agent/extensions`。
DEST 非绝对路径、或目标已存在目录 → 显式 FAILED、非零退出、消息走 stderr。
`--dry-run` 只演练不写任何文件；实质改动后提示 `/reload` 或新会话生效。

## 原生 Go 扩展共存保护

宿主机上某扩展可能已由原生 Go 实现接管（pig 直接加载 Go 模块）。判定
（`native_go_ext_dir`，纯只读探测）：

- 目标 `<destDir>/<ext>.ts` 存在**同名目录** `<destDir>/<ext>`——软链指向目录
  也算（`-d` 跟随软链）——**且目录内含 `go.mod`** → 命中；
- 不命中形态：无同名目录 / 悬空软链（`-d` 为假）/ 同名普通文件 / 目录但无 `go.mod`。

命中时（hosted 与 federated 一致）：

- **跳过** TS 安装 / 上游级联（级联连 `env` 变量都不会注入，上游脚本不被调用）；
- 旁挂的残留 `.ts`（含悬空 `.ts` 软链）`rm -f` **自愈删除**；
- `--dry-run` 只打印 `would remove stale` / `would skip`，不删不写；
- `--list` / `--audit` 报 `native Go active (<dir>)`（audit 标 `NATIVE`）；旁挂
  残留 `.ts` 时即使 md5 与源一致也报 `DRIFT (stale .ts alongside native Go ext …)`
  （audit 标 `MISMATCH`，绝不出 `MATCH — DRIFT`）；
- federated 上游脚本缺失且原生 Go 在位时：安装路径跳过且**零 WARN**；只读状态
  合并上报为 `missing (upstream script not found; native Go active at …)`，
  audit 标 `MISMATCH`，不掩盖上游脚本缺失。

## zero-node 无原生 Go 兜底保护

背景：zero-node 宿主机（无 node）上 pig 启动扫描到 `.ts` 扩展会直接崩溃。凡目标
落进**真实 pig 扩展目录**且未被原生 Go 目录接管的条目，绝不写 `.ts`。

受保护判定（`is_zero_node_protected_dest`，三条件同时成立）：

1. 未设 `PI_EXT_ALLOW_ZERO_NODE_TS=1`（显式逃生门，仅供测试/特殊场景）；
2. 当前环境无 node（`command -v node` 失败；测试可用
   `PI_EXT_TEST_PATH_NO_NODE=1` 模拟，不改 PATH）；
3. 目标是真实 pig 扩展目录：未显式指定 dest（默认目录即该路径），或显式指定的
   `DEST_DIR` 经 `realpath -m` 规范化后等于以下任一锚点——
   `$HOME/.pig/agent/extensions`（`$HOME` 非空时）或 registry `.defaultDestDir`
   （非空时）。`realpath -m` 自动消除尾随 `/`、`..`、`.` 并解析已存在前缀上的
   全部符号链接、支持末级目录尚不存在；规范化异常失败时**保守视为受保护**、拒绝放行。

命中时：

- `--all`：**跳过**该条并 `rm -f` 自愈残留 `.ts`（federated 连带跳过级联，上游
  探针脚本不被调用），整体退出码 `0`；
- 显式单装（hosted / federated 一样）：**显式 FAILED 拒绝**（exit 1、消息走
  stderr），不静默跳过、不写 `.ts`——避免用户误以为安装生效；
- 显式 `--dest-dir` 指向其它目录（如 `/tmp` 暂存）**不受限**，离线打包与单测在
  无 node 主机照常工作；
- `PI_EXT_ALLOW_ZERO_NODE_TS=1` 显式放行（非 `1` 的任意值不生效）。

已拦截的路径等价绕过（回归测试覆盖）：尾斜杠 + 受保护目录尚不存在、路径含 `..`、
`$HOME` 为软链、`env -u HOME` 经 `.defaultDestDir` 锚点、经 `PI_EXT_DEST_DIR`
环境变量（非 CLI 参数）指向受保护目录。

## `--audit` 只读巡检边界

`--audit` **严格只读**：不写任何文件、删除任何条目或调用上游安装脚本，退出码
保持 `0`（所有差集/缺失/漂移一律走 WARN 输出而非非零退出）。巡检内容：

- **全部注册条目逐项状态**（TS 条目：md5 源与部署副本比对、权限、`MATCH`/`MISMATCH`/
  `NATIVE`/`----` 判定；external 项只读态标注 `read-only (herdr-managed)`；
  native 项按 `destName` 检测部署目录 `go.mod`，报 `NATIVE — native Go active` /
  `MISMATCH — absent/DRIFT`）；federated 项附上游 `install.sh` 存在性行。
- **dest 目录差集告警**（`audit_unregistered_dest`）：
  - 顶层枚举与已登记条目名集合（TS `.destFile` ∪ native `.destName`）做差集——兼容
    name/destFile 异名条目（如 `pi-cache-stats` → `cache-stats.ts`）与 native 条目
    目录名（如 `codegraph-go-ext` → `codegraph-go`）；
  - 递归扫描子目录（`find -P -mindepth 2`，无限深度，不跟随符号链接、不进入
    符号链接目录），大小写不敏感收集嵌套 `.ts`；非 `.ts` 嵌套文件只进汇总计数；
  - `find`/`sort` 缺失或遍历报错时显式打印 `INCOMPLETE` WARN（说明可能少报），
    绝不静默；
  - 明细表 8 类 `KIND`：`other *.ts` / `other-file` / `historical backup`
    （`*.bak-*` 与 `*.bak`，大小写敏感）/ `__tests__ dir` / `__tests__ file`
    （任意层级 `__tests__/` 内 `.ts`，合法冻结、不点名）/ `nested *.ts` /
    `dir` / `other`；
  - 非普通文件（fifo/socket/device/目录/软链）一律跳过 md5 内容校验
    （`DEST_MD5=----`），防 fifo 等阻塞 `md5sum` 挂死；
  - 表头 `nested .ts scanned=N` 为递归命中 `.ts` 总数（与 KIND 名
    `nested *.ts` 区分）；汇总行给出全部 8 类可分解计数（`dir`/`other` 合并桶
    显式拆分子项）；`other *.ts` 与 `nested *.ts` 进专项 WARN 点名；
  - 差集 WARN 直接 echo 到 stdout（与巡检表同流，非 `warn()` 的 stderr 通道）。

## NEVERWRITE 三重防护（herdr-agent-state.ts）

`herdr-agent-state.ts` 由 herdr 二进制托管，本仓任何路径都绝不写它——
三道独立防线互不替代：

1. **external 类别剔除**：registry `category=external` 的项不进 `installable_names`
   名单；显式 `install.sh herdr-agent-state` 在 install 分支直接 FAILED
   （`refusing to install external/herdr-managed extension`）。
2. **NEVERWRITE destFile 口径枚举过滤**：即使有人把该条目 category 篡改成非
   external，`--all` 的枚举层仍按 `.destFile` 全等比对硬白名单滤掉它（比对口径
   必须是 `.destFile` 而非 `.name`，否则永不匹配形同虚设——该 bug 已修并有回归）。
3. **`refuse_if_neverwrite` 写入层兜底**：hosted 直拷与 federated 级联两条写路径
   在真正写文件前都调用它，按 destFile 全等比对硬白名单，命中即 FAILED
   （`refusing to write neverWrite target`）。

`NEVERWRITE` 数组为脚本内硬编码、与 registry `.neverWrite` 一致，构成双保险；
即使 registry 被篡改也无法移除写入层兜底。

## 参考

- 仓库契约与铁律：`AGENTS.md`
- 回归测试：`tests/install.test.sh`（85 断言覆盖原生 Go 共存、zero-node 兜底（含等价绕过）与 NEVERWRITE 三重防护路径；--audit dest 差集巡检暂未纳入回归测试覆盖）
- 入口：`install.sh`（头注含同一契约摘要；本文为其引用目标）
