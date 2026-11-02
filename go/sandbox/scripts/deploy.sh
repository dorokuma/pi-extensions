#!/usr/bin/env bash
# deploy.sh — go/sandbox (sbx) 受控发布脚本
#
# 用途：把当前仓库（仅限 main 分支、干净工作树、非 worktree）编译出的 sbx
# 二进制安全地部署到 ${SBX_DEPLOY_TARGET:-/root/.local/bin/sbx}，并通过
# chattr +i 内核级 immutable 锁防止误写。
#
# 环境变量：
#   SBX_DEPLOY_TARGET            部署目标路径（默认 /root/.local/bin/sbx）；
#                                必须指向文件路径（首次安装尚不存在，或升级
#                                覆盖已存在普通文件），空值、以 / 结尾或已
#                                存在目录（含指向目录的软链）一律拒绝；
#                                父目录仅在三道硬门禁、版本
#                                提取与 go vet/go test 预检全部通过后才
#                                mkdir -p（门禁前零文件系统副作用）。
#   SBX_DEPLOY_SKIP_IMMUTABLE    =1 跳过 chattr +i（仅供无 CAP_LINUX_IMMUTABLE
#                                的单测/临时目录使用；生产必须为 0/未设置）
#   SBX_DEPLOY_FAST_VERIFY       =1 跳过 go test -race 预检与 reviewer-agent
#                                冒烟（仍执行 go vet -p 2；仅供 go test 内调用
#                                deploy.sh 防止递归套娃 go test；生产必须为
#                                0/未设置）
#   SBX_DEPLOY_ALLOW_NO_LANDLOCK =1 允许 Linux 下 --check 未确认 Landlock 时
#                                降级为 WARN 放行（仅供无 Landlock 内核的特殊
#                                测试环境；默认硬阻断，生产必须为 0/未设置）

set -euo pipefail

# ---------------------------------------------------------------- 配置

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd -P)"
MODULE_DIR="$(cd -- "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd -P)"   # go/sandbox

TARGET="${SBX_DEPLOY_TARGET:-/root/.local/bin/sbx}"
SKIP_IMMUTABLE="${SBX_DEPLOY_SKIP_IMMUTABLE:-0}"
FAST_VERIFY="${SBX_DEPLOY_FAST_VERIFY:-0}"
ALLOW_NO_LANDLOCK="${SBX_DEPLOY_ALLOW_NO_LANDLOCK:-0}"

die() { echo "deploy.sh: ERROR: $*" >&2; exit 1; }
info() { echo "deploy.sh: $*" >&2; }

# ---------------------------------------------------------------- 调用方 cwd 拦截
# 在任何 cd / 子 shell 之前检查调用方原始物理目录：禁止从 .wt 工作树目录
# 直接调主仓 deploy.sh（.wt 工作树只用于开发，发布必须从主仓 main 发起）。
# 同时匹配顶层 .wt 本身（*"/.wt"）与其下任意路径（*"/.wt/"*），避免
# cwd 恰好等于 <repo>/.wt 时漏网。

CALLER_PWD="$(pwd -P)"
if [[ "$CALLER_PWD" == *"/.wt/"* || "$CALLER_PWD" == *"/.wt" ]]; then
    die "禁止从 .wt 工作树目录发起发布: $CALLER_PWD"
fi

# ---------------------------------------------------------------- TARGET 形状校验（内存操作，零文件系统副作用）
# 先拒绝空值与以 / 结尾的目录型路径；相对路径仅在内存中拼接为绝对路径
# （严禁在三道 Git 硬门禁通过前 mkdir -p 或做其他任何文件系统写操作）；
# 已存在目录（含指向目录的软链）立即拒绝——SBX_DEPLOY_TARGET 必须指向
# 文件路径（首次安装尚不存在，或升级覆盖已存在普通文件），空值、
# 以 / 结尾或已存在目录（含指向目录的软链）一律拒绝。

[[ -n "$TARGET" && "$TARGET" != */ ]] \
    || die "SBX_DEPLOY_TARGET 不能为空或以 / 结尾的目录路径: $TARGET"
if [[ "$TARGET" != /* ]]; then
    TARGET="${CALLER_PWD}/${TARGET}"
fi
if [[ -d "$TARGET" ]]; then
    die "部署目标不能是已存在目录: $TARGET"
fi
TARGET_DIR="$(dirname -- "$TARGET")"

# ---------------------------------------------------------------- GIT_* 环境隔离
# 外部注入的 GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE / GIT_COMMON_DIR 等变量
# 会把下方 git 查询指向主仓，伪造「主仓 main 干净」假象从而绕过 worktree 门禁
# 并从 worktree 编译代码。先统一剥掉所有 git 本地环境变量再做任何 git 查询。

for _gv in $(git rev-parse --local-env-vars 2>/dev/null || true); do
    unset "$_gv"
done
unset _gv

# ---------------------------------------------------------------- 硬门禁 1
# 禁止在 git worktree（链接工作树）内发布：--git-dir 与 --git-common-dir 的
# realpath 必须相等（主仓内二者同一 .git 目录），且 top-level 路径不得以
# /.wt 结尾或含 /.wt/。

GIT_DIR="$(git -C "$MODULE_DIR" rev-parse --absolute-git-dir)" \
    || die "无法解析 git-dir（$MODULE_DIR 不是 Git 仓库？）"
# --path-format=absolute 强制 common-dir 返回绝对路径（避免相对解析依赖 cwd）。
GIT_COMMON_DIR="$(git -C "$MODULE_DIR" rev-parse --path-format=absolute --git-common-dir)" \
    || die "无法解析 git-common-dir"
TOPLEVEL="$(git -C "$MODULE_DIR" rev-parse --path-format=absolute --show-toplevel)" \
    || die "无法解析 show-toplevel"

GIT_DIR_REAL="$(realpath "$GIT_DIR")" || die "realpath git-dir 失败: $GIT_DIR"
GIT_COMMON_REAL="$(realpath "$GIT_COMMON_DIR")" || die "realpath git-common-dir 失败: $GIT_COMMON_DIR"
TOPLEVEL_REAL="$(cd "$TOPLEVEL" && pwd -P)" || die "realpath toplevel 失败: $TOPLEVEL"
MODULE_REAL="$(cd "$MODULE_DIR" && pwd -P)" || die "realpath module 失败: $MODULE_DIR"

# 模块路径归属强校验：即使 GIT_* 变量曾把 git 查询指向主仓，MODULE_DIR 的
# 物理路径也必须严格位于该 toplevel 的 go/sandbox 下，否则拒绝编译发布。
if [[ "$MODULE_REAL" != "${TOPLEVEL_REAL}/go/sandbox" ]]; then
    die "模块路径异常: $MODULE_REAL 不归属 $TOPLEVEL_REAL/go/sandbox"
fi

if [[ "$GIT_DIR_REAL" != "$GIT_COMMON_REAL" ]]; then
    die "禁止从 git worktree 发布（git-dir=$GIT_DIR_REAL != git-common-dir=$GIT_COMMON_REAL）。请在主仓 main 分支执行。"
fi
if [[ "$TOPLEVEL_REAL" == *"/.wt/"* || "$TOPLEVEL_REAL" == *"/.wt" ]]; then
    die "禁止从 .wt worktree 发布（toplevel=$TOPLEVEL_REAL）。请在主仓 main 分支执行。"
fi
REPO_ROOT="$TOPLEVEL_REAL"

# ---------------------------------------------------------------- 硬门禁 2
# 禁止在非 main 分支发布。

BRANCH="$(git -C "$REPO_ROOT" symbolic-ref --short HEAD 2>/dev/null || true)"
if [[ "$BRANCH" != "main" ]]; then
    die "禁止在非 main 分支发布（当前分支: ${BRANCH:-<detached>}）。请切换到 main。"
fi

# ---------------------------------------------------------------- 硬门禁 3
# 禁止在 dirty 工作树发布（含未跟踪文件）。独立命令替换保留 git status 的
# 非零退出（if [[ -n "$(...)" ]] 会把命令失败吞成空串判为干净）。

STATUS_OUT="$(git -C "$REPO_ROOT" status --porcelain --untracked-files=normal)" \
    || die "git status 检查失败"
if [[ -n "$STATUS_OUT" ]]; then
    die "禁止在 dirty 工作树发布（存在未提交或未跟踪改动）。请先 git status 清理。"
fi

SHORT_SHA="$(git -C "$REPO_ROOT" rev-parse --short=12 HEAD)" \
    || die "无法解析 HEAD short SHA"

# ---------------------------------------------------------------- 版本戳
# 权威源：VERSIONING.md 规定 cmd/sbx/main.go 的 `var version = "dev"` 仅是
# 未注入时的兜底默认值，模块真实已发布 SemVer 记录在 ${MODULE_REAL}/
# CHANGELOG.md 的 `## [X.Y.Z] - YYYY-MM-DD` 标题。基础版本号必须取首个
# 已发布 SemVer 章节（跳过 [Unreleased]），严禁产出 sbx dev*。

grep -qE '^var[[:space:]]+version[[:space:]]*=' "${MODULE_REAL}/cmd/sbx/main.go" \
    || die "cmd/sbx/main.go 缺少可注入的 var version 定义"
BASE_VER="$(sed -n 's/^## \[\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\(-[0-9A-Za-z.-][0-9A-Za-z.-]*\)\?\)\].*/\1/p' "${MODULE_REAL}/CHANGELOG.md" | head -n 1)"
[[ "$BASE_VER" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
    || die "无法从 ${MODULE_REAL}/CHANGELOG.md 提取合法 SemVer 基础版本号（got '${BASE_VER:-<empty>}'）"
STAMP_VERSION="${BASE_VER}+${SHORT_SHA}"

info "目标: $TARGET"
info "版本: $STAMP_VERSION"
info "模块: $MODULE_REAL"
[[ "$SKIP_IMMUTABLE" == "1" ]] && info "WARN: SBX_DEPLOY_SKIP_IMMUTABLE=1 —— 跳过 chattr +i（仅限测试）"
[[ "$FAST_VERIFY" == "1" ]] && info "WARN: SBX_DEPLOY_FAST_VERIFY=1 —— 跳过 go test -race 预检与 reviewer-agent 冒烟（仅限测试）"
[[ "$ALLOW_NO_LANDLOCK" == "1" ]] && info "WARN: SBX_DEPLOY_ALLOW_NO_LANDLOCK=1 —— 允许 --check 未确认 Landlock 时放行（仅限无 Landlock 内核测试环境）"

# ---------------------------------------------------------------- 预检（显式 -p 2 限流，不依赖可被外部覆盖的 GOFLAGS）

if [[ "$FAST_VERIFY" != "1" ]]; then
    info "运行 go vet -p 2 ./... 与 go test -p 2 -race -count=1 ./... 预检 ..."
    (cd "$MODULE_REAL" && go vet -p 2 ./... ) \
        || die "go vet -p 2 ./... 预检失败"
    (cd "$MODULE_REAL" && go test -p 2 -race -count=1 ./... ) \
        || die "go test -p 2 -race -count=1 ./... 预检失败"
else
    info "运行 go vet -p 2 ./... 预检（FAST_VERIFY 模式跳过 go test 防递归）..."
    (cd "$MODULE_REAL" && go vet -p 2 ./... ) \
        || die "go vet -p 2 ./... 预检失败"
fi

# ---------------------------------------------------------------- 统一 EXIT trap
# 在创建任何临时资源之前注册：负责清理冒烟私有目录（SMOKE_PIG_HOME 与
# 沙盒内回传的 /tmp/pi-reviewer-* / /tmp/pi-reviewer-exec-*）、删除暂存
# 二进制（先解可能存在的 +i），并在已解锁线上 TARGET 但部署未成功时恢复
# chattr +i（恢复失败必须显式 ERROR，不得静默吞掉）。

STAGED=""
SMOKE_PIG_HOME=""
SMOKE_SBX_TMPDIR=""
SMOKE_SBX_EXECDIR=""
UNLOCKED_TARGET=0
DEPLOY_SUCCEEDED=0

# 严格前缀校验：仅删除路径形态为 /tmp/pi-reviewer-<id> 或
# /tmp/pi-reviewer-exec-<id> 的单个私有目录；绝不用 /tmp/pi-reviewer-*
# 通配符——那会误删其它并发审查窗格的沙盒目录。
smoke_rm_private_dir() {
    local d="$1" prefix="$2"
    local re="^${prefix}[A-Za-z0-9._-]+$"
    if [[ -n "$d" && "$d" =~ $re && ! -L "$d" && -d "$d" ]]; then
        rm -rf -- "$d"
    fi
}

cleanup_on_exit() {
    local rc=$?
    smoke_rm_private_dir "${SMOKE_SBX_TMPDIR:-}" "/tmp/pi-reviewer-"
    smoke_rm_private_dir "${SMOKE_SBX_EXECDIR:-}" "/tmp/pi-reviewer-exec-"
    if [[ -n "${SMOKE_PIG_HOME:-}" && -d "$SMOKE_PIG_HOME" ]]; then
        rm -rf -- "$SMOKE_PIG_HOME"
    fi
    if [[ -n "${STAGED:-}" && -e "$STAGED" ]]; then
        chattr -i -- "$STAGED" 2>/dev/null || true
        rm -f -- "$STAGED"
    fi
    if [[ "$UNLOCKED_TARGET" == "1" && "$DEPLOY_SUCCEEDED" != "1" \
          && "$SKIP_IMMUTABLE" != "1" && -e "$TARGET" ]]; then
        if ! chattr +i -- "$TARGET" 2>/dev/null; then
            echo "deploy.sh: ERROR: 未能恢复 $TARGET 的 chattr +i 锁，请立即检查 lsattr" >&2
        fi
    fi
    exit "$rc"
}
trap cleanup_on_exit EXIT

# ---------------------------------------------------------------- TARGET 父目录创建与物理路径规范化
# 仅在三道硬门禁、版本提取与 go vet/go test 预检全部通过后才触碰文件系统：
# 创建目标父目录并把 TARGET 规范化为物理绝对路径，保证其在后续
# `( cd "$REPO_ROOT" && ... )` 子 shell 切换目录后依然有效；规范化后再次
# 确认目标不是已存在目录（消除与前置校验之间的 TOCTOU 窗口）。

mkdir -p -- "$(dirname -- "$TARGET")" \
    || die "创建部署目标父目录失败: $(dirname -- "$TARGET")"
TARGET_DIR="$(cd -- "$(dirname -- "$TARGET")" >/dev/null 2>&1 && pwd -P)" \
    || die "无法物理规范化目标父目录: $(dirname -- "$TARGET")"
TARGET="${TARGET_DIR}/$(basename -- "$TARGET")"
if [[ -d "$TARGET" ]]; then
    die "部署目标不能是已存在目录: $TARGET"
fi

# ---------------------------------------------------------------- 限流构建到目标同目录

STAGED="${TARGET_DIR%/}/.sbx.new.$$"

info "编译临时二进制: $STAGED"
(cd "$MODULE_REAL" && go build -p 2 -trimpath \
    -ldflags "-s -w -X main.version=${STAMP_VERSION}" \
    -o "$STAGED" ./cmd/sbx) \
    || die "go build 失败"

[[ -f "$STAGED" ]] || die "构建产物缺失: $STAGED"

# ---------------------------------------------------------------- 替换前冒烟（任一失败即由 EXIT trap 清理 STAGED 退出，绝不触碰线上目标）

# (a) --version 必须精确等于 "sbx <BASE_VER>+<12位shortSHA>"（双断言：精确相等
# 与 SemVer+SHA 正则），任何 sbx dev* 输出一律 die。
VERSION_OUT="$("$STAGED" --version 2>&1)" \
    || die "冒烟失败: $STAGED --version 非零退出"
info "--version => $VERSION_OUT"
if [[ "$VERSION_OUT" != "sbx ${STAMP_VERSION}" ]]; then
    die "冒烟失败: --version 输出 '$VERSION_OUT' 与期望 'sbx ${STAMP_VERSION}' 不一致（版本戳未正确注入）"
fi
if [[ ! "$VERSION_OUT" =~ ^sbx\ [0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?\+[0-9a-f]{12}$ ]]; then
    die "冒烟失败: --version 输出 '$VERSION_OUT' 不匹配 sbx <SemVer>+<12位shortSHA> 格式"
fi

# (b) Linux 下 --check 硬阻断：必须零退出且输出含 "Landlock supported"；
# 仅显式 SBX_DEPLOY_ALLOW_NO_LANDLOCK=1 时降级为 WARN（无 Landlock 内核的
# 特殊测试环境）。非 Linux 平台仅作信息记录。
if [[ "$(uname -s)" == "Linux" ]]; then
    if CHECK_OUT="$("$STAGED" --check 2>&1)" && [[ "$CHECK_OUT" == *"Landlock supported"* ]]; then
        info "--check => $CHECK_OUT"
    elif [[ "$ALLOW_NO_LANDLOCK" == "1" ]]; then
        info "WARN: $STAGED --check 未确认 Landlock 支持（输出: ${CHECK_OUT:-<none>}），SBX_DEPLOY_ALLOW_NO_LANDLOCK=1 放行（仅限无 Landlock 内核测试环境）"
    else
        die "--check 硬阻断: Linux 下 $STAGED --check 非零退出或未输出 'Landlock supported'（输出: ${CHECK_OUT:-<none>}）；如确属无 Landlock 内核测试环境，可显式设置 SBX_DEPLOY_ALLOW_NO_LANDLOCK=1"
    fi
else
    if "$STAGED" --check >/dev/null 2>&1; then
        info "--check => Landlock supported"
    else
        info "WARN: $STAGED --check 非零退出（非 Linux 平台仅记录不阻断）"
    fi
fi

# (c) reviewer-agent 预设 + codegraph-go 冒烟（非 FAST_VERIFY 且宿主存在 codegraph-go）
# workdir 与 reports/ 全部落在私有 SMOKE_PIG_HOME 内（绝不触碰主仓
# $REPO_ROOT/reports）；经白名单内的 /bin/sh 在 exec codegraph-go 前精确回传
# 沙盒创建的两个私有目录（$TMPDIR=/tmp/pi-reviewer-* 与
# $PI_SANDBOX_GO_EXEC_DIR=/tmp/pi-reviewer-exec-*），冒烟结束无论成败都先
# 严格前缀校验清理三者再判退出码——绝不泄漏 /tmp/pi-reviewer-*。
CODEGRAPH_GO="/root/.local/bin/codegraph-go"
if [[ "$FAST_VERIFY" != "1" && -x "$CODEGRAPH_GO" ]]; then
    SMOKE_PIG_HOME="$(mktemp -d)" || die "mktemp -d 失败"
    SMOKE_C_RC=0
    SMOKE_C_OUT="$(cd "$SMOKE_PIG_HOME" && env PIG_HOME="$SMOKE_PIG_HOME" \
        "$STAGED" --preset reviewer-agent --workdir "$SMOKE_PIG_HOME" -- \
        /bin/sh -c 'printf "__SBX_SMOKE_TMPDIR__=%s\n__SBX_SMOKE_EXECDIR__=%s\n" "${TMPDIR:-}" "${PI_SANDBOX_GO_EXEC_DIR:-}"; exec "$@"' \
        sh "$CODEGRAPH_GO" --help 2>&1)" || SMOKE_C_RC=$?
    SMOKE_SBX_TMPDIR="$(printf '%s\n' "$SMOKE_C_OUT" | sed -n 's/^__SBX_SMOKE_TMPDIR__=//p' | head -n 1)"
    SMOKE_SBX_EXECDIR="$(printf '%s\n' "$SMOKE_C_OUT" | sed -n 's/^__SBX_SMOKE_EXECDIR__=//p' | head -n 1)"
    # 无论成败先清理：沙盒私有目录严格前缀校验删除，清空变量防 EXIT trap
    # 重复删；SMOKE_PIG_HOME 一并清理并置空。
    smoke_rm_private_dir "$SMOKE_SBX_TMPDIR" "/tmp/pi-reviewer-"
    smoke_rm_private_dir "$SMOKE_SBX_EXECDIR" "/tmp/pi-reviewer-exec-"
    SMOKE_SBX_TMPDIR=""
    SMOKE_SBX_EXECDIR=""
    if [[ -d "$SMOKE_PIG_HOME" ]]; then
        rm -rf -- "$SMOKE_PIG_HOME"
    fi
    SMOKE_PIG_HOME=""
    if [[ "$SMOKE_C_RC" != "0" ]]; then
        die "冒烟失败: reviewer-agent 预设执行 codegraph-go --help 非零退出（rc=$SMOKE_C_RC）: ${SMOKE_C_OUT:-<no output>}"
    fi
    info "reviewer-agent + codegraph-go 冒烟通过（私有目录已全部清理，零残留）"
fi

# ---------------------------------------------------------------- 替换前加固与 chattr 前置探测
# 在触碰任何备份或线上 TARGET 之前完成：先固化 STAGED 权限，再验证本环境
# 确实支持 chattr +i/-i。探测失败立即退出——此时既未修改/删除任何旧备份，
# 也未解锁或覆盖线上 TARGET。

chmod 0555 -- "$STAGED" || die "chmod 0555 $STAGED 失败"

if [[ "$SKIP_IMMUTABLE" != "1" ]]; then
    chattr +i -- "$STAGED" 2>/dev/null \
        || die "chattr +i 前置探测失败（无 CAP_LINUX_IMMUTABLE 或文件系统不支持 chattr）；未触碰备份与线上 $TARGET"
    chattr -i -- "$STAGED" 2>/dev/null \
        || die "chattr -i 前置探测失败；未触碰备份与线上 $TARGET"
fi

# ---------------------------------------------------------------- 备份旧目标
# 仅在目标已存在时备份；.bak* 裁剪整体延后到 mv -f（及 chattr +i）成功之后
# 执行——mv 或新锁失败时绝不提前删掉任何历史备份。

BAK=""
if [[ -f "$TARGET" ]]; then
    # 提取旧二进制版本标识：--version 首行取空白分隔最后一列（awk 不产生
    # 尾随换行，避免 "sbx 0.3.0" 被 tr 清洗出尾随 _），清洗非
    # [A-Za-z0-9._+-] 字符为 _，为空回退 unknown，截断 40 字符。
    OLD_VER_RAW="$("$TARGET" --version 2>/dev/null | head -n 1 || true)"
    OLD_TAG="$(printf '%s' "$OLD_VER_RAW" | awk 'NF{printf "%s", $NF; exit}' | tr -c 'A-Za-z0-9._+-' '_' | cut -c1-40)"
    [[ -n "$OLD_TAG" ]] || OLD_TAG="unknown"
    BAK="${TARGET}.bak-$(date +%Y%m%d-%H%M%S)-$$-${OLD_TAG}"
    # 不用 cp -p：新备份 mtime 必须为当前备份时刻，否则当旧 TARGET mtime
    # 早于既有备份时，新备份会在随后的裁剪中被误删。
    cp -- "$TARGET" "$BAK" || die "备份 $TARGET -> $BAK 失败"
    chmod 0444 -- "$BAK" || die "chmod 0444 $BAK 失败"
    info "已备份现有目标: $BAK"
fi

# ---------------------------------------------------------------- 原子替换 + immutable 锁
# 仅在紧贴 mv -f 前解除旧 TARGET 的 immutable，最小化无锁窗口；先置
# UNLOCKED_TARGET=1 再执行 chattr -i，并用 lsattr 复核 i 标志确实已清除
#（解除失败立即 die，由 EXIT trap 回锁兜底——不赌静默成功）。

if [[ "$SKIP_IMMUTABLE" != "1" && -e "$TARGET" ]]; then
    UNLOCKED_TARGET=1
    chattr -i -- "$TARGET" 2>/dev/null || true
    if lsattr -d -- "$TARGET" 2>/dev/null | awk '{print $1}' | grep -q 'i'; then
        die "无法解除 $TARGET 的 chattr +i 锁"
    fi
fi

mv -f -- "$STAGED" "$TARGET" || die "mv -f $STAGED $TARGET 失败"
STAGED=""   # 已被 mv 走，避免 EXIT trap 误删

if [[ "$SKIP_IMMUTABLE" != "1" ]]; then
    chattr +i -- "$TARGET" \
        || die "chattr +i $TARGET 失败（目标已替换但未能加锁，请立即人工处理）"
    info "已启用 chattr +i 内核级防误写锁"
fi

# ---------------------------------------------------------------- .bak* 裁剪（仅保留最新 3 个，新备份固定排保留首位）
# 仅在 mv -f 与新 chattr +i（非跳过模式下）全部成功后执行：此前任何失败
# 路径都不会触碰既有历史备份。
# 安全裁剪：严禁 ls -1t（含空格/换行文件名会断行误删）。用 bash nullglob
# 原生数组收集，新备份固定排在保留首位，其余按纯 bash [[ a -nt b ]] 降序。

shopt -s nullglob
baks=( "${TARGET}.bak"* )
shopt -u nullglob
others=()
for _f in ${baks[@]+"${baks[@]}"}; do
    [[ "$_f" == "$BAK" ]] || others+=("$_f")
done
sorted=()
for _f in ${others[@]+"${others[@]}"}; do
    _ins=0
    for _i in ${sorted[@]+"${!sorted[@]}"}; do
        if [[ "$_f" -nt "${sorted[$_i]}" ]]; then
            sorted=( "${sorted[@]:0:$_i}" "$_f" "${sorted[@]:$_i}" )
            _ins=1
            break
        fi
    done
    (( _ins )) || sorted+=("$_f")
done
keep=("${sorted[@]+"${sorted[@]}"}")
[[ -n "$BAK" ]] && keep=("$BAK" "${sorted[@]+"${sorted[@]}"}")
if (( ${#keep[@]} > 3 )); then
    for old in "${keep[@]:3}"; do
        chmod u+w -- "$old" 2>/dev/null || true
        rm -f -- "$old" && info "裁剪旧备份: $old"
    done
fi

DEPLOY_SUCCEEDED=1

info "部署完成: $TARGET ($STAMP_VERSION)"
