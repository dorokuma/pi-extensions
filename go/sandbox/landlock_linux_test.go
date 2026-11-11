//go:build linux

package sandbox

import (
	"errors"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// makeLandlockRulesetFD 创建带全部 FS handled bits 的 Landlock 规则集 fd，
// 供 addPathRule 直接挂载路径规则；内核不支持时 t.Skip。
func makeLandlockRulesetFD(t *testing.T) (fd, abi int) {
	t.Helper()
	abi, ok := supported()
	if !ok {
		t.Skip("Landlock not supported on this kernel")
	}
	attr := landlockRulesetAttr{HandledAccessFS: handledFSForABI(abi)}
	attrSize := unsafe.Sizeof(attr)
	if abi < 4 {
		attrSize = unsafe.Sizeof(attr.HandledAccessFS)
	}
	r1, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		attrSize,
		0,
	)
	if errno != 0 {
		t.Fatalf("landlock_create_ruleset: %v", errno)
	}
	t.Cleanup(func() { _ = unix.Close(int(r1)) })
	return int(r1), abi
}

// TestBuildPathRuleEntriesMarksOptionalReadWriteTransient 验证
// OptionalReadWrite 归一化集合的打标口径：同时登记在 ReadWrite 与
// OptionalReadWrite 的瞬态路径标记 transient=true；其余 ReadWrite /
// ReadOnly / Exec 条目一律 transient=false（含未登记的缺失路径）。
func TestBuildPathRuleEntriesMarksOptionalReadWriteTransient(t *testing.T) {
	transientFile := "/tmp/sbx-test-transient.db-shm"
	strictRW := "/tmp/sbx-test-strict-rw-missing"
	p := Policy{
		ReadOnly:          []string{"/usr"},
		ReadWrite:         []string{transientFile, strictRW},
		OptionalReadWrite: []string{transientFile},
	}
	entries, err := buildPathRuleEntries(p, 3)
	if err != nil {
		t.Fatalf("buildPathRuleEntries: %v", err)
	}
	transientByPath := make(map[string]bool, len(entries))
	for _, e := range entries {
		transientByPath[e.path] = e.transient
	}
	if !transientByPath[transientFile] {
		t.Fatalf("OptionalReadWrite member %q must be marked transient, entries=%+v", transientFile, entries)
	}
	if transientByPath[strictRW] {
		t.Fatalf("plain ReadWrite %q must NOT be marked transient", strictRW)
	}
	for _, path := range []string{"/usr", "/dev/null", "/dev/zero", "/dev/full", "/dev/urandom", "/dev/random"} {
		if transientByPath[path] {
			t.Fatalf("non-ReadWrite entry %q must NOT be marked transient", path)
		}
	}
}

// TestAddPathRuleTransientENOENT 验证 addPathRule 的 ENOENT 容忍面：
// transient 条目缺失时安全跳过返回 nil（修复前以 ENOENT 击穿整个 Apply）；
// 非 transient 的普通 ReadWrite 条目缺失仍 fail-closed 返回 ENOENT 错误。
func TestAddPathRuleTransientENOENT(t *testing.T) {
	rulesetFD, abi := makeLandlockRulesetFD(t)
	missing := "/tmp/sbx-test-transient-gone.db-shm"
	p := Policy{}

	if err := addPathRule(rulesetFD, missing, accessReadWrite(abi), abi, false, true, p); err != nil {
		t.Fatalf("transient missing path must be skipped (nil), got %v", err)
	}
	err := addPathRule(rulesetFD, missing, accessReadWrite(abi), abi, false, false, p)
	if err == nil {
		t.Fatal("non-transient missing path must fail closed, got nil")
	}
	if !errors.Is(err, unix.ENOENT) {
		t.Fatalf("non-transient missing path error = %v, want ENOENT", err)
	}
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("non-transient missing path error = %q, want 'no such file or directory'", err)
	}
}
