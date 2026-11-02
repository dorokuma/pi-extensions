#!/usr/bin/env bash
# install.test.sh — install.sh 原生 Go 扩展共存保护 + zero-node 兜底保护回归测试
# 覆盖：hosted/federated 命中 native Go 目录（含软链）时跳过安装/级联并自愈残留 .ts；
# 悬空软链/无 go.mod 不触发；dry-run 不写不删；--list/--audit NATIVE/DRIFT 状态
#（残留 .ts 与源同 md5 时 audit 必定 MISMATCH，绝不出现 MATCH — DRIFT）；
# federated 上游脚本缺失 + 原生 Go 在位时只读状态合并为 missing (upstream script not found;
# native Go active at ...)（audit 标 MISMATCH）；NEVERWRITE 三层防线（external 类别剔除 /
# installable_names 枚举过滤 / refuse_if_neverwrite 写入层兜底）全覆盖；zero-node（无 node）
# 真实 pig 扩展目录保护：隔离 registry 双锚点（$HOME/.pig/... 与 .defaultDestDir）realpath -m
# 规范化判定，hosted/federated 显式单装拒绝、4 类路径等价绕过（尾斜杠/../HOME 软链/env -u HOME）
# 全部拦截、--all 跳过+自愈、PI_EXT_ALLOW_ZERO_NODE_TS=1 放行、显式 --dest-dir 其它目录不受限。
set -euo pipefail

# 立即切断外部调用环境的变量污染（如调用方 shell 已 export 这些变量会干扰断言）。
unset PI_EXT_DEST_DIR PI_EXT_ALLOW_ZERO_NODE_TS PI_EXT_TEST_PATH_NO_NODE

PROG="install.test.sh"
PASS=0
FAIL=0
TEST_NAMES=()

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL="$SCRIPT_DIR/install.sh"
REGISTRY="$SCRIPT_DIR/federated/registry.json"

fail() { echo "$PROG FAILED: $*" >&2; exit 1; }

pass() { PASS=$((PASS + 1)); TEST_NAMES+=("PASS: $1"); echo "  PASS: $1"; }
fail_test() { FAIL=$((FAIL + 1)); TEST_NAMES+=("FAIL: $1"); echo "  FAIL: $1"; }

assert_eq() { # $1=expected $2=actual $3=label
  if [ "$1" = "$2" ]; then pass "$3"; else fail_test "$3 (expected='$1' actual='$2')"; fi
}
assert_contains() { # $1=haystack $2=needle $3=label
  if printf '%s' "$1" | grep -qF -- "$2"; then pass "$3"; else fail_test "$3 (missing '$2')"; fi
}
assert_not_contains() { # $1=haystack $2=needle $3=label
  if printf '%s' "$1" | grep -qF -- "$2"; then fail_test "$3 (unexpected '$2')"; else pass "$3"; fi
}
assert_file_exists() { # $1=path $2=label — 显式 if/else，不用短路链（[ ] || [ ] && 形态在
  # 首测失败时会把第二个 [ ] 的退出码误当整体结果，造成假阳性/假阴性）
  if [ -e "$1" ] || [ -L "$1" ]; then pass "$2"; else fail_test "$2 (missing $1)"; fi
}
assert_file_not_exists() { # $1=path $2=label
  if [ ! -e "$1" ] && [ ! -L "$1" ]; then pass "$2"; else fail_test "$2 (still exists $1)"; fi
}

# 全部临时夹具统一建在一个根目录下，EXIT trap 一次清理，/tmp 零泄漏。
TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# 从 registry 取一个 hosted 与 federated 扩展名/destFile
hosted_name="$(jq -r '.extensions[] | select(.category=="hosted") | .name' "$REGISTRY" | head -n1)"
hosted_dest="$(jq -r --arg n "$hosted_name" '.extensions[] | select(.name==$n) | .destFile' "$REGISTRY")"
fed_name="$(jq -r '.extensions[] | select(.category=="federated") | .name' "$REGISTRY" | head -n1)"
fed_dest="$(jq -r --arg n "$fed_name" '.extensions[] | select(.name==$n) | .destFile' "$REGISTRY")"
fed_env="$(jq -r --arg n "$fed_name" '.extensions[] | select(.name==$n) | .authority.installDestEnv' "$REGISTRY")"
fed_script="$(jq -r --arg n "$fed_name" '.extensions[] | select(.name==$n) | .authority.installScript' "$REGISTRY")"
external_name="$(jq -r '.extensions[] | select(.category=="external") | .name' "$REGISTRY" | head -n1)"
external_dest="$(jq -r --arg n "$external_name" '.extensions[] | select(.name==$n) | .destFile' "$REGISTRY")"
# zero-node 测试用：取一个无同名原生 Go 目录的 hosted 扩展（registry 中 hosted 末位为 magpie-image，
# 其部署目录无 <name>/ 同名 Go 目录），名字从 registry 动态取避免硬编码。
magpie_name="$(jq -r '.extensions[] | select(.name=="magpie-image") | .name' "$REGISTRY")"
magpie_dest="$(jq -r --arg n "$magpie_name" '.extensions[] | select(.name==$n) | .destFile' "$REGISTRY")"
magpie_src="$(jq -r --arg n "$magpie_name" '.extensions[] | select(.name==$n) | .authority.sourcePath' "$REGISTRY")"

[ -n "$hosted_name" ] && [ -n "$hosted_dest" ] || fail "registry missing hosted entry"
[ -n "$fed_name" ] && [ -n "$fed_dest" ] && [ -n "$fed_env" ] || fail "registry missing federated entry"
[ -n "$external_name" ] && [ -n "$external_dest" ] || fail "registry missing external entry"
[ -n "$magpie_name" ] && [ -n "$magpie_dest" ] && [ -n "$magpie_src" ] && [ -f "$magpie_src" ] \
  || fail "registry missing magpie-image hosted entry (needed for zero-node tests)"

# ========== 1) hosted 命中原生 Go 扩展（软链目录）+ 残留 .ts 自愈 ==========
echo "== hosted: native Go dir (symlink) + stale .ts"
T1="$TMP_ROOT/t1"
mkdir -p "$T1/go-auto-continue"
touch "$T1/go-auto-continue/go.mod"
ln -s "$T1/go-auto-continue" "$T1/auto-continue"   # 软链指向含 go.mod 的目录
echo "stale" > "$T1/auto-continue.ts"
ln -s "$T1/nonexistent" "$T1/stale-link.ts"          # 悬空软链

out="$(bash "$INSTALL" --dest-dir "$T1" "$hosted_name" 2>&1)"
assert_contains "$out" "skipped — native Go extension active at $T1/auto-continue" "hosted skip message"
assert_contains "$out" "removed stale $T1/auto-continue.ts" "hosted stale .ts removed"
assert_file_not_exists "$T1/auto-continue.ts" "hosted .ts self-healed"
assert_file_exists "$T1/auto-continue/go.mod" "hosted go.mod intact"
assert_not_contains "$out" "install -m 644" "hosted no install cmd"

# 残留悬空 .ts 软链（指向不存在目标）→ 也应自愈清除
T1b="$TMP_ROOT/t1b"
hosted_name2="$(jq -r '.extensions[] | select(.category=="hosted") | .name' "$REGISTRY" | sed -n 2p)"
hosted_dest2="$(jq -r --arg n "$hosted_name2" '.extensions[] | select(.name==$n) | .destFile' "$REGISTRY")"
mkdir -p "$T1b/${hosted_dest2%.ts}"
touch "$T1b/${hosted_dest2%.ts}/go.mod"
ln -s "$T1b/nonexistent" "$T1b/$hosted_dest2"        # 悬空 .ts 软链
out="$(bash "$INSTALL" --dest-dir "$T1b" "$hosted_name2" 2>&1)"
assert_contains "$out" "removed stale $T1b/$hosted_dest2" "hosted dangling .ts symlink removed"
assert_file_not_exists "$T1b/$hosted_dest2" "hosted dangling .ts symlink self-healed"

# 再次运行无变化
out="$(bash "$INSTALL" --dest-dir "$T1" "$hosted_name" 2>&1)"
assert_not_contains "$out" "removed stale" "hosted idempotent no stale msg"
assert_not_contains "$out" "/reload" "hosted idempotent no /reload"

# ========== 2) federated 命中原生 Go 扩展 + 探针脚本（含正向对照）==========
echo "== federated: native Go dir + spy upstream"
# 正向对照：无原生 Go 目录时级联必须真的调用上游探针脚本（证明探针/registry 注入有效，
# 防止「探针根本没被换进去」造成的假阴性）。
T_POS="$TMP_ROOT/t_pos"
mkdir -p "$T_POS/federated"
spy_pos="$T_POS/spy.sh"
cat > "$spy_pos" <<EOF
#!/usr/bin/env bash
echo "SPY_CALLED" > "$T_POS/spy_called"
EOF
chmod +x "$spy_pos"
jq --arg name "$fed_name" --arg spy "$spy_pos" \
  '(.extensions[] | select(.name==$name) | .authority.installScript) = $spy' \
  "$REGISTRY" > "$T_POS/federated/registry.json"
cp "$INSTALL" "$T_POS/install.sh"
out="$(cd "$T_POS" && bash ./install.sh --dest-dir "$T_POS" "$fed_name" 2>&1)"
assert_file_exists "$T_POS/spy_called" "federated positive control: spy IS called without native Go dir"

# 负向：有原生 Go 目录时级联必须被跳过，探针不得被调用。
T2="$TMP_ROOT/t2"
mkdir -p "$T2/go-codegraph-go"
touch "$T2/go-codegraph-go/go.mod"
ln -s "$T2/go-codegraph-go" "$T2/codegraph-go"
echo "stale" > "$T2/codegraph-go.ts"
spy="$T2/spy.sh"
cat > "$spy" <<EOF
#!/usr/bin/env bash
echo "SPY_CALLED" > "$T2/spy_called"
EOF
chmod +x "$spy"
mkdir -p "$T2/federated"
jq --arg name "$fed_name" --arg spy "$spy" \
  '(.extensions[] | select(.name==$name) | .authority.installScript) = $spy' \
  "$REGISTRY" > "$T2/federated/registry.json"
cp "$INSTALL" "$T2/install.sh"
out="$(cd "$T2" && bash ./install.sh --dest-dir "$T2" "$fed_name" 2>&1)"
assert_contains "$out" "skipped — native Go extension active at $T2/codegraph-go" "federated skip message"
assert_contains "$out" "cascade skipped" "federated cascade skipped"
assert_file_not_exists "$T2/codegraph-go.ts" "federated .ts self-healed"
assert_file_not_exists "$T2/spy_called" "federated spy NOT called"
assert_not_contains "$out" "WARN" "federated zero WARN"

# ========== 3) 上游脚本不存在 + 原生 Go 在位：安装跳过零 WARN；只读状态合并上报 ==========
echo "== federated: missing upstream script + native Go active"
T3="$TMP_ROOT/t3"
mkdir -p "$T3/go-cache-stats"
touch "$T3/go-cache-stats/go.mod"
ln -s "$T3/go-cache-stats" "$T3/cache-stats"
mkdir -p "$T3/federated"
# registry 中 pi-cache-stats 上游脚本指向不存在路径
sed 's|/root/workspace/pi-cache-stats/integrations/pi/install.sh|/nonexistent/install.sh|' "$REGISTRY" > "$T3/federated/registry.json"
cp "$INSTALL" "$T3/install.sh"
out="$(cd "$T3" && bash ./install.sh --dest-dir "$T3" pi-cache-stats 2>&1)"
assert_not_contains "$out" "WARN" "federated missing upstream but native Go → no WARN"
assert_contains "$out" "skipped" "federated missing upstream but native Go → skipped"

# 只读状态合并（F2）：安装路径命中 native_dir 仍直接跳过零 WARN，但 --list/--audit
# 必须把「注册表上游脚本路径失效」与「native Go active」两个信号合并上报为
# missing (upstream script not found; native Go active at ...) / MISMATCH，不得只报
# native Go active 而掩盖上游脚本缺失。
out="$(cd "$T3" && bash ./install.sh --list --dest-dir "$T3" 2>&1)"
t3line="$(printf '%s\n' "$out" | grep '^pi-cache-stats')"
assert_contains "$t3line" "missing (upstream script not found; native Go active at $T3/cache-stats)" "list merges missing upstream + native Go"
out="$(cd "$T3" && bash ./install.sh --audit --dest-dir "$T3" 2>&1)"
t3line="$(printf '%s\n' "$out" | grep '^pi-cache-stats')"
assert_contains "$t3line" "MISMATCH — missing (upstream script not found; native Go active at $T3/cache-stats)" "audit MISMATCH for merged status"

# 无 go.mod 对照：federated 上游脚本缺失且无原生 Go → missing (upstream script not found)。
T3N="$TMP_ROOT/t3n"
mkdir -p "$T3N/federated"
sed 's|/root/workspace/pi-cache-stats/integrations/pi/install.sh|/nonexistent/install.sh|' "$REGISTRY" > "$T3N/federated/registry.json"
cp "$INSTALL" "$T3N/install.sh"
out="$(cd "$T3N" && bash ./install.sh --list --dest-dir "$T3N" 2>&1)"
t3nline="$(printf '%s\n' "$out" | grep '^pi-cache-stats')"
assert_contains "$t3nline" "missing (upstream script not found)" "list plain missing upstream"
assert_not_contains "$t3nline" "native Go active at" "list plain missing upstream no native dir"
out="$(cd "$T3N" && bash ./install.sh --audit --dest-dir "$T3N" 2>&1)"
t3nline="$(printf '%s\n' "$out" | grep '^pi-cache-stats')"
assert_contains "$t3nline" "MISMATCH — missing (upstream script not found)" "audit MISMATCH plain missing upstream"

# ========== 4) 对照组：悬空软链 / 无 go.mod 不触发 ==========
echo "== negative: dangling symlink / no go.mod"
T4="$TMP_ROOT/t4"
mkdir -p "$T4"
ln -s "$T4/nonexistent" "$T4/auto-continue"   # 悬空软链
out="$(bash "$INSTALL" --dest-dir "$T4" "$hosted_name" 2>&1)"
assert_contains "$out" "changed — installed" "dangling symlink → normal install"
assert_file_exists "$T4/auto-continue.ts" "dangling symlink → .ts installed"

T5="$TMP_ROOT/t5"
mkdir -p "$T5/auto-continue"                  # 目录但无 go.mod
out="$(bash "$INSTALL" --dest-dir "$T5" "$hosted_name" 2>&1)"
assert_contains "$out" "changed — installed" "dir without go.mod → normal install"

# ========== 5) dry-run 不删不调 ==========
echo "== dry-run"
T6="$TMP_ROOT/t6"
mkdir -p "$T6/go-auto-continue"
touch "$T6/go-auto-continue/go.mod"
ln -s "$T6/go-auto-continue" "$T6/auto-continue"
echo "stale" > "$T6/auto-continue.ts"
out="$(bash "$INSTALL" --dry-run --dest-dir "$T6" "$hosted_name" 2>&1)"
assert_contains "$out" "[dry-run]" "dry-run hosted message"
assert_contains "$out" "would remove stale" "dry-run hosted stale msg"
assert_file_exists "$T6/auto-continue.ts" "dry-run does NOT remove .ts"

T7="$TMP_ROOT/t7"
mkdir -p "$T7/go-codegraph-go"
touch "$T7/go-codegraph-go/go.mod"
ln -s "$T7/go-codegraph-go" "$T7/codegraph-go"
echo "stale" > "$T7/codegraph-go.ts"
spy7="$T7/spy.sh"
cat > "$spy7" <<EOF
#!/usr/bin/env bash
echo "SPY_CALLED" > "$T7/spy_called"
EOF
chmod +x "$spy7"
mkdir -p "$T7/federated"
jq --arg name "$fed_name" --arg spy "$spy7" \
  '(.extensions[] | select(.name==$name) | .authority.installScript) = $spy' \
  "$REGISTRY" > "$T7/federated/registry.json"
cp "$INSTALL" "$T7/install.sh"
out="$(cd "$T7" && bash ./install.sh --dry-run --dest-dir "$T7" "$fed_name" 2>&1)"
assert_contains "$out" "[dry-run]" "dry-run federated message"
assert_contains "$out" "would remove stale" "dry-run federated stale msg"
assert_file_exists "$T7/codegraph-go.ts" "dry-run federated does NOT remove .ts"
assert_file_not_exists "$T7/spy_called" "dry-run spy NOT called"

# ========== 6) --list / --audit NATIVE vs DRIFT ==========
echo "== list/audit native Go"
T8="$TMP_ROOT/t8"
mkdir -p "$T8/go-auto-continue"
touch "$T8/go-auto-continue/go.mod"
ln -s "$T8/go-auto-continue" "$T8/auto-continue"
out="$(bash "$INSTALL" --list --dest-dir "$T8" 2>&1)"
assert_contains "$out" "native Go active ($T8/auto-continue)" "list native Go active"

out="$(bash "$INSTALL" --audit --dest-dir "$T8" 2>&1)"
assert_contains "$out" "NATIVE — native Go active" "audit NATIVE status"

# DRIFT: 残留 .ts 且 md5 与源一致——audit 必须输出 MISMATCH（同 md5 也不得是 MATCH）。
src="$(jq -r --arg n "$hosted_name" '.extensions[] | select(.name==$n) | .authority.sourcePath' "$REGISTRY")"
cp "$src" "$T8/auto-continue.ts"
out="$(bash "$INSTALL" --audit --dest-dir "$T8" 2>&1)"
drift_line="$(printf '%s\n' "$out" | grep -F 'auto-continue ' || true)"
assert_contains "$drift_line" "MISMATCH — DRIFT (stale .ts alongside native Go ext" "audit MISMATCH — DRIFT even with same md5"
drift_bad="$(printf '%s\n' "$out" | grep -v 'MISMATCH — DRIFT' | grep 'MATCH — DRIFT' || true)"
assert_eq "" "$drift_bad" "audit never emits bare MATCH — DRIFT"

# ========== 7) NEVERWRITE 三层防线 ==========
echo "== NEVERWRITE"
T9="$TMP_ROOT/t9"
mkdir -p "$T9/herdr-agent-state"
touch "$T9/herdr-agent-state/go.mod"
echo "x" > "$T9/herdr-agent-state.ts"
rc=0
out="$(bash "$INSTALL" --dest-dir "$T9" herdr-agent-state 2>&1)" || rc=$?
assert_eq "1" "$rc" "explicit install external → exit 1"
assert_contains "$out" "refusing to install external/herdr-managed" "external refuse msg"
assert_file_exists "$T9/herdr-agent-state.ts" "NEVERWRITE file intact"
# 第 1 道防线补充：external 条目在 --all 枚举中本就不出现（installable_names 类别剔除）。
out="$(bash "$INSTALL" --dry-run --all --dest-dir "$T9" 2>&1)"
assert_not_contains "$out" "herdr-agent-state" "NEVERWRITE --all dry-run no external entry"

# NEVERWRITE 第 2/3 道防线隔离回归：把 herdr-agent-state 的 category 篡改为 hosted
# 并指向一个假源文件——绕过第 1 道防线（external 类别剔除）后，--all 枚举仍被第 2 道
# 防线 installable_names（destFile 口径过滤）剔除；显式单装仍被第 3 道防线
# refuse_if_neverwrite（写入层兜底）拦截。
T9B="$TMP_ROOT/t9b"
mkdir -p "$T9B/federated"
jq --arg fake "$T9B/fake-herdr-src.ts" '
  (.extensions[] | select(.name=="herdr-agent-state")) |=
    (.category = "hosted" | .authority = {kind:"in-repo", sourcePath:$fake, hostRepo:null, installScript:null, installDestEnv:null})
' "$REGISTRY" > "$T9B/federated/registry.json"
echo "fake herdr source" > "$T9B/fake-herdr-src.ts"
echo "original herdr dest content" > "$T9B/herdr-agent-state.ts"
cp "$INSTALL" "$T9B/install.sh"
# 篡改后该条目确为 hosted（第 1 道防线已无效），验证前置条件成立：
t9b_cat="$(jq -r '.extensions[] | select(.name=="herdr-agent-state") | .category' "$T9B/federated/registry.json")"
assert_eq "hosted" "$t9b_cat" "NEVERWRITE spoof: category tampered to hosted"
rc=0
out="$(cd "$T9B" && bash ./install.sh --all --dest-dir "$T9B" 2>&1)" || rc=$?
assert_eq "0" "$rc" "NEVERWRITE --all exits 0 under tampered registry"
assert_not_contains "$out" "herdr-agent-state" "NEVERWRITE --all installable_names filtered out"
assert_eq "original herdr dest content" "$(cat "$T9B/herdr-agent-state.ts")" "NEVERWRITE --all dest file intact"
rc=0
out="$(cd "$T9B" && bash ./install.sh --dest-dir "$T9B" herdr-agent-state 2>&1)" || rc=$?
assert_eq "1" "$rc" "NEVERWRITE explicit install exit 1 (refuse_if_neverwrite)"
assert_contains "$out" "refusing to write neverWrite target" "NEVERWRITE write-layer refuse msg"
assert_eq "original herdr dest content" "$(cat "$T9B/herdr-agent-state.ts")" "NEVERWRITE dest file intact after refuse"

# ========== 8) zero-node 宿主机真实 pig 扩展目录保护（隔离副本 + 双锚点 realpath -m）==========
# 在 $TMP_ROOT/t8_iso 内放置拷贝的 install.sh 与隔离 federated/registry.json：
# .defaultDestDir 指向 $FAKE_DEST（第二锚点，杜绝 env -u HOME 绕过且不硬编码 /root）；
# 全部 federated 的 installScript 改为写 spy_called 的本地探针（彻底杜绝触碰真实宿主仓）。
# 用 PI_EXT_TEST_PATH_NO_NODE=1 模拟无 node 环境（不改 PATH，保证 install.sh 子进程的
# md5sum/jq/realpath/find 等仍可用）。
echo "== zero-node protected dest (isolated copy + dual anchors)"

T8I="$TMP_ROOT/t8_iso"
FAKE_HOME="$T8I/fake_home"
FAKE_DEST="$FAKE_HOME/.pig/agent/extensions"
mkdir -p "$T8I/federated" "$FAKE_DEST"

cat > "$T8I/spy.sh" <<EOF
#!/usr/bin/env bash
echo "SPY_CALLED" > "$T8I/spy_called"
EOF
chmod +x "$T8I/spy.sh"

jq --arg dest "$FAKE_DEST" --arg spy "$T8I/spy.sh" '
  .defaultDestDir = $dest
  | (.extensions[] | select(.category=="federated") | .authority.installScript) = $spy
' "$REGISTRY" > "$T8I/federated/registry.json"
cp "$INSTALL" "$T8I/install.sh"
T8_INSTALL="$T8I/install.sh"

# ① --all：有 go.mod 的扩展走 native Go 跳过；无 go.mod 的 magpie-image 走 zero-node 跳过，
#    残留 magpie-image.ts 自愈删除；federated 无 go.mod 全部 zero-node 跳过（探针未触发）；
#    整体退出码 0。
mkdir -p "$FAKE_DEST/$hosted_name"
touch "$FAKE_DEST/$hosted_name/go.mod"
echo "stale" > "$FAKE_DEST/$magpie_dest"
rm -f "$T8I/spy_called"
rc=0
out="$(env PI_EXT_TEST_PATH_NO_NODE=1 HOME="$FAKE_HOME" bash "$T8_INSTALL" --all 2>&1)" || rc=$?
assert_eq "0" "$rc" "zero-node --all exits 0"
assert_contains "$out" "native Go extension active" "zero-node --all native Go skip"
assert_contains "$out" "$magpie_name (hosted): skipped — zero-node host" "zero-node --all magpie skipped"
assert_contains "$out" "removed stale $FAKE_DEST/$magpie_dest" "zero-node --all stale msg"
assert_contains "$out" "(federated): skipped — zero-node host" "zero-node --all federated skipped"
assert_file_not_exists "$FAKE_DEST/$magpie_dest" "zero-node --all stale .ts self-healed"
assert_file_not_exists "$T8I/spy_called" "zero-node --all spy NOT called"
assert_not_contains "$out" "install -m 644" "zero-node --all no TS install"

# ② 显式单装无原生 Go 的扩展 → 显式 FAILED 拒绝（exit 1），不静默跳过、不写 .ts。
#    双用例：hosted（magpie-image）+ federated（无 go.mod 的 codegraph-go，闭合 oracle F3）。
rc=0
out="$(env PI_EXT_TEST_PATH_NO_NODE=1 HOME="$FAKE_HOME" bash "$T8_INSTALL" "$magpie_name" 2>&1)" || rc=$?
assert_eq "1" "$rc" "zero-node explicit hosted install refused (exit 1)"
assert_contains "$out" "refusing to install TypeScript extension" "zero-node hosted refuse message"
assert_file_not_exists "$FAKE_DEST/$magpie_dest" "zero-node hosted refused install wrote nothing"

rm -f "$T8I/spy_called"
rc=0
out="$(env PI_EXT_TEST_PATH_NO_NODE=1 HOME="$FAKE_HOME" bash "$T8_INSTALL" "$fed_name" 2>&1)" || rc=$?
assert_eq "1" "$rc" "zero-node explicit federated install refused (exit 1)"
assert_contains "$out" "refusing to install TypeScript extension" "zero-node federated refuse message"
assert_file_not_exists "$T8I/spy_called" "zero-node federated spy NOT called"
assert_file_not_exists "$FAKE_DEST/$fed_dest" "zero-node federated refused install wrote nothing"

# ③ 4 类路径等价绕过攻击回归（reviewer #1 + oracle F1）：全部必须被拦截
#    （exit 1，零 .ts 落盘）——旧实现 cd+pwd -P / 词法比对 / ${HOME:-/root} 兜底下
#    均曾放行，realpath -m 双锚点规范化后全部受保护。
#
# ③a 尾斜杠 + 受保护目录尚不存在：--dest-dir "$FAKE_DEST_MISSING/"
#    （realpath -m 消除尾随 / 且支持末级目录尚不存在；旧实现目录不存在时回退词法
#    路径 '$FAKE_DEST/' != '$FAKE_DEST' → 放行漏洞）。
FAKE_DEST_MISSING="$FAKE_HOME/.pig/agent/extensions"
rmdir "$FAKE_DEST_MISSING" 2>/dev/null || true
rc=0
out="$(env PI_EXT_TEST_PATH_NO_NODE=1 HOME="$FAKE_HOME" bash "$T8_INSTALL" --dest-dir "$FAKE_DEST_MISSING/" "$magpie_name" 2>&1)" || rc=$?
assert_eq "1" "$rc" "zero-node trailing-slash on nonexistent protected dir → refused"
assert_contains "$out" "refusing to install TypeScript extension" "trailing-slash refuse msg"
assert_file_not_exists "$FAKE_DEST_MISSING/$magpie_dest" "trailing-slash wrote nothing"
mkdir -p "$FAKE_DEST_MISSING"
# 对照：非受保护目录 + 尾斜杠且尚不存在 → 不受限（暂存目录语义不误伤）。
FAKE_DEST_NEW="$FAKE_HOME/.pig/agent/extensions-new"
rc=0
out="$(env PI_EXT_TEST_PATH_NO_NODE=1 HOME="$FAKE_HOME" bash "$T8_INSTALL" --dest-dir "$FAKE_DEST_NEW/" "$magpie_name" 2>&1)" || rc=$?
assert_eq "0" "$rc" "zero-node trailing-slash nonexistent staging dir → allowed"
assert_file_exists "$FAKE_DEST_NEW/$magpie_dest" "staging dir trailing-slash installs .ts"

# ③b 路径含 '..'：--dest-dir "$FAKE_HOME/.pig/agent/../agent/extensions"（规范化后 = 受保护目录）
rc=0
out="$(env PI_EXT_TEST_PATH_NO_NODE=1 HOME="$FAKE_HOME" bash "$T8_INSTALL" --dest-dir "$FAKE_HOME/.pig/agent/../agent/extensions" "$magpie_name" 2>&1)" || rc=$?
assert_eq "1" "$rc" "zero-node dotdot path on protected dir → refused"
assert_contains "$out" "refusing to install TypeScript extension" "dotdot refuse msg"
assert_file_not_exists "$FAKE_DEST/$magpie_dest" "dotdot wrote nothing"

# ③c HOME 为软链：HOME="$TMP_ROOT/home_symlink"（→ $FAKE_HOME），--dest-dir "$FAKE_DEST"
#    （规范化前 dest_norm=$FAKE_DEST、home_pig=$TMP_ROOT/home_symlink/.pig/... 词法不等；
#    realpath -m 解析前缀软链后二者相等 → 受保护）。
ln -s "$FAKE_HOME" "$TMP_ROOT/home_symlink"
rc=0
out="$(env PI_EXT_TEST_PATH_NO_NODE=1 HOME="$TMP_ROOT/home_symlink" bash "$T8_INSTALL" --dest-dir "$FAKE_DEST" "$magpie_name" 2>&1)" || rc=$?
assert_eq "1" "$rc" "zero-node symlinked HOME → refused"
assert_contains "$out" "refusing to install TypeScript extension" "symlinked HOME refuse msg"
assert_file_not_exists "$FAKE_DEST/$magpie_dest" "symlinked HOME wrote nothing"

# ③d env -u HOME：--dest-dir "$FAKE_DEST/"（$HOME 缺失时跳过 HOME 锚点，命中隔离
#    registry 的 .defaultDestDir=$FAKE_DEST 锚点 → 受保护；旧实现 ${HOME:-/root} 硬编码
#    回退对隔离 registry 路径无法匹配 → 放行漏洞）。
rc=0
out="$(env -u HOME PI_EXT_TEST_PATH_NO_NODE=1 bash "$T8_INSTALL" --dest-dir "$FAKE_DEST/" "$magpie_name" 2>&1)" || rc=$?
assert_eq "1" "$rc" "zero-node env -u HOME via defaultDestDir anchor → refused"
assert_contains "$out" "refusing to install TypeScript extension" "env -u HOME refuse msg"
assert_file_not_exists "$FAKE_DEST/$magpie_dest" "env -u HOME wrote nothing"

# ③e PI_EXT_DEST_DIR 环境变量入口（非 --dest-dir CLI 参数）：env 指向受保护目录同样被拦
#    （闭合 oracle R2：EXPLICIT_DEST_DIR=1 由非空 PI_EXT_DEST_DIR 触发，经 realpath -m 命中
#    $HOME/.pig/... 锚点 → 受保护）。
rc=0
out="$(env PI_EXT_TEST_PATH_NO_NODE=1 PI_EXT_DEST_DIR="$FAKE_DEST/" HOME="$FAKE_HOME" bash "$T8_INSTALL" "$magpie_name" 2>&1)" || rc=$?
assert_eq "1" "$rc" "zero-node PI_EXT_DEST_DIR env entry → refused"
assert_contains "$out" "refusing to install TypeScript extension" "PI_EXT_DEST_DIR env refuse msg"
assert_file_not_exists "$FAKE_DEST/$magpie_dest" "PI_EXT_DEST_DIR env wrote nothing"

# ④ 放行对照：PI_EXT_ALLOW_ZERO_NODE_TS=1 逃生门放行 + 显式 --dest-dir 指向非 pig 目录
#    不受 zero-node 保护限制（离线打包/单测照常）。
rm -f "$FAKE_DEST/$magpie_dest"
rc=0
out="$(env PI_EXT_TEST_PATH_NO_NODE=1 PI_EXT_ALLOW_ZERO_NODE_TS=1 HOME="$FAKE_HOME" bash "$T8_INSTALL" "$magpie_name" 2>&1)" || rc=$?
assert_eq "0" "$rc" "zero-node override install exits 0"
assert_file_exists "$FAKE_DEST/$magpie_dest" "zero-node override installs .ts"

T8E="$TMP_ROOT/t8e"
mkdir -p "$T8E"
rc=0
out="$(env PI_EXT_TEST_PATH_NO_NODE=1 bash "$T8_INSTALL" --dest-dir "$T8E" "$magpie_name" 2>&1)" || rc=$?
assert_eq "0" "$rc" "zero-node explicit --dest-dir other dir exits 0"
assert_file_exists "$T8E/$magpie_dest" "zero-node explicit --dest-dir other dir installs .ts"

# 有 node 时不受限（PI_EXT_TEST_PATH_NO_NODE 未设且本机有 node 才断言；
# 本机无 node 时跳过此正向对照，仅记录）。
if command -v node >/dev/null 2>&1; then
  rm -f "$FAKE_DEST/$magpie_dest"
  rc=0
  out="$(env HOME="$FAKE_HOME" bash "$T8_INSTALL" "$magpie_name" 2>&1)" || rc=$?
  assert_eq "0" "$rc" "node present → install allowed"
  assert_file_exists "$FAKE_DEST/$magpie_dest" "node present → .ts installed"
else
  echo "  SKIP: node present → install allowed (no node on this host)"
fi

# ========== 汇总 ==========
echo ""
echo "=================================================="
echo "PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf '%s\n' "${TEST_NAMES[@]}" | grep '^FAIL:' || true
  exit 1
fi
exit 0
