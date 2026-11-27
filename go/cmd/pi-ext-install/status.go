package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type extStatus struct {
	status   string
	srcMD5   string
	destMD5  string
	perms    string
	nativeAt string
}

func nativeGoExtDir(destFilePath string) (string, bool) {
	if !strings.HasSuffix(destFilePath, ".ts") {
		return "", false
	}
	nativeDir := strings.TrimSuffix(destFilePath, ".ts")
	return checkGoModDir(nativeDir)
}

func nativeGoDirByName(destDir, destName string) (string, bool) {
	return checkGoModDir(filepath.Join(destDir, destName))
}

func checkGoModDir(dir string) (string, bool) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	goModInfo, err := os.Stat(filepath.Join(dir, "go.mod"))
	if err != nil || goModInfo.IsDir() {
		return "", false
	}
	return dir, true
}

func (a *app) isZeroNodeProtectedDest() bool {
	if a.getenv("PI_EXT_ALLOW_ZERO_NODE_TS") == "1" {
		return false
	}
	if a.getenv("PI_EXT_TEST_PATH_NO_NODE") != "1" {
		if _, err := exec.LookPath("node"); err == nil {
			return false
		}
	}
	if !a.explicitDestDir {
		return true
	}
	destNorm, err := realpathMissing(a.destDir)
	if err != nil {
		return true
	}
	if home := a.getenv("HOME"); home != "" {
		if homePig, err := realpathMissing(filepath.Join(home, ".pig", "agent", "extensions")); err == nil && homePig != "" {
			if destNorm == homePig {
				return true
			}
		}
	}
	if a.reg != nil && a.reg.DefaultDestDir != "" {
		if regPig, err := realpathMissing(a.reg.DefaultDestDir); err == nil && regPig != "" {
			if destNorm == regPig {
				return true
			}
		}
	}
	return false
}

func statusOf(ext Extension, destDir string) extStatus {
	return statusOfWithRoot(ext, destDir, "")
}

func statusOfWithRoot(ext Extension, destDir, repoRoot string) extStatus {
	cat := ext.effectiveCategory()
	src := resolveSourcePath(ext.Authority.sourcePath(), repoRoot)
	var dest string
	if cat == "native" {
		dest = filepath.Join(destDir, ext.effectiveDestName())
	} else {
		dest = filepath.Join(destDir, ext.DestFile)
	}

	extExist := "present"
	if cat == "federated" {
		script := ext.Authority.installScript()
		if script == "" || !pathExistsOrSymlink(script) {
			extExist = "missing"
		}
	}

	nativeDir := ""
	if cat == "native" {
		if d, ok := nativeGoDirByName(destDir, ext.effectiveDestName()); ok {
			nativeDir = d
		}
	} else if cat != "external" {
		if d, ok := nativeGoExtDir(dest); ok {
			nativeDir = d
		}
	}

	srcMD5 := ""
	if src != "" && isRegularNoFollow(src) {
		if m, err := md5OfRegular(src); err == nil {
			srcMD5 = m
		}
	}

	destMD5 := "(none)"
	perms := "(n/a)"
	if isRegularNoFollow(dest) {
		if m, err := md5OfRegular(dest); err == nil {
			destMD5 = m
		} else {
			destMD5 = "(err)"
		}
		perms = filePermsOctal(dest)
	} else if pathExistsOrSymlink(dest) {
		destMD5 = "----"
		perms = filePermsOctal(dest)
	}

	var st string
	switch {
	case cat == "external" || (src == "" && cat != "native"):
		if isRegularNoFollow(dest) {
			st = "read-only (herdr-managed)"
		} else if pathExistsOrSymlink(dest) {
			st = "read-only (herdr-managed; non-regular entry, content check skipped)"
		} else {
			st = "absent (herdr-managed)"
		}
	case cat == "native":
		if nativeDir != "" {
			st = fmt.Sprintf("native Go active (%s)", nativeDir)
		} else if info, err := os.Stat(dest); err == nil && info.IsDir() {
			st = fmt.Sprintf("DRIFT (directory exists but missing go.mod: %s)", dest)
		} else {
			st = "absent (native Go extension not deployed)"
		}
	case nativeDir != "":
		if !pathExistsOrSymlink(dest) {
			if extExist != "missing" {
				st = fmt.Sprintf("native Go active (%s)", nativeDir)
			}
		} else {
			if extExist != "missing" {
				st = fmt.Sprintf("DRIFT (stale .ts alongside native Go ext %s)", nativeDir)
			}
		}
	case srcMD5 == "":
		st = fmt.Sprintf("source missing: %s", src)
	case !pathExistsOrSymlink(dest):
		st = "not installed"
	case !isRegularNoFollow(dest):
		st = "DRIFT (non-regular entry; content check skipped)"
	case srcMD5 == destMD5:
		st = "unchanged"
	default:
		st = "DRIFT"
	}

	if cat == "federated" && extExist == "missing" {
		if nativeDir != "" {
			if !pathExistsOrSymlink(dest) {
				st = fmt.Sprintf("missing (upstream script not found; native Go active at %s)", nativeDir)
			} else {
				st = fmt.Sprintf("DRIFT (stale .ts alongside native Go ext %s; upstream script not found)", nativeDir)
			}
		} else {
			st = "missing (upstream script not found)"
		}
	}

	if srcMD5 == "" {
		srcMD5 = "--"
	}
	return extStatus{
		status:   st,
		srcMD5:   srcMD5,
		destMD5:  destMD5,
		perms:    perms,
		nativeAt: nativeDir,
	}
}
