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

echo "== 结果: PASS=$PASS FAIL=$FAIL =="
[[ "$FAIL" -eq 0 ]]
