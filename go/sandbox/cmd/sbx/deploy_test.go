//go:build linux

// deploy_test.go 覆盖 scripts/deploy.sh 的三道硬门禁（含 .wt 顶层 cwd 边界、
// 门禁前零文件系统副作用）、GIT_* 环境隔离/模块归属强校验、CHANGELOG.md 首个
// 已发布 SemVer 版本戳权威源（Unreleased/缺 var version 拒绝）、TARGET 形状
// 校验（空值/以 / 结尾/已存在目录）、备份裁剪回归（OLD_TAG 无尾随 _）、
// 首次安装、Linux --check 硬阻断、chattr 前置探测阻断，以及解锁校验失败/
// mv 失败时 EXIT trap 对旧 target 的 chattr +i 回锁。
// 所有用例全程在 t.TempDir() 内创建临时 Git 仓（及 linked worktree），通过
// SBX_DEPLOY_TARGET 指向临时路径；默认注入 SBX_DEPLOY_SKIP_IMMUTABLE=1 与
// SBX_DEPLOY_FAST_VERIFY=1（后者防止 deploy.sh 递归套娃 go test）。
// 绝不触碰真实 /root/.local/bin/sbx。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// repoRoot 返回 deploy.sh 所属 Git 仓 top-level（本测试文件位于
// go/sandbox/cmd/sbx，向上四级即仓库根）。
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// cmd/sbx -> cmd -> sandbox -> go -> <repo>
	return filepath.Clean(filepath.Join(wd, "..", "..", "..", ".."))
}

func deployScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "go", "sandbox", "scripts", "deploy.sh")
}

// changelogPath 返回真实 go/sandbox/CHANGELOG.md 路径；deploy.sh 的基础版本号
// 权威源是该文件首个已发布 `## [X.Y.Z] - YYYY-MM-DD` 标题（跳过 [Unreleased]）。
func changelogPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "go", "sandbox", "CHANGELOG.md")
}

// isSemverLike 与 deploy.sh 的 SemVer 校验同形：X.Y.Z（可带 -prerelease）。
func isSemverLike(s string) bool {
	core := s
	if i := strings.IndexByte(core, '-'); i >= 0 {
		core = core[:i]
	}
	nums := strings.Split(core, ".")
	if len(nums) != 3 {
		return false
	}
	for _, n := range nums {
		if n == "" {
			return false
		}
		for _, r := range n {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// expectedBaseVersion 解析真实 CHANGELOG.md 的首个已发布 SemVer 章节号
// （与 deploy.sh 提取规则等价），供 --version 精确匹配断言使用。
func expectedBaseVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(changelogPath(t))
	if err != nil {
		t.Fatalf("ReadFile CHANGELOG.md: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "## [") {
			continue
		}
		inner := line[len("## ["):]
		idx := strings.IndexByte(inner, ']')
		if idx <= 0 {
			continue
		}
		if ver := inner[:idx]; isSemverLike(ver) {
			return ver
		}
	}
	t.Fatalf("no published SemVer section found in %s", changelogPath(t))
	return ""
}

// cleanGitEnv 返回剔除了全部 GIT_* 变量的环境（GIT_DIR / GIT_WORK_TREE /
// GIT_INDEX_FILE / GIT_COMMON_DIR 等），避免外部 git hook / worktree 环境
// 污染合成测试仓的 git 操作；GIT_DIR 等显式注入由调用方在 extraEnv 中完成。
func cleanGitEnv() []string {
	var kept []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GIT_") {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = cleanGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (cwd=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// initTestRepo 在 dir 初始化一个 main 分支、含一次提交的临时 Git 仓，
// 并生成最小可编译模块（go.mod + cmd/sbx stub + 真实 CHANGELOG.md），
// 供 deploy.sh 的 go vet / go build 使用。返回该仓 top-level。
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-b", "main")
	gitCmd(t, dir, "config", "user.name", "sbx-deploy-test")
	gitCmd(t, dir, "config", "user.email", "sbx-deploy-test@example.invalid")

	// 最小 Go 模块：deploy.sh 在 go/sandbox 下执行 go vet / go build ./cmd/sbx。
	// stub main 刻意与真实 cmd/sbx/main.go 一致使用 `var version = "dev"`
	//（未注入默认值；deploy.sh 的基础版本号权威源是 CHANGELOG.md 首个已发布
	// SemVer 章节，严禁据此产出 dev 版本），支持 --version 与 --check（默认
	// 输出 'sbx: Landlock supported (ABI v3)' 零退出；FAKE_SBX_FAIL_CHECK=1
	// 时 --check 非零退出以测试阻断分支）。
	mustWrite := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatalf("WriteFile %s: %v", rel, err)
		}
	}
	mustWrite("go/sandbox/go.mod", "module sbxdeploytest\n\ngo 1.22\n", 0o644)
	mustWrite("go/sandbox/cmd/sbx/main.go",
		`package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	for _, a := range os.Args[1:] {
		switch a {
		case "--version":
			fmt.Printf("sbx %s\n", version)
			os.Exit(0)
		case "--check":
			if os.Getenv("FAKE_SBX_FAIL_CHECK") == "1" {
				fmt.Fprintln(os.Stderr, "sbx: Landlock is not supported by the running kernel")
				os.Exit(1)
			}
			fmt.Println("sbx: Landlock supported (ABI v3)")
			os.Exit(0)
		}
	}
	fmt.Printf("sbx %s\n", version)
}
`, 0o644)
	// 版本戳权威源：把真实 go/sandbox/CHANGELOG.md 原样拷入合成测试仓，
	// 杜绝脚本内或测试内的硬编码版本假绿桩。
	changelog, err := os.ReadFile(changelogPath(t))
	if err != nil {
		t.Fatalf("ReadFile real CHANGELOG.md: %v", err)
	}
	mustWrite("go/sandbox/CHANGELOG.md", string(changelog), 0o644)
	// deploy.sh 需要在 scripts/ 下、模块根是 go/sandbox。
	src := deployScript(t)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("ReadFile deploy.sh: %v", err)
	}
	mustWrite("go/sandbox/scripts/deploy.sh", string(data), 0o755)

	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "chore: init")
	return dir
}

// runDeploy 在 cwd 下执行 **该仓** scripts/deploy.sh，注入测试目标与两个开关，
// extraEnv 逐条追加覆盖（如 SBX_DEPLOY_SKIP_IMMUTABLE=0 / PATH=... /
// FAKE_SBX_FAIL_CHECK=1 / GIT_DIR=...），返回退出码与合并输出。
func runDeploy(t *testing.T, cwd, target string, extraEnv ...string) (int, string) {
	t.Helper()
	script := filepath.Join(cwd, "go", "sandbox", "scripts", "deploy.sh")
	return runDeployScript(t, cwd, script, target, extraEnv...)
}

// runDeployScript 与 runDeploy 同语义，但脚本路径与调用方 cwd 解耦：用于
// 覆盖「从外部无关目录 / .wt 目录调用合成主仓 deploy.sh」的场景。
func runDeployScript(t *testing.T, cwd, script, target string, extraEnv ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", script)
	cmd.Dir = cwd
	cmd.Env = append(cleanGitEnv(),
		"SBX_DEPLOY_TARGET="+target,
		"SBX_DEPLOY_SKIP_IMMUTABLE=1",
		"SBX_DEPLOY_FAST_VERIFY=1",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run deploy.sh: %v\n%s", err, out)
	return -1, ""
}

func mustWriteFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func assertTargetAbsent(t *testing.T, target string) {
	t.Helper()
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target must not be created on refusal, stat err=%v", err)
	}
}

// assertNotCreated 断言给定路径（通常是 target 尚不存在的父目录）在拒绝后
// 未被创建——门禁前必须零文件系统副作用。
func assertNotCreated(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("path %s must not be created on refusal, stat err=%v", path, err)
	}
}

func assertNoStagedLeftover(t *testing.T, targetDir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(targetDir, ".sbx.new.*"))
	if err != nil {
		t.Fatalf("glob staged: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("unexpected .sbx.new.* leftovers: %v", matches)
	}
}

func shortSHA(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--short=12", "HEAD")
	cmd.Dir = repo
	cmd.Env = cleanGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------- 门禁拒绝分支

// TestDeployRejectsWtCallerCwd 验证：即使 deploy.sh 本体位于干净主仓，只要
// 调用方物理 cwd 位于 */.wt/* 路径之下（含 cwd 恰好等于 <repo>/.wt 顶层边界），
// 脚本在任何 git 查询与文件系统副作用之前即被非 0 拒绝。
func TestDeployRejectsWtCallerCwd(t *testing.T) {
	repo := initTestRepo(t)
	script := filepath.Join(repo, "go", "sandbox", "scripts", "deploy.sh")

	// 合成一个形如 <tmp>/.wt/<repo>/<branch> 的调用方目录（无需真实 git
	// worktree，门禁只检查调用方物理路径）；target 指向尚不存在的子目录，
	// 断言拒绝后父目录未被 mkdir -p（门禁前零文件系统副作用）。
	wtRoot := filepath.Join(t.TempDir(), ".wt")
	callerDir := filepath.Join(wtRoot, "sbx-deploy-test", "wt-branch")
	if err := os.MkdirAll(callerDir, 0o755); err != nil {
		t.Fatalf("MkdirAll callerDir: %v", err)
	}

	parent := t.TempDir()
	target := filepath.Join(parent, "nonexistent-sub", "sbx")
	code, out := runDeployScript(t, callerDir, script, target)
	if code == 0 {
		t.Fatalf("deploy.sh from .wt caller cwd unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(out, ".wt") {
		t.Fatalf("expected .wt caller-cwd refusal message, got:\n%s", out)
	}
	assertTargetAbsent(t, target)
	assertNotCreated(t, filepath.Join(parent, "nonexistent-sub"))

	// 边界：cwd 恰好等于 <tmp>/.wt 顶层（*"/.wt" 精确匹配分支）同样拒绝。
	parent2 := t.TempDir()
	target2 := filepath.Join(parent2, "nonexistent-sub", "sbx")
	code, out = runDeployScript(t, wtRoot, script, target2)
	if code == 0 {
		t.Fatalf("deploy.sh from /.wt top-level caller cwd unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(out, ".wt") {
		t.Fatalf("expected .wt caller-cwd refusal message, got:\n%s", out)
	}
	assertTargetAbsent(t, target2)
	assertNotCreated(t, filepath.Join(parent2, "nonexistent-sub"))
}

// TestDeployRejectsWorktree 验证在 linked worktree 中执行 deploy.sh 被非 0 拒绝。
func TestDeployRejectsWorktree(t *testing.T) {
	main := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "linked-wt")
	gitCmd(t, main, "worktree", "add", "-b", "wt-test", wt)

	parent := t.TempDir()
	target := filepath.Join(parent, "nonexistent-sub", "sbx")
	code, out := runDeploy(t, wt, target)
	if code == 0 {
		t.Fatalf("deploy.sh in linked worktree unexpectedly succeeded (exit=0)\n%s", out)
	}
	if !strings.Contains(out, "worktree") && !strings.Contains(out, ".wt") &&
		!strings.Contains(out, "不归属") {
		t.Fatalf("expected worktree refusal message, got:\n%s", out)
	}
	assertTargetAbsent(t, target)
	assertNotCreated(t, filepath.Join(parent, "nonexistent-sub"))
}

// TestDeployRejectsNonMainBranch 验证非 main 分支被拒绝。
func TestDeployRejectsNonMainBranch(t *testing.T) {
	repo := initTestRepo(t)
	gitCmd(t, repo, "checkout", "-b", "feat/not-main")

	parent := t.TempDir()
	target := filepath.Join(parent, "nonexistent-sub", "sbx")
	code, out := runDeploy(t, repo, target)
	if code == 0 {
		t.Fatalf("deploy.sh on non-main branch unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(out, "main") {
		t.Fatalf("expected non-main refusal message, got:\n%s", out)
	}
	assertTargetAbsent(t, target)
	assertNotCreated(t, filepath.Join(parent, "nonexistent-sub"))
}

// TestDeployRejectsDirtyTree 验证 dirty 工作树被拒绝（含未跟踪文件）。
func TestDeployRejectsDirtyTree(t *testing.T) {
	repo := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	parent := t.TempDir()
	target := filepath.Join(parent, "nonexistent-sub", "sbx")
	code, out := runDeploy(t, repo, target)
	if code == 0 {
		t.Fatalf("deploy.sh on dirty tree unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(out, "dirty") && !strings.Contains(out, "porcelain") {
		t.Fatalf("expected dirty-tree refusal message, got:\n%s", out)
	}
	assertTargetAbsent(t, target)
	assertNotCreated(t, filepath.Join(parent, "nonexistent-sub"))
}

// TestDeployRejectsGitDirSpoofFromWorktree 验证：即便外部注入指向主仓的
// GIT_DIR，从 linked worktree 调用 deploy.sh 仍被拒绝（GIT_* 剥离后的
// worktree 校验或模块归属强校验命中其一）。
func TestDeployRejectsGitDirSpoofFromWorktree(t *testing.T) {
	main := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "linked-wt")
	gitCmd(t, main, "worktree", "add", "-b", "wt-test", wt)

	target := filepath.Join(t.TempDir(), "sbx")
	code, out := runDeploy(t, wt, target,
		"GIT_DIR="+filepath.Join(main, ".git"),
		"GIT_WORK_TREE="+main,
	)
	if code == 0 {
		t.Fatalf("deploy.sh with spoofed GIT_DIR from worktree unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(out, "worktree") && !strings.Contains(out, ".wt") &&
		!strings.Contains(out, "不归属") {
		t.Fatalf("expected worktree/module-ownership refusal message, got:\n%s", out)
	}
	assertTargetAbsent(t, target)
}

// ---------------------------------------------------------------- TARGET 形状与版本戳权威源拒绝分支

// TestDeployRejectsInvalidTargetShape 验证 SBX_DEPLOY_TARGET 形状校验：
// 以 / 结尾的目录型路径与已存在目录（含指向目录的软链）一律在任何文件系统
// 副作用之前被拒绝。
func TestDeployRejectsInvalidTargetShape(t *testing.T) {
	repo := initTestRepo(t)

	// (a) 以 / 结尾：拒绝且目录内不得生成任何文件。
	dirTarget := t.TempDir()
	code, out := runDeploy(t, repo, dirTarget+"/")
	if code == 0 {
		t.Fatalf("deploy.sh with trailing-slash target unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(out, "结尾") && !strings.Contains(out, "SBX_DEPLOY_TARGET") {
		t.Fatalf("expected trailing-slash refusal message, got:\n%s", out)
	}
	entries, err := os.ReadDir(dirTarget)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dirTarget, err)
	}
	if len(entries) != 0 {
		t.Fatalf("trailing-slash target dir must stay empty, got entries: %v", entries)
	}

	// (b) 已存在目录（不带尾随 /）：同样拒绝。
	code, out = runDeploy(t, repo, dirTarget)
	if code == 0 {
		t.Fatalf("deploy.sh with existing-dir target unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(out, "已存在目录") {
		t.Fatalf("expected existing-dir refusal message, got:\n%s", out)
	}
	entries, err = os.ReadDir(dirTarget)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dirTarget, err)
	}
	if len(entries) != 0 {
		t.Fatalf("existing-dir target must stay empty, got entries: %v", entries)
	}

	// (c) 指向目录的软链：[[ -d ]] 跟随软链，同样拒绝。
	linkTarget := filepath.Join(t.TempDir(), "sbx-link")
	if err := os.Symlink(dirTarget, linkTarget); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	code, out = runDeploy(t, repo, linkTarget)
	if code == 0 {
		t.Fatalf("deploy.sh with symlink-to-dir target unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(out, "已存在目录") {
		t.Fatalf("expected symlink-dir refusal message, got:\n%s", out)
	}
}

// TestDeployRejectsUnreleasedChangelog 破坏性用例：CHANGELOG.md 仅含
// `## [Unreleased]` 而无任何 `## [X.Y.Z]` SemVer 发布章节时，deploy.sh 必须
// 非零退出（基础版本号权威源无法给出合法 SemVer）且不触碰 target、不创建
// 父目录。
func TestDeployRejectsUnreleasedChangelog(t *testing.T) {
	repo := initTestRepo(t)
	mustWriteFile(t, filepath.Join(repo, "go", "sandbox", "CHANGELOG.md"),
		"# Changelog\n\n## [Unreleased]\n\n- work in progress\n", 0o644)
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-m", "test: changelog without published semver")

	parent := t.TempDir()
	target := filepath.Join(parent, "nonexistent-sub", "sbx")
	code, out := runDeploy(t, repo, target)
	if code == 0 {
		t.Fatalf("deploy.sh with Unreleased-only CHANGELOG unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(out, "CHANGELOG.md") {
		t.Fatalf("expected CHANGELOG semver refusal message, got:\n%s", out)
	}
	assertTargetAbsent(t, target)
	assertNotCreated(t, filepath.Join(parent, "nonexistent-sub"))
}

// TestDeployRejectsMissingVersionVar 破坏性用例：cmd/sbx/main.go 删掉
// `var version` 定义（失去 ldflags -X 注入点）时，deploy.sh 必须非零退出。
func TestDeployRejectsMissingVersionVar(t *testing.T) {
	repo := initTestRepo(t)
	mustWriteFile(t, filepath.Join(repo, "go", "sandbox", "cmd", "sbx", "main.go"),
		`package main

import (
	"fmt"
	"os"
)

func main() {
	for _, a := range os.Args[1:] {
		if a == "--version" {
			fmt.Println("sbx dev")
			os.Exit(0)
		}
	}
	fmt.Println("sbx dev")
}
`, 0o644)
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-m", "test: drop var version")

	parent := t.TempDir()
	target := filepath.Join(parent, "nonexistent-sub", "sbx")
	code, out := runDeploy(t, repo, target)
	if code == 0 {
		t.Fatalf("deploy.sh without var version unexpectedly succeeded\n%s", out)
	}
	if !strings.Contains(out, "var version") {
		t.Fatalf("expected missing var version refusal message, got:\n%s", out)
	}
	assertTargetAbsent(t, target)
	assertNotCreated(t, filepath.Join(parent, "nonexistent-sub"))
}

// ---------------------------------------------------------------- 干净 main 发布分支

// TestDeployCleanMainDeploys 验证干净 main 上部署成功：
// 产物 --version 精确等于 `sbx <CHANGELOG首个SemVer>+<12位shortSHA>`（绝不
// 为 dev）、权限 0555、.bak* 裁剪至最新 3 个。
func TestDeployCleanMainDeploys(t *testing.T) {
	repo := initTestRepo(t)
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "sbx")

	// 预置 4 个旧 .bak-*（deploy.sh 应裁剪至最新 3 个）。
	// mtime 递增，保证 "最新 3 个" 语义确定：最旧的一个应被删。
	var bakNames []string
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("%s.bak-2026110%d-000000-deadbeef", target, i+1)
		mustWriteFile(t, name, fmt.Sprintf("old-%d", i), 0o444)
		mt := time.Date(2026, 11, i+1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatalf("Chtimes %s: %v", name, err)
		}
		bakNames = append(bakNames, name)
	}

	// 预置一个已存在的 target，验证备份路径。
	mustWriteFile(t, target, "existing-sbx", 0o755)

	sha := shortSHA(t, repo)
	wantVersion := "sbx " + expectedBaseVersion(t) + "+" + sha

	code, out := runDeploy(t, repo, target)
	if code != 0 {
		t.Fatalf("deploy.sh on clean main failed (exit=%d)\n%s", code, out)
	}

	// (1) 产物存在且可执行。
	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("deployed target missing: %v", err)
	}
	if st.Mode().Perm() != 0o555 {
		t.Fatalf("deployed target mode=%v, want 0555", st.Mode().Perm())
	}

	// (2) --version 精确等于 sbx <SemVer>+<shortSHA>，绝非 dev。
	ver := exec.Command(target, "--version")
	verOut, err := ver.CombinedOutput()
	if err != nil {
		t.Fatalf("--version: %v\n%s", err, verOut)
	}
	vs := strings.TrimSpace(string(verOut))
	if vs != wantVersion {
		t.Fatalf("--version %q, want exact %q", vs, wantVersion)
	}

	// (3) .bak* 裁剪至最新 3 个（含本次新生成的备份，共应为 3 个）。
	matches, err := filepath.Glob(target + ".bak*")
	if err != nil {
		t.Fatalf("glob bak: %v", err)
	}
	sort.Strings(matches)
	if len(matches) != 3 {
		t.Fatalf("expected exactly 3 .bak* after pruning, got %d: %v", len(matches), matches)
	}
	// 最旧的预置备份（index 0）必须被裁掉。
	for _, m := range matches {
		if m == bakNames[0] {
			t.Fatalf("oldest preseeded backup %s should have been pruned; remaining: %v", bakNames[0], matches)
		}
	}
}

// TestDeployBackupPruningKeepsFresh 裁剪回归：旧 TARGET 的 mtime（2020-01-01）
// 早于全部 4 个预置备份（2026-01-01..04）时，新备份 mtime 必须取当前时刻并
// 固定排在保留首位——绝不能因沿用旧 TARGET 的 mtime 被误裁；且备份名精确以
// -<旧版本号> 结尾（OLD_TAG 无尾随换行清洗出的 `_`）。
func TestDeployBackupPruningKeepsFresh(t *testing.T) {
	repo := initTestRepo(t)
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "sbx")

	// 旧 target 为可执行 stub：--version 输出 "sbx 0.3.0"，备份文件名应精确
	// 以 -0.3.0 结尾（无尾随 `_`）；文件字节（含 "existing-sbx" 标记）须
	// 原样入备份。
	oldTarget := "#!/bin/sh\nif [ \"${1:-}\" = \"--version\" ]; then\n" +
		"\techo \"sbx 0.3.0\"\nelse\n\techo \"existing-sbx\"\nfi\n"
	mustWriteFile(t, target, oldTarget, 0o755)
	oldMtime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(target, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes target: %v", err)
	}

	// 4 个预置备份，mtime 2026-01-01..04（均晚于旧 target 的 2020）。
	var bakNames []string
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("%s.bak-2026010%d-000000-preseed", target, i+1)
		mustWriteFile(t, name, fmt.Sprintf("old-%d", i), 0o444)
		mt := time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatalf("Chtimes %s: %v", name, err)
		}
		bakNames = append(bakNames, name)
	}

	code, out := runDeploy(t, repo, target)
	if code != 0 {
		t.Fatalf("deploy.sh failed (exit=%d)\n%s", code, out)
	}

	// 保留恰好 3 个 .bak*。
	matches, err := filepath.Glob(target + ".bak*")
	if err != nil {
		t.Fatalf("glob bak: %v", err)
	}
	if len(matches) != 3 {
		t.Fatalf("expected exactly 3 .bak* after pruning, got %d: %v", len(matches), matches)
	}
	contains := func(name string) bool {
		for _, m := range matches {
			if m == name {
				return true
			}
		}
		return false
	}
	// 最旧的两个预置备份被删，较新的两个保留。
	if contains(bakNames[0]) || contains(bakNames[1]) {
		t.Fatalf("oldest preseeded backups should be pruned; remaining: %v", matches)
	}
	if !contains(bakNames[2]) || !contains(bakNames[3]) {
		t.Fatalf("newer preseeded backups should be kept; remaining: %v", matches)
	}

	// 新备份：文件名精确以 -0.3.0 结尾（无尾随 `_`），内容原样保留旧
	// target 字节，权限 0444。
	var newBak string
	for _, m := range matches {
		if m != bakNames[2] && m != bakNames[3] {
			newBak = m
		}
	}
	if newBak == "" {
		t.Fatalf("fresh backup not found among: %v", matches)
	}
	if !strings.HasSuffix(newBak, "-0.3.0") {
		t.Fatalf("fresh backup name %q must end with %q (no trailing underscore)", newBak, "-0.3.0")
	}
	data, err := os.ReadFile(newBak)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", newBak, err)
	}
	if string(data) != oldTarget {
		t.Fatalf("fresh backup content mismatch:\n got %q\nwant %q", data, oldTarget)
	}
	if !strings.Contains(string(data), "existing-sbx") {
		t.Fatalf("fresh backup should preserve old target bytes incl. 'existing-sbx'")
	}
	st, err := os.Stat(newBak)
	if err != nil {
		t.Fatalf("Stat %s: %v", newBak, err)
	}
	if st.Mode().Perm() != 0o444 {
		t.Fatalf("fresh backup mode=%v, want 0444", st.Mode().Perm())
	}
	// 新备份 mtime 为当前备份时刻（晚于旧 target 的 2020），非沿用旧 mtime。
	if !st.ModTime().After(oldMtime) {
		t.Fatalf("fresh backup mtime %v should be after old target mtime %v", st.ModTime(), oldMtime)
	}
}

// TestDeployFirstInstallWithSpaceDir 首次安装：目标不存在且父目录含空格，
// 断言自动创建父目录、装出 0555 二进制且不生成 .bak*。
func TestDeployFirstInstallWithSpaceDir(t *testing.T) {
	repo := initTestRepo(t)
	target := filepath.Join(t.TempDir(), "bin with space", "sbx")

	code, out := runDeploy(t, repo, target)
	if code != 0 {
		t.Fatalf("deploy.sh first install failed (exit=%d)\n%s", code, out)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("deployed target missing: %v", err)
	}
	if st.Mode().Perm() != 0o555 {
		t.Fatalf("deployed target mode=%v, want 0555", st.Mode().Perm())
	}
	matches, err := filepath.Glob(target + ".bak*")
	if err != nil {
		t.Fatalf("glob bak: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("first install must not create .bak*, got: %v", matches)
	}
	// --version sanity：注入版本精确等于 sbx <SemVer>+<shortSHA>。
	ver := exec.Command(target, "--version")
	verOut, err := ver.CombinedOutput()
	if err != nil {
		t.Fatalf("--version: %v\n%s", err, verOut)
	}
	want := "sbx " + expectedBaseVersion(t) + "+" + shortSHA(t, repo)
	if strings.TrimSpace(string(verOut)) != want {
		t.Fatalf("--version %q, want exact %q", strings.TrimSpace(string(verOut)), want)
	}
}

// TestDeployRelativeTargetFromExternalCwd 验证：从与仓库无关的外部目录、以
// **相对路径** SBX_DEPLOY_TARGET（relbin/sbx）调用合成主仓的 deploy.sh，
// 目标路径会被规范化为物理绝对路径，安装精确落在 <caller>/relbin/sbx，
// 而不受脚本内部 `cd "$REPO_ROOT"` 子 shell 切换目录影响。
func TestDeployRelativeTargetFromExternalCwd(t *testing.T) {
	repo := initTestRepo(t)
	script := filepath.Join(repo, "go", "sandbox", "scripts", "deploy.sh")
	callerDir := t.TempDir()

	code, out := runDeployScript(t, callerDir, script, filepath.Join("relbin", "sbx"))
	if code != 0 {
		t.Fatalf("deploy.sh with relative SBX_DEPLOY_TARGET failed (exit=%d)\n%s", code, out)
	}
	target := filepath.Join(callerDir, "relbin", "sbx")
	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("relative target not installed at expected location: %v", err)
	}
	if st.Mode().Perm() != 0o555 {
		t.Fatalf("deployed target mode=%v, want 0555", st.Mode().Perm())
	}
	ver := exec.Command(target, "--version")
	verOut, err := ver.CombinedOutput()
	if err != nil {
		t.Fatalf("--version: %v\n%s", err, verOut)
	}
	want := "sbx " + expectedBaseVersion(t) + "+" + shortSHA(t, repo)
	if strings.TrimSpace(string(verOut)) != want {
		t.Fatalf("--version %q, want exact %q", strings.TrimSpace(string(verOut)), want)
	}
}

// ---------------------------------------------------------------- 冒烟与 chattr 阻断分支

// TestDeployCheckFailureBlocks Linux --check 硬阻断：stub 使 --check 非零退出，
// 断言 deploy.sh 非零退出、既有 TARGET 内容未被覆盖、无 .sbx.new.* 残留。
func TestDeployCheckFailureBlocks(t *testing.T) {
	repo := initTestRepo(t)
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "sbx")

	oldContent := "existing-sbx"
	mustWriteFile(t, target, oldContent, 0o755)

	code, out := runDeploy(t, repo, target, "FAKE_SBX_FAIL_CHECK=1")
	if code == 0 {
		t.Fatalf("deploy.sh should hard-block on --check failure\n%s", out)
	}
	if !strings.Contains(out, "--check") {
		t.Fatalf("expected --check block message, got:\n%s", out)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != oldContent {
		t.Fatalf("existing target must not be overwritten; got %q", data)
	}
	assertNoStagedLeftover(t, targetDir)
	// --check 阻断发生在备份之前，不得生成 .bak*。
	matches, err := filepath.Glob(target + ".bak*")
	if err != nil {
		t.Fatalf("glob bak: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("no .bak* should be created on --check block, got: %v", matches)
	}
}

// TestDeployChattrProbeFailureBlocks chattr 前置探测失败阻断：PATH 注入伪造
// chattr（退出码 1）且不设 SBX_DEPLOY_SKIP_IMMUTABLE，断言 deploy.sh 在触碰
// 任何备份与线上 TARGET 之前即非零退出——既有 4 个预置备份与 TARGET 原封未动，
// 且无 .sbx.new.* 残留。
func TestDeployChattrProbeFailureBlocks(t *testing.T) {
	repo := initTestRepo(t)
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "sbx")

	oldContent := "existing-sbx"
	mustWriteFile(t, target, oldContent, 0o755)

	var bakNames []string
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("%s.bak-2026010%d-000000-preseed", target, i+1)
		mustWriteFile(t, name, fmt.Sprintf("old-%d", i), 0o444)
		bakNames = append(bakNames, name)
	}

	// PATH 注入伪造 chattr（仅影子 chattr 一个名字，其余工具正常）。
	fakeBin := t.TempDir()
	mustWriteFile(t, filepath.Join(fakeBin, "chattr"), "#!/bin/sh\nexit 1\n", 0o755)

	code, out := runDeploy(t, repo, target,
		"SBX_DEPLOY_SKIP_IMMUTABLE=0",
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
	)
	if code == 0 {
		t.Fatalf("deploy.sh should die on chattr probe failure\n%s", out)
	}
	if !strings.Contains(out, "chattr") {
		t.Fatalf("expected chattr probe failure message, got:\n%s", out)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != oldContent {
		t.Fatalf("existing target must be untouched on probe failure; got %q", data)
	}
	for _, b := range bakNames {
		st, err := os.Stat(b)
		if err != nil {
			t.Fatalf("preseeded backup %s must be untouched: %v", b, err)
		}
		if st.Mode().Perm() != 0o444 {
			t.Fatalf("preseeded backup %s mode=%v, want 0444 (untouched)", b, st.Mode().Perm())
		}
	}
	assertNoStagedLeftover(t, targetDir)
}

// ---------------------------------------------------------------- 解锁/替换失败回锁分支（非 SKIP_IMMUTABLE）

// fakeChattrLogScript 记录型伪造 chattr：把每次调用参数追加到
// $FAKE_CHATTR_LOG，恒成功退出（用于断言 EXIT trap 的回锁确实发生）。
const fakeChattrLogScript = "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"${FAKE_CHATTR_LOG:-/dev/null}\"\nexit 0\n"

// fakeLsattrNoIScript 伪造 lsattr：对 `lsattr -d <path>` 恒报告无 i 标志，
// 模拟 chattr -i 已成功解锁。
const fakeLsattrNoIScript = "#!/bin/sh\nlast=\"\"\nfor a in \"$@\"; do last=\"$a\"; done\nprintf -- '------------- %s\\n' \"$last\"\nexit 0\n"

// TestDeployUnlockVerifyFailureRelocks 回锁回归（解锁校验分支）：伪造 lsattr
// 使 `chattr -i` 后 lsattr 仍报告 i 标志，deploy.sh 必须在 mv 前 die 且
// EXIT trap 已对旧 target 执行 `chattr +i` 回锁；旧 target 内容未被覆盖、
// 已生成的本次 .bak* 保留（裁剪只在 mv 成功后执行）、无暂存件残留。
func TestDeployUnlockVerifyFailureRelocks(t *testing.T) {
	repo := initTestRepo(t)
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "sbx")

	oldContent := "existing-sbx"
	mustWriteFile(t, target, oldContent, 0o755)

	fakeBin := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "chattr.log")
	mustWriteFile(t, filepath.Join(fakeBin, "chattr"), fakeChattrLogScript, 0o755)
	// 伪造 lsattr：只要目标存在就恒报告含 i 标志，模拟 chattr -i 解锁失败。
	mustWriteFile(t, filepath.Join(fakeBin, "lsattr"),
		"#!/bin/sh\nlast=\"\"\nfor a in \"$@\"; do last=\"$a\"; done\n"+
			"if [ -e \"$last\" ]; then printf -- '----i-------- %s\\n' \"$last\"; else printf -- '------------- %s\\n' \"$last\"; fi\nexit 0\n",
		0o755)

	code, out := runDeploy(t, repo, target,
		"SBX_DEPLOY_SKIP_IMMUTABLE=0",
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"FAKE_CHATTR_LOG="+logFile,
	)
	if code == 0 {
		t.Fatalf("deploy.sh should die when lsattr still reports i after chattr -i\n%s", out)
	}
	if !strings.Contains(out, "chattr +i") {
		t.Fatalf("expected unlock-verify refusal message, got:\n%s", out)
	}
	// 旧 target 内容未被覆盖（解锁校验失败发生在 mv 之前）。
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != oldContent {
		t.Fatalf("existing target must not be overwritten; got %q", data)
	}
	// EXIT trap 回锁证据：伪造 chattr 日志中必须出现对旧 target 的 "+i --"。
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("ReadFile chattr log: %v", err)
	}
	if !strings.Contains(string(logData), "+i -- "+target) {
		t.Fatalf("expected chattr +i relock of %s in fake chattr log, got:\n%s", target, logData)
	}
	// 已生成的本次备份必须保留（裁剪仅在 mv 成功后执行，不得在失败路径丢备份）。
	matches, err := filepath.Glob(target + ".bak*")
	if err != nil {
		t.Fatalf("glob bak: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("exactly one fresh .bak* should survive the failed deploy, got: %v", matches)
	}
	assertNoStagedLeftover(t, targetDir)
}

// TestDeployMvFailureRelocksKeepsBackups 回锁回归（mv 分支）：伪造 mv 恒失败，
// deploy.sh 必须在原子替换处 die 且 EXIT trap 已对旧 target 执行 `chattr +i`
// 回锁；全部 .bak*（含本次新备份与预置备份）一律保留——裁剪整体移至 mv -f
// 成功之后，失败路径绝不提前删历史备份。
func TestDeployMvFailureRelocksKeepsBackups(t *testing.T) {
	repo := initTestRepo(t)
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "sbx")

	oldContent := "existing-sbx"
	mustWriteFile(t, target, oldContent, 0o755)

	var preBaks []string
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("%s.bak-2026010%d-000000-preseed", target, i+1)
		mustWriteFile(t, name, fmt.Sprintf("old-%d", i), 0o444)
		preBaks = append(preBaks, name)
	}

	fakeBin := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "chattr.log")
	mustWriteFile(t, filepath.Join(fakeBin, "chattr"), fakeChattrLogScript, 0o755)
	mustWriteFile(t, filepath.Join(fakeBin, "lsattr"), fakeLsattrNoIScript, 0o755)
	mustWriteFile(t, filepath.Join(fakeBin, "mv"), "#!/bin/sh\nexit 1\n", 0o755)

	code, out := runDeploy(t, repo, target,
		"SBX_DEPLOY_SKIP_IMMUTABLE=0",
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"FAKE_CHATTR_LOG="+logFile,
	)
	if code == 0 {
		t.Fatalf("deploy.sh should die on mv failure\n%s", out)
	}
	if !strings.Contains(out, "mv -f") {
		t.Fatalf("expected mv failure message, got:\n%s", out)
	}
	// 旧 target 内容未被覆盖。
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != oldContent {
		t.Fatalf("existing target must not be overwritten on mv failure; got %q", data)
	}
	// EXIT trap 回锁证据："+i -- <target>" 必须出现在伪造 chattr 日志中。
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("ReadFile chattr log: %v", err)
	}
	if !strings.Contains(string(logData), "+i -- "+target) {
		t.Fatalf("expected chattr +i relock of %s in fake chattr log, got:\n%s", target, logData)
	}
	// 全部 .bak* 保留：4 个预置 + 1 个本次新备份，一个都不许提前裁剪。
	//（预置 4 个 > 裁剪上限 3：若裁剪在 mv 失败前提前执行必然可观测，
	// 消除 2 预置时「即便提前裁剪也不删文件」的假绿盲区。）
	matches, err := filepath.Glob(target + ".bak*")
	if err != nil {
		t.Fatalf("glob bak: %v", err)
	}
	if len(matches) != 5 {
		t.Fatalf("all backups must survive failed mv (4 preseeded + 1 fresh), got: %v", matches)
	}
	for _, b := range preBaks {
		found := false
		for _, m := range matches {
			if m == b {
				found = true
			}
		}
		if !found {
			t.Fatalf("preseeded backup %s must survive failed mv; remaining: %v", b, matches)
		}
	}
	assertNoStagedLeftover(t, targetDir)
}
