package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const tableDivider = "-------------------------------------------------------------------------------------------------------------------------------"

func (a *app) runList() int {
	fmt.Fprintf(a.stdout, "federated pi-extensions registry: %s\n", a.registryPath)
	fmt.Fprintf(a.stdout, "dest-dir: %s\n", a.destDir)
	fmt.Fprintf(a.stdout, "%-17s %-9s %-15s %-44s %s\n", "NAME", "CATEGORY", "KIND", "AUTHORITY-SOURCE", "TARGET (status)")
	fmt.Fprintln(a.stdout, tableDivider)

	for _, ext := range a.reg.Extensions {
		cat := ext.effectiveCategory()
		src := resolveSourcePath(ext.Authority.sourcePath(), a.repoRoot)
		if src == "" {
			src = "(out of band — external tool)"
		}
		var dest string
		if cat == "native" {
			dest = filepath.Join(a.destDir, ext.effectiveDestName())
		} else {
			dest = filepath.Join(a.destDir, ext.DestFile)
		}
		st := statusOfWithRoot(ext, a.destDir, a.repoRoot)
		fmt.Fprintf(a.stdout, "%-17s %-9s %-15s %-44s %s [%s]\n",
			ext.Name, cat, ext.Authority.Kind, src, dest, st.status)
	}
	fmt.Fprintln(a.stdout, "note: herdr-agent-state.ts + moshi-hooks.ts are external — never written by this repo (read-only); native Go extensions are deployed by PiG (install.sh only audits).")
	return 0
}

func (a *app) runAudit() int {
	fmt.Fprintf(a.stdout, "%s audit (READ-ONLY) — %s\n", progName, time.Now().Format("2006-01-02 15:04:05 -0700"))
	fmt.Fprintf(a.stdout, "registry: %s\n", a.registryPath)
	fmt.Fprintf(a.stdout, "dest-dir: %s\n", a.destDir)
	fmt.Fprintln(a.stdout, tableDivider)
	fmt.Fprintf(a.stdout, "%-17s %-9s %-34s %-5s %s\n", "NAME", "CATEGORY", "DEST_MD5", "PERM", "AUDIT")

	for _, ext := range a.reg.Extensions {
		cat := ext.effectiveCategory()
		st := statusOfWithRoot(ext, a.destDir, a.repoRoot)
		match := "MISMATCH"
		if st.srcMD5 != "--" && st.srcMD5 == st.destMD5 {
			match = "MATCH"
		}
		if cat == "external" || st.srcMD5 == "--" {
			match = "----"
		}
		switch {
		case strings.HasPrefix(st.status, "native Go active"):
			match = "NATIVE"
		case strings.HasPrefix(st.status, "DRIFT"), strings.HasPrefix(st.status, "missing"):
			match = "MISMATCH"
		}
		fmt.Fprintf(a.stdout, "%-17s %-9s %-34s %-5s %s\n",
			ext.Name, cat, st.destMD5, st.perms, match+" — "+st.status)

		switch cat {
		case "federated":
			script := ext.Authority.installScript()
			if script == "" || !pathExistsOrSymlink(script) {
				fmt.Fprintf(a.stdout, "    └─ federated upstream install.sh: MISSING (%s)\n", script)
			} else {
				fmt.Fprintf(a.stdout, "    └─ federated upstream install.sh: present (%s)\n", script)
			}
		case "external":
			fmt.Fprintln(a.stdout, "    └─ external/out-of-band managed: READ-ONLY — this repo will never modify it")
		case "native":
			fmt.Fprintln(a.stdout, "    └─ native PiG Go extension: deployed by PiG (go.mod dir); install.sh only audits, never installs TS")
		}
	}
	fmt.Fprintln(a.stdout, tableDivider)
	a.auditUnregisteredDest()
	fmt.Fprintln(a.stdout, tableDivider)
	fmt.Fprintln(a.stdout, "audit complete: no files written (this mode is strictly read-only).")
	return 0
}

func (a *app) auditUnregisteredDest() {
	ddir := a.destDir
	info, err := os.Stat(ddir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(a.stdout, "unregistered entries in dest-dir (%s): none (dest-dir does not exist)\n", ddir)
		return
	}

	regSet := a.reg.registeredDestNames()
	dirEntries, err := os.ReadDir(ddir)
	var topEntries []string
	var scanErrs []string
	if err != nil {
		scanErrs = append(scanErrs, err.Error())
	} else {
		for _, de := range dirEntries {
			name := de.Name()
			if _, ok := regSet[name]; !ok {
				topEntries = append(topEntries, filepath.Join(ddir, name))
			}
		}
		sort.Strings(topEntries)
	}

	var nestedTSEntries []string
	nestedNonTSCount := 0
	for _, de := range dirEntries {
		if de.Type()&fs.ModeSymlink != 0 || !de.IsDir() {
			continue
		}
		subRoot := filepath.Join(ddir, de.Name())
		walkErr := filepath.WalkDir(subRoot, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				scanErrs = append(scanErrs, werr.Error())
				return nil
			}
			if p == subRoot {
				return nil
			}
			if d.Type()&fs.ModeSymlink == 0 && d.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(ddir, p)
			if rerr != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if strings.HasSuffix(strings.ToLower(relSlash), ".ts") {
				nestedTSEntries = append(nestedTSEntries, p)
			} else {
				nestedNonTSCount++
			}
			return nil
		})
		if walkErr != nil {
			scanErrs = append(scanErrs, walkErr.Error())
		}
	}
	sort.Strings(nestedTSEntries)

	scanRC := 0
	scanErrClean := ""
	scanStatusTag := ""
	if len(scanErrs) > 0 {
		scanRC = 1
		scanErrClean = strings.Join(scanErrs, "; ")
		scanStatusTag = fmt.Sprintf(" [INCOMPLETE: exit %d]", scanRC)
	}

	if scanRC == 0 && len(topEntries) == 0 && len(nestedTSEntries) == 0 && nestedNonTSCount == 0 {
		fmt.Fprintf(a.stdout, "unregistered entries in dest-dir (%s): 0 (all dest-dir entries are registered in registry; recursive scan: unlimited depth, symlinks not followed [-P])\n", ddir)
		return
	}

	listedTotal := len(topEntries) + len(nestedTSEntries)
	fmt.Fprintf(a.stdout, "unregistered entries in dest-dir (present in %s, absent from registry): top-level=%d, nested .ts scanned=%d%s, nested non-.ts (count-only)=%d (listed in table: %d)\n",
		ddir, len(topEntries), len(nestedTSEntries), scanStatusTag, nestedNonTSCount, listedTotal)
	if scanRC == 0 {
		fmt.Fprintln(a.stdout, "scan scope: top-level all entries + recursive subdirectories (depth >= 2, unlimited depth) for *.ts (case-insensitive); symlinks not followed (find -P / lstat)")
	} else {
		fmt.Fprintf(a.stdout, "scan scope: top-level all entries + recursive subdirectories (INCOMPLETE: exit %d — %s; nested entries may be under-reported)\n", scanRC, scanErrClean)
	}
	if listedTotal == 0 && nestedNonTSCount == 0 {
		fmt.Fprintf(a.stdout, "%s WARN: recursive subdirectory scan INCOMPLETE in %s (exit %d: %s) — nested entries may be under-reported (read-only audit; exit code unchanged).\n",
			progName, ddir, scanRC, scanErrClean)
		return
	}

	fmt.Fprintf(a.stdout, "%-42s %-22s %-34s %-5s %s\n", "UNREGISTERED_ENTRY", "KIND", "DEST_MD5", "PERM", "AUDIT")

	var (
		total, topTotal, nestedTSTotal            int
		topTSCount, nestedTSCount, testsFileCount int
		bakCount, dirCount, plainDirCount         int
		testsDirCount, otherCount                 int
		otherFileCount, otherSpecialCount         int
		tsList                                    []string
	)

	for _, entry := range topEntries {
		base := filepath.Base(entry)
		topTotal++
		total++
		dmd5 := "----"
		perms := filePermsOctal(entry)
		linfo, lerr := os.Lstat(entry)

		var kind, detail string
		switch {
		case lerr == nil && linfo.Mode()&os.ModeSymlink != 0:
			otherSpecialCount++
			otherCount++
			kind = "other"
			detail = "WARN: UNREGISTERED — symlink entry in dest-dir (symlink not followed; content check skipped)"
		case lerr == nil && linfo.IsDir():
			dirCount++
			if base == "__tests__" {
				testsDirCount++
				kind = "__tests__ dir"
				detail = "WARN: UNREGISTERED — test directory in dest-dir (not in registry; content check skipped)"
			} else {
				plainDirCount++
				kind = "dir"
				detail = "WARN: UNREGISTERED — directory in dest-dir (not in registry; content check skipped)"
			}
			base += "/"
		case lerr == nil && linfo.Mode().IsRegular():
			if m, err := md5OfRegular(entry); err == nil {
				dmd5 = m
			} else {
				dmd5 = "(err)"
			}
			switch {
			case strings.Contains(base, ".bak-") || strings.HasSuffix(base, ".bak"):
				bakCount++
				kind = "historical backup"
				detail = "WARN: UNREGISTERED — historical backup (frozen per AGENTS.md #6; not in registry)"
			case strings.HasSuffix(strings.ToLower(base), ".ts"):
				topTSCount++
				tsList = append(tsList, base)
				kind = "other *.ts"
				detail = "WARN: UNREGISTERED — active .ts extension not in registry"
			default:
				otherFileCount++
				otherCount++
				kind = "other-file"
				detail = "WARN: UNREGISTERED — non-registry file in dest-dir"
			}
		default:
			otherSpecialCount++
			otherCount++
			kind = "other"
			detail = "WARN: UNREGISTERED — special non-regular entry in dest-dir (fifo/socket/device; content check skipped)"
		}
		fmt.Fprintf(a.stdout, "%-42s %-22s %-34s %-5s %s\n", base, kind, dmd5, perms, detail)
	}

	for _, sub := range nestedTSEntries {
		rel, _ := filepath.Rel(ddir, sub)
		relSlash := filepath.ToSlash(rel)
		nestedTSTotal++
		total++
		dmd5 := "----"
		perms := filePermsOctal(sub)
		isReg := isRegularNoFollow(sub)
		if isReg {
			if m, err := md5OfRegular(sub); err == nil {
				dmd5 = m
			} else {
				dmd5 = "(err)"
			}
		}

		var kind, detail string
		if strings.HasPrefix(relSlash, "__tests__/") || strings.Contains(relSlash, "/__tests__/") {
			testsFileCount++
			kind = "__tests__ file"
			if isReg {
				detail = "WARN: UNREGISTERED — test file under __tests__/ (frozen/legitimate; not in registry)"
			} else {
				detail = "WARN: UNREGISTERED — non-regular test entry under __tests__/ (fifo/socket/device/symlink; content check skipped)"
			}
		} else {
			nestedTSCount++
			tsList = append(tsList, relSlash)
			kind = "nested *.ts"
			if isReg {
				detail = "WARN: UNREGISTERED — nested .ts file in subdirectory (not in registry)"
			} else {
				detail = "WARN: UNREGISTERED — nested .ts non-regular entry in subdirectory (fifo/socket/device/symlink; content check skipped)"
			}
		}
		fmt.Fprintf(a.stdout, "%-42s %-22s %-34s %-5s %s\n", relSlash, kind, dmd5, perms, detail)
	}

	fmt.Fprintf(a.stdout, "%s WARN: dest-dir (%s) has %d unregistered listed entry/entries not in registry (top-level: %d, nested .ts scanned: %d; other *.ts: %d, nested *.ts: %d, __tests__ file: %d, historical backup: %d, dir [merged: dir=%d + __tests__ dir=%d]: %d, other [merged: other-file=%d + other=%d]: %d; nested non-.ts not listed: %d).\n",
		progName, ddir, total, topTotal, nestedTSTotal,
		topTSCount, nestedTSCount, testsFileCount, bakCount,
		plainDirCount, testsDirCount, dirCount,
		otherFileCount, otherSpecialCount, otherCount, nestedNonTSCount)
	if len(tsList) > 0 {
		fmt.Fprintf(a.stdout, "%s WARN: unregistered *.ts in dest-dir: %s — verify whether standalone/external or stale leftover (read-only audit; exit code unchanged).\n",
			progName, strings.Join(tsList, " "))
	}
	if scanRC != 0 {
		fmt.Fprintf(a.stdout, "%s WARN: recursive subdirectory scan INCOMPLETE in %s (exit %d: %s) — nested entries may be under-reported (read-only audit; exit code unchanged).\n",
			progName, ddir, scanRC, scanErrClean)
	}
}
