#!/usr/bin/env bash
# update_integrity_test.sh — 确认在线升级只从官方源读取脚本信任锚。
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TPL="$ROOT/installer-template.sh"
TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT
PASS=0; FAIL=0
ck() { if [[ "$3" == "$2" ]]; then PASS=$((PASS+1)); echo "  [PASS] $1"; else FAIL=$((FAIL+1)); echo "  [FAIL] $1 (期望 rc=$2 实得 rc=$3)"; fi; }

sed -n '/^# >>> checksum-helpers/,/^# <<< checksum-helpers/p' "$TPL" > "$TMPD/helpers.sh"
sed -n '/^do_update()/,/^apply_update()/p' "$TPL" | sed '$d' > "$TMPD/update.sh"
grep -q 'verify_remote_script()' "$TMPD/helpers.sh" || { echo "verify_remote_script 未位于 checksum-helpers"; exit 1; }
grep -q 'sbx.sh.sha256' "$TMPD/update.sh"; ck "do_update 获取 sbx.sh.sha256" 0 $?
grep -q 'OFFICIAL_SCRIPT_URL' "$TMPD/update.sh"; ck "升级脚本本体使用固定官方 URL" 0 $?
if grep -Fq 'gh_url "$RAW_URL"' "$TMPD/update.sh" || grep -Fq 'gh_url "$OFFICIAL_SCRIPT_URL"' "$TMPD/update.sh"; then
  ck "升级脚本本体不经过镜像" 0 1
else
  ck "升级脚本本体不经过镜像" 0 0
fi
grep -q 'OFFICIAL_RAW_BASE.*sbx.sh.sha256' "$TMPD/update.sh"; ck "哈希地址固定到官方 RAW 基址" 0 $?
grep -q 'verify_remote_script.*\$tmp' "$TMPD/update.sh"; ck "do_update 用 helper 校验下载脚本" 0 $?
grep -q 'sha256_of "\$tmp"' "$TMPD/update.sh"; ck "do_update 用 sha256_of 计算远程脚本哈希" 0 $?
if grep -E 'gh_url.*(SHA256SUMS|sbx.sh.sha256)' "$TPL" >/dev/null; then
  ck "哈希清单不经过 gh_url 镜像" 0 1
else
  ck "哈希清单不经过 gh_url 镜像" 0 0
fi
grep -Fq 'curl -fsSL -m 60 -o "$tmp/SHA256SUMS" "$trust_base/SHA256SUMS"' "$TPL"; ck "core 校验清单直连 trust_base" 0 $?
# 校验失败的分支必须先 die，且不能输出任何升级成功话术。
grep -q '在线升级脚本校验失败' "$TMPD/update.sh"; ck "哈希不匹配有明确 fail-closed 错误" 0 $?
grep -q '无法从官方源获取 sbx.sh.sha256' "$TMPD/update.sh"; ck "官方清单不可用时中止并提示显式哈希" 0 $?

# 行为 PoC：镜像返回恶意脚本，但官方清单仍给出真实脚本哈希，必须在覆盖前失败。
cat "$TMPD/helpers.sh" "$TMPD/update.sh" > "$TMPD/lib.sh"
# shellcheck disable=SC1090
source "$TMPD/lib.sh"
warn() { printf '[warn] %s\n' "$*" >&2; }
ok() { printf '[ok] %s\n' "$*"; }
info() { printf '[*] %s\n' "$*"; }
err() { printf '[x] %s\n' "$*" >&2; }
die() { printf '[die] %s\n' "$*" >&2; exit 1; }
banner() { :; }
require_root() { :; }
detect_platform() { :; }
OFFICIAL_SCRIPT_URL="https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh"
OFFICIAL_RAW_BASE="https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/dist"
RAW_BASE="https://evil-mirror.invalid/custom-artifacts" # SBX_RAW_BASE 只能改 artifact 字节流，不能改变信任锚
APP_VERSION="3.0.30"
remote_version() { grep -m1 '^APP_VERSION=' "$1" | sed -E 's/^APP_VERSION="?([^\"]+)"?.*/\1/'; }
SELF_PATH="$TMPD/installed-sbx.sh"
printf '#!/usr/bin/env bash\nAPP_VERSION="3.0.30"\n' > "$TMPD/legitimate.sh"
cp "$TMPD/legitimate.sh" "$SELF_PATH"
LEGIT_HASH=$(sha256_of "$TMPD/legitimate.sh")
printf '#!/usr/bin/env bash\nAPP_VERSION="3.0.30"\necho malicious\n' > "$TMPD/malicious.sh"
printf '%s  sbx.sh\n' "$LEGIT_HASH" > "$TMPD/official.sha256"
CURL_MODE=poison
GH_PROXY="https://evil-mirror.invalid"
gh_url() { printf '%s/%s' "$GH_PROXY" "$1"; }
MIRROR_USED="$TMPD/mirror-used"
OFFICIAL_RESPONSE="$TMPD/malicious.sh"
curl() {
  local output="" url=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -o) output="$2"; shift 2 ;;
      -*) shift ;;
      *) url="$1"; shift ;;
  esac
  done
  case "$url" in
    "$OFFICIAL_SCRIPT_URL") cp "$OFFICIAL_RESPONSE" "$output" ;;
    https://evil-mirror.invalid/*) touch "$MIRROR_USED"; cp "$TMPD/malicious.sh" "$output" ;;
    "$OFFICIAL_RAW_BASE/sbx.sh.sha256") cp "$TMPD/official.sha256" "$output" ;;
    *) return 22 ;;
  esac
}
C_B=""; C_DIM=""; C_RESET=""
set +e
OUT=$(do_update 2>&1)
RC=$?
set -e
ck "镜像脚本与官方哈希不符时升级失败" 1 "$RC"
if [[ -e "$MIRROR_USED" ]]; then ck "在线升级未向镜像请求脚本字节流" 0 1; else ck "在线升级未向镜像请求脚本字节流" 0 0; fi
if [[ "$OUT" == *"在线升级脚本校验失败"* && "$OUT" != *"升级完成"* && "$OUT" != *"已是最新版本"* ]]; then
  ck "校验失败不输出成功话术" 0 0
else
  ck "校验失败不输出成功话术" 0 1
  printf '%s\n' "$OUT"
fi

# 合法官方发布时即使 RAW/HTTP 镜像配置为恶意地址，升级脚本本体和哈希仍走官方源。
OFFICIAL_RESPONSE="$TMPD/legitimate.sh"
install_sbx_core() { CORE_REPLACED=0; return 0; }
set +e
OUT=$(do_update 2>&1)
RC=$?
set -e
ck "合法官方脚本通过校验并正常完成幂等升级" 0 "$RC"
if [[ -e "$MIRROR_USED" ]]; then ck "有效升级也未向恶意镜像请求脚本" 0 1; else ck "有效升级也未向恶意镜像请求脚本" 0 0; fi
if [[ "$OUT" == *"已是最新版本"* ]]; then ck "有效官方升级正常输出成功" 0 0; else ck "有效官方升级正常输出成功" 0 1; fi

# 官方清单无法访问且未显式给哈希时，也必须 fail closed 并给出重试指引。
curl() {
  local output="" url=""
  while [[ $# -gt 0 ]]; do
    case "$1" in -o) output="$2"; shift 2 ;; -*) shift ;; *) url="$1"; shift ;; esac
  done
  case "$url" in
    "$OFFICIAL_SCRIPT_URL") cp "$OFFICIAL_RESPONSE" "$output" ;;
    "$OFFICIAL_RAW_BASE/sbx.sh.sha256") return 22 ;;
    *) return 22 ;;
  esac
}
unset SBX_SCRIPT_SHA256
set +e
OUT=$(do_update 2>&1)
RC=$?
set -e
ck "无法取官方哈希且无显式锚时中止" 1 "$RC"
if [[ "$OUT" == *"SBX_SCRIPT_SHA256=<官方哈希>"* ]]; then ck "错误提示包含显式哈希重试指引" 0 0; else ck "错误提示包含显式哈希重试指引" 0 1; fi

# ---- install_self：管道 / 进程替换安装时重新落盘的脚本也必须来自官方源且校验通过 ----
# 本段沿用文件既有风格：用 `cmd; ck "..." <期望> $?` 逐条报告，因此必须关掉 -e
# （上面几个 set +e/set -e 块已经把 -e 打开；开着 -e 时断言失败会直接中止脚本，
#   只剩一个非零退出码，看不到是第几条失败）。
set +e

# 覆盖真实分支：install_self 里的 ${BASH_SOURCE[0]} 在 `bash <(curl ...)` 一键安装时
# 不是普通文件（实测为 "main"），因此必然走"重新下载并落盘"这条分支——而这份副本会成为
# 常驻的 sbx 命令，被菜单与 systemd 反复执行。旧实现用 gh_url 拉取且完全不校验。
sed -n '/^install_self()/,/^}/p' "$TPL" > "$TMPD/install_self.sh"
grep -q 'OFFICIAL_SCRIPT_URL' "$TMPD/install_self.sh"; ck "install_self 使用固定官方脚本 URL" 0 $?
# 只检查非注释行：函数内的说明注释会提到 gh_url（解释为什么不能这么用），
# 那属于文档而非调用点，不应该让断言误报。
if grep -v '^[[:space:]]*#' "$TMPD/install_self.sh" | grep -Fq 'gh_url'; then
  ck "install_self 不经过 gh_url 镜像" 0 1
else
  ck "install_self 不经过 gh_url 镜像" 0 0
fi
grep -q 'verify_remote_script' "$TMPD/install_self.sh"; ck "install_self 落盘前校验官方哈希" 0 $?
grep -q 'OFFICIAL_RAW_BASE.*sbx.sh.sha256' "$TMPD/install_self.sh"; ck "install_self 哈希固定到官方 RAW 基址" 0 $?
grep -q 'if \[\[ -f "$SELF_PATH" \]\]' "$TMPD/install_self.sh"; ck "install_self 仅在脚本存在时写 sbx 包裹命令" 0 $?

# 用 `bash < harness` 运行：此时 ${BASH_SOURCE[0]} 不是普通文件，与一键安装同路径。
cat > "$TMPD/self_harness.sh" <<'HARNESS'
set -u
warn() { printf '[warn] %s\n' "$*" >&2; }
ok()   { printf '[ok] %s\n' "$*"; }
info() { printf '[*] %s\n' "$*"; }
err()  { printf '[x] %s\n' "$*" >&2; }
die()  { printf '[die] %s\n' "$*" >&2; exit 1; }
BIN_DIR="$H_TMPD/bin"
SELF_PATH="$H_TMPD/installed/sbx.sh"
CMD_PATH="$H_TMPD/bin/sbx"
OFFICIAL_SCRIPT_URL="https://official.invalid/main/sbx.sh"
OFFICIAL_RAW_BASE="https://official.invalid/dist"
GH_PROXY="https://evil-mirror.invalid"
CURL_LOG="$H_TMPD/curl.log"
gh_url() { printf 'MIRROR:%s\n' "$1" >> "$CURL_LOG"; printf '%s/%s' "$GH_PROXY" "$1"; }
curl() {
  local output="" url=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -o) output="$2"; shift 2 ;;
      -*) shift ;;
      *) url="$1"; shift ;;
    esac
  done
  printf '%s\n' "$url" >> "$CURL_LOG"
  [[ "$H_MODE" == "unreachable" ]] && return 22
  case "$url" in
    "$OFFICIAL_SCRIPT_URL") cp "$H_SCRIPT_RESPONSE" "$output" ;;
    "$OFFICIAL_RAW_BASE/sbx.sh.sha256") cp "$H_SUMS_RESPONSE" "$output" ;;
    *) return 22 ;;
  esac
}
mkdir -p "$BIN_DIR" "$(dirname "$SELF_PATH")"
HARNESS
sed -n '/^# >>> checksum-helpers/,/^# <<< checksum-helpers/p' "$TPL" >> "$TMPD/self_harness.sh"
cat "$TMPD/install_self.sh" >> "$TMPD/self_harness.sh"
printf '\ninstall_self\nexit 0\n' >> "$TMPD/self_harness.sh"

FIX="$TMPD/self"
mkdir -p "$FIX"
printf '#!/usr/bin/env bash\nAPP_VERSION="3.0.30"\n' > "$FIX/legit.sh"
printf '#!/usr/bin/env bash\nAPP_VERSION="3.0.30"\necho malicious\n' > "$FIX/mal.sh"
LEGIT_SHA=$(sha256sum "$FIX/legit.sh" | awk '{print $1}')
printf '%s  sbx.sh\n' "$LEGIT_SHA" > "$FIX/official.sha256"
printf '%s  sbx.sh\n%s  sbx.sh\n' "$LEGIT_SHA" "$LEGIT_SHA" > "$FIX/dup.sha256"

run_install_self() { # run_install_self <工作目录> <模式> <脚本来源> <清单来源>
  local work="$1"
  rm -rf "$work"; mkdir -p "$work"
  H_TMPD="$work" H_MODE="$2" H_SCRIPT_RESPONSE="$3" H_SUMS_RESPONSE="$4" \
    bash < "$TMPD/self_harness.sh" 2>&1
}

# 1) 官方脚本被投毒（与官方哈希不符）→ 不得落盘，也不得写包裹命令
W="$TMPD/poison"; OUT=$(run_install_self "$W" ok "$FIX/mal.sh" "$FIX/official.sha256"); rc=$?
ck "投毒脚本：install_self 不因校验失败中断安装" 0 "$rc"
[[ ! -e "$W/installed/sbx.sh" ]]; ck "投毒脚本：不落盘 $SELF_PATH" 0 $?
[[ ! -e "$W/bin/sbx" ]]; ck "投毒脚本：不写 sbx 包裹命令" 0 $?
printf '%s' "$OUT" | grep -q "未通过官方校验"; ck "投毒脚本：给出明确告警" 0 $?
grep -q "MIRROR:" "$W/curl.log" 2>/dev/null && ck "投毒脚本：未向镜像请求脚本字节流" 0 1 || ck "投毒脚本：未向镜像请求脚本字节流" 0 0

# 2) 官方发布合法 → 正常落盘并可执行
W="$TMPD/legit"; OUT=$(run_install_self "$W" ok "$FIX/legit.sh" "$FIX/official.sha256"); rc=$?
ck "合法脚本：install_self 成功" 0 "$rc"
[[ -f "$W/installed/sbx.sh" ]]; ck "合法脚本：落盘 $SELF_PATH" 0 $?
cmp -s "$W/installed/sbx.sh" "$FIX/legit.sh"; ck "合法脚本：落盘内容与官方脚本一致" 0 $?
[[ -x "$W/bin/sbx" ]]; ck "合法脚本：写入可执行的 sbx 包裹命令" 0 $?
grep -q "MIRROR:" "$W/curl.log" 2>/dev/null && ck "合法脚本：同样不向镜像请求脚本" 0 1 || ck "合法脚本：同样不向镜像请求脚本" 0 0

# 3) 官方哈希清单出现重复条目（歧义）→ 必须拒绝
W="$TMPD/dup"; OUT=$(run_install_self "$W" ok "$FIX/legit.sh" "$FIX/dup.sha256"); rc=$?
[[ ! -e "$W/installed/sbx.sh" ]]; ck "清单重复条目：拒绝落盘" 0 $?

# 4) 官方源不可达 → 不落盘、不给用户留下坏掉的 sbx 命令
W="$TMPD/unreach"; OUT=$(run_install_self "$W" unreachable "$FIX/legit.sh" "$FIX/official.sha256"); rc=$?
ck "官方源不可达：install_self 仍返回成功（不阻断安装）" 0 "$rc"
[[ ! -e "$W/installed/sbx.sh" ]]; ck "官方源不可达：不落盘" 0 $?
[[ ! -e "$W/bin/sbx" ]]; ck "官方源不可达：不写坏掉的 sbx 命令" 0 $?
printf '%s' "$OUT" | grep -q "无法从官方源获取"; ck "官方源不可达：给出明确告警" 0 $?

echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]]
