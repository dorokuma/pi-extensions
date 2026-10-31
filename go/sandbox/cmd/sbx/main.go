// Command sbx 在应用 Landlock 沙盒策略后原位 exec 目标命令。
//
// 用法：
//
//	sbx --ro DIR... --rw DIR... --exec BIN... [--deny-net] [--best-effort] [--reviewer WORKDIR] -- <cmd> [args...]
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/dorokuma/pi-extensions/go/sandbox"
	"golang.org/x/sys/unix"
)

type cliConfig struct {
	policy          sandbox.Policy
	reviewerWorkdir string
	cmd             string
	args            []string
}

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx: %v\n\n%s", err, usageText())
		os.Exit(2)
	}

	policy := cfg.policy
	if cfg.reviewerWorkdir != "" {
		base := sandbox.ReadOnlyReviewer(cfg.reviewerWorkdir)
		base.ReadOnly = append(base.ReadOnly, policy.ReadOnly...)
		base.ReadWrite = append(base.ReadWrite, policy.ReadWrite...)
		base.Exec = append(base.Exec, policy.Exec...)
		base.Env = append(base.Env, policy.Env...)
		base.DenyNet = policy.DenyNet
		base.BestEffort = policy.BestEffort
		policy = base
	}

	binPath, err := resolveCommandPath(cfg.cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx: resolve command %q: %v\n", cfg.cmd, err)
		os.Exit(127)
	}

	if err := sandbox.Apply(policy); err != nil {
		fmt.Fprintf(os.Stderr, "sbx: apply sandbox policy: %v\n", err)
		os.Exit(1)
	}

	argv := append([]string{cfg.cmd}, cfg.args...)
	envv := mergeEnv(os.Environ(), policy.Env)
	execErr := syscall.Exec(binPath, argv, envv)
	fmt.Fprintf(os.Stderr, "sbx: exec %q: %v\n", binPath, execErr)
	if errors.Is(execErr, unix.EACCES) || errors.Is(execErr, unix.EPERM) {
		os.Exit(126)
	}
	if errors.Is(execErr, unix.ENOENT) {
		os.Exit(127)
	}
	os.Exit(1)
}

func resolveCommandPath(cmd string) (string, error) {
	if strings.ContainsRune(cmd, '/') {
		return sandbox.NormalizePath(cmd)
	}
	return exec.LookPath(cmd)
}

// mergeEnv 将 overrides（"KEY=VALUE"）合并进 baseEnv，同名 KEY 以 overrides 最后一个值为准。
func mergeEnv(baseEnv, overrides []string) []string {
	if len(overrides) == 0 {
		return baseEnv
	}
	out := make([]string, 0, len(baseEnv)+len(overrides))
	idxByKey := make(map[string]int, len(baseEnv)+len(overrides))
	for _, kv := range baseEnv {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		idxByKey[k] = len(out)
		out = append(out, kv)
	}
	for _, kv := range overrides {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if idx, exists := idxByKey[k]; exists {
			out[idx] = kv
		} else {
			idxByKey[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

// parseArgs 解析命令行参数，同时支持：
//   - 单标志跟多值：`--ro / /usr --rw /tmp/a /tmp/b --exec /bin/sh /usr/bin/git -- cmd args`
//   - 重复标志：`--ro / --ro /usr --rw /tmp/a --exec /bin/sh -- cmd args`
//   - 等号赋值：`--ro=/,/usr --rw=/tmp --exec=/bin/sh -- cmd args`
func parseArgs(rawArgs []string) (cliConfig, error) {
	var cfg cliConfig
	if len(rawArgs) == 0 {
		return cfg, errors.New("missing arguments")
	}

	var cmdTokens []string
	for i := 0; i < len(rawArgs); i++ {
		tok := rawArgs[i]
		if tok == "--" {
			cmdTokens = rawArgs[i+1:]
			break
		}
		if tok == "-h" || tok == "--help" {
			return cfg, errors.New("help requested")
		}

		name, val, hasEq := strings.Cut(tok, "=")
		switch name {
		case "--ro", "-ro":
			if hasEq {
				items := splitNonEmpty(val)
				if len(items) == 0 {
					return cfg, fmt.Errorf("flag %s requires at least one path", name)
				}
				cfg.policy.ReadOnly = append(cfg.policy.ReadOnly, items...)
				continue
			}
			consumed, nextIdx := consumeValues(rawArgs, i+1)
			if len(consumed) == 0 {
				return cfg, fmt.Errorf("flag %s requires at least one path", name)
			}
			cfg.policy.ReadOnly = append(cfg.policy.ReadOnly, consumed...)
			i = nextIdx - 1

		case "--rw", "-rw":
			if hasEq {
				items := splitNonEmpty(val)
				if len(items) == 0 {
					return cfg, fmt.Errorf("flag %s requires at least one path", name)
				}
				cfg.policy.ReadWrite = append(cfg.policy.ReadWrite, items...)
				continue
			}
			consumed, nextIdx := consumeValues(rawArgs, i+1)
			if len(consumed) == 0 {
				return cfg, fmt.Errorf("flag %s requires at least one path", name)
			}
			cfg.policy.ReadWrite = append(cfg.policy.ReadWrite, consumed...)
			i = nextIdx - 1

		case "--exec", "-exec":
			if hasEq {
				items := splitNonEmpty(val)
				if len(items) == 0 {
					return cfg, fmt.Errorf("flag %s requires at least one binary path", name)
				}
				cfg.policy.Exec = append(cfg.policy.Exec, items...)
				continue
			}
			consumed, nextIdx := consumeValues(rawArgs, i+1)
			if len(consumed) == 0 {
				return cfg, fmt.Errorf("flag %s requires at least one binary path", name)
			}
			cfg.policy.Exec = append(cfg.policy.Exec, consumed...)
			i = nextIdx - 1

		case "--reviewer", "-reviewer":
			if hasEq {
				if val == "" {
					return cfg, fmt.Errorf("flag %s requires workdir path", name)
				}
				cfg.reviewerWorkdir = val
				continue
			}
			if i+1 >= len(rawArgs) || rawArgs[i+1] == "--" || strings.HasPrefix(rawArgs[i+1], "-") {
				return cfg, fmt.Errorf("flag %s requires workdir path", name)
			}
			cfg.reviewerWorkdir = rawArgs[i+1]
			i++

		case "--deny-net", "-deny-net":
			if hasEq {
				b, err := parseBoolFlag(val)
				if err != nil {
					return cfg, fmt.Errorf("invalid value for %s: %w", name, err)
				}
				cfg.policy.DenyNet = b
			} else {
				cfg.policy.DenyNet = true
			}

		case "--best-effort", "-best-effort":
			if hasEq {
				b, err := parseBoolFlag(val)
				if err != nil {
					return cfg, fmt.Errorf("invalid value for %s: %w", name, err)
				}
				cfg.policy.BestEffort = b
			} else {
				cfg.policy.BestEffort = true
			}

		default:
			if strings.HasPrefix(tok, "-") {
				return cfg, fmt.Errorf("unknown flag %q", tok)
			}
			cmdTokens = rawArgs[i:]
			i = len(rawArgs)
		}
	}

	if len(cmdTokens) == 0 || strings.TrimSpace(cmdTokens[0]) == "" {
		return cfg, errors.New("missing target command after '--'")
	}
	cfg.cmd = cmdTokens[0]
	cfg.args = cmdTokens[1:]
	return cfg, nil
}

func consumeValues(args []string, start int) ([]string, int) {
	var values []string
	i := start
	for ; i < len(args); i++ {
		tok := args[i]
		if tok == "--" || strings.HasPrefix(tok, "-") {
			break
		}
		values = append(values, splitNonEmpty(tok)...)
	}
	return values, i
}

func splitNonEmpty(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseBoolFlag(val string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("expected boolean, got %q", val)
	}
}

func usageText() string {
	return `Usage:
  sbx --ro DIR... --rw DIR... --exec BIN... [--deny-net] [--best-effort] -- <cmd> [args...]
  sbx --reviewer WORKDIR [--deny-net] [--best-effort] -- <cmd> [args...]
`
}
