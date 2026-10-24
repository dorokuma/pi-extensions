# exclude-tools-guard (Go 移植版)

本组件是原 TS 版 `exclude-tools-guard.ts` 向 PiG Go 扩展体系的行为等价移植实现。

> **基准与文件声明**：
> - 部署版（`/root/.pi/agent/extensions/exclude-tools-guard.ts`）与仓库未提交版（`extensions/exclude-tools-guard/src/exclude-tools-guard.ts`）经 `md5sum` 校验逐字节一致（MD5：`30722dbb05328b63a2cb87729072b7a0`）。
> - 本 Go 移植以该内容为权威基准。
> - 本组件自身 diff 不含这两个 TS 文件（`extensions/exclude-tools-guard/README.md` 与 `extensions/exclude-tools-guard/src/exclude-tools-guard.ts`）；分支上它们的未提交改动来自 exclude-tools-guard 的 TS 侧加固，不是本组件所为。部署版与仓内版 MD5 断言保持一致。本组件严格遵守铁律，只修改 `go/exclude-tools-guard/` 目录与自身依赖，不修改 `/root/.pi/agent/` 下的任何文件。

---

## 四大核心行为要点

### 1. 子代理身份判定（SUBAGENT-IDENTITY 块，fail-closed）
- **判定模型**：主代理必须具备四项正向证据（S1-S4）全部成立；任一项缺位、查询失败、超时或无法确证，一律 fail-closed 判定为子代理。
  - **负向标记直接推断**：若 `PI_SUBAGENT=1`、`PI_ROLE` 非空、或 `argv`（包含通过 `PIG_HARNESS_ARGV_FILE` 重建的宿主参数）含有 `--name role-`、`/tmp/herdr-role-sessions`、`/tmp/dispatch-`，立即判定为子代理。
  - **S1 pane id 必要条件（pane 命名不构成判据）**：`HERDR_PANE_ID` 必须非空——它只是向 herdr 查询 `shell_pid` 的**查找键**。**pane 命名（是否以 `:p1` 结尾等）绝不参与判定**：pane 可被用户或 herdr 重命名/重编号，命名与「谁拥有这个会话」无关。pane id 只作为辅助证据写入 reason 字符串（例如 `pane=w32:p23 (aux)`）。
  - **S2 herdr 运行态查询**：向运行中的 herdr unix socket（常量写死 `/root/.config/herdr/herdr.sock`，避免环境变量伪造）发送 JSON-RPC 请求：
    ```json
    {"id":"pi-identity","method":"pane.process_info","params":{"pane_id":"<paneId>"}}
    ```
    一次查询硬上限 1500ms（从函数入口起算的总预算硬上限，**不延长预算、不叠加阻塞式重试层**）。回执必须满足 `reply.id === "pi-identity"`、`reply.result.process_info.pane_id === paneId`、且 `shell_pid > 0`。查询失败/超时一律 fail-closed 判子代理，并只向 stderr 记**一条** warning（`sync.Once` 去重）。**S1/S3/S4 的 fail-closed 分支同样走这条去重 warning**（reason 里带分支名，例如 `S1: HERDR_PANE_ID unset or empty`、`S3: no agent-runtime ancestor …`、`S4: agent runtime ancestor …`），`PI_IDENTITY_DEBUG=1` 只增加细节、不改变判定。
    > **已知局限（stderr 在部署形态可能不可见）**：pig 会把扩展子进程的 stderr 重定向到临时文件且通常不保留（`ref/PiG/coding/extension/host/subprocess/host.go:3238-3248`，D56），所以这条 warning 在真实部署里**可能看不到**。它只是尽力而为的信号，不是可依赖的交付通道；排障时以 `PI_IDENTITY_DEBUG=1` 的前台运行或 reason 字符串为准。
  - **S3 内核进程表祖先链与宿主锚定**：通过回溯本进程的 `/proc/<pid>/stat`（最多 32 跳），提取祖先进程链。该祖先链必须包含 S2 查询得到的 `shell_pid`，且 `shell_pid` 必须位于宿主进程的严格祖先方向（`shellIndex > hostIndex`）。宿主位置由**进程拓扑**锚定，与 pane 命名无关：
    - **subprocess 形态（主判别式 `DetectSubprocessForm`）**：从 self 向上遍历，**第一个** `IsAgentRuntimeCmdline(cmdline)` 为真的祖先即宿主；若到 pane shell 之前都没找到 → fail-closed 判子代理（reason 写清 `no agent-runtime ancestor` 与命中的环境键）。subprocess 形态的判据覆盖全部三种 out-of-process 传输形态：
      - isolated 进程：`PIG_EXT_SOCKET` 非空；
      - packed cell 成员：任一以 `PIG_EXT_SOCKET_` 为前缀且**值非空**的环境变量（真实键名由 `runtimecell.SocketEnvName` 生成，本组件为 `PIG_EXT_SOCKET_EXCLUDE_TOOLS_GUARD`）；
      - packed cell 进程标记：`PIG_EXT_PACKED_CELL` / `PIG_EXT_ACTIVE_MEMBERS` 非空。

      前缀按键名枚举后**逐个回查 `getenv(key)` 并要求非空**：列在环境里但值为空的键（`PIG_EXT_SOCKET_X=`）不算证明；键名按枚举原样查询，不做大小写折叠（Linux 环境变量名大小写敏感）。
    - **fused 形态（上述全部缺失）**：self 即宿主（`hostIndex = 0`）；若 self cmdline 识别不出 agent runtime → fail-closed 判子代理。
  - **S4 无中间 agent 嵌套**：宿主进程（`hostIndex`）与 `shell_pid` 之间的所有祖先进程，其 `/proc/<pid>/cmdline` 绝不能含有 agent 运行时（`pi`、`pig`、`ctxmode`、`pi-coding-agent`、`bundle/cli.js`）。扫描范围严格限定为 `for hop := hostIndex + 1; hop < shellIndex; hop++`，跳过宿主自身。
- **fail-closed 处置**：任何 socket 连接失败、读取超时、JSON 解析错误、字段不匹配、宿主锚定失败或内核链断裂，均按子代理会话处置。
- **诊断开关**：`PI_IDENTITY_DEBUG=1` 时向 stderr 输出判定理由（如 `[subagent-identity] subagent: S1: HERDR_PANE_ID unset or empty`），不改变判定逻辑。

### 2. 排除工具集合
- **所有会话（主代理 + 子代理）**：强制剔除 `grep`、`find`、`ls`、`bash`（强制走 CodeGraph、`ctx_fs`、`ctx_run`）。
- **子代理会话（额外排除）**：额外剔除 `jev_check`（主代理门禁信号）与 `ask_user_question`（面向用户的问询通道）。
- **过滤准则**：「只移除、不新增」，严格保持现有活跃工具的相对顺序。若过滤后工具列表与过滤前长度一致，绝不调用 `SetActiveTools`。

### 3. 生效时机（两次兜底）
- **`session_start` 事件**：会话初始化启动时首次执行身份判定与活跃工具列表过滤。
- **`before_agent_start` 事件**：在每个 agent turn 开始时进行二次兜底，防范「会话恢复」、「中途激活」或运行时工具列表被重置。

### 4. 子代理系统提示词清理
- 在 `before_agent_start` 事件中，若判定为子代理会话：
  1. 强制清空 `systemPromptOptions.appendSystemPrompt`（设为 `""`）。
  2. 删除 `systemPromptOptions.sections` 里的 `addendum` 自定义 section（`DeleteAddendumSection`）。
  3. 若原 `appendSystemPrompt` 非空且 `forceSystemPrompt` 存在，按两步剥离 `<addendum>` 块：
     - 第一步：剥离 `\n\n<addendum>\n${leakedAppend}\n</addendum>`。
     - 第二步：剥离 `<addendum>\n${leakedAppend}\n</addendum>`。
- 主代理会话则完全不触碰 `systemPromptOptions`。
- **为何要删 section**：`RenderSystemPrompt` 先用 `appendSystemPrompt` 构建 base sections（`internal/codingagent/prompts/coding.go:124-131` 生成同名 `addendum` section），再用自定义 `sections` 叠加（`inproc/runner.go:2569-2582` → `prompts.ApplyCustomSystemPromptSections`：非空同名 section 是**覆盖/追加**，并把值重新包成 `<addendum>…</addendum>`）。只清 `appendSystemPrompt` 不动 `sections["addendum"]`，泄漏会被重新叠回提示词。

---

## 行为对照表（TS 行号 → Go 符号名）

| 行为模块 | TS 原版 (`exclude-tools-guard.ts`) | Go 移植版 (`exclude-tools-guard/extension.go`) | 说明 |
|---|---|---|---|
| **常量定义** | L49-L53, L240-L242 | `HerdrSocketFile`, `IdentityQueryTimeout`, `IdentityAncestorMaxHops`, `excludedTools`, `subagentExcludedTools`, `ExcludedTools()`, `SubagentExcludedTools()` | 常量与内部过滤集合及只读访问函数 |
| **查询 herdr socket** | L69-L128 (`queryPaneShellPid`) | `QueryPaneShellPid` | 从函数入口起算硬上限 1500ms 总预算，使用标准库 `net.DialTimeout`，严格支持超时、1MB 缓冲截断与 JSON 验证，fail-closed |
| **读取 /proc ppid** | L131-L142 (`readProcPpid`) | `ReadProcPpid` | 按最后一个 `)` 之后切分并读取第 2 个字段，防御性处理空格与括号 |
| **读取 /proc cmdline** | L145-L151 (`readProcCmdline`) | `ReadProcCmdline` | 以 `\0` 分隔并过滤空串，空格拼接 |
| **进程祖先链回溯** | L154-L168 (`readAncestorChain`) | `ReadAncestorChain` | 32 跳上限，环路与终止检测（`ppid == 0 \|\| ppid == current`） |
| **agent 运行时特征检测** | L171-L177 (`isAgentRuntimeCmdline`) | `IsAgentRuntimeCmdline` | 匹配 `pi`、`pig`、`ctxmode` 及正则 `pi-coding-agent\|bundle/cli\.js` |
| **负向标记检测** | L180-L189 (`subagentLaunchMarker`) | `SubagentLaunchMarker`, `LoadEffectiveArgs` | 检测 `PI_SUBAGENT=1`、`PI_ROLE` 与 argv 特征（含通过 `PIG_HARNESS_ARGV_FILE` 桥接宿主参数） |
| **fail-closed 身份推断** | L194-L236 (`resolveAgentIdentity`, `isSubagentSession`) | `ResolveAgentIdentityWithEnv`, `ResolveAgentIdentity`, `IsSubagentSession`, `DetectSubprocessForm`, `failClosed` | 形态判据为 `DetectSubprocessForm`（`PIG_EXT_SOCKET` / `PIG_EXT_SOCKET_<NAME>` / `PIG_EXT_PACKED_CELL` / `PIG_EXT_ACTIVE_MEMBERS`），宿主锚定为第一个 agent-runtime 祖先（subprocess）或 self（fused），S3 校验 `shellIndex > hostIndex`，S4 只扫中间进程，pane 命名仅作辅助证据，S1-S4 任一失败均 fail-closed + 单次去重 warning |
| **工具过滤纯函数** | L244-L255 (`enforceExclusions`) | `FilterActiveTools`, `EnforceToolExclusions` | 纯切片过滤保持相对顺序；无变更时不调用 `SetActiveTools`；debug 下记录打点 |
| **提示词清理** | L266-L277 (before_agent_start) | `StripAddendumBlock`, `CleanPromptOptions`, `DeleteAddendumSection` | 清空 `appendSystemPrompt` + 删除 `sections` 里的 `addendum` section + 两段式剥离 `forceSystemPrompt` 中的 `<addendum>` 块，含非字符串防护。**与 TS 加固版的行为差**：TS 加固版只清 `appendSystemPrompt`、不删 `sections["addendum"]`，Go 版多做了这一步（PiG 会用 custom section 覆盖并重包 `<addendum>`，只清 `appendSystemPrompt` 会被重新叠回） |
| **扩展工厂与事件接线** | L239, L258-L278 | `Extension` | 导出 `func Extension() *sdk.Extension`，挂载 `session_start` 与 `before_agent_start`，首错去重 warning |

---

## 与 TS 原版的细微差异

1. **进程拓扑假设与宿主锚定（M1 必修，已定稿）**：
   - **TS 原版拓扑**：跑在 Node 宿主同进程（fused / in-process），`process.pid` 即宿主本身，祖先链 `chain[0]=pi(宿主)`、`chain[1]=pane shell`。其 S4 检查循环 `for (let hop = 1; hop < index; hop++)` 在 `index = 1` 时遍历区间为空，实际执行 0 次，自然跳过了宿主本身。
   - **Go 移植版拓扑**：作为 PiG subprocess 运行时，`os.Getpid()` 为扩展子进程（`chain[0]`），其 agent-runtime 祖先才是宿主进程，再往上一级才是 pane shell。若原样按照 hop=1 检查，循环将直接检查到宿主本身，因 `IsAgentRuntimeCmdline("pi")`（或将来的 `pig`）命中而误将主代理判为子代理。
   - **主判别式与宿主锚定（最终语义）**：以 **`DetectSubprocessForm`** 区分形态——命中即 subprocess 形态，全不命中即 fused 形态。判据覆盖三种 out-of-process 形态：isolated 的 `PIG_EXT_SOCKET`、packed cell 成员的 `PIG_EXT_SOCKET_<NAME>`（前缀枚举 + 逐个回查非空）、packed cell 标记 `PIG_EXT_PACKED_CELL` / `PIG_EXT_ACTIVE_MEMBERS`。**pane 命名（`HERDR_PANE_ID` 是否以 `:p1` 结尾）不参与判定**，只作为辅助证据写入 reason。宿主位置由拓扑推导：
     - subprocess：从 self 向上取**第一个** agent-runtime 祖先为 `hostIndex`；到 pane shell 之前仍未找到 → fail-closed 判子代理（`no agent-runtime ancestor`）。
     - fused：`hostIndex = 0`（self 即宿主）；self cmdline 识别不出 agent runtime → fail-closed 判子代理。
   - **已移除的旁路**：早先的 `|| len(chain) >= 2` 推断（`PIG_EXT_SOCKET` 缺失时靠链长猜宿主）会误锚定扫描窗口，把 `ext→sh→pi→bash` 这类隔层 spawn 拓扑误判为子代理，已删除。
   - S3 校验 `shellIndex > hostIndex`，S4 扫描范围限定为 `for hop := hostIndex + 1; hop < shellIndex; hop++`，跳过宿主自身，只扫描「宿主之上、pane shell 之下」的中间进程。若中间嵌套了其他 agent runtime，依然判定为子代理，兼顾了正确识别主代理与高强度防御。同时扩展 `IsAgentRuntimeCmdline` 识别 `pig`，为后续宿主切换至 pig 提供无缝支撑。
   - **herdr 查询失败/超时**：一律 fail-closed 判子代理，只向 stderr 记一条 warning（`sync.Once` 去重），**不延长 1500ms 预算、不叠加阻塞式重试层**。
   - **packed-go cell 形态修复（阻断项，2026-10-24 修复）**：Go 扩展在真实 PiG 下**默认作为 packed cell 成员运行**——进程环境里没有 `PIG_EXT_SOCKET`，只有每成员一条 `PIG_EXT_SOCKET_<NAME>`（`runtimecell.SocketEnvName`，`host/runtimecell/go_packed.go:844-846`；`host/subprocess/packed_go.go:422` 逐个写入）以及 `PIG_EXT_PACKED_CELL` / `PIG_EXT_ACTIVE_MEMBERS`（`packed_go.go:495-497`）。早先「只看 `PIG_EXT_SOCKET`」会把 packed runner（cmdline 形如 `<cells>/go/<hash>/runner`）误判为 fused 形态、自身 cmdline 认不出 → fail-closed 把**真实主代理**判成子代理，进而摘掉 `jev_check` / `ask_user_question` 并每个 turn 清 `appendSystemPrompt`。现由 `DetectSubprocessForm` 覆盖三种形态，回归用例用真实键名 `PIG_EXT_SOCKET_EXCLUDE_TOOLS_GUARD` 断言判 main、夹 agent runtime 时判 subagent、isolated 形态不回归。
2. **宿主 argv 桥接策略（M2 与使用后清理）**：
   - 原 TS 注释明确拒绝依赖可被伪造的环境变量；但由于 PiG 派生 Go 扩展子进程时为无参（`os.Args` 仅包含自身二进制路径），不得不通过宿主写入的 `PIG_HARNESS_ARGV_FILE` 文件桥接宿主参数（aligned with `runtime-node/process-identity.mjs:21-22` 及 `harness_identity.go:24`）。
   - **Fail-closed 防御策略**：若 `PIG_HARNESS_ARGV_FILE` 环境变量存在但文件缺失或包含坏 JSON，立即 fail-closed 判定为子代理（防止攻击者通过篡改环境变量屏蔽 `--name role-` 等负向标记）；仅当该环境变量完全未设置时，才安全退化为仅使用 `os.Args`。
   - **敏感路径清理**：读取完毕后立即执行 `os.Unsetenv("PIG_HARNESS_ARGV_FILE")`，与 TS 侧 `delete process.env.PIG_HARNESS_ARGV_FILE` 对齐，防止包含宿主完整参数的临时文件路径泄漏给扩展后续拉起的子进程。
   - **进程内 argv 缓存（M2 回归修复）**：`ResolveAgentIdentityWithEnv` 每个 turn 都会调用身份判定，而 `unsetenv` 是进程级的——第一次读取后环境变量即被清空，同进程第二次判定会退回 `os.Args`（子进程 argv 仅含二进制路径），`--name role-` 等负向标记从第 2 个 turn 起失效。现由 `ArgvCache`（`LoadEffectiveArgsWithCache`，默认包级缓存 + 测试可注入的独立缓存）缓存解析结果，后续 turn 复用。缓存只区分三种情形：① env 未设且无缓存 → 合法回落 `os.Args`，**不写入缓存**（避免把合法回落误当成缓存事实）；② env 已设且解析成功 → 缓存结果，供 unsetenv 之后的 turn 复用；③ env 已设但读/解析失败 → 返回错误并 fail-closed 判子代理，**不缓存失败**，因此每次调用都会重新判定，不会把一次故障永久固化成「永远子代理」。
3. **`shell_pid` 类型解析与严格校验**：
   - TS 原版使用 `Number(info?.shell_pid)`，对于 JSON 字符串形式（如 `"4242"`）会宽松转换为数字并通过验证。
   - Go 移植版严格遵循 JSON-RPC 类型规范，只认 JSON number（float64 整数或 int），对字符串形式的 pid 一律拒绝（fail-closed 判子代理）。
4. **`ReadProcPpid` 字段切分差异**：
   - TS 原版使用 `stat.slice(close + 2).trim().split(/\s+/u)` 从 `)` 后切分。
   - Go 移植版采用 `strings.TrimSpace(stat[closeIdx+1:])` 配合 `strings.Fields(rest)`，更具防御性地容忍括号后多空格或换行等格式畸变。
5. **`forceSystemPrompt` 非字符串分支防护**：
   - TS 原版直接对 `forceSystemPrompt` 调用 `replaceAll`，若上游传入非字符串可能触发异常。
   - Go 移植版在 `CleanPromptOptions` 中对 `forceSystemPrompt` 进行显式字符串类型断言（`isStr`），非字符串时安全忽略，不 panic。
6. **平台支持与通信协议（仅 Linux/macOS）**：
   - TS 原版写死 Linux 路径 `/root/.config/herdr/herdr.sock`。
   - Go 早期移植保留的 `HerdrNamedPipeWindows` / `network="npipe"` 属死代码（Go 标准库 `net` 原生不支持 `npipe` 协议，在 Windows 运行时会导致拨号错误）。现已彻底移除 Windows npipe 死代码，明确说明 herdr socket 查询仅支持 Unix domain socket（Linux/macOS）。
7. **错误处理、告警去重与已知 fail-open 面**：
   - TS 原版在 handler 中静默忽略异常。
   - Go 移植版在 `session_start` 与 `before_agent_start` 中，若 `EnforceToolExclusions` 失败，采用 `sync.Once` 进行首次无条件 warning 告警（输出到 stderr），避免每个 turn 重复刷屏；debug 模式下输出详细诊断，且不改变事件返回值（保持返回 `nil, nil`）。
   - 当 `SetActiveTools` 执行时，因底层 SDK 无返回值，在 `PI_IDENTITY_DEBUG=1` 下增加 debug 打点确认生效。
   - **已知 fail-open 面**：
     - 若 `ctx.GetActiveTools()` 出错，当前策略为保留错误并放弃本次工具过滤，依赖上层或后续 turn 恢复。
     - **子代理在自己 pane 且不带任何负向标记时会被判 main**。现网证据：`herdr agent start --kind pi` 不注入 `--name role-` / `--session-dir` / `--system-prompt`；实测本会话的子代理 pane = `w32:p26`、`shell_pid=2475328`、pi cmdline 仅为 `pi`、`env` 里没有 `PI_SUBAGENT` / `PI_ROLE`。此时 S1-S4 全部成立（宿主是 pi、父进程是 pane shell、无中间 agent runtime），故判 main，`jev_check` / `ask_user_question` 不会被摘、addendum 不会被清。
     - **TS 原块自认的另一条残余面**：「另一个 pi 塞进主代理 pane shell」——即某个 pi 进程被嵌在主代理 pane 的 shell 之下但不带负向标记，同样满足 S1-S4 而被判 main。
     - **pane 命名不作判据的代价**：判定面从「非 `:p1` 的 pane」扩大到了「任意 pane」。早先靠 `:p1` 后缀能顺手挡掉一部分子代理 pane（代价是误伤被重命名/重编号的主代理 pane，且语义上并不成立）；现在身份完全由负向标记 + herdr 运行态 + 进程拓扑决定，pane 命名只写进 reason 作辅助证据。**受管派发（走 dispatch-agent）的判定依赖宿主 argv 桥（带前提）**：三条负向标记（`--name role-`、`/tmp/herdr-role-sessions`、`/tmp/dispatch-`）确实在宿主 argv 里，但**能否被 Go 扩展看到取决于宿主是否写 `PIG_HARNESS_ARGV_FILE`**（PiG 的 subprocess / packed cell 宿主会写，见 `ref/PiG/coding/extension/host/subprocess/harness_identity.go`）。在 PiG 宿主下三条标记可见，受管派发稳定判子代理；若运行在没有该桥的宿主上（例如 TS 版 pi 的进程内扩展），`/proc/<pid>/argv` 可能已被进程 title 改写成 `pi`，标记就不可见，此时落回残余面 1（子代理在自己 pane 且无可见负向标记时判 main）。
8. **TS 侧残留实现的核实记录（pi 1.4.0，仅核实与记录，不删实现）**：
   - `forceSystemPrompt` 在 pi 1.4.0 **并非不存在**：`dist/core/system-prompt.d.ts` 的 `BuildSystemPromptOptions` 里有 `/** Exact full prompt replacement set by a before_agent_start handler. */ forceSystemPrompt?: string;`，且只在某个 handler 返回 `systemPrompt` 覆盖时被填充（`agent-session.ts:1724`）。
   - 因此 TS 侧 `StripAddendumBlock(forceSystemPrompt, …)` **不是死代码**，但实际几乎不触发（常规子代理会话不会强制整块提示词）；**保留作防御**，不因「很少命中」而删除。Go 侧对本分支另有非字符串类型断言防护（见差异 5）。
   - 验证命令：`grep -rn 'forceSystemPrompt' /root/.pi/agent/install/releases/1.4.0/node_modules/@earendil-works/pi-coding-agent/dist/core/system-prompt.d.ts`。
9. **`PIG_EXT_SOCKET_` 前缀是 PiG 保留命名空间（环境输入边界）**：
   - `DetectSubprocessForm` 把**任何**以 `PIG_EXT_SOCKET_` 为前缀且值非空的环境变量都当作 packed-cell 成员形态证明（真实键名由 `runtimecell.SocketEnvName` 生成，本组件为 `PIG_EXT_SOCKET_EXCLUDE_TOOLS_GUARD`）。因此该前缀属于 **PiG 保留命名空间**：用户脚本、CI、第三方扩展都**不应**设置以该前缀开头的环境变量，以免改变本组件的形态判定输入。
   - **误设后果**：若该前缀的变量被误设，fused 形态会被误判成 subprocess 形态。常规拓扑（self 即宿主且 cmdline 可识别为 agent runtime）下两种形态锚定到同一个 `hostIndex = 0`，判定结果不变；差异只出现在非常规拓扑（self cmdline 认不出 agent runtime 等），此时判据会走 subprocess 分支。方向上该误判**偏 fail-open（趋向判 main）**，后果有限，但仍意味着「形态判定依赖的环境输入不完全可信」，故按保留命名空间管理。

---

## 怎样运行测试

在 `/root/workspace/pi-extensions/go/exclude-tools-guard` 目录下执行：

```bash
# 静态分析
go vet ./...

# 编译检查
go build ./...

# 运行全量单元测试（含竞态检测）
go test -count=1 -race -v ./...
```

测试覆盖内容：
- **真实 Unix Socket 假服务端测试**：模拟真实 socket 通信，覆盖主代理成功响应、`pane_id` 不符、半开连接断开、字符串 `shell_pid` 严格拒绝、`shell_pid <= 0`、垃圾 JSON、不应答超时（50ms）、socket 文件不存在等所有 fail-closed 分支。
- **S1-S4 身份推导测试（M1/M3 实证）**：覆盖全部负向标记（`PI_SUBAGENT=1`、`PI_ROLE`、各种 argv）、S1 pane id 未设、S2 socket 失败、S3 祖先链丢失 `shell_pid`、S3 祖先链为空；重点验证正向拓扑（扩展是 `pi`/`pig` 宿主子进程判定为 main agent）与反向拓扑（宿主与 shell 之间存在 agent runtime 判定为 subagent）。
- **身份判据重构回归（oracle2 反例 A/B 实证）**：六条定点用例——① subprocess 拓扑下宿主自身 cmdline 带 `--name role-reviewer`（经 `PIG_HARNESS_ARGV_FILE` 桥接）判 subagent；② 隔层 spawn `ext→sh→pi→bash` 判 main；③ fused 形态但 self cmdline 为 `node /x/index.js` 判 subagent（fail-closed）；④ subprocess 拓扑但链上无 agent-runtime 祖先判 subagent；⑤ 同进程第二次调用仍判 subagent（argv 缓存）；⑥ `HERDR_PANE_ID=w32:p23`（非 `:p1`）且其余条件都像主代理时判 main（证明 pane 命名不再作判据）。另验证 herdr 查询失败/超时时 fail-closed 判子代理且只记一条 warning（`sync.Once` 去重、1500ms 预算不被改写）。
- **PIG_HARNESS_ARGV_FILE 宿主参数桥接测试（M2 实证）**：覆盖文件包含 role-reviewer、herdr-role-sessions、dispatch 任务；覆盖环境变量存在但文件缺失或坏 JSON 时 fail-closed 判定为 subagent；覆盖环境变量未设时安全 fallback 为 main；覆盖使用后环境变量被及时 unset 清理。
- **packed-go cell 形态回归（阻断项实证）**：四条定点用例——① 只注入真实键名 `PIG_EXT_SOCKET_EXCLUDE_TOOLS_GUARD`（不设 `PIG_EXT_SOCKET`）、宿主为 `pi`、父进程为 pane shell → **判 main**（并断言 `DetectSubprocessForm` 的证明键就是该成员键）；② 同一形态下宿主与 pane shell 之间夹 agent runtime（`pi-coding-agent`）→ **判 subagent**（S4）；③ isolated 形态只有 `PIG_EXT_SOCKET` → 仍走 subprocess 分支、**仍判 main**（原行为不回归）；④ 成员键列在环境里但**值为空** → 不算 subprocess 证明，退化为 fused 形态并 fail-closed 判 subagent。
- **身份降级可见性（S1/S3/S4 去重 warning）**：三条分支各调三次，断言每次仍 fail-closed 判 subagent、reason 带分支名，且 stderr 上只出现 **1** 条 `identity not verified, fail-closed to subagent:` warning（`sync.Once` 去重）。
- **工具过滤与 Setter 记录测试**：验证主代理排除 4 个、子代理排除 6 个、顺序不变；验证工具无变化时 `SetActiveTools` 绝不被调用；验证失败时不假装已过滤且留下诊断。
- **提示词清理测试**：验证带 `\n\n` 与不带 `\n\n` 的 `<addendum>` 块剥离、多块替换、`sections` 里 `addendum` section 的删除（其他 section 保留，非空 section 也不再被重新叠回）、未知 `sections` 形态不 panic 不改动、以及非子代理保持不动。
- **扩展工厂与事件接线测试（SF-1 实证）**：验证 `Extension()` 已挂载 `session_start` 与 `before_agent_start` 两个事件，在 `before_agent_start` 中就地清洗提示词选项；验证工具过滤失败时首次无条件 warning 告警且去重不刷屏。

---

## 怎样加载与验证

通过 `go/bin/pig-test` 包装脚本（隔离环境 `/root/.pig-test`）：

```bash
cd /root/workspace/pi-extensions/go

# 运行提示词并加载组件
./bin/pig-test -e ./exclude-tools-guard -p "回复 OK"

# 以 JSON 模式检查活跃工具列表
./bin/pig-test -e ./exclude-tools-guard --mode json -p "回复 OK"
```

