# verifier-expect-fail

双快照对照 / 反例探针专用的**纯机械四条件输出断言包装器**：运行一条「预期失败」的被测
命令，仅当其输出机械地满足全部条件时才放行（退出 0），否则如实失败并透传真实退出码。
零第三方依赖（仅标准库）。

## 用法

```
verifier-expect-fail --expect-re <正则> [--reject-re <排除正则>] [--min-tests <N>] -- <被测命令> [被测参数...]
```

## 参数语义

| 参数 | 必需 | 语义 |
|---|---|---|
| `--expect-re` | 是（非空） | 合并输出（stdout+stderr）**必须**命中该正则；非法正则即参数错误 |
| `--reject-re` | 否 | 非空时编译；合并输出**不得**命中（排除反例里的泄漏/次生错误） |
| `--min-tests` | 否 | 未传不做用例数下限断言（供外部 CLI 探针）；显式传入时必须 `N >= 1`，否则参数错误 |
| `-- <cmd> [args...]` | 是 | `--` 后（或剩余位置参数）必须非空；按纯 argv 执行，**绝不**经 shell 解释 |

任何参数校验失败 → stderr 清晰错误 + **退出码 2**。

子进程继承当前环境变量、工作目录与 stdin；stdout/stderr **实时原样透传**到包装器
自身的 stdout/stderr，同时写入互斥锁保护的合并缓冲区供正则与用例数解析（无 data race）。

## 四项纯机械通过条件（全部满足 → 退出 0）

1. **inner-exit-nonzero**：子进程已实际启动且退出码 ≠ 0（信号终止按 `128+sig` 折算）；
2. **expect-re**：合并输出命中 `--expect-re`；
3. **reject-re**：未传，或合并输出未命中 `--reject-re`；
4. **min-tests**：未传，或机械解析出的实际执行用例数 ≥ N。

### 用例数机械解析（条件④）

仅锚定已知运行器结构，任意行上裸的 `N passed|failed|error(s)` **不计入**
（防止 `build failed: 2 errors` 这类编译错误日志击穿 `--min-tests` 底线）：

- Go test：`(?m)^=== RUN\s+\S+` 与 `(?m)^---\s+(?:FAIL|PASS):\s+\S+` 计数取较大值
  （非 `-v` 下 `go test` 失败仍输出 `--- FAIL: TestX`；全绿或编译失败无用例时不输出，
  红灯步配合 `--expect-re` 天然精确）；`[no test files]` / `no tests to run` 得 **0**；
- Python unittest：`Ran (\d+) tests?`；
- pytest 底部汇总行：行须匹配 `(?m)^=+\s+.*\bin\s+\d+(?:\.\d+)?s\b.*=+\s*$`，
  仅在该汇总行内累加 `N passed|failed|error(s)` 项（如 `== 3 failed, 5 passed in 0.01s ==`）；
- cargo test 汇总行：`test result: ok|FAILED. N passed; M failed;`（取 passed + failed）。

## 退出码透传规则

| 场景 | 退出码 |
|---|---|
| 四条件全满足 | **0**（stderr 追加 `PASS inner_exit=<码> match=<片段> [tests=<数>]`） |
| 任一条件不满足且子进程退出码 ≠ 0 | **原样透传**该非零退出码（信号 → `128+sig`） |
| 任一条件不满足且子进程退出 0（假绿） | **1** |
| 子进程未能启动：ENOENT（不存在） | **127** |
| 子进程未能启动：其它（如 EACCES 无执行权限） | **126** |
| 参数校验失败 | **2** |

未启动绝不误判为「子进程非零退出」；条件不满足时 stderr 追加
`FAIL inner_exit=<码> failed=[<未满足项,...>]`。

## 编译与安装

```bash
cd /root/workspace/pi-extensions/go/verifier-expect-fail
GOWORK=off go build -o verifier-expect-fail .   # 单文件二进制，零依赖
# 安装（本阶段待门禁通过后再执行；安装前先备份同名旧二进制）：
# cd /root/workspace/pi-extensions/go/verifier-expect-fail && \
#   go build -o /root/.local/bin/verifier-expect-fail .
```

本地开发走 `go/go.work`（已注册 `./verifier-expect-fail`），仓库级 `make build` /
`make test` / `make vet`（`COMPONENTS := */go.mod` 通配）自动覆盖本组件。
