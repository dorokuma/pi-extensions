// Command sbx 使用 Linux Landlock 沙盒策略受限运行目标程序。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dorokuma/pi-extensions/go/sandbox"
)

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
	ReadOnly   []string
	ReadWrite  []string
	Exec       []string
	Env        []string
	Preset     string
	WorkDir    string
	DenyNet    bool
	BestEffort bool
	CheckOnly  bool
	Command    []string
}

const usageLine = "usage: sbx [--reviewer WORKDIR | --preset reviewer --workdir DIR] [--ro PATH]... [--rw PATH]... [--exec PATH]... [--env KEY=VALUE]... [--deny-net] [--best-effort] [--check] -- COMMAND [ARGS...]"

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
		default:
			return cliConfig{}, fmt.Errorf("sbx: unknown preset %q (supported: reviewer)", presetName)
		}
		if strings.TrimSpace(workDir) == "" {
			workDir = "."
		}
	}

	cmdArgs := fs.Args()
	if !checkOnly && len(cmdArgs) == 0 {
		return cliConfig{}, errors.New("sbx: missing command")
	}

	return cliConfig{
		ReadOnly:   []string(roPaths),
		ReadWrite:  []string(rwPaths),
		Exec:       []string(execPaths),
		Env:        []string(envVars),
		Preset:     presetName,
		WorkDir:    workDir,
		DenyNet:    denyNet,
		BestEffort: bestEffort,
		CheckOnly:  checkOnly,
		Command:    cmdArgs,
	}, nil
}

// buildPolicy 根据解析后的命令行配置构造最终的 sandbox.Policy。
func buildPolicy(cfg cliConfig) (sandbox.Policy, error) {
	var policy sandbox.Policy
	if cfg.Preset != "" {
		switch cfg.Preset {
		case "reviewer", "read-only-reviewer":
			wd := cfg.WorkDir
			if strings.TrimSpace(wd) == "" {
				wd = "."
			}
			policy = sandbox.ReadOnlyReviewer(wd)
		default:
			return sandbox.Policy{}, fmt.Errorf("sbx: unknown preset %q (supported: reviewer)", cfg.Preset)
		}
	}

	policy.ReadOnly = append(policy.ReadOnly, cfg.ReadOnly...)
	policy.ReadWrite = append(policy.ReadWrite, cfg.ReadWrite...)
	policy.Exec = append(policy.Exec, cfg.Exec...)
	policy.Env = mergeEnv(policy.Env, cfg.Env)
	if cfg.DenyNet {
		policy.DenyNet = true
	}
	if cfg.BestEffort {
		policy.BestEffort = true
	}
	return policy, nil
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
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n%s\n", err, usageLine)
		os.Exit(2)
	}

	if cfg.CheckOnly {
		abi, ok := sandbox.Supported()
		if !ok {
			fmt.Fprintln(os.Stderr, "sbx: Landlock is not supported by the running kernel")
			os.Exit(1)
		}
		fmt.Printf("sbx: Landlock supported (ABI v%d)\n", abi)
		os.Exit(0)
	}

	policy, err := buildPolicy(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	// 当启用了 Exec 白名单时，在应用沙盒前双重校验目标命令是否属于被禁用的代理或动态链接器。
	if len(policy.Exec) > 0 {
		if sandbox.IsForbiddenExec(cfg.Command[0]) || sandbox.IsELFInterpreter(cfg.Command[0]) {
			fmt.Fprintf(os.Stderr, "sbx: execution of %q is forbidden by sandbox policy\n", cfg.Command[0])
			os.Exit(126)
		}
	}

	// 在调用 sandbox.Apply 收紧目录读取权限前先解析目标命令路径（优先参考 policy.Env 中注入的受控 PATH）。
	bin, err := resolveCommandPath(cfg.Command[0], policy.Env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx: resolve command %q: %v\n", cfg.Command[0], err)
		os.Exit(127)
	}

	if len(policy.Exec) > 0 {
		if sandbox.IsForbiddenExec(bin) || sandbox.IsELFInterpreter(bin) {
			fmt.Fprintf(os.Stderr, "sbx: execution of %q is forbidden by sandbox policy\n", bin)
			os.Exit(126)
		}
	}

	if err := sandbox.Apply(policy); err != nil {
		fmt.Fprintf(os.Stderr, "sbx: apply sandbox policy failed: %v\n", err)
		os.Exit(1)
	}

	execEnv := mergeEnv(os.Environ(), policy.Env)
	if err := syscall.Exec(bin, cfg.Command, execEnv); err != nil {
		fmt.Fprintf(os.Stderr, "sbx: exec %q failed: %v\n", bin, err)
		os.Exit(126)
	}
}
