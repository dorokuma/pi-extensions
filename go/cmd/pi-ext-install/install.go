package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type app struct {
	repoRoot        string
	registryPath    string
	reg             *Registry
	destDir         string
	explicitDestDir bool
	mode            string
	dryRun          bool
	requested       []string
	changedAny      bool
	federatedTouch  bool
	warns           []string
	stdout          io.Writer
	stderr          io.Writer
	getenv          func(string) string
	runScript       func(script, envName, dest string) ([]byte, error)
}

func defaultRunScript(script, envName, dest string) ([]byte, error) {
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), envName+"="+dest)
	return cmd.CombinedOutput()
}

func (a *app) fail(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(a.stderr, "%s FAILED: %s\n", progName, msg)
	return fmt.Errorf("%s", msg)
}

func (a *app) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(a.stderr, "%s WARN: %s\n", progName, msg)
	a.warns = append(a.warns, msg)
}

func (a *app) usage() {
	fmt.Fprint(a.stdout, `install.sh — 联邦式 Pi 扩展统一安装/巡检入口

Usage:
  install.sh --list                list all entries (name/category/authority/target/status)
  install.sh --audit               read-only inspect all (md5/perms/upstream scripts/herdr+moshi/native Go) + warn on unregistered dest-dir entries
  install.sh --dry-run <names...>  rehearse only; write nothing
  install.sh <names...>            install named extensions (hosted copy | federated cascade)
  install.sh --all                 install all hosted+federated (never writes herdr-agent-state.ts)
  install.sh --dest-dir <DIR> ...  override target dir; defaults to $PI_EXT_DEST_DIR or ~/.pig/agent/extensions
  install.sh -h | --help           this help

Env:
  PI_EXT_DEST_DIR            default target dir (CLI --dest-dir wins over it)
  PI_EXT_ALLOW_ZERO_NODE_TS  set to 1 to allow TypeScript installs into the real
                             pig extensions dir on a zero-node host (node not in PATH
                             and no native Go extension dir); default 0 protects it

Data source: federated/registry.json. This repo NEVER writes herdr-agent-state.ts.
`)
}

func (a *app) validateTargetFile(dest string) error {
	if dest == "" {
		return a.fail("destination is empty")
	}
	if !isAbsPath(dest) {
		return a.fail("destination is not an absolute path: %s", dest)
	}
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		return a.fail("destination is a directory: %s — expected a file path (<destDir>/<ext>.ts), not a directory", dest)
	}
	return nil
}

func (a *app) refuseIfNeverWrite(destFile string) error {
	if isNeverWrite(destFile) {
		return a.fail("refusing to write neverWrite target '%s' — managed by herdr binary out of band; this repo NEVER writes it", destFile)
	}
	return nil
}

// handleNativeAndZeroNode applies native-Go takeover and zero-node protection
// consistently across hosted and federated installs. Returns (handled=true, err).
func (a *app) handleNativeAndZeroNode(ext Extension, dest string, isFederated bool) (bool, error) {
	cat := "hosted"
	skipSuffix := ""
	noInstallMsg := fmt.Sprintf("TypeScript %s not installed", dest)
	if isFederated {
		cat = "federated"
		skipSuffix = "cascade skipped; "
		noInstallMsg = fmt.Sprintf("cascade skipped; %s not installed", dest)
	}

	if nativeDir, ok := nativeGoExtDir(dest); ok {
		if a.dryRun {
			if pathExistsOrSymlink(dest) {
				action := "skip TypeScript install"
				if isFederated {
					action = "skip upstream cascade"
				}
				fmt.Fprintf(a.stdout, "[dry-run] %s (%s): native Go extension active at %s — would remove stale %s and %s\n", ext.Name, cat, nativeDir, dest, action)
			} else {
				action := "skip TypeScript install"
				if isFederated {
					action = "skip upstream cascade"
				}
				fmt.Fprintf(a.stdout, "[dry-run] %s (%s): native Go extension active at %s — would %s (%s)\n", ext.Name, cat, nativeDir, action, dest)
			}
			return true, nil
		}
		if pathExistsOrSymlink(dest) {
			if err := os.Remove(dest); err != nil {
				return true, a.fail("failed to remove stale TypeScript extension: %s", dest)
			}
			a.changedAny = true
			fmt.Fprintf(a.stdout, "%s (%s): skipped — native Go extension active at %s (%sremoved stale %s)\n", ext.Name, cat, nativeDir, skipSuffix, dest)
		} else {
			fmt.Fprintf(a.stdout, "%s (%s): skipped — native Go extension active at %s (%s)\n", ext.Name, cat, nativeDir, noInstallMsg)
		}
		return true, nil
	}

	if a.isZeroNodeProtectedDest() {
		nativeTarget := strings.TrimSuffix(dest, ".ts")
		if a.mode == "all" {
			if a.dryRun {
				if pathExistsOrSymlink(dest) {
					action := "skip TypeScript install"
					if isFederated {
						action = "skip upstream cascade"
					}
					fmt.Fprintf(a.stdout, "[dry-run] %s (%s): zero-node host without native Go extension — would remove stale %s and %s\n", ext.Name, cat, dest, action)
				} else {
					fmt.Fprintf(a.stdout, "[dry-run] %s (%s): zero-node host without native Go extension — would skip TypeScript install (%s)\n", ext.Name, cat, dest)
				}
				return true, nil
			}
			if pathExistsOrSymlink(dest) {
				if err := os.Remove(dest); err != nil {
					return true, a.fail("failed to remove stale TypeScript extension: %s", dest)
				}
				a.changedAny = true
				if isFederated {
					fmt.Fprintf(a.stdout, "%s (federated): skipped — zero-node host (node not found) and no native Go extension at %s (cascade skipped; removed stale %s)\n", ext.Name, nativeTarget, dest)
				} else {
					fmt.Fprintf(a.stdout, "%s (hosted): skipped — zero-node host (node not found) and no native Go extension at %s (removed stale %s; TypeScript not installed)\n", ext.Name, nativeTarget, dest)
				}
			} else {
				fmt.Fprintf(a.stdout, "%s (%s): skipped — zero-node host (node not found) and no native Go extension at %s (%sTypeScript %s not installed)\n", ext.Name, cat, nativeTarget, skipSuffix, dest)
			}
			return true, nil
		}
		return true, a.fail("refusing to install TypeScript extension '%s' (%s): node not found in PATH and no native Go extension at %s — writing .ts into pig extensions on a zero-node host crashes pig at startup (pass --dest-dir <staging-dir> or PI_EXT_ALLOW_ZERO_NODE_TS=1 to override)", ext.Name, dest, nativeTarget)
	}

	return false, nil
}

func (a *app) installHosted(ext Extension) error {
	src := resolveSourcePath(ext.Authority.sourcePath(), a.repoRoot)
	if src == "" {
		return a.fail("hosted extension '%s' has no authority.sourcePath in registry", ext.Name)
	}
	if info, err := os.Stat(src); err != nil || info.IsDir() {
		return a.fail("source not found: %s", src)
	}
	if ext.DestFile == "" {
		return a.fail("hosted extension '%s' has no .destFile in registry", ext.Name)
	}
	dest := filepath.Join(a.destDir, ext.DestFile)
	if err := a.validateTargetFile(dest); err != nil {
		return err
	}
	if err := a.refuseIfNeverWrite(ext.DestFile); err != nil {
		return err
	}
	if handled, err := a.handleNativeAndZeroNode(ext, dest, false); handled {
		return err
	}

	smd5, err := md5OfRegular(src)
	if err != nil {
		return a.fail("md5sum failed for source: %s", src)
	}
	if a.dryRun {
		fmt.Fprintf(a.stdout, "[dry-run] %s (hosted): would install -m 644 %s -> %s (source md5 %s)\n", ext.Name, src, dest, smd5)
		return nil
	}
	if isRegularNoFollow(dest) {
		if dmd5, err := md5OfRegular(dest); err == nil && dmd5 == smd5 {
			fmt.Fprintf(a.stdout, "%s (hosted): unchanged — destination already matches source (md5 %s); nothing written\n", ext.Name, smd5)
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return a.fail("failed to create parent directory for %s: %v", dest, err)
	}
	if err := copyFileAtomic(src, dest, 0o644); err != nil {
		return a.fail("failed to install %s -> %s: %v", src, dest, err)
	}
	fmt.Fprintf(a.stdout, "%s (hosted): changed — installed %s (md5 %s)\n", ext.Name, dest, smd5)
	a.changedAny = true
	return nil
}

func (a *app) installFederated(ext Extension) error {
	script := ext.Authority.installScript()
	envName := ext.Authority.installDestEnv()
	if ext.DestFile == "" {
		return a.fail("federated extension '%s' has no .destFile in registry", ext.Name)
	}
	dest := filepath.Join(a.destDir, ext.DestFile)
	if err := a.validateTargetFile(dest); err != nil {
		return err
	}
	if err := a.refuseIfNeverWrite(ext.DestFile); err != nil {
		return err
	}
	if handled, err := a.handleNativeAndZeroNode(ext, dest, true); handled {
		return err
	}

	if script == "" || !pathExistsOrSymlink(script) {
		shown := script
		if shown == "" {
			shown = "<none in registry>"
		}
		a.warn("%s: missing (upstream script not found): %s — cannot cascade; edit the host repo instead", ext.Name, shown)
		return nil
	}

	if a.dryRun {
		fmt.Fprintf(a.stdout, "[dry-run] %s (federated): would cascade '%s'=%s bash %s\n", ext.Name, envName, dest, script)
		return nil
	}

	fmt.Fprintf(a.stdout, "%s (federated): cascading host repo install.sh: %s=%s bash %s\n", ext.Name, envName, dest, script)
	runner := a.runScript
	if runner == nil {
		runner = defaultRunScript
	}
	outBytes, err := runner(script, envName, dest)
	outStr := strings.TrimRight(string(outBytes), "\n")
	if err == nil {
		if outStr != "" {
			for _, line := range strings.Split(outStr, "\n") {
				fmt.Fprintf(a.stdout, "    %s\n", line)
			}
		}
		if strings.Contains(outStr, "changed:") {
			a.changedAny = true
			a.federatedTouch = true
		}
	} else {
		if outStr != "" {
			for _, line := range strings.Split(outStr, "\n") {
				fmt.Fprintf(a.stderr, "    %s\n", line)
			}
		}
		rc := 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			rc = exitErr.ExitCode()
		}
		a.warn("%s: upstream install.sh failed (exit %d): %s — host repo install did NOT complete; reconcile in the host repo", ext.Name, rc, script)
		a.federatedTouch = true
	}
	return nil
}

func copyFileAtomic(src, dest string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".pi-ext-install-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		_ = os.Remove(dest)
		if err2 := os.Rename(tmpPath, dest); err2 != nil {
			return err2
		}
	}
	cleanup = false
	return nil
}

func (a *app) run(args []string) int {
	a.mode = "install"
	destDirEnv := a.getenv("PI_EXT_DEST_DIR")
	a.destDir = destDirEnv
	destDirSet := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--list":
			a.mode = "list"
		case arg == "--audit":
			a.mode = "audit"
		case arg == "--all":
			a.mode = "all"
		case arg == "--dry-run":
			a.dryRun = true
		case arg == "--dest-dir":
			if i+1 >= len(args) {
				_ = a.fail("--dest-dir requires an argument")
				return 1
			}
			i++
			a.destDir = args[i]
			destDirSet = true
		case strings.HasPrefix(arg, "--dest-dir="):
			a.destDir = strings.TrimPrefix(arg, "--dest-dir=")
			destDirSet = true
		case arg == "--repo-root":
			if i+1 >= len(args) {
				_ = a.fail("--repo-root requires an argument")
				return 1
			}
			i++
			a.repoRoot = args[i]
		case strings.HasPrefix(arg, "--repo-root="):
			a.repoRoot = strings.TrimPrefix(arg, "--repo-root=")
		case arg == "-h" || arg == "--help":
			a.usage()
			return 0
		case strings.HasPrefix(arg, "--"):
			_ = a.fail("unknown option: %s (see --help)", arg)
			return 1
		default:
			a.requested = append(a.requested, arg)
		}
	}

	if a.repoRoot == "" {
		if envRoot := a.getenv("PI_EXT_REPO_ROOT"); envRoot != "" {
			a.repoRoot = envRoot
		} else if cwd, err := os.Getwd(); err == nil {
			a.repoRoot = cwd
		}
	}
	a.registryPath = filepath.Join(a.repoRoot, "federated", "registry.json")
	reg, err := loadRegistry(a.registryPath)
	if err != nil {
		_ = a.fail("%v", err)
		return 1
	}
	a.reg = reg

	if destDirSet && a.destDir == "" {
		_ = a.fail("--dest-dir was given but is empty")
		return 1
	}
	if a.destDir == "" {
		home := a.getenv("HOME")
		if home == "" {
			_ = a.fail("HOME is not set (or empty) — export HOME or set --dest-dir / PI_EXT_DEST_DIR")
			return 1
		}
		a.destDir = filepath.Join(home, ".pig", "agent", "extensions")
	}
	a.explicitDestDir = destDirSet || destDirEnv != ""

	if !isAbsPath(a.destDir) {
		_ = a.fail("dest-dir is not an absolute path: %s", a.destDir)
		return 1
	}
	if info, err := os.Stat(a.destDir); err == nil && !info.IsDir() {
		_ = a.fail("dest-dir exists but is not a directory: %s", a.destDir)
		return 1
	}

	switch a.mode {
	case "list":
		return a.runList()
	case "audit":
		return a.runAudit()
	case "all":
		for _, ext := range a.reg.installableExtensions() {
			var err error
			if ext.effectiveCategory() == "federated" {
				err = a.installFederated(ext)
			} else {
				err = a.installHosted(ext)
			}
			if err != nil {
				return 1
			}
		}
	case "install":
		if len(a.requested) == 0 {
			fmt.Fprintf(a.stderr, "%s: no action given. Try --list, --audit, --all, or one/more extension names.\n", progName)
			a.usage()
			return 2
		}
		for _, name := range a.requested {
			ext, ok := a.reg.find(name)
			if !ok {
				_ = a.fail("unknown extension: %s (see --list)", name)
				return 1
			}
			switch ext.effectiveCategory() {
			case "external":
				_ = a.fail("refusing to install external/herdr-managed extension: %s — this repo NEVER writes it; reconcile via the herdr binary", name)
				return 1
			case "native":
				_ = a.fail("refusing to install native Go extension: %s — native PiG extensions are loaded by PiG from <destDir>/<destName>/go.mod, not installed as TypeScript by this script", name)
				return 1
			case "federated":
				if err := a.installFederated(*ext); err != nil {
					return 1
				}
			default:
				if err := a.installHosted(*ext); err != nil {
					return 1
				}
			}
		}
	}

	if a.changedAny || a.federatedTouch {
		fmt.Fprintln(a.stdout, "note: Pi only loads extensions at session start — run /reload or start a new session for changes to take effect.")
	}
	if len(a.warns) > 0 {
		fmt.Fprintf(a.stdout, "================= %s WARN SUMMARY (%d) =================\n", progName, len(a.warns))
		for _, w := range a.warns {
			fmt.Fprintf(a.stdout, "WARN: %s\n", w)
		}
		fmt.Fprintln(a.stdout, "note: tolerated non-fatal issues above did NOT abort the run; reconcile them in the respective host repo.")
	}
	return 0
}
