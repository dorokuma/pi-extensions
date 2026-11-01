package main

import (
	"flag"
	"fmt"
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

func main() {
	var (
		roPaths    stringSlice
		rwPaths    stringSlice
		execPaths  stringSlice
		envVars    stringSlice
		presetName string
		workDir    string
		denyNet    bool
		bestEffort bool
		checkOnly  bool
	)

	flag.Var(&roPaths, "ro", "只读路径（可重复指定或逗号分隔）")
	flag.Var(&rwPaths, "rw", "读写路径（可重复指定或逗号分隔，默认不含执行权限）")
	flag.Var(&execPaths, "exec", "允许执行的二进制或目录白名单（可重复指定或逗号分隔）")
	flag.Var(&envVars, "env", "注入的环境变量 KEY=VALUE（可重复指定）")
	flag.StringVar(&presetName, "preset", "", "预设策略名称（如 reviewer / read-only-reviewer）")
	flag.StringVar(&workDir, "workdir", ".", "预设策略使用的工作目录")
	flag.BoolVar(&denyNet, "deny-net", false, "禁止所有 TCP bind 与 connect（需 Landlock ABI >= 4）")
	flag.BoolVar(&bestEffort, "best-effort", false, "当内核不支持或路径缺失时告警降级而非报错退出")
	flag.BoolVar(&checkOnly, "check", false, "仅检测当前内核 Landlock 支持状态并退出")
	flag.Parse()

	if checkOnly {
		abi, ok := sandbox.Supported()
		if !ok {
			fmt.Fprintln(os.Stderr, "sbx: Landlock is not supported by the running kernel")
			os.Exit(1)
		}
		fmt.Printf("sbx: Landlock supported (ABI v%d)\n", abi)
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: sbx [--preset reviewer --workdir DIR] [--ro PATH]... [--rw PATH]... [--exec PATH]... [--deny-net] [--best-effort] -- COMMAND [ARGS...]")
		os.Exit(2)
	}

	var policy sandbox.Policy
	if presetName != "" {
		switch strings.ToLower(strings.TrimSpace(presetName)) {
		case "reviewer", "read-only-reviewer":
			policy = sandbox.ReadOnlyReviewer(workDir)
		default:
			fmt.Fprintf(os.Stderr, "sbx: unknown preset %q (supported: reviewer)\n", presetName)
			os.Exit(2)
		}
	}

	policy.ReadOnly = append(policy.ReadOnly, roPaths...)
	policy.ReadWrite = append(policy.ReadWrite, rwPaths...)
	policy.Exec = append(policy.Exec, execPaths...)
	policy.Env = append(policy.Env, envVars...)
	if denyNet {
		policy.DenyNet = true
	}
	if bestEffort {
		policy.BestEffort = true
	}

	// 当启用了 Exec 白名单时，在应用沙盒前双重校验目标命令是否属于被禁用的代理或动态链接器。
	if len(policy.Exec) > 0 {
		if sandbox.IsForbiddenExec(args[0]) || sandbox.IsELFInterpreter(args[0]) {
			fmt.Fprintf(os.Stderr, "sbx: execution of %q is forbidden by sandbox policy\n", args[0])
			os.Exit(126)
		}
	}

	// 在调用 sandbox.Apply 收紧目录读取权限前先解析目标命令路径（优先参考 policy.Env 中注入的受控 PATH）。
	bin, err := resolveCommandPath(args[0], policy.Env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx: resolve command %q: %v\n", args[0], err)
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

	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "sbx: exec %q failed: %v\n", bin, err)
		os.Exit(126)
	}
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
