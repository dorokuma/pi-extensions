#!/usr/bin/env bash
# install.sh — 联邦式 Pi 扩展分发中枢的统一安装/巡检入口。
#
# 数据源：federated/registry.json（9 个扩展的类别与绝对权威路径）。本仓是 5 个
# 「hosted」内生扩展的唯一权威源，逐字节同步到部署目录；对 3 个「federated」外生
# 扩展只以子进程级联调用其宿主仓的 install.sh（源留在宿主仓，改动回宿主仓走既有
# 流程）；对 1 个「external」项 herdr-agent-state.ts 只读巡检、本仓绝不写它。
#
# 契约与样板 /root/workspace/codegraph-go/integrations/pi/install.sh 对齐：
#   - set -euo pipefail；command -v md5sum 预检（缺失即显式 FAILED，绝不静默）。
#   - md5 幂等：目标与源一致 → 报 unchanged 且不重写、不动 mtime。
#   - 源缺失立即失败；目标父目录 mkdir -p；install -m 644。
#   - DEST 非绝对路径 / DEST 指向已存在目录 → 显式 FAILED、非零退出、消息走 stderr。
#   - 实质改动后提示 Pi 需 /reload 或新会话才生效。
#
# 用法：
#   install.sh --list                 # 列出 9 个扩展：名称/类别/权威源/目标/当前状态
#   install.sh --audit                # 只读巡检全机 9 个：md5/权限/外生脚本/ herdr 只读态
#   install.sh --dry-run <names...>   # 只演练，不写任何文件
#   install.sh <names...>             # 安装指定扩展（hosted 直拷；federated 级联宿主 install.sh）
#   install.sh --all                  # 安装全部 hosted+federated（绝不写 herdr-agent-state.ts）
#   install.sh --dest-dir <DIR> ...   # 覆盖目标目录；默认 $PI_EXT_DEST_DIR 或 ~/.pi/agent/extensions
#
# 铁律相关（详见 AGENTS.md）：
#   - --all / 位置参数都绝不可能写 herdr-agent-state.ts：external 类别剔除 + NEVERWRITE 硬白名单
#     （安装枚举层按 destFile 口径过滤）+ refuse_if_neverwrite 写入层兜底，三重防护。
#   - 写 herdsman-bridge 前读 settings.json 的 packages，若已含同名 npm 包则显式报错退出（防双实例）；
#     settings.json 存在但非法 JSON / packages 非数组时 fail-closed（显式 FAILED、exit 1，宁可拦住
#     也不放行）；缺失则 note + 放行。读取路径可用 $PI_EXT_SETTINGS_JSON 覆盖（测试/演练用，README 已登记）。
set -euo pipefail

PROG="install.sh"
# 只用内建求仓库根，受限 PATH 下也能先跑到 md5sum 预检再报错（而非被外部 dirname 绊倒）。
_src="${BASH_SOURCE[0]}"
_dir="${_src%/*}"; [ "$_dir" = "$_src" ] && _dir="."
REPO_ROOT="$(cd "$_dir" && pwd -P)"
REGISTRY="$REPO_ROOT/federated/registry.json"

# 硬编码禁写白名单：与 registry.neverWrite 一致，双保险。herdr-agent-state.ts
# 由 herdr 二进制托管，本仓任何路径（包括 --all）都绝不写它。
NEVERWRITE=(herdr-agent-state.ts)

WARNS=()
fail() { echo "$PROG FAILED: $*" >&2; exit 1; }
warn() { echo "$PROG WARN: $*" >&2; WARNS+=("$*"); }

usage() {
  cat <<'EOF'
install.sh — 联邦式 Pi 扩展统一安装/巡检入口

Usage:
  install.sh --list                list 9 extensions (name/category/authority/target/status)
  install.sh --audit               read-only inspect all 9 (md5/perms/upstream scripts/herdr)
  install.sh --dry-run <names...>  rehearse only; write nothing
  install.sh <names...>            install named extensions (hosted copy | federated cascade)
  install.sh --all                 install all hosted+federated (never writes herdr-agent-state.ts)
  install.sh --dest-dir <DIR> ...  override target dir; defaults to $PI_EXT_DEST_DIR or ~/.pi/agent/extensions
  install.sh -h | --help           this help

Env:
  PI_EXT_DEST_DIR        default target dir (CLI --dest-dir wins over it)
  PI_EXT_SETTINGS_JSON   test/rehearsal-only override for the herdsman double-load guard's
                         settings.json (documented in README; normally leave unset)

Data source: federated/registry.json. This repo NEVER writes herdr-agent-state.ts.
EOF
}

# ---- 预检：md5sum 缺了必须显式失败（对齐样板：曾经的静默 exit 127 / 输出全空形态）----
command -v md5sum >/dev/null 2>&1 || fail "md5sum not found in PATH (PATH=$PATH) — install.sh requires GNU md5sum"
command -v jq    >/dev/null 2>&1 || fail "jq not found in PATH (PATH=$PATH) — install.sh reads federated/registry.json via jq"
[ -f "$REGISTRY" ] || fail "registry not found: $REGISTRY"
jq -e . "$REGISTRY" >/dev/null 2>&1 || fail "registry is not valid JSON: $REGISTRY"

md5_of() {
  local out rc=0
  out="$(md5sum -- "$1")" || rc=$?
  [ "$rc" -eq 0 ] || return 1
  [ -n "$out" ] || return 1
  printf '%s' "${out%% *}"
}

# ---- 注册表读取助手 ----
extension_exists() { jq -e --arg n "$1" '.extensions[] | select(.name==$n)' "$REGISTRY" >/dev/null 2>&1; }
regq() { jq -r --arg n "$1" ".extensions[] | select(.name==\$n) | ${2} // \"\"" "$REGISTRY"; }
all_names() { jq -r '.extensions[].name' "$REGISTRY"; }
# --all 目标 = 非 external（类别剔除 herdr）。NEVERWRITE 记的是 destFile（如
# herdr-agent-state.ts），故这里必须按 destFile 口径与 .destFile 比对（早期版本比对的是
# .name，口径不一致 → 永不匹配、这一层形同虚设的死代码，已修）。
#
# 三条防线各自的作用与生效路径（互不替代）：
#   ① external 类别剔除（本函数 + install 模式显式请求分支）：registry 里 category=external
#      的项根本不进安装名单；显式 `install.sh herdr-agent-state` 也在 install 分支直接 FAILED。
#   ② NEVERWRITE destFile 口径过滤（本函数）：即使未来有人把 herdr-agent-state 的 category
#      改成非 external，--all 的枚举层也会把它滤掉——这才是硬白名单的第一道生效路径。
#   ③ refuse_if_neverwrite（写入层兜底）：hosted 直拷与 federated 级联两条写路径在真正写
#      文件前都调用它，按 destFile 全等比对硬白名单，命中即 FAILED。
installable_names() {
  local nw_joined
  nw_joined="$(printf '%s\n' "${NEVERWRITE[@]}")"
  jq -r --arg nw "$nw_joined" '
    ($nw | split("\n") | map(select(length > 0))) as $deny
    | .extensions[]
    | select((.category // "hosted") != "external")
    | select((.destFile // "") as $df | ($deny | index($df)) == null)
    | .name
  ' "$REGISTRY"
}

refuse_if_neverwrite() { # $1=destFile
  local f="$1" nw
  for nw in "${NEVERWRITE[@]}"; do
    if [ "$f" = "$nw" ]; then
      fail "refusing to write neverWrite target '$f' — managed by herdr binary out of band; this repo NEVER writes it"
    fi
  done
  return 0
}

# ---- 目标目录解析：CLI --dest-dir > $PI_EXT_DEST_DIR > ~/.pi/agent/extensions ----
DEST_DIR="${PI_EXT_DEST_DIR:-}"
DEST_DIR_SET=0
MODE="install"       # install | list | audit | all
DRY_RUN=0
REQUESTED=()

while [ $# -gt 0 ]; do
  case "$1" in
    --list)    MODE="list"; shift ;;
    --audit)   MODE="audit"; shift ;;
    --all)     MODE="all"; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --dest-dir)
      [ $# -ge 2 ] || fail "--dest-dir requires an argument"
      DEST_DIR="$2"; DEST_DIR_SET=1; shift 2 ;;
    --dest-dir=*)
      DEST_DIR="${1#*=}"; DEST_DIR_SET=1; shift ;;
    -h|--help) usage; exit 0 ;;
    --*)       fail "unknown option: $1 (see --help)" ;;
    *)         REQUESTED+=("$1"); shift ;;
  esac
done

if [ "$DEST_DIR_SET" -eq 1 ] && [ -z "$DEST_DIR" ]; then
  fail "--dest-dir was given but is empty"
fi
if [ -z "$DEST_DIR" ]; then
  HOME_DIR="${HOME:-}"
  [ -n "$HOME_DIR" ] || fail "HOME is not set (or empty) — export HOME or set --dest-dir / PI_EXT_DEST_DIR"
  DEST_DIR="$HOME_DIR/.pi/agent/extensions"
fi
case "$DEST_DIR" in
  /*) ;;
  *) fail "dest-dir is not an absolute path: $DEST_DIR" ;;
esac
# 若 DEST_DIR 已存在但为普通文件，则无法作为目录（mkdir -p 会失败）——显式报错更清晰。
if [ -e "$DEST_DIR" ] && [ ! -d "$DEST_DIR" ]; then
  fail "dest-dir exists but is not a directory: $DEST_DIR"
fi

# DEST 文件级校验：非绝对 / 已存在目录 → 显式 FAILED（对齐样板 DEST 语义）。
validate_target_file() { # $1 = full file path
  local dest="$1"
  [ -n "$dest" ] || fail "destination is empty"
  case "$dest" in /*) ;; *) fail "destination is not an absolute path: $dest" ;; esac
  if [ -d "$dest" ]; then
    fail "destination is a directory: $dest — expected a file path (<destDir>/<ext>.ts), not a directory"
  fi
  return 0
}

# ---- herdsman 双加载防护（fail-closed）----
# settings 解析优先级（显式覆盖优先，均为「测试/演练用覆盖点」语义，见 README 环境变量表）：
#   1) $PI_EXT_SETTINGS_JSON    —— 显式环境变量覆盖（演练/测试指哪读哪）
#   2) registry .doubleLoadGuard.settingsFile —— 默认即真实 /root/.pi/agent/settings.json
#   3) dirname($DEST_DIR)/settings.json     —— 最后回落（registry 未配 settingsFile 时）
# 实际保护范围（如实披露，见 README）：仅当读到真实 /root/.pi/agent/settings.json 时才对
# 真实 npm 双装载生效；显式 --dest-dir 的演练场景回落到该目录旁的 settings.json，通常不存在。
# 判定口径（fail-closed，宁可拦住也不放行）：
#   - settings.json 缺失      → note + 放行（无 npm 包可装，维持既有行为）
#   - 存在但非法 JSON        → 显式 FAILED、exit 1（无法证明没有 herdsman-pi npm 包）
#   - .packages 非数组       → 显式 FAILED、exit 1（同上，含 {} / 字符串 / 数字等类型）
#   - .packages 缺失或 null  → 等价于 []，放行
guard_herdsman_doubleload() { # $1 = name
  local name="$1" settings packages_json
  settings="${PI_EXT_SETTINGS_JSON:-}"
  if [ -z "$settings" ]; then settings="$(regq "$name" '.doubleLoadGuard.settingsFile // ""')"; fi
  if [ -z "$settings" ]; then settings="$(dirname "$DEST_DIR")/settings.json"; fi
  if [ ! -f "$settings" ]; then
    echo "$PROG note: double-load guard settings.json not found at $settings — skipping (no npm package can be present)"
    return 0
  fi
  # fail-closed ①：文件存在但 jq 解析失败（旧写法把它吞成 '[]' 静默放行）→ 显式失败。
  if ! packages_json="$(jq -c '.packages // []' "$settings" 2>/dev/null)"; then
    fail "double-load guard: settings.json exists but is NOT valid JSON: $settings — cannot verify absence of the herdsman-pi npm package. Refusing to install herdsman-bridge (fail-closed). Fix the file, or point PI_EXT_SETTINGS_JSON at a valid settings file (run 'jq . \"$settings\"' to see the parse error)"
  fi
  # fail-closed ②：.packages 必须是数组（旧写法让对象/字符串等非数组类型穿透检测）。
  if ! printf '%s' "$packages_json" | jq -e 'type == "array"' >/dev/null 2>&1; then
    fail "double-load guard: $settings '.packages' is NOT an array (got: $packages_json) — cannot verify absence of the herdsman-pi npm package. Refusing to install herdsman-bridge (fail-closed). Fix the file, or point PI_EXT_SETTINGS_JSON at a valid settings file"
  fi
  if printf '%s' "$packages_json" | jq -e 'type=="array" and (any(.[]; type=="string" and test("herdsman-pi"; "i")))' >/dev/null 2>&1; then
    fail "herdsman double-load guard: $settings 'packages' already loads a herdsman-pi npm package — installing the bridge too would create TWO instances (double daemon client / duplicate registrations). Remove the npm package first (packages=$packages_json)"
  fi
  echo "$PROG note: double-load guard passed (no herdsman-pi npm package in $settings.packages)"
}

# ---- 状态判定（list/audit 共用，只读）----
# 打印：<status>\t<src_md5>\t<dest_md5>\t<dest_perms>
status_of() { # $1=name  $2=dest_dir
  local name="$1" ddir="$2" cat src dest smd5="" dmd5="(none)" perms="(n/a)" status
  cat="$(regq "$name" '.category')"
  src="$(regq "$name" '.authority.sourcePath // ""')"
  dest="$ddir/$(regq "$name" '.destFile')"
  local ext_exist="present"
  if [ "$cat" = "federated" ]; then
    local script; script="$(regq "$name" '.authority.installScript // ""')"
    if [ -z "$script" ] || [ ! -e "$script" ]; then ext_exist="missing"; fi
  fi
  if [ -n "$src" ] && md5_of "$src" >/dev/null 2>&1; then smd5="$(md5_of "$src")" || smd5=""; fi
  if [ -f "$dest" ]; then
    if md5_of "$dest" >/dev/null 2>&1; then dmd5="$(md5_of "$dest")" || dmd5="(err)"; fi
    perms="$(stat -c '%a' "$dest" 2>/dev/null || echo '(err)')"
  fi
  if [ "$cat" = "external" ] || [ -z "$src" ]; then
    if [ -f "$dest" ]; then status="read-only (herdr-managed)"; else status="absent (herdr-managed)"; fi
  elif [ -z "$smd5" ]; then
    status="source missing: $src"
  elif [ ! -f "$dest" ]; then
    status="not installed"
  elif [ "$smd5" = "$dmd5" ]; then
    status="unchanged"
  else
    status="DRIFT"
  fi
  if [ "$cat" = "federated" ] && [ "$ext_exist" = "missing" ]; then
    status="missing (upstream script not found)"
  fi
  printf '%s\t%s\t%s\t%s\n' "$status" "${smd5:---}" "${dmd5}" "$perms"
}

# ---- hosted 安装（逐字节、md5 幂等）----
install_hosted() { # $1=name
  local name="$1" src dest smd5
  src="$(regq "$name" '.authority.sourcePath // ""')"
  [ -n "$src" ] || fail "hosted extension '$name' has no authority.sourcePath in registry"
  [ -f "$src" ] || fail "source not found: $src"
  dest="$DEST_DIR/$(regq "$name" '.destFile')"
  validate_target_file "$dest"
  refuse_if_neverwrite "$(regq "$name" '.destFile')"
  smd5="$(md5_of "$src")" || fail "md5sum failed for source: $src"
  if [ "$name" = "herdsman-bridge" ]; then guard_herdsman_doubleload "$name"; fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] $name (hosted): would install -m 644 $src -> $dest (source md5 $smd5)"
    return 0
  fi
  if [ -f "$dest" ] && [ "$(md5_of "$dest")" = "$smd5" ]; then
    echo "$name (hosted): unchanged — destination already matches source (md5 $smd5); nothing written"
    return 0
  fi
  mkdir -p "$(dirname "$dest")"
  install -m 644 "$src" "$dest"
  echo "$name (hosted): changed — installed $dest (md5 $smd5)"
  CHANGED_ANY=1
}

# ---- federated 安装（级联宿主仓 install.sh；缺失/失败都容忍并记 WARN）----
install_federated() { # $1=name
  local name="$1" script envname dest out rc
  script="$(regq "$name" '.authority.installScript // ""')"
  envname="$(regq "$name" '.authority.installDestEnv // ""')"
  dest="$DEST_DIR/$(regq "$name" '.destFile')"
  validate_target_file "$dest"
  refuse_if_neverwrite "$(regq "$name" '.destFile')"
  if [ -z "$script" ] || [ ! -e "$script" ]; then
    warn "$name: missing (upstream script not found): ${script:-<none in registry>} — cannot cascade; edit the host repo instead"
    return 0
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] $name (federated): would cascade '$envname'=$dest bash $script"
    return 0
  fi
  echo "$name (federated): cascading host repo install.sh: $envname=$dest bash $script"
  rc=0
  out="$(env "$envname=$dest" bash "$script" 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s\n' "$out" | sed 's/^/    /'
    if printf '%s' "$out" | grep -q 'changed:'; then CHANGED_ANY=1; FEDERATED_TOUCHED=1; fi
  else
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    warn "$name: upstream install.sh failed (exit $rc): $script — host repo install did NOT complete; reconcile in the host repo"
    FEDERATED_TOUCHED=1
  fi
}

# ================= 模式分派 =================
CHANGED_ANY=0
FEDERATED_TOUCHED=0

case "$MODE" in
  list)
    echo "federated pi-extensions registry: $REGISTRY"
    echo "dest-dir: $DEST_DIR"
    printf '%-17s %-9s %-15s %-44s %s\n' "NAME" "CATEGORY" "KIND" "AUTHORITY-SOURCE" "TARGET (status)"
    echo "-------------------------------------------------------------------------------------------------------------------------------"
    while IFS= read -r nm; do
      [ -n "$nm" ] || continue
      cat="$(regq "$nm" '.category')"
      kind="$(regq "$nm" '.authority.kind // ""')"
      src="$(regq "$nm" '.authority.sourcePath // ""')"
      [ -z "$src" ] && src="(herdr binary — out of band)"
      dest="$DEST_DIR/$(regq "$nm" '.destFile')"
      st="$(status_of "$nm" "$DEST_DIR")"; st="${st%%$'\t'*}"
      printf '%-17s %-9s %-15s %-44s %s [%s]\n' "$nm" "$cat" "$kind" "$src" "$dest" "$st"
    done < <(all_names)
    echo "note: herdr-agent-state.ts is external — never written by this repo (read-only)."
    exit 0
    ;;

  audit)
    echo "$PROG audit (READ-ONLY) — $(date '+%Y-%m-%d %H:%M:%S %z')"
    echo "registry: $REGISTRY"
    echo "dest-dir: $DEST_DIR"
    echo "-------------------------------------------------------------------------------------------------------------------------------"
    printf '%-17s %-9s %-34s %-5s %s\n' "NAME" "CATEGORY" "DEST_MD5" "PERM" "AUDIT"
    while IFS= read -r nm; do
      [ -n "$nm" ] || continue
      cat="$(regq "$nm" '.category')"
      line="$(status_of "$nm" "$DEST_DIR")"
      st="${line%%$'\t'*}"; rest="${line#*$'\t'}"; smd5="${rest%%$'\t'*}"; rest2="${rest#*$'\t'}"; dmd5="${rest2%%$'\t'*}"; perms="${rest2#*$'\t'}"
      match="MISMATCH"; if [ -n "$smd5" ] && [ "$smd5" = "$dmd5" ]; then match="MATCH"; fi
      if [ "$cat" = "external" ] || [ -z "$smd5" ]; then match="----"; fi
      printf '%-17s %-9s %-34s %-5s %s\n' "$nm" "$cat" "$dmd5" "$perms" "$match — $st"
      if [ "$cat" = "federated" ]; then
        script="$(regq "$nm" '.authority.installScript // ""')"
        if [ -z "$script" ] || [ ! -e "$script" ]; then
          echo "    └─ federated upstream install.sh: MISSING ($script)"
        else
          echo "    └─ federated upstream install.sh: present ($script)"
        fi
      fi
      if [ "$cat" = "external" ]; then
        echo "    └─ external/herdr-managed: READ-ONLY — this repo will never modify it; governed by the herdr binary"
      fi
    done < <(all_names)
    echo "-------------------------------------------------------------------------------------------------------------------------------"
    echo "audit complete: no files written (this mode is strictly read-only)."
    exit 0
    ;;

  all)
    while IFS= read -r nm; do
      [ -n "$nm" ] || continue
      cat="$(regq "$nm" '.category')"
      if [ "$cat" = "federated" ]; then install_federated "$nm"; else install_hosted "$nm"; fi
    done < <(installable_names)
    ;;

  install)
    [ "${#REQUESTED[@]}" -gt 0 ] || { echo "$PROG: no action given. Try --list, --audit, --all, or one/more extension names." >&2; usage; exit 2; }
    for nm in "${REQUESTED[@]}"; do
      extension_exists "$nm" || fail "unknown extension: $nm (see --list)"
      cat="$(regq "$nm" '.category')"
      if [ "$cat" = "external" ]; then
        fail "refusing to install external/herdr-managed extension: $nm — this repo NEVER writes it; reconcile via the herdr binary"
      fi
      if [ "$cat" = "federated" ]; then install_federated "$nm"; else install_hosted "$nm"; fi
    done
    ;;
esac

# ---- 收尾：提示生效条件 + 外部失败 WARN 汇总（务必是脚本最后的输出）----
if [ "$CHANGED_ANY" -eq 1 ] || [ "$FEDERATED_TOUCHED" -eq 1 ]; then
  echo "note: Pi only loads extensions at session start — run /reload or start a new session for changes to take effect."
fi

if [ "${#WARNS[@]}" -gt 0 ]; then
  echo "================= $PROG WARN SUMMARY (${#WARNS[@]}) ================="
  for w in "${WARNS[@]}"; do echo "WARN: $w"; done
  echo "note: tolerated non-fatal issues above did NOT abort the run; reconcile them in the respective host repo."
fi

exit 0
