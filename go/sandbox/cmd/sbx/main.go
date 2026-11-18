// Command sbx 使用 Linux Landlock 沙盒策略受限运行目标程序。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dorokuma/pi-extensions/go/sandbox"
	"golang.org/x/sys/unix"
)

// version 由构建时 -ldflags "-X main.version=X.Y.Z" 注入；未注入时为 "dev"。
var version = "dev"

// envSbxChildSpecKey 是 sbx 父进程向受限子进程传递沙盒策略与目标命令的内部环境变量名。
// 子进程启动后立即将该变量从环境中移除，再执行 sandbox.Apply 与 syscall.Exec。
const envSbxChildSpecKey = "PI_SANDBOX_INTERNAL_CHILD_SPEC"

type stringSlice []string

func (s *stringSlice) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSlice) Set(val string) error {
	for _, part := range strings.Split(val, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// cliConfig 保存解析后的命令行配置。
type cliConfig struct {
	ReadOnly    []string
	ReadWrite   []string
	Exec        []string
	Env         []string
	Preset      string
	WorkDir     string
	DenyNet     bool
	BestEffort  bool
	CheckOnly   bool
	ShowVersion bool
	ReportTo    string
	Command     []string
}

// childExecSpec 封装父进程传递给受限子进程的沙盒配置与目标命令。
type childExecSpec struct {
	Bin       string   `json:"bin"`
	Command   []string `json:"command"`
	ReadOnly  []string `json:"ro"`
	ReadWrite []string `json:"rw"`
	// OptionalReadWrite 透传 Policy.OptionalReadWrite（appendAgyStateReadWrite
	// 快照枚举的瞬态状态文件登记位）：缺失该字段会导致子进程对宿主已删除的
	// 瞬态文件（如 .db-shm）恢复 fail-closed ENOENT 崩溃，必须随 spec 100% 透传。
	OptionalReadWrite []string `json:"orw"`
	Exec              []string `json:"exec"`
	DenyNet           bool     `json:"deny_net"`
	Env               []string `json:"env"`
	BestEffort        bool     `json:"best_effort"`
	EntryAgents       []string `json:"entry_agents"`
	// MountNS 记录父进程是否以 CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC
	// 三命名空间联合 Cloneflags 派生子进程的标记；仅当为 true 时
	// 子进程才执行挂载命名空间内的 socket 白名单视图（--best-effort 下
	// unshare 失败回退不 unshare 时绝不碰宿主挂载命名空间）。
	MountNS bool `json:"mount_ns"`
	// FD3Forward 标记 agent 预设下 fd3（claude-pty debugW 写端）须经
	// ExtraFiles 显式单路转发给最终目标进程：内层子进程在 Apply（派生
	// helper/broker）前先对 fd3 置 CLOEXEC，再由 runAsSandboxInit 经
	// ExtraFiles 传给目标，杜绝辅助进程/常驻 daemon 继承（oracle FM-3）。
	FD3Forward bool `json:"fd3_forward"`
}

const usageLine = "usage: sbx [--reviewer WORKDIR | --preset reviewer|reviewer-agent --workdir DIR] [--ro PATH]... [--rw PATH]... [--exec PATH]... [--env KEY=VALUE]... [--deny-net] [--best-effort] [--check] [--version] [--report-to ID] -- COMMAND [ARGS...]"

// parseArgs 解析命令行参数（不含 os.Args[0]），支持 `--reviewer WORKDIR` 作为 `--preset reviewer --workdir DIR` 的兼容别名。
func parseArgs(args []string) (cliConfig, error) {
	var (
		roPaths     stringSlice
		rwPaths     stringSlice
		execPaths   stringSlice
		envVars     stringSlice
		presetName  string
		workDir     string
		reviewerDir string
		denyNet     bool
		bestEffort  bool
		checkOnly   bool
		showVersion bool
		reportTo    string
	)

	fs := flag.NewFlagSet("sbx", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.Var(&roPaths, "ro", "只读路径（可重复指定或逗号分隔）")
	fs.Var(&rwPaths, "rw", "读写路径（可重复指定或逗号分隔，默认不含执行权限）")
	fs.Var(&execPaths, "exec", "允许执行的二进制或目录白名单（可重复指定或逗号分隔）")
	fs.Var(&envVars, "env", "注入的环境变量 KEY=VALUE（可重复指定）")
	fs.StringVar(&presetName, "preset", "", "预设策略名称（如 reviewer / read-only-reviewer）")
	fs.StringVar(&workDir, "workdir", "", "预设策略使用的工作目录")
	fs.StringVar(&reviewerDir, "reviewer", "", "只读审查员预设及其工作目录（等价于 --preset reviewer --workdir WORKDIR）")
	fs.BoolVar(&denyNet, "deny-net", false, "禁止所有 TCP bind 与 connect（需 Landlock ABI >= 4）")
	fs.BoolVar(&bestEffort, "best-effort", false, "当内核不支持或路径缺失时告警降级而非报错退出")
	fs.BoolVar(&checkOnly, "check", false, "仅检测当前内核 Landlock 支持状态并退出")
	fs.BoolVar(&showVersion, "version", false, "打印版本号并退出")
	fs.StringVar(&reportTo, "report-to", "", "只读角色回报中继的父终端地址（如 t28；仅 reviewer-agent 预设生效）")

	if err := fs.Parse(args); err != nil {
		return cliConfig{}, err
	}

	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		visited[f.Name] = true
	})

	if visited["reviewer"] {
		trimmedReviewer := strings.TrimSpace(reviewerDir)
		if trimmedReviewer == "" || trimmedReviewer == "--" {
			return cliConfig{}, errors.New("sbx: --reviewer requires a workdir argument")
		}
		if visited["preset"] {
			normPreset := strings.ToLower(strings.TrimSpace(presetName))
			if normPreset != "reviewer" && normPreset != "read-only-reviewer" {
				return cliConfig{}, fmt.Errorf("sbx: --reviewer conflicts with --preset %q", presetName)
			}
		}
		if visited["workdir"] && strings.TrimSpace(workDir) != "" && filepath.Clean(workDir) != filepath.Clean(trimmedReviewer) {
			return cliConfig{}, fmt.Errorf("sbx: conflicting --reviewer %q and --workdir %q", trimmedReviewer, workDir)
		}
		presetName = "reviewer"
		workDir = trimmedReviewer
	}

	if presetName != "" {
		switch strings.ToLower(strings.TrimSpace(presetName)) {
		case "reviewer", "read-only-reviewer":
			presetName = "reviewer"
		case "reviewer-agent":
			presetName = "reviewer-agent"
		default:
			return cliConfig{}, fmt.Errorf("sbx: unknown preset %q (supported: reviewer, reviewer-agent)", presetName)
		}
		if strings.TrimSpace(workDir) == "" {
			workDir = "."
		}
	}

	cmdArgs := fs.Args()
	if !checkOnly && !showVersion && len(cmdArgs) == 0 {
		return cliConfig{}, errors.New("sbx: missing command")
	}

	return cliConfig{
		ReadOnly:    []string(roPaths),
		ReadWrite:   []string(rwPaths),
		Exec:        []string(execPaths),
		Env:         []string(envVars),
		Preset:      presetName,
		WorkDir:     workDir,
		DenyNet:     denyNet,
		BestEffort:  bestEffort,
		CheckOnly:   checkOnly,
		ShowVersion: showVersion,
		ReportTo:    strings.TrimSpace(reportTo),
		Command:     cmdArgs,
	}, nil
}

// sandboxEnvStripKeys 是从沙盒子进程环境中剔除的外部指针类变量：
// 除 HC_TERMINAL_ID（保留给角色识别父终端）外所有 HC_ 前缀变量
// （HC_TERMINAL_SOCKET/HC_SCOPE/HC_SOCKET/HC_SESSION 等），以及凭据与
// 会话 socket 指针。--env 显式传入这些键同样被剔除（buildPolicy 对
// cfg.Env 同口径过滤）；预设自身注入的值（如 reviewer-agent 私有
// XDG_RUNTIME_DIR 指向沙盒内 0700 目录）不属于外部指针，不剔除。
//
// PIG_USE_PI_DIRS / PI_CODING_AGENT_DIR 不入本集合：仅对宿主
// os.Environ() 经 sandboxHostStripExtraKeys 追加剔除（--env 显式声明
// 的合法 PI 自定义入口不能被误伤，N7）。
var sandboxEnvStripKeys = map[string]bool{
	"GPG_AGENT_INFO":           true,
	"SSH_AUTH_SOCK":            true,
	"DBUS_SESSION_BUS_ADDRESS": true,
	"XDG_RUNTIME_DIR":          true,
}

// sandboxHostStripExtraKeys 是仅对宿主 os.Environ() 追加剔除的键：宿主
// 残留的 PIG_USE_PI_DIRS / PI_CODING_AGENT_DIR 属外部指针——pig 包装器
// 后置 export PIG_USE_PI_DIRS=1 会激活宿主 PI_* 目录，仅删 policy.Env 侧
// 同名键而保留 os.Environ() 中的宿主值，会让宿主 PIG_USE_PI_DIRS=1 /
// PI_CODING_AGENT_DIR 直接进入最终目标 env（N2）。
var sandboxHostStripExtraKeys = map[string]bool{
	"PIG_USE_PI_DIRS":     true,
	"PI_CODING_AGENT_DIR": true,
}

// stripSandboxEnv 剔除 env 列表中的外部指针类变量。
func stripSandboxEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "HC_") && k != "HC_TERMINAL_ID" {
			continue
		}
		if sandboxEnvStripKeys[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// stripHostEnv 在 stripSandboxEnv 之上额外剔除宿主残留 AgentDir/PI-dirs
// 指针（sandboxHostStripExtraKeys）；仅用于最终 exec 环境与受限子进程
// 环境构造，不作用于 --env 显式声明。
func stripHostEnv(env []string) []string {
	out := stripSandboxEnv(env)
	res := make([]string, 0, len(out))
	for _, kv := range out {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if sandboxHostStripExtraKeys[k] {
			continue
		}
		res = append(res, kv)
	}
	return res
}

// buildPolicy 根据解析后的命令行配置构造最终的 sandbox.Policy。
func buildPolicy(cfg cliConfig) (sandbox.Policy, error) {
	// 先剔除 --env 中的外部指针类变量，防止它们经 policy.Env 写回沙盒。
	cfg.Env = stripSandboxEnv(cfg.Env)
	var policy sandbox.Policy
	var pigHome string
	var claudeConfigDir string
	var goExecDir string
	// N1/N7：在 cfgEnv merge 前捕获 ReviewerAgentWithEnv 判定的结构化受信
	// AgentDir 事实（哪个目录是真正预建的私有目录、哪个是经校验的自定义
	// 目录、有效目录是谁），merge 后只使用该事实做归一化——绝不从用户可控
	// TMPDIR 重新 filepath.Join 重建私有目录身份。
	var agentFact reviewerAgentDirFact
	if cfg.Preset != "" {
		switch cfg.Preset {
		case "reviewer", "read-only-reviewer":
			wd := cfg.WorkDir
			if strings.TrimSpace(wd) == "" {
				wd = "."
			}
			policy = sandbox.ReadOnlyReviewer(wd)
		case "reviewer-agent":
			wd := cfg.WorkDir
			if strings.TrimSpace(wd) == "" {
				wd = "."
			}
			policy = sandbox.ReviewerAgentWithEnv(wd, cfg.Env)
			// merge 前捕获：预设已经把受信判定结果写进 policy.Env
			//（effective = 受信自定义或私有；private = 预建 pig-agent 目录）。
			agentFact = captureReviewerAgentDirFact(policy)
			if captureReviewerAgentDirFactHook != nil {
				captureReviewerAgentDirFactHook(agentFact)
			}
			// 启动前 fail-closed：本次目标集（宿主已构建的有效 Go 缓存单元）经
			// ReviewerAgentWithEnv 枚举与投影校验；有 Go 扩展但宿主产物为 0，或
			// 存在未成功投影/Exec 未命中的目标时，此处返回错误拒绝启动并指名
			// 缺失项，绝不进入「沙盒内 build 成功但 exec 失败」的不完全兼容态。
			// fail-closed 仅对真实 pig 入口生效：claude/agy/sh 等非 pig 命令
			// MaybeWarmGoExtensionCells 不会预热，冷缓存下无条件校验会把它们
			// 误拒启动（oracle FM-2B）。校验失败时必须先清理已创建的
			// pi-reviewer-*/pi-reviewer-exec-*（含 pig-agent/auth.json 凭据副本），
			// 再返回错误，不得在 /tmp 泄漏私有目录（oracle FM-2A）。
			if sandbox.IsPigEntryCommand(cfg.Command) {
				if err := sandbox.ReviewerAgentCellTargetError(); err != nil {
					cleanupPresetTempDirsFromPolicy(policy)
					return sandbox.Policy{}, fmt.Errorf("sbx: reviewer-agent pig cell target check failed: %w", err)
				}
			}
			// ReviewerAgentWithEnv 把与预热对齐的 PIG_HOME 固定注入 Policy.Env；
			// 记下来供 mergeEnv 之后回钉（--env PIG_HOME= 空覆盖会把空串写进
			// 最终 env，导致沙盒内 pig 按外部 HOME 另算缓存根打穿预热对齐）。
			pigHome = envListValue(policy.Env, "PIG_HOME")
			// CLAUDE_CONFIG_DIR 同理：ReviewerAgentWithEnv 把 CC 私有配置根
			// （<privateTmpDir>/claude，随 privateTmpDir 清理、无跨会话状态）
			// 固定注入 Policy.Env；记下来供 mergeEnv 之后回钉，防止
			// --env CLAUDE_CONFIG_DIR=<其他路径或空> 把 CC 写面指回宿主
			// ~/.claude 或与 ReadWrite 策略脱配。
			claudeConfigDir = envListValue(policy.Env, "CLAUDE_CONFIG_DIR")
			// PI_SANDBOX_GO_EXEC_DIR 同理：ReviewerAgentWithEnv 把受控执行目录
			//（/tmp/pi-reviewer-exec-*，go 包装器/broker.sock/report.sock 所在）
			// 注入 Policy.Env；记下来供 mergeEnv 之后回钉，防 --env
			// PI_SANDBOX_GO_EXEC_DIR=<其他路径> 把回报中继监听路径与 go
			// 包装器目录劫持到调用方指定的任意目录。
			goExecDir = envListValue(policy.Env, "PI_SANDBOX_GO_EXEC_DIR")
		default:
			return sandbox.Policy{}, fmt.Errorf("sbx: unknown preset %q (supported: reviewer, reviewer-agent)", cfg.Preset)
		}
	}

	policy.ReadOnly = append(policy.ReadOnly, cfg.ReadOnly...)
	policy.ReadWrite = append(policy.ReadWrite, cfg.ReadWrite...)
	policy.Exec = append(policy.Exec, cfg.Exec...)
	policy.Env = mergeEnv(policy.Env, cfg.Env)
	if cfg.Preset == "reviewer-agent" && pigHome != "" {
		policy.Env = mergeEnv(policy.Env, []string{"PIG_HOME=" + pigHome})
	}
	// 仅 reviewer-agent 预设回钉 CLAUDE_CONFIG_DIR：把最终 env 中该键强制
	// 回写为 ReviewerAgentWithEnv 注入的每会话私有目录（<privateTmpDir>/claude），
	// 仿照 PIG_HOME 回钉，防 --env 覆盖把写面指回宿主或与策略脱配。
	if cfg.Preset == "reviewer-agent" && claudeConfigDir != "" {
		policy.Env = mergeEnv(policy.Env, []string{"CLAUDE_CONFIG_DIR=" + claudeConfigDir})
	}
	// 仅 reviewer-agent 预设回钉 PI_SANDBOX_GO_EXEC_DIR：把最终 env 中该键
	// 强制回写为预设注入的 pi-reviewer-exec-* 目录（与 PIG_HOME 同口径），
	// 防 --env 劫持回报中继监听路径或 go 包装器/broker 目录。
	if cfg.Preset == "reviewer-agent" && goExecDir != "" {
		policy.Env = mergeEnv(policy.Env, []string{"PI_SANDBOX_GO_EXEC_DIR=" + goExecDir})
	}
	// M1/M2：cfgEnv 合并完成后统一归一化有效 AgentDir。显式空串（含重复
	// 键末项为空）不得把空串写进最终 env（M1）；显式非空自定义分支也须
	// 清理宿主残留的 PI_CODING_AGENT_DIR / PIG_USE_PI_DIRS（M2），否则 pig
	// 包装器后置 export PIG_USE_PI_DIRS=1 时会激活宿主 PI 残留。
	// N1：私有目录身份只使用 merge 前捕获的预设事实 agentFact（绝不从
	// 用户可控 TMPDIR 重建）；预热、配置复制与最终 exec 使用同一有效目录。
	if cfg.Preset == "reviewer-agent" {
		policy.Env = normalizeEffectiveAgentDirs(policy.Env, agentFact)
	}
	if cfg.DenyNet {
		policy.DenyNet = true
	}
	if cfg.BestEffort {
		policy.BestEffort = true
	}
	return policy, nil
}

// reviewerAgentDirFact 是 ReviewerAgentWithEnv 在 merge 前写入 policy.Env
// 的结构化受信 AgentDir 判定结果：哪个目录是真正预建的私有 pig-agent、
// 哪个是经校验的自定义目录、有效目录最终是谁。该事实在 cfgEnv merge
// 之前捕获，merge 后只使用它做归一化，绝不从用户可控 TMPDIR 重新推导
// （N1：TMPDIR 与 AgentDir 同时被 --env 覆盖时，从 TMPDIR filepath.Join
// 重建出的目录会重新消费宿主配置，如 --env TMPDIR=/root/.pig +
// --env PIG_CODING_AGENT_DIR= 得 /root/.pig/pig-agent）。
type reviewerAgentDirFact struct {
	// privateDir 是 ReviewerAgentWithEnv 实际预建的私有 pig-agent 目录
	//（<privateTmpDir>/pig-agent，0700 已存在）；非空时即可信私有目录本体。
	privateDir string
	// effectiveDir 是 ReviewerAgentWithEnv 最终判定的有效 AgentDir：
	// 受信自定义（trustedCustom）= 解析后的自定义目录；其余 = privateDir。
	effectiveDir string
	// trustedCustom 记录受信自定义是否真实存在（effectiveDir != privateDir）。
	trustedCustom bool
}

// captureReviewerAgentDirFactHook 是可注入的 merge 前事实捕获 seam：
// 单测把它置为记录函数即可在同一 buildPolicy 调用内拿到真实私有目录
// （privateTmpDir 随机名跨次构造不同，断言必须用本次 buildPolicy 的
// 那一次），生产路径为 nil 零开销。
var captureReviewerAgentDirFactHook func(reviewerAgentDirFact)

// captureReviewerAgentDirFact 从 ReviewerAgentWithEnv 产出的 policy.Env
// 读取 merge 前的事实：privateDir 取预设注入的 PIG_CODING_AGENT_DIR
// （此时即有效目录；trustedCustom 分支下预设已把受信自定义写入同一键，
// 私有目录本体可由 privateTmpDir 推导但判定本身已可信），effectiveDir
// 与 privateDir 的区分仅靠 PIG_CODING_AGENT_DIR 是否等于 <TMPDIR>/pig-agent。
func captureReviewerAgentDirFact(policy sandbox.Policy) reviewerAgentDirFact {
	tmp := envListValue(policy.Env, "TMPDIR")
	pig := envListValue(policy.Env, "PIG_CODING_AGENT_DIR")
	fact := reviewerAgentDirFact{}
	if tmp == "" || pig == "" {
		return fact
	}
	fact.privateDir = filepath.Join(tmp, "pig-agent")
	if pig != fact.privateDir {
		fact.trustedCustom = true
		fact.effectiveDir = pig
	} else {
		fact.effectiveDir = fact.privateDir
	}
	return fact
}

// normalizeEffectiveAgentDirs 统一归一化最终 env 中两个 AgentDir 键与
// PIG_USE_PI_DIRS；输入只用 merge 前捕获的结构化事实 agentFact：
//   - agentFact.trustedCustom：经 ReviewerAgentWithEnv 校验的非空自定义
//     PIG_CODING_AGENT_DIR 保留为有效目录；PI_CODING_AGENT_DIR 归一到同一
//     目录（宿主残留 PI 键不得生效），PIG_USE_PI_DIRS 不再注入。
//   - 其余情形（键缺失 / 显式空串 / 不可信被拒）两个键都回钉
//     agentFact.privateDir（真实预建私有目录，非 TMPDIR 重建值）。
//
// cfgEnv 空覆盖语义：显式空串屏蔽宿主同名变量后仍归一化回钉，不得把
// 空串写进最终 env（M1）。有效目录一律来自预设事实，绝不从合并后的
// TMPDIR 重新 filepath.Join 重建（N1）。
func normalizeEffectiveAgentDirs(env []string, agentFact reviewerAgentDirFact) []string {
	// 受信自定义：agentFact.effectiveDir 即 ReviewerAgentWithEnv 校验过的
	// 自定义目录（trustedCustom=true）；privateDir 仍是真实私有目录本体。
	if agentFact.trustedCustom && agentFact.effectiveDir != "" {
		eff := agentFact.effectiveDir
		env = overrideEnvKeyLocal(env, "PIG_CODING_AGENT_DIR", eff)
		env = overrideEnvKeyLocal(env, "PI_CODING_AGENT_DIR", eff)
		env = removeEnvKey(env, "PIG_USE_PI_DIRS")
		return env
	}
	// 非受信自定义（含不可信被拒 / 显式空串 / 键缺失）：两键统一回钉
	// agentFact.privateDir——真实预建私有目录；显式空串被归一化覆盖，
	// 不再把空串写进最终 env（M1）。PIG_USE_PI_DIRS 不注入。
	privateDir := agentFact.privateDir
	env = overrideEnvKeyLocal(env, "PIG_CODING_AGENT_DIR", privateDir)
	env = overrideEnvKeyLocal(env, "PI_CODING_AGENT_DIR", privateDir)
	env = removeEnvKey(env, "PIG_USE_PI_DIRS")
	return env
}

// overrideEnvKeyLocal 与 sandbox.overrideEnvKey 同语义（package main 无法
// 访问未导出符号）：替换首个 key= 条目并去除重复，未命中则追加。
func overrideEnvKeyLocal(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			if replaced {
				continue
			}
			out = append(out, prefix+val)
			replaced = true
		} else {
			out = append(out, kv)
		}
	}
	if !replaced {
		out = append(out, prefix+val)
	}
	return out
}

// removeEnvKey 从 env 切片中删除所有 key= 前缀条目（mergeEnv 无法删除键，
// 只能覆盖——清理宿主 PIG_USE_PI_DIRS 残留需真正的删除）。
func removeEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// envListValue 返回 env 中首个 KEY= 前缀条目的值；未命中返回空串。
func envListValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

// mergeEnv 将 overrides 中的 KEY=VALUE 覆盖合并到 base 环境变量切片中（保持顺序并原地替换同名键）。
func mergeEnv(base, overrides []string) []string {
	if len(overrides) == 0 {
		return append([]string(nil), base...)
	}
	out := make([]string, 0, len(base)+len(overrides))
	indexByKey := make(map[string]int, len(base)+len(overrides))

	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if idx, exists := indexByKey[k]; exists {
			out[idx] = kv
		} else {
			indexByKey[k] = len(out)
			out = append(out, kv)
		}
	}

	for _, kv := range overrides {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if idx, exists := indexByKey[k]; exists {
			out[idx] = kv
		} else {
			indexByKey[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

func resolveCommandPath(cmdName string, envList []string) (string, error) {
	if strings.ContainsRune(cmdName, filepath.Separator) {
		return exec.LookPath(cmdName)
	}
	for i := len(envList) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(envList[i], "=")
		if ok && k == "PATH" && v != "" {
			for _, dir := range filepath.SplitList(v) {
				if dir == "" {
					dir = "."
				}
				cand := filepath.Join(dir, cmdName)
				if st, err := os.Stat(cand); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
					return cand, nil
				}
			}
			break
		}
	}
	return exec.LookPath(cmdName)
}

func main() {
	if os.Getenv(envSbxChildSpecKey) != "" {
		os.Exit(runChildFromEnv())
	}
	os.Exit(runMain(os.Args[1:]))
}

// agentPreset 统一判定需要 fd3（claude-pty debugW 写端）显式单路转发的
// agent 预设：reviewer-agent。未来 exec-agent 同名加入。非 agent 预设
// 不改变 fd3 语义。
func isAgentPreset(name string) bool {
	return name == "reviewer-agent"
}

// fd3Active 探测 fd 3 是否为有效打开描述符（F_GETFD）。
func fd3Active() bool {
	_, err := unix.FcntlInt(3, unix.F_GETFD, 0)
	return err == nil
}

// sealInheritedFD3 在 agent 预设入口对继承的 fd3 置 FD_CLOEXEC：claude-pty
// 经 ExtraFiles 传入的 debugW 写端进入 sbx 时无 CLOEXEC，若不收口会被
// 后续派生的 codegraph-go -warm-daemon 常驻 daemon、pig 预热子进程与
// reportRelay hc send 子进程继承，永久劫持 drainDebug 的 EOF（oracle FM-3）。
// 返回 fd3 是否有效（供转发决策）；非 agent 预设不动。
func sealInheritedFD3(preset string) bool {
	if !isAgentPreset(preset) || !fd3Active() {
		return false
	}
	unix.CloseOnExec(3)
	return true
}

// fd3Owner 持有 fd3（claude-pty debugW 写端）在父进程生命周期内的唯一
// *os.File 包装器，保证 forwardFD3ToChild 跨 mountns 重试复用同一 owner
// （不产生第二个带终结器的 wrapper），releaseFd3File 显式 Close 后终结器
// 被撤销——杜绝旧 wrapper 的 GC 终结器在重试前关闭仍需转发的 fd3，或关闭
// 被复用的 fd3 槽位（oracle FM-3）。
var fd3Owner struct {
	sync.Mutex
	f *os.File
}

// acquireFd3File 返回 fd3 的唯一 owner *os.File；首次调用建立 owner，后续
// 调用直接复用（不新建 wrapper）。fd3 无效时返回错误。
func acquireFd3File() (*os.File, error) {
	fd3Owner.Lock()
	defer fd3Owner.Unlock()
	if fd3Owner.f != nil {
		return fd3Owner.f, nil
	}
	if !fd3Active() {
		return nil, errors.New("fd3 not open")
	}
	f := os.NewFile(3, "claude-debug")
	if f == nil {
		return nil, errors.New("fd3 not open")
	}
	fd3Owner.f = f
	return f, nil
}

// releaseFd3File 显式 Close fd3 owner：Close 撤销 (*file).close 终结器，
// 后续 GC 不会对已关闭的整数 3 二次关闭，也不会关闭被复用的 fd3 槽位。
// 仅在确认子进程已接管（Start 成功）或明确放弃时调用。
func releaseFd3File() error {
	fd3Owner.Lock()
	defer fd3Owner.Unlock()
	if fd3Owner.f == nil {
		return nil
	}
	err := fd3Owner.f.Close()
	fd3Owner.f = nil
	return err
}

// forwardFD3ToChild 在 agent 预设下把 fd3 经 ExtraFiles 显式单路转发给目标
// 子进程。调用方负责在 cmd.Start() 成功后调用 releaseFd3File 关闭本进程侧
// fd3 owner（解除 drainDebug 的 EOF 悬挂）。
func forwardFD3ToChild(cmd *exec.Cmd, active bool) {
	if !active {
		return
	}
	if f, err := acquireFd3File(); err == nil && f != nil {
		cmd.ExtraFiles = []*os.File{f}
	}
}

// closeFD3 关闭父进程侧 fd3 owner（Start 后调用），解除 drainDebug 的 EOF
// 悬挂；同时撤销 owner 的终结器，杜绝幽灵关闭复用槽位。
func closeFD3(active bool) {
	if active {
		_ = releaseFd3File()
	}
}

func runMain(args []string) int {
	cfg, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n%s\n", err, usageLine)
		return 2
	}

	if cfg.ShowVersion {
		fmt.Printf("sbx %s\n", version)
		return 0
	}

	if cfg.CheckOnly {
		abi, ok := sandbox.Supported()
		if !ok {
			fmt.Fprintln(os.Stderr, "sbx: Landlock is not supported by the running kernel")
			return 1
		}
		fmt.Printf("sbx: Landlock supported (ABI v%d)\n", abi)
		return 0
	}

	// agent 预设入口先对继承 fd3 置 FD_CLOEXEC：claude-pty 经 ExtraFiles
	// 传入的 debugW 写端无 CLOEXEC，不收口会被 MaybeWarmGoExtensionCells
	// 预热子进程、codegraph-go -warm-daemon 常驻 daemon 及 reportRelay
	// hc send 继承，永久劫持 drainDebug 的 EOF。仅在真实有效时记录
	// active 供 runSandboxedChild/runAsSandboxInit 经 ExtraFiles 单路转发。
	fd3Fwd := sealInheritedFD3(cfg.Preset)

	// reviewer-agent 启动 pig 前：宿主态按需预热 Go 扩展 cell，确保
	// appendCellCachePolicy 在 buildPolicy 内扫描时能命中已构建 runner。
	// 常态指纹未变时零子进程短路；预热失败仅告警降级，不阻断后续流程；
	// 预热期被 SIGINT/SIGTERM/SIGHUP/SIGQUIT 中断时返回 128+信号 直接退出，
	// 不再进入沙盒。
	if code := sandbox.MaybeWarmGoExtensionCells(cfg.Preset, cfg.Command, cfg.Env); code != 0 {
		return code
	}

	policy, err := buildPolicy(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}
	defer func() {
		cleanupPresetTempDirsFromPolicy(policy)
	}()

	// 当启用了 Exec 白名单时，在启动受限子进程前双重校验目标命令是否属于被禁用的代理或动态链接器。
	// reviewer-agent 预设允许顶层 EntryAgents（agy/pig/pi/claude）；其余 ForbiddenExec 仍拒绝。
	if len(policy.Exec) > 0 {
		if sandbox.IsELFInterpreter(cfg.Command[0]) {
			fmt.Fprintf(os.Stderr, "sbx: execution of %q is forbidden by sandbox policy\n", cfg.Command[0])
			return 126
		}
		if sandbox.IsForbiddenExec(cfg.Command[0]) && !sandbox.IsEntryAgentPath(policy.EntryAgents, cfg.Command[0]) {
			fmt.Fprintf(os.Stderr, "sbx: execution of %q is forbidden by sandbox policy\n", cfg.Command[0])
			return 126
		}
	}

	// 在进入受限子进程前先解析目标命令路径（优先参考 policy.Env 中注入的受控 PATH）。
	bin, err := resolveCommandPath(cfg.Command[0], policy.Env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx: resolve command %q: %v\n", cfg.Command[0], err)
		return 127
	}

	if len(policy.Exec) > 0 {
		if sandbox.IsELFInterpreter(bin) {
			fmt.Fprintf(os.Stderr, "sbx: execution of %q is forbidden by sandbox policy\n", bin)
			return 126
		}
		if sandbox.IsForbiddenExec(bin) && !sandbox.IsEntryAgentPath(policy.EntryAgents, bin) {
			fmt.Fprintf(os.Stderr, "sbx: execution of %q is forbidden by sandbox policy\n", bin)
			return 126
		}
	}

	return runSandboxedChild(bin, cfg, policy, fd3Fwd)
}

// runSandboxedChild 在当前父进程中派生子进程，由子进程完成 sandbox.Apply 与目标命令启动；
// 父进程保持在 Landlock 规则集之外，负责转发信号、等待子进程结束并返回其退出码，使外层 defer
// 能够在子进程退出后清理 /tmp/pi-reviewer-* 与 /tmp/pi-reviewer-exec-*。
// Linux 下子进程以 CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWIPC 进入独立挂载、PID
// 与 IPC 命名空间（隔离 SysV shm/sem/msg 与按名 mq_open/mq_unlink
// （继承 /dev/mqueue 挂载未覆盖）；
// 子进程即新 PID ns
// 的 PID 1，完成隔离后担任轻量 init 派生目标命令（记录目标子进程 PID——
// sandbox.Apply 会先派生 helper/broker，业务进程 PID 不固定为 2；socket
// 白名单视图见 mountns_linux.go）；
// --best-effort 下 unshare 失败时告警回退不 unshare。reviewer-agent + --report-to 时先在
// <goExecDir>/report.sock 起回报中继再派生子进程，子进程退出后先关中继再清目录。
func runSandboxedChild(bin string, cfg cliConfig, policy sandbox.Policy, fd3Fwd bool) int {
	selfExe, err := os.Executable()
	if err != nil || selfExe == "" {
		fmt.Fprintf(os.Stderr, "sbx: resolve executable failed: %v\n", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(selfExe); err == nil && resolved != "" {
		selfExe = resolved
	}

	// 回报中继必须在 cmd.Start() 之前就绪：沙盒内 hc 客户端启动后即可连接。
	relay := maybeStartReportRelay(cfg, policy.Env)
	defer relay.close()

	sysProcAttr := childSysProcAttr()
	spec := childExecSpec{
		Bin:               bin,
		Command:           cfg.Command,
		ReadOnly:          policy.ReadOnly,
		ReadWrite:         policy.ReadWrite,
		OptionalReadWrite: policy.OptionalReadWrite,
		Exec:              policy.Exec,
		DenyNet:           policy.DenyNet,
		Env:               policy.Env,
		BestEffort:        policy.BestEffort,
		EntryAgents:       policy.EntryAgents,
		MountNS:           sysProcAttr != nil,
		FD3Forward:        fd3Fwd,
	}
	specBytes, err := json.Marshal(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx: marshal child spec failed: %v\n", err)
		return 1
	}

	// 传入子进程的环境同样剔除外部指针类变量（子进程 Apply 前的辅助进程不继承 HC_*）。
	childEnv := mergeEnv(stripHostEnv(os.Environ()), []string{envSbxChildSpecKey + "=" + string(specBytes)})

	newChildCmd := func() *exec.Cmd {
		cmd := exec.Command(selfExe)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = childEnv
		cmd.SysProcAttr = sysProcAttr
		// agent 预设：fd3 经 ExtraFiles 显式单路转发给子进程（继承 fd3 已在
		// runMain 入口置 CLOEXEC，不转发即不会泄漏到子进程树）。
		forwardFD3ToChild(cmd, fd3Fwd)
		return cmd
	}
	cmd := newChildCmd()

	sigCh := make(chan os.Signal, 8)
	// 外层父进程仅监听并转发 SIGINT/SIGTERM/SIGHUP/SIGQUIT 4 种信号给受限
	// 子进程；容器内 PID-1 init 另监听 8 种信号（SIGCHLD 仅用于唤醒
	// Wait4 收尸不转发，其余 7 种转发给目标子进程），见 runAsSandboxInit。
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		// --best-effort 且启用了挂载命名空间时，回退不 unshare 重启一次；
		// 此时 spec.MountNS=false，子进程跳过 socket 覆盖，绝不动宿主挂载命名空间。
		if policy.BestEffort && sysProcAttr != nil {
			fmt.Fprintf(os.Stderr, "sbx: WARN mount namespace unavailable (%v); retrying without socket hiding\n", err)
			sysProcAttr = nil
			spec.MountNS = false
			if specBytes, err = json.Marshal(spec); err == nil {
				childEnv = mergeEnv(stripHostEnv(os.Environ()), []string{envSbxChildSpecKey + "=" + string(specBytes)})
				cmd = newChildCmd()
				err = cmd.Start()
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "sbx: start sandboxed child failed: %v\n", err)
			return 126
		}
	}
	// 父进程 Start 成功后立即关闭自身 fd3 副本：debugW 仅剩目标子进程树
	// 持有的写端，claude-pty drainDebug 在目标退出后能正确收到 EOF。
	closeFD3(fd3Fwd)

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	for {
		select {
		case sig := <-sigCh:
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		case waitErr := <-waitCh:
			return exitCodeFromWaitErr(waitErr)
		}
	}
}

// runChildFromEnv 在子进程内读取父进程传入的沙盒配置，清除内部环境变量，施加沙盒限制后调用 syscall.Exec。
func runChildFromEnv() int {
	// 入口即锁定 OS 线程（不解锁——本函数最终以 init 身份退进程）：
	// -race/cgo 下 syscall.AllThreadsSyscall 返回 ENOTSUP 时，降权、封锁与
	// Apply 会回退到「当前线程」口径，必须保证 isolateRuntimeSockets
	//（drop-capabilities/seal-mount-namespace）→ sandbox.Apply（派生 helper
	// 及后续 go test 子进程）→ runAsSandboxInit（派生目标子进程）全程跑在
	// 同一个已降权/已封锁的 OS 线程上，子孙经 fork/exec 继承同一能力集。
	runtime.LockOSThread()

	raw := os.Getenv(envSbxChildSpecKey)
	_ = os.Unsetenv(envSbxChildSpecKey)
	if raw == "" {
		fmt.Fprintln(os.Stderr, "sbx: empty internal child spec")
		return 1
	}

	var spec childExecSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		fmt.Fprintf(os.Stderr, "sbx: parse internal child spec failed: %v\n", err)
		return 1
	}
	if spec.Bin == "" || len(spec.Command) == 0 {
		fmt.Fprintln(os.Stderr, "sbx: invalid command in internal child spec")
		return 1
	}

	policy := sandbox.Policy{
		ReadOnly:          spec.ReadOnly,
		ReadWrite:         spec.ReadWrite,
		OptionalReadWrite: spec.OptionalReadWrite,
		Exec:              spec.Exec,
		DenyNet:           spec.DenyNet,
		Env:               spec.Env,
		BestEffort:        spec.BestEffort,
		EntryAgents:       spec.EntryAgents,
	}
	// 仅当父进程确实以 CLONE_NEWNS 派生（MountNS）时收紧 socket 白名单视图；
	// 挂载步骤失败默认拒绝启动（退出码 1），--best-effort 下仅告警。
	if spec.MountNS {
		if err := isolateRuntimeSockets(spec.BestEffort); err != nil {
			fmt.Fprintf(os.Stderr, "sbx: %v\n", err)
			return 1
		}
	}
	// agent 预设：fd3 经外层 ExtraFiles 传入后为非 CLOEXEC，必须在
	// sandbox.Apply 派生 helper/broker 前置 CLOEXEC，杜绝辅助进程继承
	// debugW 写端；目标进程由 runAsSandboxInit 经 ExtraFiles 显式接收。
	if spec.FD3Forward && fd3Active() {
		unix.CloseOnExec(3)
	}
	if err := sandbox.Apply(policy); err != nil {
		fmt.Fprintf(os.Stderr, "sbx: apply sandbox policy failed: %v\n", err)
		return 1
	}

	// mergeEnv 只能覆盖不能删：对宿主继承部分剔除外部指针类变量后再合并，
	// policy.Env 中预设注入的同名键（如私有 XDG_RUNTIME_DIR）不受影响。
	// 宿主侧额外剔除 PIG_USE_PI_DIRS / PI_CODING_AGENT_DIR 残留（N2）。
	execEnv := mergeEnv(stripHostEnv(os.Environ()), policy.Env)
	// MountNS 路径下本进程是新 PID 命名空间的 PID 1：不得直接 syscall.Exec
	// 顶替 PID 1（业务进程作为 PID 1 会忽略默认未捕获信号，且无人回收孤儿
	// 僵尸进程），改由 runAsSandboxInit 担任轻量 init 派生目标子进程。
	if spec.MountNS {
		return runAsSandboxInit(spec.Bin, spec.Command, execEnv, spec.FD3Forward)
	}
	// 无 mountns 分支直接 syscall.Exec：agent 预设下恢复 fd3 的
	// FD_CLOEXEC=0，让内核 exec 保留 fd3 透传到目标进程（替代
	// ExtraFiles——syscall.Exec 无 ExtraFiles 概念）；目标退出后
	// drainDebug 收到 EOF。非 agent 预设保持 CLOEXEC 不转发。
	if spec.FD3Forward && fd3Active() {
		_, _ = unix.FcntlInt(3, unix.F_SETFD, 0)
	}
	if err := syscall.Exec(spec.Bin, spec.Command, execEnv); err != nil {
		fmt.Fprintf(os.Stderr, "sbx: exec %q failed: %v\n", spec.Bin, err)
		return 126
	}
	return 0
}

// runAsSandboxInit 在新 PID 命名空间内以 PID 1 运行轻量 init：派生目标
// 命令并记录其子进程 PID（不固定为 2——sandbox.Apply 会先派生
// helper/broker 辅助进程），转发信号并回收孤儿僵尸进程。PID 1 有特殊
// 语义——默认未捕获的信号对它不生效（kill 不掉），且其退出的子孙会被
// 内核转给它收尸；让业务进程直接当 PID 1 会导致 SIGTERM 等被静默吞掉
// 并残留僵尸。
//
// 本进程监听 8 种信号：SIGCHLD 仅用于唤醒 Wait4 排空循环（不转发），
// 其余 SIGINT/SIGTERM/SIGHUP/SIGQUIT/SIGUSR1/SIGUSR2/SIGWINCH 共 7 种
// 转发给目标子进程（外层 sbx 父进程仅监听并转发
// SIGINT/SIGTERM/SIGHUP/SIGQUIT 4 种）。
//
// 收尸与转发在同一循环内完成：每轮先以 Wait4(-1, WNOHANG) 排空全部已
// 退出子孙（命中目标子进程 PID 即返回其退出码/128+信号；PID 1 退出后
// 内核自动清理命名空间内残余进程），无待收割子进程时阻塞等信号——
// SIGCHLD 继续排空，其余信号转发给目标子进程。Wait4 返回 EINTR 时重试，
// ECHILD（无任何子进程，含目标子进程已被收割）时直接返回 0，绝不落入
// <-sigCh 空转阻塞。cmd.Process.Release() 避免 os/exec 的
// pidfd Wait 路径与 Wait4(-1) 竞争 ECHILD。
func runAsSandboxInit(bin string, argv []string, env []string, fd3Fwd bool) int {
	sigCh := make(chan os.Signal, 16)
	signal.Notify(sigCh,
		syscall.SIGCHLD, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP,
		syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	cmd := &exec.Cmd{
		Path:   bin,
		Args:   argv,
		Env:    env,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	// agent 预设：fd3 经 ExtraFiles 显式单路转发给目标进程（已在 Apply 前
	// 置 CLOEXEC 阻断 helper/broker 继承；此处显式放行最终目标）。
	forwardFD3ToChild(cmd, fd3Fwd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "sbx: exec %q failed: %v\n", bin, err)
		return 126
	}
	// PID 1 父进程 Start 后关闭自身 fd3 副本：目标进程树独占写端。
	closeFD3(fd3Fwd)
	childPid := cmd.Process.Pid
	_ = cmd.Process.Release()

	for {
		// 先非阻塞排空全部已退出子孙：SIGCHLD 信号可合并丢失，不能依赖
		// 一次信号对应一次收割。
		for {
			var ws unix.WaitStatus
			reapedPid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if err != nil {
				// EINTR：被信号打断，重试；ECHILD：无任何子进程可收割，
				// 绝不能落到 <-sigCh 永久阻塞（PID 1 此时已没有等待对象），
				// 直接退出让内核清理命名空间。
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				if errors.Is(err, syscall.ECHILD) {
					return 0
				}
				break
			}
			if reapedPid <= 0 {
				break
			}
			if reapedPid != childPid {
				continue // 孤儿子孙：仅收尸，不改变主进程退出语义
			}
			if ws.Exited() {
				return ws.ExitStatus()
			}
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
		}

		sig := <-sigCh
		if sig == syscall.SIGCHLD {
			continue // 下轮循环开头重新排空
		}
		if s, ok := sig.(syscall.Signal); ok {
			_ = unix.Kill(childPid, s) // 目标子进程已退出时 ESRCH，无害
		}
	}
}

func exitCodeFromWaitErr(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			if ws.Exited() {
				return ws.ExitStatus()
			}
		}
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
	}
	return 1
}

// cleanupPresetTempDirsFromPolicy 综合 Policy.Env、Policy.ReadWrite 与 Policy.Exec
// 清理预设构造阶段创建的私有临时目录（即便用户通过 --env 覆盖了 TMPDIR 也不会漏删）。
func cleanupPresetTempDirsFromPolicy(policy sandbox.Policy) {
	cleanupPresetTempDirsFromEnv(policy.Env)
	for _, rw := range policy.ReadWrite {
		if isPresetTempDir(rw, "pi-reviewer-") && !isPresetTempDir(rw, "pi-reviewer-exec-") {
			removePresetTempDir(rw)
		}
	}
	for _, ex := range policy.Exec {
		if isPresetTempDir(ex, "pi-reviewer-exec-") {
			removePresetTempDir(ex)
		}
	}
}

// cleanupPresetTempDirsFromEnv 删除预设构造阶段创建的私有临时目录
// （pi-reviewer-* 与 pi-reviewer-exec-*），同时覆盖 /tmp 与 os.TempDir() 兜底路径。
// 仅删位于合法临时根下且带固定前缀的第一层子目录，绝不递归删除任意 TMPDIR。
func cleanupPresetTempDirsFromEnv(env []string) {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	if dir := m["TMPDIR"]; isPresetTempDir(dir, "pi-reviewer-") && !isPresetTempDir(dir, "pi-reviewer-exec-") {
		removePresetTempDir(dir)
	}
	if goTmp := m["GOTMPDIR"]; goTmp != "" {
		parent := filepath.Dir(filepath.Clean(goTmp))
		if isPresetTempDir(parent, "pi-reviewer-") && !isPresetTempDir(parent, "pi-reviewer-exec-") {
			removePresetTempDir(parent)
		}
	}
	if dir := m["PI_SANDBOX_GO_EXEC_DIR"]; isPresetTempDir(dir, "pi-reviewer-exec-") {
		removePresetTempDir(dir)
	}
}

// isPresetTempDir 校验 dir 是否为位于 /tmp 或 os.TempDir() 下、基名以指定 prefix 开头的预设临时目录。
func isPresetTempDir(dir, prefix string) bool {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" || prefix == "" {
		return false
	}
	cleanDir := filepath.Clean(trimmed)
	if !filepath.IsAbs(cleanDir) {
		return false
	}
	base := filepath.Base(cleanDir)
	if !strings.HasPrefix(base, prefix) || len(base) <= len(prefix) {
		return false
	}
	parent := filepath.Dir(cleanDir)
	for _, root := range allowedTempRoots() {
		if parent == root {
			return true
		}
	}
	return false
}

func allowedTempRoots() []string {
	roots := []string{"/tmp"}
	if td := filepath.Clean(strings.TrimSpace(os.TempDir())); td != "" && filepath.IsAbs(td) {
		roots = append(roots, td)
	}
	out := make([]string, 0, len(roots)*2)
	seen := make(map[string]struct{}, len(roots)*2)
	for _, r := range roots {
		if _, ok := seen[r]; !ok {
			seen[r] = struct{}{}
			out = append(out, r)
		}
		if resolved, err := filepath.EvalSymlinks(r); err == nil && resolved != "" {
			cleanResolved := filepath.Clean(resolved)
			if _, ok := seen[cleanResolved]; !ok {
				seen[cleanResolved] = struct{}{}
				out = append(out, cleanResolved)
			}
		}
	}
	return out
}

// removePresetTempDir 删除预设临时目录；若目录内含 0500/0400 只读子目录或与辅助进程退出发生短暂并发竞争，
// 自动补齐目录写权限并重试。
func removePresetTempDir(dir string) {
	cleanDir := filepath.Clean(dir)
	for attempt := 0; attempt < 4; attempt++ {
		if err := os.RemoveAll(cleanDir); err == nil {
			if _, statErr := os.Lstat(cleanDir); os.IsNotExist(statErr) {
				return
			}
		}
		_ = filepath.WalkDir(cleanDir, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr == nil && d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		time.Sleep(5 * time.Millisecond)
	}
	_ = os.RemoveAll(cleanDir)
}
