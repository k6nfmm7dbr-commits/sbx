#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 流量统计面板
#  安装:  按项目 README 下载并校验安装器
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="3.0.30"

# SBX_ROOT 仅用于测试/沙箱安装（把整套目录挪到前缀下），正常安装留空
ROOT="${SBX_ROOT:-}"
APP_DIR="$ROOT/etc/sbx"
WEB_DIR="$APP_DIR/web"
BIN_DIR="$ROOT/usr/local/bin"
SB_BIN="$BIN_DIR/sing-box"
SB_DIR="$ROOT/etc/sing-box"
SB_CONF="$SB_DIR/config.json"
CORE_BIN="$BIN_DIR/sbx-core"
PANEL_CONF="$APP_DIR/panel.json"
NODES_JSON="$APP_DIR/nodes.json"
CERT_DIR="$APP_DIR/certs"
STATE_JSON="$APP_DIR/state.json"
SELF_PATH="$APP_DIR/sbx.sh"
CMD_PATH="$BIN_DIR/sbx"

export SBX_DIR="$APP_DIR"
export SBX_SB_CONF="$SB_CONF"
export SBX_CONF="$PANEL_CONF"

SB_VERSION_PIN="${SBX_SB_VERSION:-}"     # 留空=取最新稳定版
GH_PROXY="${SBX_GH_PROXY:-}"             # 例: https://ghfast.top/
# The official artifact repository is public; the source repository is private.
DIST_PROTOCOL="sbx-dist-v1"
DIST_REPO="k6nfmm7dbr-commits/sbx"
OFFICIAL_RAW_REPO="https://raw.githubusercontent.com/$DIST_REPO"
DIST_SHA=""
DIST_BASE=""
UNIT_DIR="$ROOT/etc/systemd/system"
INIT_DIR="$ROOT/etc/init.d"
CONNTRACK_SYSCTL_CONF="$ROOT/etc/sysctl.d/99-sbx-conntrack.conf"

OS_FAMILY="unknown"
INIT_SYS="unknown"
PKG=""

# >>> env-validation
# 校验安装器输入，不解析 DNS，也不把镜像当作哈希信任源。
env_valid_ipv4() {
  local ip="$1" octet re='(0|[1-9][0-9]{0,2})'
  [[ "$ip" =~ ^$re(\.$re){3}$ ]] || return 1
  while :; do
    octet="${ip%%.*}"
    (( octet <= 255 )) || return 1
    [[ "$ip" == *.* ]] || return 0
    ip="${ip#*.}"
  done
}

env_valid_ipv6() {
  local ip="$1" left right colons group='[0-9A-Fa-f]{1,4}'
  if [[ "$ip" == *.* ]]; then
    [[ "$ip" == *:* ]] && env_valid_ipv4 "${ip##*:}" || return 1
    ip="${ip%:*}:0:0"  # 末尾 IPv4 占两个 hextet
  fi
  if [[ "$ip" != *::* ]]; then
    [[ "$ip" =~ ^$group(:$group){7}$ ]]
    return
  fi
  left="${ip%%::*}" right="${ip#*::}"
  [[ -z "$left" || "$left" =~ ^$group(:$group)*$ ]] || return 1
  [[ -z "$right" || "$right" =~ ^$group(:$group)*$ ]] || return 1
  ip="${left:+$left:}$right"
  ip="${ip%:}"
  colons="${ip//[^:]/}"
  # :: 至少替代一组，最多只能显式写出七组。
  [[ ${#colons} -lt 7 ]]
}

env_valid_https_url() {
  local rest authority host port path
  local authority_re='^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)(:([0-9]{1,5}))?$'
  local label='[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?'
  local path_re='^([A-Za-z0-9._~/:@+,=-]|%[0-9A-Fa-f]{2})*$'
  local unsafe_escape='%([01][0-9A-Fa-f]|7[fF]|2[5fF]|5[cC])'

  [[ "$1" == [Hh][Tt][Tt][Pp][Ss]://* ]] || return 1
  rest="${1:8}"
  authority="${rest%%/*}" path="${rest#"$authority"}"
  [[ "$authority" =~ $authority_re ]] || return 1
  host="${BASH_REMATCH[1]}" port="${BASH_REMATCH[3]}"
  [[ -z "$port" ]] || (( 10#$port >= 1 && 10#$port <= 65535 )) || return 1
  if [[ "$host" == \[* ]]; then
    env_valid_ipv6 "${host:1:${#host}-2}" || return 1
  else
    host="${host%.}"  # 支持末尾带点的全限定域名
    [[ ${#host} -le 253 && "$host" =~ ^$label(\.$label)*$ ]] || return 1
    if [[ "$host" =~ ^[0-9.]+$ ]]; then
      env_valid_ipv4 "$host" || return 1
    fi
  fi

  # 镜像需拼接路径，不接受查询/片段；拒绝编码的控制符、分隔符和 %。
  [[ "$path" =~ $path_re && ! "$path" =~ $unsafe_escape ]] || return 1
  path="${path//%2[eE]/.}"
  [[ "$path/" != */../* && "$path/" != */./* ]]
}

env_valid_absolute_path() {
  # 允许空格、普通文件名中的两个点，以及指向正常文件的系统符号链接。
  [[ "$1" == /* && "$1" != *[[:cntrl:]]* && "$1/" != */../* ]]
}

env_valid_executable() {
  local mode
  env_valid_absolute_path "$1" && [[ -f "$1" && -x "$1" ]] || return 1
  # 同时检查执行位，不能仅依赖特权进程的访问检查。
  mode=$(stat -Lc %a -- "$1" 2>/dev/null) || return 1
  [[ "$mode" =~ ^[0-7]{1,4}$ ]] && (( (8#$mode & 0111) != 0 ))
}

validate_env() {
  local LC_ALL=C bad=0 var value

  for var in SBX_GH_PROXY SBX_RAW_BASE SBX_RAW_URL; do
    value="${!var:-}"
    if [[ -n "$value" ]] && ! env_valid_https_url "$value"; then
      err "$var 必须是合法 HTTPS URL，不得包含凭据、查询、片段或不安全的路径"
      bad=1
    fi
  done

  # SBX_ROOT 会写入服务配置，保留更窄的字符白名单。
  if [[ -n "${SBX_ROOT:-}" && ( ! "$SBX_ROOT" =~ ^/[A-Za-z0-9._/-]*$ || "$SBX_ROOT" == *..* || "$(readlink -f -- "$SBX_ROOT" 2>/dev/null)" == / ) ]]; then
    err "SBX_ROOT 必须是绝对路径，只能包含字母、数字、点、下划线、连字符和斜杠，不得包含 .. 或指向 /"
    bad=1
  fi

  for var in SBX_SB_BIN SBX_CORE_BIN; do
    value="${!var:-}"
    if [[ -n "$value" ]] && ! env_valid_executable "$value"; then
      err "$var 必须是无路径穿越的绝对路径，并指向可执行的普通文件"
      bad=1
    fi
  done
  for var in SBX_RESOLVCONF_HEAD SBX_RESOLV_CONF; do
    value="${!var:-}"
    # resolver 文件可能还未生成；若已存在，允许普通文件或其符号链接。
    if [[ -n "$value" ]] && ! { env_valid_absolute_path "$value" && [[ ! -e "$value" || -f "$value" ]]; }; then
      err "$var 必须是无路径穿越的绝对路径，且若存在须指向普通文件"
      bad=1
    fi
  done

  # 未设置与显式空值不同：不能因空哈希或空提交意外降级。
  for var in SBX_SCRIPT_SHA256 SBX_UPDATE_SHA256; do
    if [[ ${!var+x} && ! "${!var}" =~ ^[0-9a-fA-F]{64}$ ]]; then
      err "$var 必须是 64 位十六进制 sha256"
      bad=1
    fi
  done
  if [[ ${SBX_DIST_SHA+x} && ! "$SBX_DIST_SHA" =~ ^[0-9a-f]{40}$ ]]; then
    err "SBX_DIST_SHA 必须是 40 位小写 Git SHA"
    bad=1
  fi

  # SBX_SB_VERSION 的显式空值仍表示使用默认稳定版本。
  if [[ -n "${SBX_SB_VERSION:-}" && ! "$SBX_SB_VERSION" =~ ^[0-9]+(\.[0-9]+)*(-[A-Za-z0-9.]+)?$ ]]; then
    err "SBX_SB_VERSION 格式非法（应形如 1.14.0 或 1.14.0-rc.1）"
    bad=1
  fi

  [[ "$bad" == 0 ]]
}
# <<< env-validation

# >>> script-integrity（CI/tests/script_integrity_test.sh 提取本区块做一致性测试）
# 脚本自校验（审计项「sbx.sh 远程脚本执行风险」）。
#
# SBX_DIST_SHA fixes the release revision; SBX_SCRIPT_SHA256 verifies the
# actual file being executed. Streams can bootstrap but cannot self-verify.
# Capture the top-level source: inside a function Bash may synthesize a source
# name for stdin, and that name differs across builds.
sbx_script_source=${BASH_SOURCE[0]:-}
verify_script_integrity() {
  [[ -v SBX_SCRIPT_SHA256 ]] || return 0
  local want self actual path
  [[ "$SBX_SCRIPT_SHA256" =~ ^[0-9a-fA-F]{64}$ ]] || { err "SBX_SCRIPT_SHA256 必须是 64 位十六进制"; return 1; }
  want=${SBX_SCRIPT_SHA256,,}
  self=$sbx_script_source
  # Reject FD/stdin even when the FD happens to reference a regular file.
  # Checking a pipe by reopening it may consume input or block indefinitely.
  if [[ -z "$self" || "$self" == - ]]; then
    err "无法从 fd 或 stdin 回读脚本；请先下载到普通文件，再设置 SBX_SCRIPT_SHA256 执行"
    return 1
  fi
  path=$(readlink -f -- "$self" 2>/dev/null) || path=$self
  case "$self:$path" in
    /dev/fd/*|/dev/stdin:*|/proc/*/fd/*|*:/dev/fd/*|*:/dev/stdin|*:/proc/*/fd/*)
      err "无法从 fd 或 stdin 回读脚本；请先下载到普通文件，再设置 SBX_SCRIPT_SHA256 执行"
      return 1 ;;
  esac
  if [[ ! -f "$self" || ! -r "$self" ]]; then
    err "无法回读脚本（包括 fd 或 stdin）；请先下载到可读的普通文件，再设置 SBX_SCRIPT_SHA256 执行"
    return 1
  fi
  actual="$(sha256_of "$self")" || { err "计算脚本 sha256 失败"; return 1; }
  if [[ "$actual" != "$want" ]]; then
    err "脚本完整性校验失败，已中止（未做任何改动）"
    err "  期望: $want"
    err "  实得: $actual"
    err "脚本可能被篡改、镜像投毒或下载不完整。请从官方地址重新下载并核对哈希。"
    return 1
  fi
  ok "脚本完整性校验通过"
  return 0
}
# <<< script-integrity

# ---------------------------------------------------------------- 输出样式
init_colors() {
  if [[ -t 1 && -z "${NO_COLOR:-}" && "${TERM:-dumb}" != "dumb" ]]; then
    C_RESET=$'\033[0m'; C_B=$'\033[1m'
    C_CYAN=$'\033[38;5;44m'; C_BLUE=$'\033[38;5;39m'; C_GREEN=$'\033[38;5;42m'
    C_YEL=$'\033[38;5;214m'; C_RED=$'\033[38;5;196m'; C_DIM=$'\033[38;5;245m'
  else
    C_RESET="" C_B="" C_CYAN="" C_BLUE="" C_GREEN="" C_YEL="" C_RED="" C_DIM=""
  fi
}
info() { printf '%s[*]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf '%s[+]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%s[!]%s %s\n' "$C_YEL" "$C_RESET" "$*" >&2; }
err()  { printf '%s[x]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }
die()  { printf '%s[x]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }
hr()   { printf '%s%s%s\n' "$C_DIM" "────────────────────────────────────────────────" "$C_RESET"; }

banner() {
  if [[ -t 1 ]]; then clear 2>/dev/null || true; fi
  printf '%s%s' "$C_CYAN" "$C_B"
  cat <<'EOF'
   ___ ___  __  __
  / __| _ )\ \/ /   sing-box 节点 + 流量面板
  \__ \ _ \ >  <
  |___/___//_/\_\
EOF
  printf '%s  v%s%s\n\n' "$C_DIM" "$APP_VERSION" "$C_RESET"
}

# ---------------------------------------------------------------- 环境探测
require_root() { [[ ${EUID:-$(id -u)} -eq 0 ]] || die "需要 root 权限，请用 sudo 运行"; }

detect_platform() {
  if [[ -f /etc/alpine-release ]]; then
    OS_FAMILY="alpine"; PKG="apk"
  elif command -v apt-get >/dev/null 2>&1; then
    OS_FAMILY="debian"; PKG="apt"
  elif command -v dnf >/dev/null 2>&1; then
    OS_FAMILY="rhel"; PKG="dnf"
  elif command -v yum >/dev/null 2>&1; then
    OS_FAMILY="rhel"; PKG="yum"
  else
    die "不支持的系统（需要 Debian/Ubuntu、RHEL 系或 Alpine）"
  fi

  if [[ -d /run/systemd/system ]] && command -v systemctl >/dev/null 2>&1; then
    INIT_SYS="systemd"
  elif command -v rc-update >/dev/null 2>&1; then
    INIT_SYS="openrc"
  elif [[ -n "$ROOT" || -n "${SBX_NO_SERVICE:-}" ]]; then
    INIT_SYS="none"
  else
    die "未找到 systemd 或 OpenRC，无法安装服务"
  fi
}

pkg_install() {
  # 不要为了显示诊断而重跑失败的安装；直接保留包管理器的输出。
  case "$PKG" in
    apk) apk add --no-cache "$@" ;;
    apt) DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" ;;
    dnf) dnf install -y "$@" ;;
    yum) yum install -y "$@" ;;
    *) return 1 ;;
  esac
}

pkg_install_hint() {
  local cmd
  case "$PKG" in
    apt) cmd="apt-get install -y" ;;
    dnf) cmd="dnf install -y" ;;
    yum) cmd="yum install -y" ;;
    apk) cmd="apk add" ;;
    *)   cmd="请用本发行版的包管理器安装" ;;
  esac
  printf '%s %s\n' "$cmd" "$*"
}

install_deps() {
  info "检查依赖..."
  if ! sbx_host_runtime; then
    # A filesystem prefix is not a package or kernel namespace.
    command -v tar >/dev/null || die "沙箱安装需要 tar（不会修改宿主机软件包）"
    if [[ -z "${SBX_CORE_BIN:-}" || -z "${SBX_SB_BIN:-}" ]]; then
      command -v curl >/dev/null || die "沙箱在线安装需要 curl"
      if ! command -v sha256sum >/dev/null && ! command -v openssl >/dev/null; then
        die "沙箱在线安装需要 sha256sum 或 openssl"
      fi
    fi
    ensure_conntrack_acct
    return 0
  fi
  local -a required=() missing_req=()
  local c need_sha=0
  for c in curl tar; do
    command -v "$c" >/dev/null 2>&1 || required+=("$c")
  done
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v openssl >/dev/null 2>&1; then
    need_sha=1
  fi
  # 已具备所有依赖时不刷新索引；需要安装时至多刷新一次。
  if [[ "$PKG" == apt ]] && { ((${#required[@]} || need_sha)) ||
      ! command -v jq >/dev/null 2>&1 || ! command -v nft >/dev/null 2>&1; }; then
    apt-get update -qq >/dev/null 2>&1 || true  # 已有索引可能仍可用，由实际安装和复检决定
  fi

  # ---- 必需依赖：缺失则无法下载/解包，装不上必须中止 ----
  if ((${#required[@]})); then
    info "安装必需依赖: ${required[*]}"
    pkg_install "${required[@]}" || true   # 失败留给下面的复检统一报错
    for c in curl tar; do
      command -v "$c" >/dev/null 2>&1 || missing_req+=("$c")
    done
  fi
  if ((${#missing_req[@]})); then
    err "必需依赖安装失败: ${missing_req[*]}"
    err "请先手动安装后重试：  $(pkg_install_hint "${missing_req[@]}")"
    err "若 apt 报 404 Not Found：该发行版很可能已 EOL、软件源被移除"
    err "（例如 Debian 11 bullseye 需把源指向 archive.debian.org 后再 update）"
    die "依赖缺失，已中止安装（未做任何改动）"
  fi

  # 已有 SHA-256 工具时不额外装 openssl；否则单独补齐，避免受 jq 失败影响。
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v openssl >/dev/null 2>&1; then
    info "安装校验工具: openssl"
    pkg_install openssl || true
    if ! command -v sha256sum >/dev/null 2>&1 && ! command -v openssl >/dev/null 2>&1; then
      err "既没有 sha256sum 也没有 openssl，无法校验下载产物的完整性"
      err "请安装 coreutils 或 openssl 后重试：  $(pkg_install_hint coreutils)"
      die "缺少校验工具，已中止安装（未做任何改动）"
    fi
  fi

  # 正式候选配置的 route.final 引用自检需要 jq；安装失败只降级为告警。
  if ! command -v jq >/dev/null 2>&1; then
    info "安装可选依赖: jq"
    pkg_install jq || true
    if ! command -v jq >/dev/null 2>&1; then
      warn "可选依赖未能安装: jq（不影响安装，相关功能自动降级）"
      warn "  常见原因：发行版已 EOL，软件源被移除（apt 报 404 Not Found）"
      warn "  如需补装：  $(pkg_install_hint jq)"
      warn "  已 EOL 的 Debian（如 11 bullseye）需先把源指向 archive.debian.org"
    fi
  fi

  # 计数/策略后端：nftables 是唯一后端（v3.0.9 起 nftables-only）。
  # 不存在回退：装不上或装完仍不可用一律中止安装，绝不静默降级到其它后端。
  if ! command -v nft >/dev/null 2>&1; then
    info "安装 nftables（SBX 的唯一防火墙后端）"
    pkg_install nftables || true
  fi
  if ! command -v nft >/dev/null 2>&1; then
    err "未找到 nft 命令：SBX 依赖 nftables（流量统计 / 节点暂停 / IP 限制 / 限速均由其实现）"
    err "SBX 只支持 nftables 这一种防火墙后端，不存在其它后端降级。请先安装后重试："
    err "  $(pkg_install_hint nftables)"
    err "  安装后确认可用：nft list tables"
    die "依赖缺失，已中止安装（未做任何改动）"
  fi
  # 命令存在还不够：容器/受限内核里 nft 可能无法访问 netlink。
  # 这里只做只读探测（list tables 不改变任何规则）。
  if ! nft list tables >/dev/null 2>&1; then
    err "nft 命令存在但不可用（无法列出规则表），SBX 无法继续安装"
    err "常见原因与排查："
    err "  · 未以 root 运行 / 缺少 CAP_NET_ADMIN（容器需 --privileged 或 --cap-add=NET_ADMIN）"
    err "  · 内核未启用 nftables（nf_tables 模块未加载）：lsmod | grep nf_tables"
    err "  · 与其它防火墙工具冲突，或内核过旧（建议 5.4+）"
    err "  自检命令：nft list tables"
    die "nftables 不可用，已中止安装（未做任何改动）"
  fi
  ensure_conntrack_acct
  ok "依赖就绪（Go 单二进制后端，无需 Python）"
}

# >>> conntrack-acct
# ensure_conntrack_acct 开启 conntrack 字节计费并持久化。
#
# 为什么必需：Debian/Ubuntu 默认 net.netfilter.nf_conntrack_acct=0，此时
# /proc/net/nf_conntrack 不输出 bytes= 字段。面板的「在线 IP / TCP 连接」
# 判活依赖字节增量，全零会导致正在使用的连接在空闲窗口后被判死——
# IP 限制开启时会把真实客户端从 allow set 移除（真的踢线）。
#
# 后端已内置降级（检测到全零则退回「ESTABLISHED 即在线」），因此这里
# 失败只告警不阻断安装。
ensure_conntrack_acct() {
  local cur="" conf="$CONNTRACK_SYSCTL_CONF"
  if [[ -n "$ROOT" ]]; then
    # 只生成目标目录的配置，不查询或修改当前内核。
    mkdir -p -- "${conf%/*}" || { warn "无法创建 ${conf%/*}"; return 0; }
  else
    [[ -z "${SBX_NO_SERVICE:-}" ]] || return 0
    cur=$(sysctl -n net.netfilter.nf_conntrack_acct 2>/dev/null || true)
    [[ "$cur" != 1 ]] || return 0
    # 模块可能尚未加载；内建或稍后由 nft 触发也没关系。
    modprobe nf_conntrack 2>/dev/null || true
    if sysctl -w net.netfilter.nf_conntrack_acct=1 >/dev/null 2>&1; then
      info "已开启 conntrack 字节计费（nf_conntrack_acct=1）"
    else
      warn "无法开启 nf_conntrack_acct，在线 IP 判活将降级为「ESTABLISHED 即在线」"
      return 0
    fi
  fi
  # 正常安装仍仅在发行版提供 sysctl.d 时持久化。
  if [[ -d "${conf%/*}" ]]; then
    printf '# 由 sbx 写入：面板在线 IP/连接判活依赖 conntrack 字节计费\nnet.netfilter.nf_conntrack_acct = 1\n' \
      2>/dev/null > "$conf" \
      || warn "写入 $conf 失败（重启后需手工设置）"
  fi
}
# <<< conntrack-acct

# ---------------------------------------------------------------- 通用工具
rand_hex() {  # crypto/rand 优先，fallback openssl
  if "$CORE_BIN" secret hex "$1" 2>/dev/null; then return 0; fi
  openssl rand -hex "$1"
}
rand_uuid() {
  if [[ -r /proc/sys/kernel/random/uuid ]]; then cat /proc/sys/kernel/random/uuid
  else "$SB_BIN" generate uuid 2>/dev/null || od -x -N 16 /dev/urandom \
       | awk 'NR==1{printf "%s%s-%s-%s-%s-%s%s%s\n",substr($2,3),substr($3,1,4),substr($3,5,4),substr($4,1,4),substr($4,5,4),substr($5,1,4),substr($5,5,4),substr($6,1,4)}'; fi
}

core_node() { "$CORE_BIN" node "$@"; }

# >>> checksum-helpers（CI/tests/checksum_flow_test.sh 提取本区块做一致性测试）
sha256_of() {  # sha256_of <file>
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else openssl dgst -sha256 -r "$1" | awk '{print $1}'; fi
}

verify_core_checksum() {  # <binary_file> <SHA256SUMS_file> <binary_name>
  local f="$1" sums="$2" name="$3" n expect got
  [[ -s "$f" ]] || { warn "二进制为空或不存在: $f"; return 1; }
  [[ -s "$sums" ]] || { warn "SHA256SUMS 缺失或为空"; return 1; }
  # 目标 binary 必须在 SHA256SUMS 中恰好出现一次（缺失/重复/格式异常都算失败）
  n=$(awk -v x="$name" '$2==x{c++} END{print c+0}' "$sums")
  if [[ "$n" != "1" ]]; then
    warn "SHA256SUMS 中 $name 出现 ${n} 次（应为 1 次）"
    return 1
  fi
  expect=$(awk -v x="$name" '$2==x && NF==2{print $1}' "$sums") || return 1
  [[ "$expect" =~ ^[0-9a-fA-F]{64}$ ]] || { warn "SHA256SUMS 中 $name 的哈希格式无效"; return 1; }
  expect=${expect,,}
  got=$(sha256_of "$f") || { warn "计算二进制 SHA-256 失败: $f"; return 1; }
  if [[ "$expect" != "$got" ]]; then
    warn "校验和不匹配: 期望 ${expect:-空} 实得 $got"
    return 1
  fi
  return 0
}

verify_remote_script() {  # <script_file> <expected_sha256> <source_description>
  local f="$1" expect="$2" source="$3" got
  [[ -s "$f" ]] || { warn "待校验脚本为空或不存在($source)"; return 1; }
  if [[ ! "$expect" =~ ^[0-9a-fA-F]{64}$ ]]; then
    warn "脚本校验哈希缺失或格式异常($source)"
    return 1
  fi
  expect=$(printf '%s' "$expect" | tr 'A-F' 'a-f')
  got=$(sha256_of "$f") || { warn "计算脚本 sha256 失败($source)"; return 1; }
  if [[ "$expect" != "$got" ]]; then
    warn "脚本完整性校验失败($source): 期望 $expect 实得 $got"
    return 1
  fi
  ok "脚本完整性校验通过($source)"
  return 0
}
# <<< checksum-helpers

# core_version_of <binary> —— 解析 sbx-core version 输出中的纯版本号（机器可读）。
# 输出形如 "sbx-core v3.0.5"，提取 v 后的版本号做严格比较（不用模糊 substring）。
core_version_of() {
  "$1" version 2>/dev/null | head -1 | sed -nE 's/^sbx-core v?([0-9]+\.[0-9]+\.[0-9]+)$/\1/p'
}

# <<< core-version-helpers

valid_port() { [[ "$1" =~ ^[0-9]+$ ]] && ((10#$1 >= 1 && 10#$1 <= 65535)); }

# 0 = occupied, 1 = free, 2 = unknown. Never treat failed execution as free.
node_port_busy() {
  local rc=0
  [[ -x "$CORE_BIN" ]] || return 2
  core_node port-used "$1" >/dev/null 2>&1 || rc=$?
  case "$rc" in 0|1) return "$rc" ;; *) return 2 ;; esac
}

port_busy() {
  local p="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -Hlnt "sport = :$p" 2>/dev/null | grep -q . && return 0
    ss -Hlnu "sport = :$p" 2>/dev/null | grep -q . && return 0
  elif command -v netstat >/dev/null 2>&1; then
    netstat -lnt 2>/dev/null | grep -qE "[:.]${p}[[:space:]]" && return 0
    netstat -lnu 2>/dev/null | grep -qE "[:.]${p}[[:space:]]" && return 0
  fi
  node_port_busy "$p"
}

pick_port() {
  local p rc
  for _ in $(seq 1 60); do
    p=$(( (RANDOM % 40000) + 20000 ))
    rc=0
    port_busy "$p" || rc=$?
    case "$rc" in
      1) echo "$p"; return 0 ;;
      2) warn "无法检查端口：sbx-core 不存在或执行失败"; return 1 ;;
    esac
  done
  warn "未找到可用端口"
  return 1
}

public_ip() {
  if [[ -n "$ROOT" ]]; then printf '127.0.0.1\n'; return 0; fi
  local ip="" u
  # 强制 IPv4：用 -4 + 纯 IPv4 探测端点，避免返回 IPv6 地址
  for u in "https://api.ipify.org" "https://ipv4.icanhazip.com" "https://ifconfig.me/ip" "https://4.ipw.cn"; do
    ip=$(curl -q -4 -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 2 -m 3 "$u" 2>/dev/null) || continue
    # Remove trailing CR, not internal whitespace that would join tokens.
    ip=${ip%$'\r'}
    # 只接受合法 IPv4
    env_valid_ipv4 "$ip" && { printf '%s\n' "$ip"; return 0; }
    ip=""
  done
  # 兜底：本机默认路由的 IPv4 源地址
  ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}') || true
  env_valid_ipv4 "$ip" && { printf '%s\n' "$ip"; return 0; }
  # 最后再试本机第一个非回环 IPv4
  ip=$(ip -4 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1) || true
  env_valid_ipv4 "$ip" || ip=127.0.0.1
  printf '%s\n' "$ip"
}

# 探测公网 IPv6：能通过 IPv6 访问外网才算"可用"，否则视为无
public_ip6() {
  if [[ -n "$ROOT" ]]; then echo ""; return 0; fi
  local ip="" u
  for u in "https://api6.ipify.org" "https://ipv6.icanhazip.com" "https://6.ipw.cn"; do
    ip=$(curl -q -6 -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 2 -m 3 "$u" 2>/dev/null) || continue
    # Remove trailing CR, not internal whitespace that would join tokens.
    ip=${ip%$'\r'}
    # 必须是 IPv6 地址，并排除整个 fe80::/10 链路本地范围。
    if env_valid_ipv6 "$ip" && [[ "${ip,,}" != fe[89ab][0-9a-f]:* ]]; then
      echo "$ip"; return 0
    fi
    ip=""
  done
  echo ""
}

host_for_uri() {  # IPv6 需要方括号
  local h="$1"
  [[ "$h" == *:* && "$h" != \[* ]] && echo "[$h]" || echo "$h"
}

# ---------------------------------------------------------------- sing-box 安装
sb_arch() {
  case "$(uname -m)" in
    x86_64|amd64)   echo "amd64" ;;
    aarch64|arm64)  echo "arm64" ;;
    armv7l|armv7)   echo "armv7" ;;
    armv6l)         echo "armv6" ;;
    i386|i686)      echo "386" ;;
    s390x)          echo "s390x" ;;
    riscv64)        echo "riscv64" ;;
    *) die "不支持的 CPU 架构: $(uname -m)" ;;
  esac
}

gh_url() { [[ -n "$GH_PROXY" ]] && echo "${GH_PROXY%/}/$1" || echo "$1"; }

# 注：sing-box 版本改为 pin（见下方 SB_VERSION_DEFAULT + sb_expected_sha），
# 不再动态拉取 latest——动态版本无法配合 sha256 供应链校验。

# sing-box 供应链校验：官方 Release 不提供逐文件 checksum，本项目 pin 版本 + sha256。
# 升级 sing-box 版本时必须同步更新此表（下载官方包后 sha256sum）。
SB_VERSION_DEFAULT="1.14.3"
sb_expected_sha() {  # sb_expected_sha <filename> → 打印期望 sha256（无则输出空）
  awk -v n="$1" '$2==n{print $1}' <<'SBHASH'
eba8cb5fac26a7f9a03ea974dfa7334ce2f21d5d99e9bb8cbf599fb3ef00fc02  sing-box-1.14.3-linux-386-musl.tar.gz
92dad4663615d2cb30fb8feba48787e8529b1940004341712b0d0d4548cb42ff  sing-box-1.14.3-linux-386.tar.gz
41fc81fd041134d4d7d850a928d7d2cde17a7d6563953f406b22d32a68bfab1f  sing-box-1.14.3-linux-amd64-musl.tar.gz
e2bdf179c15a3652955dc44867e33ca0965c9c9b91b34267dcc5a5d639a5feee  sing-box-1.14.3-linux-amd64.tar.gz
12d6d67a4edf0dd54590865b1f4f79cd09ae043a02d648e9d1b5fccc8446ee07  sing-box-1.14.3-linux-arm64-musl.tar.gz
29dae24d76bea3bc62d07dd3e60cfc12003de7debbd6b768df9be9c68360174e  sing-box-1.14.3-linux-arm64.tar.gz
63d91abdde9b9de2d1ef9d607d17779e4d9b4c23bef0ffa06bbb8bacb82311f2  sing-box-1.14.3-linux-armv6.tar.gz
8c0cf39dc51221a9df90d432f95825a3eef25a8513cc1e80e83f23caaa84cab7  sing-box-1.14.3-linux-armv7-musl.tar.gz
8e81ee6bc1db6fec6e0342360e1de7bfabecec8d2e9c480a3563d129f84fb034  sing-box-1.14.3-linux-armv7.tar.gz
aa4e608a6379308c9ba84109512bd84fb6d0055ec616b36ea89d47b435ac3d2f  sing-box-1.14.3-linux-riscv64-musl.tar.gz
fe3620f67cac8e8d69f911116e255cb4e301fe2883cecd4b478cbe5dd97e2ba8  sing-box-1.14.3-linux-riscv64.tar.gz
6f63ecbdda61fbdd9ae4609f03a05e5d4e15b41bf08de51878e83b94967af74e  sing-box-1.14.3-linux-s390x.tar.gz
SBHASH
}

# A kernel transaction owns only this attempt's backup, never a legacy .bak.
sing_box_install_exit() {
  local rc="$1" item rollback_ok=1
  trap - EXIT
  trap '' HUP INT TERM
  if [[ "$rc" != 0 && "$replacing" == 1 ]]; then
    for item in "${targets[@]}"; do
      # A staged file still present was never renamed over the live pathname.
      [[ ! -e "$txn/new/${item##*/}" ]] || continue
      upgrade_restore_path "$txn/old/${item##*/}" "$item" || rollback_ok=0
    done
    if [[ "$restarting" == 1 && "$rollback_ok" == 1 ]]; then
      if [[ "$was_active" == 1 ]]; then
        sb_restart || rollback_ok=0
      elif ! svc_do stop sing-box || svc_do status sing-box; then
        rollback_ok=0
      fi
    fi
    if [[ "$rollback_ok" != 1 ]]; then
      err "无法确认 sing-box 回滚完成；保留备份：$txn/old"
      exit "$rc"
    fi
    warn "已恢复 sing-box 替换前的文件及服务状态"
  fi
  rm -rf -- "$txn" || warn "无法清除 sing-box 工作目录：$txn"
  exit "$rc"
}

# Mode snell also verifies Snell support and restarts the live service;
# mode stage leaves live config/service validation to the upgrade transaction.
install_sing_box() (
  local force="${1:-}" mode="${2:-}" cur_sb version local_bin=""
  # Respect an explicit pin; otherwise bring old kernels to the release baseline.
  if [[ -x "$SB_BIN" ]] && version=$("$SB_BIN" version 2>/dev/null); then
    version=${version%%$'\n'*}
    read -r _ _ cur_sb _ <<< "$version"
    if [[ -z "$force" && -z "${SB_VERSION_PIN:-}" ]] && sb_version_key "$cur_sb" >/dev/null \
       && ! sb_version_ge "$cur_sb" "$SB_VERSION_DEFAULT"; then
      info "sing-box 内核 $cur_sb 低于发布基线 $SB_VERSION_DEFAULT，自动升级..."
      force=1
    fi
    if [[ -z "$force" ]]; then
      ok "sing-box 已安装: $version"
      return 0
    fi
  elif [[ -z "$force" && -n "${SBX_SB_BIN:-}" ]]; then
    env_valid_executable "$SBX_SB_BIN" || die "SBX_SB_BIN 不再指向有效本地二进制"
    local_bin="$SBX_SB_BIN"
  fi

  local ver arch suffix tmp url name expected actual txn found lib="" item
  if [[ -z "$local_bin" ]]; then
    ver="${SB_VERSION_PIN:-$SB_VERSION_DEFAULT}"
    arch=$(sb_arch) || die "无法确定 sing-box CPU 架构"
    # Alpine 用 musl 构建，其余用默认（静态）构建。
    suffix=""
    [[ "$OS_FAMILY" == "alpine" ]] && { case "$arch" in amd64|arm64|armv7|386|riscv64) suffix="-musl";; esac; }
    name="sing-box-${ver}-linux-${arch}${suffix}.tar.gz"
    # 官方无逐文件 checksum。先查项目 pin，绝不请求未纳入校验的包。
    expected=$(sb_expected_sha "$name") || die "无法读取 sing-box 期望哈希"
    if [[ -n "$suffix" && -z "$expected" ]]; then
      suffix=""
      name="sing-box-${ver}-linux-${arch}.tar.gz"
      expected=$(sb_expected_sha "$name") || die "无法读取 sing-box 期望哈希"
    fi
    [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die "sing-box ${ver} 无有效校验哈希，拒绝安装（供应链保护）；请使用默认版本或反馈维护者补充"
  fi
  local replacing=0 restarting=0 was_active=0
  local -a targets=("$SB_BIN")
  install -d -m 0755 "$BIN_DIR" || die "无法创建 sing-box 安装目录"
  txn="$BIN_DIR/.sing-box-transaction"
  mkdir -m 0700 -- "$txn" || die "无法创建内核升级目录（如有保留的备份，请先检查 $txn）"
  trap 'sing_box_install_exit "$?"' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  tmp="$txn/download"
  mkdir -- "$tmp" "$txn/new" "$txn/old" || die "无法准备内核升级目录"

  if [[ -n "$local_bin" ]]; then
    warn "正在使用 SBX_SB_BIN 指定的本地 sing-box（不使用官方发行包校验）"
    found="$local_bin"
  else
    url="https://github.com/SagerNet/sing-box/releases/download/v${ver}/${name}"
    info "下载 sing-box v${ver} (${arch}${suffix})"
    if ! curl -q --globoff -fsSL --proto =https --proto-redir =https -m 300 -o "$tmp/sb.tar.gz" "$(gh_url "$url")"; then
      [[ -n "$suffix" ]] || die "下载 sing-box 失败，可设置 SBX_GH_PROXY 使用镜像"
      name="sing-box-${ver}-linux-${arch}.tar.gz"
      expected=$(sb_expected_sha "$name") || die "无法读取 sing-box 期望哈希"
      [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die "sing-box ${ver} 默认构建无有效校验哈希，拒绝下载"
      url="https://github.com/SagerNet/sing-box/releases/download/v${ver}/${name}"
      warn "musl 包不可用，改用默认构建"
      curl -q --globoff -fsSL --proto =https --proto-redir =https -m 300 -o "$tmp/sb.tar.gz" "$(gh_url "$url")" \
        || die "下载 sing-box 失败，可设置 SBX_GH_PROXY 使用镜像"
    fi
    if ! actual=$(sha256_of "$tmp/sb.tar.gz") || [[ "$actual" != "$expected" ]]; then
      die "sing-box 校验失败，已中止安装"
    fi
    tar xzf "$tmp/sb.tar.gz" -C "$tmp" || die "解压失败"
    found=$(find "$tmp" -type f -name sing-box -print -quit) || die "无法查找 sing-box"
    [[ -n "$found" ]] || die "压缩包中未找到 sing-box"
    lib=$(find "$tmp" -type f -name 'libcronet.so' -print -quit) || die "无法查找 libcronet.so"
  fi
  install -m 0755 -- "$found" "$txn/new/${SB_BIN##*/}" || die "无法暂存新 sing-box"
  if [[ -n "$lib" ]]; then
    install -m 0644 -- "$lib" "$txn/new/libcronet.so" || die "无法暂存 libcronet.so"
    targets+=("$BIN_DIR/libcronet.so")
  fi
  for item in "${targets[@]}"; do
    if [[ -e "$item" || -L "$item" ]]; then
      [[ -f "$item" && ! -L "$item" ]] || die "不支持以符号链接或非普通文件保存内核文件：$item"
      cp -a -- "$item" "$txn/old/${item##*/}" || die "无法备份：$item"
    fi
  done
  if [[ "$mode" == snell && -z "${SBX_NO_SERVICE:-}" ]] && svc_do status sing-box; then
    was_active=1
  fi

  # Each file is replaced by same-filesystem rename, not truncated in place.
  replacing=1
  for item in "${targets[@]}"; do
    mv -fT -- "$txn/new/${item##*/}" "$item" || die "无法替换：$item"
  done
  version=$("$SB_BIN" version 2>/dev/null) || die "新 sing-box 无法运行（架构或 libc 不匹配）"
  if [[ "$mode" == snell ]] && ! sb_supports_snell "$version"; then
    die "升级后 sing-box 仍不支持 Snell，已中止创建"
  fi
  if [[ "$mode" == snell || ( "$mode" != stage && -f "$SB_CONF" ) ]]; then
    "$SB_BIN" check -c "$SB_CONF" >/dev/null 2>&1 || die "新 sing-box 配置校验失败（原配置未动）"
  fi
  if [[ "$mode" == snell && -z "${SBX_NO_SERVICE:-}" ]]; then
    restarting=1
    sb_restart || die "新 sing-box 服务重启失败"
  fi
  ok "sing-box 安装完成: ${version%%$'\n'*}"
)

# sb_supports_snell 判断当前 sing-box 是否支持 Snell（>= 1.14.0）。
sb_supports_snell() {
  local output="${1:-}" label marker v key a b c
  if (($# == 0)); then output=$("$SB_BIN" version 2>/dev/null) || return 1; fi
  read -r label marker v _ <<< "$output"
  [[ "$marker" == version ]] || return 1
  key=$(sb_version_key "$v") || return 1
  # Snell's existing policy compares release components, including prereleases.
  read -r a b c _ <<< "$key"
  (( a > 1 || (a == 1 && b >= 14) ))
}

# ---------------------------------------------------------------- sbx-core 安装
install_sbx_core() (
  local arch name tmp dl cand_ver output
  if [[ -n "${SBX_CORE_BIN:-}" ]]; then
    env_valid_executable "$SBX_CORE_BIN" || die "SBX_CORE_BIN 不再指向有效本地二进制"
  else
    [[ "$DIST_SHA" =~ ^[0-9a-f]{40}$ && -n "$DIST_BASE" ]] \
      || die "缺少固定分发提交，拒绝下载 sbx-core"
    arch=$(sb_arch) || die "无法确定 sbx-core CPU 架构"
    name="sbx-core-linux-${arch}"
  fi
  install -d -m 0755 "$BIN_DIR" || die "无法创建 sbx-core 安装目录"
  # 私有工作目录与目标同文件系统，发布时用 rename 原子替换。
  tmp=$(mktemp -d "$BIN_DIR/.sbx-core.XXXXXXXX") || die "无法创建 sbx-core 工作目录"
  trap 'rm -rf -- "$tmp" || warn "无法清除 sbx-core 工作目录：$tmp"' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM

  # 开发/测试：直接使用本地构建的二进制
  if [[ -n "${SBX_CORE_BIN:-}" ]]; then
    warn "正在使用 SBX_CORE_BIN 指定的本地 sbx-core（不使用官方发行包校验）"
    if ! install -m 0755 -- "$SBX_CORE_BIN" "$tmp/sbx-core" ||
       ! mv -fT -- "$tmp/sbx-core" "$CORE_BIN"; then
      die "本地 sbx-core 拷贝失败；现有安装未被改动"
    fi
    ok "使用本地 sbx-core: $("$CORE_BIN" version 2>/dev/null | head -1)"
    return 0
  fi
  dl="$tmp/$name"
  # dist 是 rolling latest：先取固定提交的 SHA256SUMS；内容及版本一致才跳过下载。
  # Only binary bytes can be mirrored. The official immutable manifest
  # authenticates those bytes, even if a custom mirror serves a rolling URL.
  local base="${SBX_RAW_BASE:-$DIST_BASE}"
  curl -q --globoff -fsS --proto =https -m 60 -o "$tmp/SHA256SUMS" "$DIST_BASE/SHA256SUMS" \
    || die "从固定官方提交下载 SHA256SUMS 失败"
  if [[ -x "$CORE_BIN" ]] && verify_core_checksum "$CORE_BIN" "$tmp/SHA256SUMS" "$name" 2>/dev/null \
     && [[ "$(core_version_of "$CORE_BIN")" == "$APP_VERSION" ]]; then
    ok "sbx-core 已是最新: $("$CORE_BIN" version 2>/dev/null | head -1)"
    return 0
  fi

  info "下载 sbx-core (${arch})..."
  curl -q --globoff -fsSL --proto =https --proto-redir =https -m 300 -o "$dl" "$(gh_url "${base%/}/${name}")" \
    || die "下载 sbx-core 失败（可设置 SBX_GH_PROXY 使用镜像）；现有安装未被改动"
  verify_core_checksum "$dl" "$tmp/SHA256SUMS" "$name" \
    || die "sbx-core 校验失败；现有安装未被改动"
  chmod 0755 "$dl" || die "无法设置 sbx-core 执行权限；现有安装未被改动"
  # 必须能执行，且 candidate 版本与安装器 APP_VERSION 严格一致。
  cand_ver=$(core_version_of "$dl") \
    || die "sbx-core 自检失败（架构或 libc 不匹配）；现有安装未被改动"
  if [[ "$cand_ver" != "$APP_VERSION" ]]; then
    die "sbx-core 版本不匹配，已拒绝安装（现有安装未被改动）
  期望: $APP_VERSION    实际: ${cand_ver:-无法解析}
  提示: dist 分支产物可能尚未同步，请稍后重试或反馈维护者"
  fi

  # 备份只属于本次安装，禁止回滚至上次遗留的 .bak。
  if [[ -x "$CORE_BIN" ]]; then
    cp -p -- "$CORE_BIN" "$tmp/backup" || die "旧 sbx-core 备份创建失败，已中止安装（现有安装未被改动）"
  fi
  mv -fT -- "$dl" "$CORE_BIN" || die "无法替换 sbx-core"
  if ! output=$("$CORE_BIN" version 2>/dev/null); then
    if [[ -f "$tmp/backup" ]]; then
      if mv -fT -- "$tmp/backup" "$CORE_BIN"; then
        warn "新二进制异常，已回滚"
      else
        trap - EXIT
        err "无法恢复旧 sbx-core；保留备份：$tmp/backup"
      fi
    fi
    die "sbx-core 安装后自检失败"
  fi
  ok "sbx-core 安装完成: ${output%%$'\n'*}"
)

# ---------------------------------------------------------------- 目录与基础配置
prepare_dirs() {
  install -d -m 0755 "$APP_DIR" "$WEB_DIR" "$SB_DIR"
  install -d -m 0700 "$CERT_DIR"
  # nodes.json/state.json 含节点凭据（UUID/密码/Reality 私钥）：
  # 首次创建用 install -m 0600 预建空文件（不受 umask 影响）再写入内容；
  # 已存在的旧文件若权限过宽（历史安装/手工操作），统一收紧到 0600。
  if [[ ! -f "$NODES_JSON" ]]; then
    install -m 0600 /dev/null "$NODES_JSON"
    printf '[]\n' > "$NODES_JSON"
  fi
  if [[ ! -f "$STATE_JSON" ]]; then
    install -m 0600 /dev/null "$STATE_JSON"
    printf '{}\n' > "$STATE_JSON"
  fi
  chmod 600 "$NODES_JSON" "$STATE_JSON"
}

ensure_sb_config() {
  [[ -f "$SB_CONF" ]] && return 0
  # 先以 0600 预创建（不受 umask 影响）：后续写入的节点配置含 Reality 私钥等凭据
  install -m 0600 /dev/null "$SB_CONF"
  cat > "$SB_CONF" <<'EOF'
{
  "log": { "level": "warn", "timestamp": true },
  "dns": {
    "servers": [{ "type": "local", "tag": "local" }]
  },
  "inbounds": [],
  "outbounds": [{ "type": "direct", "tag": "direct" }],
  "route": { "final": "direct" }
}
EOF
  chmod 600 "$SB_CONF"
  ok "已生成 sing-box 基础配置"
}

ensure_panel_conf() {
  [[ ! -L "$PANEL_CONF" ]] || { err "拒绝使用符号链接作为面板配置"; return 1; }
  if [[ -f "$PANEL_CONF" ]]; then
    # 保留/补齐面板查看令牌（由 sbx-core 原子写回）。
    # 配置存在但损坏时 config-ensure-token 会 fail-closed——
    # 此时必须中止安装并让用户手工修复，绝不吞掉错误继续，
    # 更不允许用 defaults 覆盖原文件。
    if ! "$CORE_BIN" config-ensure-token >/dev/null; then
      err "panel.json 无法读取或配置已损坏（$PANEL_CONF）"
      info "请手工修复该文件后重新运行安装；原文件未被改动"
      return 1
    fi
    # nftables-only 迁移（v3.0.9）：摘掉老版本写入的废弃键 backend / ipt_script。
    # 只删这两个键（读取-改-原子写回），token/port/db/nodes_file/tz 与任何
    # 用户自定义键一律原样保留；失败只告警——废弃键存在不影响新版运行。
    "$CORE_BIN" config-migrate >/dev/null 2>&1 \
      || warn "清理废弃配置键失败（不影响运行，可稍后手工执行 sbx-core config-migrate）"
    chmod 600 "$PANEL_CONF" || { err "无法限制面板配置权限"; return 1; }
    return 0
  fi
  local port
  [[ ! -e "$PANEL_CONF" && ! -L "$PANEL_CONF" ]] || { err "面板配置路径不是普通文件"; return 1; }
  port=$(pick_port) || { err "无法分配面板端口"; return 1; }
  if [[ ! "$port" =~ ^[0-9]{1,5}$ ]] || ((10#$port < 1 || 10#$port > 65535)); then
    err "面板端口必须为 1–65535 的整数"
    return 1
  fi
  port=$((10#$port))
  # jq is optional: without it, the existing backend marshals its defaults.
  # Generate the token via the backend in either case, never in process argv.
  (
    umask 077
    local stage conf
    stage=$(mktemp -d "${PANEL_CONF%/*}/.panel-config.XXXXXXXX") || exit 1
    trap 'rm -rf -- "$stage"' EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM
    conf="$stage/panel.json"
    export SBX_CONF="$conf"
    if command -v jq >/dev/null 2>&1; then
      jq -n --arg db "$APP_DIR/traffic.db" --arg nodes "$NODES_JSON" \
        --arg nft "$APP_DIR/nft.conf" --arg web "${WEB_DIR:-$APP_DIR/web}" --argjson port "$port" \
        '{db:$db,nodes_file:$nodes,nft_conf:$nft,web_root:$web,listen:"0.0.0.0",port:$port,interval:2,tz:"Asia/Shanghai"}' > "$conf" || exit 1
      [[ -s "$conf" ]] || exit 1
    else
      "$CORE_BIN" config-set port "$port" >/dev/null || exit 1
    fi
    "$CORE_BIN" config-ensure-token >/dev/null || exit 1
    chmod 600 "$conf" || exit 1
    # Publish the finished file without overwriting anything created concurrently.
    ln -T -- "$conf" "$PANEL_CONF" || exit 1
  ) || { err "无法安全创建面板配置；未覆盖原文件"; return 1; }
  ok "面板配置已生成（端口 $port）"
}

panel_get() { "$CORE_BIN" config-get "$1"; }
panel_set() { "$CORE_BIN" config-set "$1" "$2"; }

# ---------------------------------------------------------------- 服务管理
sbx_host_runtime() { [[ -z "${ROOT:-}" && -z "${SBX_NO_SERVICE:-}" ]]; }

svc_do() {  # svc_do <start|stop|restart|enable|status> <name>
  local action="$1" name="$2"
  if ! sbx_host_runtime; then
    # 沙箱中没有运行的服务；变更服务状态的操作是空操作。
    [[ -z "${ROOT:-}" || "$action" != status ]]
    return $?
  fi
  case "$INIT_SYS" in
    systemd)
      case "$action" in
        enable) systemctl enable "$name" >/dev/null 2>&1 ;;
        status) systemctl is-active --quiet "$name" ;;
        *) systemctl "$action" "$name" >/dev/null 2>&1 ;;
      esac ;;
    openrc)
      case "$action" in
        enable) rc-update add "$name" default >/dev/null 2>&1 ;;
        status) rc-service "$name" status >/dev/null 2>&1 ;;
        *) rc-service "$name" "$action" >/dev/null 2>&1 ;;
      esac ;;
    *) return 1 ;;
  esac
}

sb_running()    { svc_do status sing-box; }
panel_running() { svc_do status sbx-panel; }

sb_restart() {
  svc_do restart sing-box || { warn "sing-box 重启失败，查看日志：sbx 菜单 → 查看日志"; return 1; }
  sleep 1
  sb_running || { warn "sing-box 未处于运行状态"; return 1; }
  return 0
}

# ---------------------------------------------------------------- 证书
ensure_certs() {
  local sni="${1:-www.bing.com}"
  local cert="$CERT_DIR/cert.pem" key="$CERT_DIR/key.pem" mode tmp first second
  # 已有密钥也要校验，且不能 chmod 符号链接指向的文件。
  [[ -d "$CERT_DIR" && ! -L "$CERT_DIR" && -O "$CERT_DIR" ]] \
    || die "证书目录无效：$CERT_DIR"
  mode=$(stat -c %a -- "$CERT_DIR") || die "无法检查证书目录权限"
  if ! [[ "$mode" =~ ^[0-7]{3,4}$ ]] || (( 8#$mode & 022 )); then
    die "证书目录不能由组或其他用户写入：$CERT_DIR"
  fi
  (
    umask 077 || die "无法设置证书生成权限掩码"
    # 用目录本身加锁，不创建可被替换的锁文件；两个 PEM 需成对生成。
    exec 8<"$CERT_DIR" || die "无法打开证书目录"
    flock -x 8 || die "无法锁定证书目录"
    local item
    for item in "$cert" "$key"; do
      [[ ! -L "$item" && ( ! -e "$item" || -f "$item" ) ]] \
        || die "证书文件路径无效：$item"
    done
    if [[ -f "$key" ]]; then
      chmod 600 -- "$key" && [[ $(stat -c %a -- "$key") == 600 ]] \
        || die "无法将现有私钥权限设为 600：$key"
    fi
    [[ -s "$cert" && -s "$key" ]] && exit 0

    info "生成自签证书 (CN=$sni)"
    tmp=$(mktemp -d -- "$CERT_DIR/.sbx-certs.XXXXXXXX") || die "无法创建证书暂存目录"
    trap 'rm -rf -- "$tmp" || warn "无法清理证书暂存目录：$tmp"' EXIT
    trap 'exit 1' HUP INT TERM
    # 先验证空文件的权限，再写入 PEM。私钥也不经过 shell 变量。
    touch -- "$tmp/cert.pem" "$tmp/key.pem" \
      && chmod 600 -- "$tmp/key.pem" && [[ $(stat -c %a -- "$tmp/key.pem") == 600 ]] \
      || die "无法安全暂存私钥（需要权限 600）"
    set -o pipefail
    if ! "$SB_BIN" generate tls-keypair "$sni" -m 1200 2>/dev/null | (
      cd -- "$tmp" || exit 1
      awk '
        /-----BEGIN CERTIFICATE-----/ {inc=1}
        inc {print > "cert.pem"}
        /-----END CERTIFICATE-----/ && inc {inc=0; cert=1}
        /-----BEGIN PRIVATE KEY-----/ {ink=1}
        ink {print > "key.pem"}
        /-----END PRIVATE KEY-----/ && ink {ink=0; key=1}
        END {if (!cert || !key || inc || ink) exit 1}
      '
    ); then
      die "证书生成或 PEM 解析失败"
    fi
    [[ -s "$tmp/cert.pem" && -s "$tmp/key.pem" ]] || die "证书或私钥为空"
    chmod 644 -- "$tmp/cert.pem" || die "无法设置公开证书权限"
    # 最后替换原先缺失/为空的文件，第二次移动失败时不会误认成一对可用证书。
    first=cert.pem second=key.pem
    if [[ -s "$key" ]]; then first=key.pem second=cert.pem; fi
    mv -fT -- "$tmp/$first" "$CERT_DIR/$first" \
      && mv -fT -- "$tmp/$second" "$CERT_DIR/$second" || die "无法安装证书或私钥"
    ok "证书就绪"
  ) || exit 1
}

# ---------------------------------------------------------------- 旧版残留清理
# >>> legacy-cleanup
# cleanup_legacy_backend 清理 **旧版 SBX（≤ v3.0.8）** 留下的 iptables 残留。
#
# 重要：这不是「继续支持 iptables 后端」。v3.0.9 起 SBX 是 nftables-only，
# 这段代码的唯一目的是把旧版自己创建的东西删干净，避免升级后无人管理的
# 计数链永久残留在内核里。做完即止，运行期不会再碰 iptables。
#
# 安全边界（绝对不可放宽）：
#   · 只删 SBX 自己创建的自定义链 SBX_IN / SBX_OUT 及其 INPUT/OUTPUT 跳转；
#   · 绝不 flush INPUT/OUTPUT/filter 表，绝不改默认 policy，
#     绝不触碰其它程序或用户自己的链与规则；
#   · iptables/ip6tables 命令不存在时直接跳过（新机器的正常情况）；
#   · 全程 best-effort：失败只告警，绝不影响 nftables 规则与安装流程。
cleanup_legacy_backend() {
  # 1) 旧版生成的 iptables 规则脚本（只删 SBX 自己的路径，不扫描其它目录）
  if [[ -f "$APP_DIR/iptables.sh" ]]; then
    if rm -f "$APP_DIR/iptables.sh"; then
      info "已删除旧版残留文件 $APP_DIR/iptables.sh"
    else
      warn "删除 $APP_DIR/iptables.sh 失败（可手工删除）"
    fi
  fi

  # 前缀不隔离网络命名空间，不能清理当前内核里的链。
  [[ -z "${ROOT:-}" && -z "${SBX_NO_SERVICE:-}" ]] || return 0
  # 2) 旧版创建的自定义计数链（v4/v6 各一套）
  local bin found=0
  for bin in iptables ip6tables; do
    command -v "$bin" >/dev/null 2>&1 || continue
    local chain
    for chain in SBX_IN SBX_OUT; do
      # 链不存在 → -S 非 0，直接跳过（不产生任何修改）
      "$bin" -w -S "$chain" >/dev/null 2>&1 || continue
      found=1
      # 先摘 INPUT/OUTPUT 里指向该链的跳转（只删这一条 -j <chain> 规则）
      case "$chain" in
        SBX_IN)  "$bin" -w -D INPUT  -j "$chain" >/dev/null 2>&1 || true ;;
        SBX_OUT) "$bin" -w -D OUTPUT -j "$chain" >/dev/null 2>&1 || true ;;
      esac
      # 再清空并删除这条自定义链本身（-F/-X 的作用域仅限该链）
      "$bin" -w -F "$chain" >/dev/null 2>&1 || true
      "$bin" -w -X "$chain" >/dev/null 2>&1 \
        || warn "删除旧版 $bin 链 $chain 失败（可手工执行 $bin -X $chain）"
    done
  done
  [[ "$found" == 1 ]] && info "已清理旧版 iptables 计数链（SBX_IN / SBX_OUT）"
  return 0
}
# <<< legacy-cleanup

# ---------------------------------------------------------------- 流量规则
fw_apply() {
  sbx_host_runtime || return 0
  # apply 失败必须向上传递：不能 warn（rc=0）把失败伪装成成功
  if "$CORE_BIN" apply; then
    return 0
  fi
  warn "计数规则应用失败，流量统计可能不准"
  return 1
}
fw_clear() {
  # Full uninstall only; callers must stop both listeners successfully first.
  "$CORE_BIN" clear --uninstall
}

# ---------------------------------------------------------------- 添加节点
prompt_port() {
  local label="$1" def="$2" p rc
  while :; do
    printf '%s端口 (%s，回车用 %s，q 取消): %s' "$C_DIM" "$label" "$def" "$C_RESET" >&2
    read -r p || { echo; return 1; }
    [[ "$p" == "q" || "$p" == "Q" ]] && { echo; return 1; }
    p="${p:-$def}"
    if ! valid_port "$p"; then warn "端口需在 1-65535"; continue; fi
    rc=0
    node_port_busy "$p" || rc=$?
    case "$rc" in
      0) warn "端口 $p 已被其它节点使用"; continue ;;
      2) warn "无法检查端口：sbx-core 不存在或执行失败"; return 1 ;;
    esac
    rc=0
    port_busy "$p" || rc=$?
    if [[ "$rc" == 2 ]]; then warn "无法检查端口 $p"; return 1; fi
    if [[ "$rc" == 0 ]]; then
      printf '%s端口 %s 已被系统其它进程监听，仍要使用? [y/N] %s' "$C_YEL" "$p" "$C_RESET" >&2
      read -r yn || true
      [[ "${yn,,}" == "y" ]] || continue
    fi
    echo "$p"; return 0
  done
}

prompt_name() {
  local def="$1" n
  printf '%s节点备注 (回车用 %s，q 取消): %s' "$C_DIM" "$def" "$C_RESET" >&2
  read -r n || { echo; return 1; }
  [[ "$n" == "q" || "$n" == "Q" ]] && { echo; return 1; }
  echo "${n:-$def}"
}

sbx_valid_sni() {
  local LC_ALL=C label='[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?'
  [[ ${#1} -le 253 && "$1" =~ ^$label(\.$label)*\.[A-Za-z]{2,63}$ ]]
}

prompt_sni() {
  local def="${1:-www.microsoft.com}" s
  printf '%s伪装域名 SNI (回车用 %s，q 取消): %s' "$C_DIM" "$def" "$C_RESET" >&2
  read -r s || { echo; return 1; }
  [[ "$s" == "q" || "$s" == "Q" ]] && { echo; return 1; }
  s="${s:-$def}"
  sbx_valid_sni "$s" || { warn "域名格式无效，使用 $def"; s="$def"; }
  echo "$s"
}

# 选择 Shadowsocks 2022 加密算法：1=128(默认) 2=256；q 取消。
prompt_ss2022_method() {
  local choice
  printf '%sShadowsocks 2022 加密算法%s\n' "$C_B" "$C_RESET" >&2
  printf '  1. 2022-blake3-aes-128-gcm（默认）\n' >&2
  printf '  2. 2022-blake3-aes-256-gcm\n' >&2
  while true; do
    printf '请选择 [1-2，默认 1，q 取消]: ' >&2
    read -r choice || { echo; return 1; }
    case "${choice:-1}" in
      1|"") echo "2022-blake3-aes-128-gcm"; return 0 ;;
      2) echo "2022-blake3-aes-256-gcm"; return 0 ;;
      q|Q) echo; return 1 ;;
      *) warn "无效选择，请输入 1 或 2"; ;;
    esac
  done
}

# ---- 节点 mutation 全局锁（跨进程，flock）----
# 覆盖「生成 candidate → 校验 → 备份 → 提交 → 重启 → fw_apply」整个事务，
# 防止两个 SSH 同时 node add/edit/remove/sync 产生 lost update / 重复 ID /
# candidate 串写。锁文件默认 /run/lock（tmpfs，重启清空），测试可经 SBX_LOCK 覆盖。
export SBX_LOCK="${SBX_LOCK:-$ROOT/run/lock/sbx.lock}"

# sbx_lock <func> [args...]：在排他锁内执行函数；获取失败报错并返回非 0。
sbx_lock() {
  mkdir -p "$(dirname "$SBX_LOCK")" 2>/dev/null || true
  (
    command -v flock >/dev/null || die "缺少 flock；请先安装提供 flock 的软件包"
    flock -n 9 || { err "检测到另一个 sbx 节点操作正在进行，请稍后重试"; exit 75; }
    # 告知 sbx-core 本进程已持锁，Go 侧跳过自锁（否则会对自己死锁等待）
    export SBX_LOCK SBX_LOCK_HELD=1
    "$@"
  ) 9>>"$SBX_LOCK"
}

# >>> commit-node-flow（tests/commit_flow_test.sh 提取本段做回滚一致性测试）
# sanitize_candidate_route <candidate_file>
# P0 修复：候选配置 route.final 引用完整性自检。
#   sbx-core 在原 config.json 缺 route 段时会注入 route.final="direct"，但
#   outbounds 里 direct 出站的 tag 未必叫 "direct"（例如发行版包/手工配置常用
#   "direct-out"）。此时 final 指向一个不存在的 tag，而 `sing-box check` 并不校验
#   该引用（返回 0），故障要到 restart 才以
#     FATAL start service: default outbound not found: direct
#   暴露 —— 表现为"配置校验通过但 sing-box 启动失败"并触发整体回滚，节点永远加不上。
#   这里在提交前把 final 校正为真实存在的 direct 出站 tag（没有可用 direct 出站时
#   删除 final，交回 sing-box 自身默认行为）。
# jq 不可用时交由后端的强制校验；已执行的检查或安全写入失败则中止提交。
sanitize_candidate_route() {
  local cand="$1" verb tag parent mode tmp
  if [[ -L "$cand" || ! -f "$cand" ]]; then
    warn "候选配置不是普通文件，已拒绝提交"
    return 1
  fi
  command -v jq >/dev/null 2>&1 || {
    warn "未找到 jq，跳过候选配置 route.final 引用自检"
    return 0
  }
  # 阶段一：只判定，不写盘。final 缺失或已指向存在的 tag → 原样放行（字节不变）。
  verb=$(jq -r '
      (.outbounds // []) as $obs
    | ($obs | map(.tag? // empty)) as $tags
    | (.route?.final? // null) as $fin
    | if $fin == null or ($tags | index($fin)) != null then "ok"
      elif (($obs | map(select(.type? == "direct")) | first | .tag?) // null) != null then "set"
      else "del" end
  ' "$cand" 2>/dev/null) || {
    warn "候选配置 route.final 自检失败（JSON 解析异常），已拒绝提交"
    return 1
  }
  case "$verb" in
    ok) return 0 ;;
    set|del) ;;
    *) warn "候选配置 route.final 自检结果异常，已拒绝提交"; return 1 ;;
  esac
  # 同目录暂存以便用已验证权限的文件原子替换；任何失败都保留原候选配置。
  if ! (
    umask 077 || { warn "无法设置候选配置权限掩码"; exit 1; }
    if ! parent=$(dirname -- "$cand") || [[ ! -O "$parent" || -L "$parent" ]] \
      || ! mode=$(stat -c %a -- "$parent") || [[ ! "$mode" =~ ^[0-7]{3,4}$ ]] \
      || (( 8#$mode & 022 )); then
      warn "候选配置目录权限不安全，已跳过自检"
      exit 1
    fi
    tmp=$(mktemp -d -- "$parent/.sbx-route.XXXXXXXX") \
      || { warn "无法创建候选配置暂存目录，已跳过自检"; exit 1; }
    trap 'rm -rf -- "$tmp" || warn "无法清理候选配置暂存目录：$tmp"' EXIT
    trap 'exit 1' HUP INT TERM
    if ! touch -- "$tmp/config" || ! chmod 600 -- "$tmp/config" \
      || [[ $(stat -c %a -- "$tmp/config") != 600 ]]; then
      warn "无法安全暂存候选配置（需要权限 600），已跳过自检"
      exit 1
    fi
    if ! jq '
        (.outbounds // []) as $obs
      | (($obs | map(select(.type? == "direct")) | first | .tag?) // null) as $d
      | if $d != null then .route.final = $d else del(.route.final) end
    ' "$cand" > "$tmp/config" 2>/dev/null; then
      warn "候选配置 route.final 改写失败，已跳过自检"
      exit 1
    fi
    [[ -s "$tmp/config" ]] || { warn "候选配置 route.final 改写结果为空，已跳过自检"; exit 1; }
    mv -fT -- "$tmp/config" "$cand" \
      || { warn "候选配置 route.final 写回失败，已跳过自检"; exit 1; }
  ); then
    return 1
  fi
  if [[ "$verb" == "set" ]]; then
    tag=$(jq -r '.route.final // "?"' "$cand" 2>/dev/null) || tag="?"
    info "已校正候选配置 route.final → ${tag}（原值指向不存在的出站 tag）"
  else
    info "已移除候选配置悬空的 route.final（无可用 direct 出站）"
  fi
  return 0
}

commit_node() {
  if ! sbx_host_runtime; then
    warn "沙箱或仅配置模式不提交会触及宿主机的节点事务"
    return 1
  fi
  # Preparation and commit share FD 9. The backend owns validation, access
  # isolation, transactional backups, sing-box restart and recovery.
  if [[ -e "$SB_CONF.candidate" || -L "$SB_CONF.candidate" ]]; then
    sanitize_candidate_route "$SB_CONF.candidate" || return 1
  fi
  core_node commit
}
# <<< commit-node-flow

sbx_add_node_commit() {
  local label="$1" rc=0
  shift
  sbx_lock "$@" || rc=$?
  case "$rc" in
    0) ok "$label 节点已添加"; show_links_for_last ;;
    2) warn "节点已创建，但流量统计规则应用失败"; show_links_for_last ;;
    *) warn "节点添加失败" ;;
  esac
}

add_vless() {
  local port name sni uuid kp priv pub sid
  port=$(prompt_port "VLESS Reality" 443) || return 0
  name=$(prompt_name "vless-$port") || return 0
  sni=$(prompt_sni www.microsoft.com) || return 0
  uuid=$(rand_uuid) || { warn "生成 UUID 失败"; return 1; }
  kp=$("$SB_BIN" generate reality-keypair) || { warn "生成 Reality 密钥失败"; return 1; }
  priv=$(echo "$kp" | awk '/PrivateKey/{print $2}')
  pub=$(echo "$kp" | awk '/PublicKey/{print $2}')
  [[ -n "$priv" && -n "$pub" ]] || { warn "Reality 密钥输出无效"; return 1; }
  sid=$(rand_hex 8) || { warn "生成 Short ID 失败"; return 1; }
  sbx_add_node_commit "VLESS Reality" do_add_vless "$port" "$name" "$sni" "$uuid" "$priv" "$pub" "$sid"
}
do_add_vless() {
  core_node add vless --port="$1" --name="$2" --uuid="$4" --sni="$3" \
    --flow xtls-rprx-vision --private-key="$5" --public-key="$6" --short-id="$7" >/dev/null || return 1
  commit_node
}

add_ss() {
  local port name method pw
  method=$(prompt_ss2022_method) || return 0
  local default_port
  default_port=$(pick_port) || return 1
  port=$(prompt_port "Shadowsocks 2022" "$default_port") || return 0
  name=$(prompt_name "ss-$port") || return 0
  pw=$(core_node ss2022-key --method="$method") || { warn "生成 Shadowsocks 2022 密钥失败"; return 1; }
  sbx_add_node_commit "Shadowsocks 2022" do_add_ss "$port" "$name" "$method" "$pw"
}
do_add_ss() {
  core_node add shadowsocks --port="$1" --name="$2" --method="$3" --password="$4" >/dev/null || return 1
  commit_node
}

add_trojan() {
  local port name sni pw
  port=$(prompt_port "Trojan" 8443) || return 0
  name=$(prompt_name "trojan-$port") || return 0
  sni=$(prompt_sni www.bing.com) || return 0
  ensure_certs "$sni"
  pw=$(rand_hex 12)
  sbx_add_node_commit "Trojan" do_add_trojan "$port" "$name" "$sni" "$pw"
}
do_add_trojan() {
  core_node add trojan --port="$1" --name="$2" --password="$4" --sni="$3" >/dev/null || return 1
  commit_node
}

add_anytls() {
  local port name sni pw
  local default_port
  default_port=$(pick_port) || return 1
  port=$(prompt_port "AnyTLS" "$default_port") || return 0
  name=$(prompt_name "anytls-$port") || return 0
  sni=$(prompt_sni www.bing.com) || return 0
  ensure_certs "$sni"
  pw=$(rand_hex 12)
  sbx_add_node_commit "AnyTLS" do_add_anytls "$port" "$name" "$sni" "$pw"
}
do_add_anytls() {
  core_node add anytls --port="$1" --name="$2" --password="$4" --sni="$3" >/dev/null || return 1
  commit_node
}

# Snell：先确保内核 >= 1.14.0（不满足则按需升级），再创建
add_snell() {
  local ver="$1"
  # 内核版本检测：Snell 需要 sing-box >= 1.14.0
  if ! sb_supports_snell; then
    warn "当前 sing-box 版本不支持 Snell，Snell 需要 sing-box >= 1.14.0"
    info "正在按现有机制升级 sing-box 内核……"
    if ! sbx_lock install_sing_box force snell; then
      err "sing-box 内核升级失败，已中止创建 Snell"
      return 1
    fi
  fi

  local port name psk extra
  local default_port
  default_port=$(pick_port) || return 1
  port=$(prompt_port "Snell v$ver" "$default_port") || return 0
  name=$(prompt_name "snell-v$ver-$port") || return 0
  psk=$("$CORE_BIN" secret hex 32) || { warn "生成 PSK 失败"; return 1; }
  extra=()
  if [[ "$ver" == 5 ]]; then
    extra=(--obfs-mode="none")
  else
    extra=(--mode="default")
  fi

  sbx_add_node_commit "Snell v$ver" do_add_snell "$ver" "$port" "$name" "$psk" "${extra[@]}"
}
do_add_snell() {
  local ver="$1" port="$2" name="$3" psk="$4"; shift 4
  core_node add snell --port="$port" --name="$name" --version="$ver" --psk="$psk" "$@" >/dev/null || return 1
  commit_node
}

show_links_for_last() {
  local last host6
  last=$(core_node last)
  [[ -z "$last" ]] && return 0
  host6=$(core_node get-host6)
  hr
  if [[ -n "$host6" ]]; then
    core_node links "$last" --host "$(core_node get-host)" --host6 "$host6"
  else
    core_node links "$last" --host "$(core_node get-host)"
  fi
}

# ---------------------------------------------------------------- 服务单元
# >>> dns-guard（tests/installer_flow_test.sh 提取本区块做隔离测试）
# fix_resolv_conf_misdetection —— 修掉让 Go 程序 DNS 全废的 resolv.conf 误判。
#
# 真机事故：节点能连上、TLS 握手正常，但打开任何网站都失败。sing-box 日志：
#   open connection to www.google.com:443 using outbound/direct: lookup www.google.com:
#   (exchange6: Unit dbus-org.freedesktop.resolve1.service not found. | exchange4: ...)
#
# 根因：Go 的 net 解析器判定"系统是否在用 systemd-resolved"是**对
# /etc/resolv.conf 做子串匹配**（找 "127.0.0.53"），并不解析注释。而 Debian 的
# resolvconf 包在 /etc/resolvconf/resolv.conf.d/head 里固定写着
#   # 127.0.0.53 is the systemd-resolved stub resolver.
# 于是即使实际 nameserver 完全正常、systemd-resolved 根本没装，Go 也会把所有
# 查询走 D-Bus，直接报错。curl/getent（glibc）不受影响，所以表现为
# "服务器自己 curl 谷歌正常，但走节点的流量全挂"——极难排查。
#
# 处理原则（保守）：
#   · systemd-resolved 真在跑 → 127.0.0.53 是合法 stub，什么都不动；
#   · 只删"提到 127.0.0.53 但不是 nameserver 行"的内容（注释/模板），
#     绝不删真正的 nameserver 行（那会直接断网）；
#   · 只改符号链接指向的真实文件（sed -i 直接作用在 /etc/resolv.conf 这个
#     符号链接上会把链接替换成普通文件，破坏 resolvconf 的托管）。
# 在沙箱中，不跟随指向前缀以外的 resolver 链接，也不修改前缀外的覆盖路径。
sbx_sandbox_resolver_path() {
  local base real
  base=$(readlink -f -- "$ROOT") && [[ "$base" != / ]] || return 1
  real=$(readlink -f -- "$1") && [[ "$real" == "$base/"* && -f "$real" ]] || return 1
  printf '%s\n' "$real"
}

fix_resolv_conf_misdetection() {
  # 正常安装时，systemd-resolved 正在运行 → 不做任何改动。
  if [[ -z "${ROOT:-}" && -z "${SBX_NO_SERVICE:-}" ]] \
    && command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet systemd-resolved 2>/dev/null; then
    return 0
  fi

  local fixed=0 head="${SBX_RESOLVCONF_HEAD:-${ROOT:-}/etc/resolvconf/resolv.conf.d/head}"
  local resolv="${SBX_RESOLV_CONF:-${ROOT:-}/etc/resolv.conf}" real
  if [[ -n "${ROOT:-}" ]]; then
    head=$(sbx_sandbox_resolver_path "$head" 2>/dev/null) || head=""
    real=$(sbx_sandbox_resolver_path "$resolv" 2>/dev/null) || real=""
  else
    # 当前生效的文件：跟随符号链接改真实文件。
    real="$(readlink -f "$resolv" 2>/dev/null || true)"
    [[ -n "$real" && -f "$real" ]] || real="$resolv"
  fi
  # 1) 源头：resolvconf 的 head 模板（改了它，重启/网络变化后也不会再生成回来）
  if [[ -f "$head" ]] && grep -q '127\.0\.0\.53' "$head"; then
    sed -i '/127\.0\.0\.53/d' "$head" && fixed=1
  fi
  # 2) 当前生效的文件：只移除误导注释，保留真正的 stub nameserver。
  if [[ -f "$real" ]] && grep -q '127\.0\.0\.53' "$real"; then
    # 仅当它**不是**真正的 nameserver 行时才删
    if ! grep -qE '^[[:space:]]*nameserver[[:space:]]+127\.0\.0\.53([[:space:]]|$)' "$real"; then
      sed -i '/127\.0\.0\.53/d' "$real" && fixed=1
    fi
  fi

  if [[ "$fixed" == 1 ]]; then
    # 让 resolvconf 用新模板重新生成（失败也不影响：文件本身已经改好了）
    if [[ -z "${ROOT:-}" && -z "${SBX_NO_SERVICE:-}" ]] && command -v resolvconf >/dev/null 2>&1; then
      resolvconf -u >/dev/null 2>&1 || true
    fi
    warn "已修掉 /etc/resolv.conf 里会误导 Go 解析器的 systemd-resolved 注释"
    warn "  原因：Go 对 resolv.conf 做子串匹配判断 systemd-resolved，"
    warn "        注释里的 127.0.0.53 会让 sing-box 的 DNS 全部走 D-Bus 而失败"
    warn "        （表现为：节点连得上但打不开任何网站）"
  fi
  return 0
}
# <<< dns-guard

setup_services() {
  local mode="${1:-install}"
  [[ -n "${SBX_NO_SERVICE:-}" ]] && { warn "已跳过服务注册（SBX_NO_SERVICE）"; return 0; }

  case "$INIT_SYS" in
    systemd)
      if [[ -n "$ROOT" ]]; then mkdir -p -- "$UNIT_DIR" || return 1; fi
      cat > "$UNIT_DIR"/sing-box.service <<EOF
[Unit]
Description=sing-box service
Documentation=https://sing-box.sagernet.org
After=network-online.target nss-lookup.target
Wants=network-online.target

[Service]
Type=simple
Environment=SBX_DIR=$APP_DIR
Environment=SBX_CONF=$PANEL_CONF
Environment=SBX_SB_CONF=$SB_CONF
ExecStartPre=$CORE_BIN enforce-access
ExecStart=$SB_BIN run -C $SB_DIR
Restart=on-failure
RestartSec=3
LimitNOFILE=infinity
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW

[Install]
WantedBy=multi-user.target
EOF

      cat > "$UNIT_DIR"/sbx-firewall.service <<EOF
[Unit]
Description=SBX traffic counters (netfilter)
After=network-pre.target
Before=sing-box.service sbx-panel.service
DefaultDependencies=no

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$CORE_BIN apply
ExecStop=$CORE_BIN clear

[Install]
WantedBy=multi-user.target
EOF


# >>> panel-unit（tests/installer_flow_test.sh 提取本区块做沙箱路径校验；
# 标记必须在行首——测试用 sed '/^# >>> panel-unit/' 提取，缩进会匹配不到）
      cat > "$UNIT_DIR"/sbx-panel.service <<EOF
[Unit]
Description=SBX traffic panel
After=network-online.target sbx-firewall.service
Wants=network-online.target sbx-firewall.service

[Service]
Type=simple
Environment=SBX_CONF=$PANEL_CONF
ExecStart=$CORE_BIN serve
Restart=always
RestartSec=3
# v3.0.5 最小权限沙箱（经真机验证）。
# 面板进程仍需 CAP_NET_ADMIN：sbx-core serve 内嵌 Collector 与策略层，需执行
# nft（读计数器 / 应用 sbx_policy）；还需读 /proc（连接数、conntrack）、
# 读写 /etc/sbx（SQLite/状态文件/节点配置）、绑定面板端口。
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
ProtectSystem=full
# 只列出**实际存在且确实要写**的路径。
# Web API 管理节点时会在 $SB_DIR 内原子写入 sing-box 候选配置；节点/策略/数据库
# 状态写入 $APP_DIR。两者都由 prepare_dirs 提前创建，并且只开放这两个目录。
#
# 为什么这条注释必须留着：ReadWritePaths 里出现不存在的路径时，
# systemd < 248（Debian 11 / Ubuntu 20.04 / RHEL 8 等）会让服务以
# status=226/NAMESPACE 无限重启，而 systemd ≥ 248 会静默忽略——同一份 unit
# 在新系统上"看起来没问题"，到老系统上就是面板永远起不来。
# 曾因此写过 /run/sbx：它在代码里从未被创建也从未被使用（全仓只有这一处提及），
# 而 /run 是 tmpfs、每次重启清空，于是 Debian 11 上安装后面板直接不可用。
# 规则：只写 $APP_DIR 与 $SB_DIR 这类 prepare_dirs 确实创建的路径；确需"可能不存在"的路径时
# 必须加 "-" 前缀（systemd 会忽略缺失项）。
ReadWritePaths=$APP_DIR $SB_DIR
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
# v3.0.9 nftables-only：去掉 CAP_NET_RAW。它是 v3.0.6 为 iptables-legacy
# （Ubuntu 20.04 等）加的——legacy 走 AF_INET SOCK_RAW setsockopt 才需要该能力。
# nft 走 AF_NETLINK/NETLINK_NETFILTER，只需 CAP_NET_ADMIN；连接数与 conntrack
# 判活读 /proc 不需要任何 capability。少一个能力即少一份攻击面。
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
# <<< panel-unit
      if sbx_host_runtime; then
        systemctl daemon-reload
        if [[ "$mode" != update ]]; then systemctl enable sing-box sbx-firewall sbx-panel >/dev/null 2>&1; fi
      fi
      ;;

    openrc)
      if [[ -n "$ROOT" ]]; then mkdir -p -- "$INIT_DIR" || return 1; fi
      cat > "$INIT_DIR"/sing-box <<EOF
#!/sbin/openrc-run
name="sing-box"
description="sing-box service"
command="$SB_BIN"
command_args="run -C $SB_DIR"
command_background=true
pidfile="$ROOT/run/\${RC_SVCNAME}.pid"
# Logs are appended; configure rotation as described in docs/openrc-logs.md.
output_log="$ROOT/var/log/sing-box.log"
error_log="$ROOT/var/log/sing-box.log"
export SBX_DIR="$APP_DIR" SBX_CONF="$PANEL_CONF" SBX_SB_CONF="$SB_CONF"
start_pre() { "$CORE_BIN" enforce-access; }
depend() { need net; after sbx-firewall; }
EOF
      cat > "$INIT_DIR"/sbx-firewall <<EOF
#!/sbin/openrc-run
name="sbx-firewall"
description="SBX traffic counters"
depend() { before sing-box sbx-panel; }
start() { $CORE_BIN apply; }
stop()  { $CORE_BIN clear; }
EOF
      cat > "$INIT_DIR"/sbx-panel <<EOF
#!/sbin/openrc-run
name="sbx-panel"
description="SBX traffic panel"
command="$CORE_BIN"
command_args="serve"
command_background=true
pidfile="$ROOT/run/\${RC_SVCNAME}.pid"
export SBX_CONF="$PANEL_CONF"
# Logs are appended; configure rotation as described in docs/openrc-logs.md.
output_log="$ROOT/var/log/sbx-panel.log"
error_log="$ROOT/var/log/sbx-panel.log"
depend() { need net; after sbx-firewall; }
EOF
      chmod +x "$INIT_DIR/sing-box" "$INIT_DIR/sbx-firewall" "$INIT_DIR/sbx-panel"
      if [[ "$mode" != update ]] && sbx_host_runtime; then
        rc-update add sbx-firewall default >/dev/null 2>&1
        rc-update add sing-box default >/dev/null 2>&1
        rc-update add sbx-panel default >/dev/null 2>&1
      fi
      ;;
    *) return 0 ;;
  esac
  if ! sbx_host_runtime; then ok "已生成沙箱服务定义（未注册到宿主机）"
  elif [[ "$mode" == update ]]; then ok "服务定义已刷新（保留开机自启设置）"
  else ok "服务已注册（开机自启）"; fi
}

# wait_service_active 轮询等待服务真正进入 active。
#
# 为什么需要：对 Restart=always 的 unit，`systemctl restart` 会立刻返回 0，
# 即使进程随后就以 226/NAMESPACE 反复崩溃（unit 处于 activating/auto-restart）。
# 只看 restart 的返回码会把"面板根本没起来"当成成功——真机事故就是这么漏掉的。
wait_service_active() { # wait_service_active <unit> [最多等几秒]
  local unit="$1" secs="${2:-10}" i
  for ((i = 0; i < secs * 2; i++)); do
    svc_do status "$unit" && return 0
    sleep 0.5
  done
  return 1
}

start_all() {
  sbx_host_runtime || return 0
  # fw_apply 失败在此处仅告警（启动流程属尽力而为，不能因 -e 中断整个菜单）
  svc_do start sbx-firewall || fw_apply || warn "计数规则应用失败，流量统计可能不准"
  svc_do restart sing-box || svc_do start sing-box
  svc_do restart sbx-panel || svc_do start sbx-panel
  sleep 1
}

# ---------------------------------------------------------------- 面板信息
panel_url() {
  local host port listen
  port=$(panel_get port)
  listen=$(panel_get listen)
  # 新装默认 0.0.0.0。用户改为仅监听回环后，只显示实际回环地址。
  if [[ "$listen" == "127.0.0.1" || "$listen" == "localhost" || "$listen" == "::1" ]]; then
    echo "http://$(host_for_uri "$listen"):$port/"
    return 0
  fi
  host=$(core_node get-host); [[ -n "$host" ]] || host="$(public_ip)"
  echo "http://$(host_for_uri "$host"):$port/"
}

show_panel_info() {
  hr
  printf '%s流量面板%s\n' "$C_B" "$C_RESET"
  printf '  地址: %s%s%s\n' "$C_CYAN" "$(panel_url)" "$C_RESET"
  printf '  令牌: %s%s%s\n' "$C_CYAN" "$(panel_get token)" "$C_RESET"
  local listen
  listen=$(panel_get listen) || listen='未知'
  case "$listen" in
    127.*|localhost|::1) ;;
    *) warn "面板监听 $listen：HTTP 不加密，令牌和会话可能被窃听。不要直接开放公网；请用 SSH 隧道，或仅监听本机并配置 HTTPS 反代。" ;;
  esac
  printf '  状态: %s\n' "$(panel_running && echo "${C_GREEN}运行中${C_RESET}" || echo "${C_RED}未运行${C_RESET}")"
  # 后端恒为 nftables（nftables-only）；这里只报告它当前是否真的可用。
  if sbx_host_runtime; then
    printf '  后端: %s\n' "$(nft list tables >/dev/null 2>&1 \
      && echo "nftables" || echo "${C_RED}nftables 不可用${C_RESET}")"
  else
    printf '  后端: nftables（未探测宿主机）\n'
  fi
  hr
}

# ---------------------------------------------------------------- 菜单动作
menu_add_node() {
  banner
  printf '%s添加节点%s\n\n' "$C_B" "$C_RESET"
  echo "  1) VLESS + Reality"
  echo "  2) Shadowsocks 2022"
  echo "  3) Trojan"
  echo "  4) AnyTLS"
  echo "  5) Snell"
  echo "  0) 返回"
  echo
  printf '请选择: '
  read -r c || true
  local rc=0
  case "$c" in
    1) add_vless || rc=$? ;; 2) add_ss || rc=$? ;; 3) add_trojan || rc=$? ;; 4) add_anytls || rc=$? ;; 5) menu_snell || rc=$? ;;
    0|"") return 0 ;;
    *) warn "无效选择" ;;
  esac
  [[ "$rc" == 0 ]] || warn "添加操作未完成"
  pause
}

# Snell 二级菜单：v5 / v6 / 返回
menu_snell() {
  banner
  printf '%sSnell%s\n\n' "$C_B" "$C_RESET"
  echo "  1. Snell v5"
  echo "  2. Snell v6"
  echo "  0. 返回"
  echo
  printf '请选择: '
  read -r c || true
  case "$c" in
    1) add_snell 5 ;;
    2) add_snell 6 ;;
    0|"") return 0 ;;
    *) warn "无效选择" ;;
  esac
  pause
}

menu_remove_node() {
  banner
  printf '%s删除节点%s\n\n' "$C_B" "$C_RESET"
  core_node list
  echo
  printf '输入要删除的节点 ID (回车取消): '
  read -r id || true
  [[ -z "$id" ]] && return 0
  local rc=0
  sbx_lock do_remove_node "$id" || rc=$?
  case $rc in
    0) ok "节点 $id 已删除" ;;
    2) warn "节点已删除，但流量统计规则应用失败" ;;
    *) warn "删除失败" ;;
  esac
  if [[ "$rc" == 0 || "$rc" == 2 ]]; then
    printf '%s该节点的历史流量数据保留在数据库中。要一并清除吗? [y/N] %s' "$C_DIM" "$C_RESET"
    read -r yn || true
    if [[ "${yn,,}" == "y" ]]; then
      "$CORE_BIN" reset "node:$id" || warn "清除该节点的历史流量失败"
    fi
  fi
  pause
}
do_remove_node() {
  core_node remove "$1" >/dev/null || return 1
  commit_node
}

menu_edit_node() {
  banner
  printf '%s修改节点（端口 / SNI）%s\n\n' "$C_B" "$C_RESET"
  core_node list
  echo
  printf '输入要修改的节点 ID (回车取消): '
  read -r id || true
  [[ -z "$id" ]] && return 0
  # 取该节点信息
  local info type sni port
  if ! info=$(core_node info "$id") || [[ -z "$info" ]]; then
    warn "未找到节点 $id，或节点信息读取失败"; pause; return 0
  fi
  type=$(echo "$info" | cut -f1); sni=$(echo "$info" | cut -f2)
  port=$(echo "$info" | cut -f3)

  local disp_type="$type"
  if [[ "$type" == "snell" ]]; then
    disp_type="Snell v$(echo "$info" | cut -f4)"
  fi
  printf '\n节点类型: %s   当前端口: %s\n' "$disp_type" "$port"
  [[ -n "$sni" ]] && printf '当前 SNI: %s\n' "$sni"
  echo

  local args=("$id") changes=()

  printf '新端口 (回车不改): '
  read -r np || true
  if [[ -n "$np" ]]; then
    valid_port "$np" || { warn "端口无效"; pause; return 1; }
    if [[ "$np" != "$port" ]]; then
      local busy=0
      node_port_busy "$np" || busy=$?
      case "$busy" in
        0) warn "端口 $np 已被其它节点占用"; pause; return 1 ;;
        2) warn "无法检查端口：sbx-core 不存在或执行失败"; pause; return 1 ;;
      esac
      busy=0
      port_busy "$np" || busy=$?
      if [[ "$busy" == 2 ]]; then warn "无法检查端口 $np"; pause; return 1; fi
      if [[ "$busy" == 0 ]]; then
        printf '%s端口 %s 已被系统其它进程监听，仍要使用? [y/N] %s' "$C_YEL" "$np" "$C_RESET"
        read -r yn || true; [[ "${yn,,}" == "y" ]] || { pause; return 1; }
      fi
    fi
    args+=("--port=$np"); changes+=("端口→$np")
  fi

  # 仅对支持 SNI 的类型询问
  case "$type" in
    vless|trojan|anytls)
      printf '新 SNI 伪装域名 (回车不改): '
      read -r ns || true
      if [[ -n "$ns" ]]; then
        sbx_valid_sni "$ns" || { warn "域名格式无效"; pause; return 1; }
        args+=("--sni=$ns"); changes+=("SNI→$ns")
      fi ;;
    shadowsocks)
      # 显示当前加密算法，允许切换（切换会由 Go 侧重新生成对应长度 key）
      local cur_method new_method
      cur_method=$(echo "$info" | cut -f4)
      [[ -n "$cur_method" ]] && printf '当前加密算法: %s\n' "$cur_method"
      printf '修改加密算法? 回车不改 [1=2022-blake3-aes-128-gcm 2=2022-blake3-aes-256-gcm]: '
      read -r mc || true
      case "${mc}" in
        1|2)
          new_method="2022-blake3-aes-$([[ "$mc" == 1 ]] && echo 128 || echo 256)-gcm"
          if [[ "$new_method" != "$cur_method" ]]; then
            args+=("--method=$new_method"); changes+=("加密算法已更新")
          fi ;;
        "") : ;;
        *) warn "无效选择，跳过算法修改"; ;;
      esac ;;
    snell)
      # 显示当前版本，允许重新生成 PSK（不直接改版本，避免半 v5 半 v6 非法配置）
      local cur_ver
      cur_ver=$(echo "$info" | cut -f4)
      [[ -n "$cur_ver" ]] && printf '当前版本: Snell v%s\n' "$cur_ver"
      printf '重新生成 PSK? [y/N]: '
      read -r rp || true
      if [[ "${rp,,}" == "y" ]]; then
        local new_psk
        new_psk=$("$CORE_BIN" secret hex 32) || { warn "生成 PSK 失败"; pause; return 1; }
        args+=("--psk=$new_psk"); changes+=("PSK 已更新")
      fi ;;
  esac

  if [[ ${#args[@]} -le 1 ]]; then echo "未做任何修改"; pause; return 0; fi

  local rc=0
  sbx_lock do_edit_node "${args[@]}" || rc=$?
  if [[ $rc -eq 0 ]]; then
    ok "节点 $id 已更新：${changes[*]}"
    printf '%s提示：修改后分享链接已变化，请重新导出发给客户端。%s\n' "$C_DIM" "$C_RESET"
    hr
    local h6; h6=$(core_node get-host6)
    if [[ -n "$h6" ]]; then
      core_node links "$id" --host "$(core_node get-host)" --host6 "$h6"
    else
      core_node links "$id" --host "$(core_node get-host)"
    fi
  elif [[ $rc -eq 2 ]]; then
    warn "节点已更新，但流量统计规则应用失败"
  else
    warn "节点修改失败"
  fi
  pause
}
do_edit_node() {
  "$CORE_BIN" node edit "$@" >/dev/null || return 1
  commit_node
}

menu_show_links() {
  banner
  local host host6
  host=$(core_node get-host); [[ -z "$host" ]] && host="$(public_ip)"
  host6=$(core_node get-host6)
  printf '%s节点分享链接%s  (IPv4: %s%s)\n' "$C_B" "$C_RESET" "$host" \
    "$([[ -n "$host6" ]] && echo "  IPv6: $host6")"
  hr
  if [[ -n "$host6" ]]; then
    core_node links --host "$host" --host6 "$host6"
  else
    core_node links --host "$host"
    printf '%s本机未检测到公网 IPv6，仅提供 IPv4 链接。%s\n' "$C_DIM" "$C_RESET"
    printf '%s（如已开通 IPv6，可在「设置分享地址」里重新探测）%s\n' "$C_DIM" "$C_RESET"
  fi
  hr
  pause
}

sbx_change_panel_port() {
  local raw="$1" port old busy=0
  if ! [[ "$raw" =~ ^0*([0-9]{1,5})$ ]]; then
    warn "无效端口"
    return 1
  fi
  port=$((10#${BASH_REMATCH[1]}))
  valid_port "$port" || { warn "无效端口"; return 1; }
  old=$(panel_get port) || { warn "无法读取面板当前端口"; return 1; }
  if [[ "$port" == "$old" ]]; then
    ok "面板已使用端口 $port"
    return 0
  fi
  port_busy "$port" || busy=$?
  case "$busy" in
    0) warn "端口 $port 已被占用，未修改配置"; return 1 ;;
    1) ;;
    *) warn "无法确认端口 $port 是否空闲，未修改配置"; return 1 ;;
  esac
  if ! panel_set port "$port"; then
    warn "设置端口失败"
    return 1
  fi
  if ! sbx_host_runtime || { svc_do restart sbx-panel && wait_service_active sbx-panel 3; }; then
    ok "已改为 $port"
    return 0
  fi
  if panel_set port "$old" && svc_do restart sbx-panel && wait_service_active sbx-panel 3; then
    warn "新端口启动失败，已恢复原端口 $old"
  else
    err "新端口启动失败，且无法确认恢复原端口 $old"
  fi
  return 1
}

menu_panel_settings() {
  banner
  printf '%s面板设置与统计维护%s\n\n' "$C_B" "$C_RESET"
  show_panel_info
  echo "  1) 修改端口"
  echo "  2) 修改采集间隔（当前 $(panel_get interval) 秒）"
  echo "  3) 仅本机访问 / 允许公网访问（当前 $(panel_get listen)）"
  echo "  4) 运行统计自检"
  echo "  5) 清空所有统计数据"
  echo "  0) 返回"
  echo
  printf '请选择: '
  read -r c || true
  case "$c" in
    1) printf '新端口: '; read -r p || true
       sbx_change_panel_port "$p" || : ;;
    2) printf '采集间隔秒数 (1-60): '; read -r n || true
       if ! [[ "$n" =~ ^([1-9]|[1-5][0-9]|60)$ ]]; then warn "无效数值"
       elif panel_set interval "$n" && svc_do restart sbx-panel; then ok "已改为 ${n}s"
       else warn "设置采集间隔或重启面板失败"; fi ;;
    3) if [[ "$(panel_get listen)" == "127.0.0.1" ]]; then
         panel_set listen "0.0.0.0"; ok "已允许公网访问"
       else
         panel_set listen "127.0.0.1"; ok "已限制为仅本机访问"
       fi
       svc_do restart sbx-panel ;;
    4) hr; "$CORE_BIN" selftest; hr ;;
    5) printf '%s确认清空全部流量统计? 此操作不可恢复 [y/N] %s' "$C_YEL" "$C_RESET"
       read -r yn || true
       [[ "${yn,,}" == "y" ]] && { "$CORE_BIN" reset; svc_do restart sbx-panel; } || echo "已取消" ;;
    0|"") return 0 ;;
    *) warn "无效选择" ;;
  esac
  pause
}

menu_service() {
  banner
  printf '%s服务管理%s\n\n' "$C_B" "$C_RESET"
  printf '  sing-box : %s\n' "$(sb_running && echo "${C_GREEN}运行中${C_RESET}" || echo "${C_RED}已停止${C_RESET}")"
  printf '  面板     : %s\n\n' "$(panel_running && echo "${C_GREEN}运行中${C_RESET}" || echo "${C_RED}已停止${C_RESET}")"
  echo "  1) 全部重启"
  echo "  2) 全部停止"
  echo "  3) 全部启动"
  echo "  4) 重建流量计数规则"
  echo "  5) 查看 sing-box 日志"
  echo "  6) 查看面板日志"
  echo "  0) 返回"
  echo
  printf '请选择: '
  read -r c || true
  case "$c" in
    1|2|3|4) if ! sbx_host_runtime; then warn "沙箱或仅配置模式不操作宿主机服务或防火墙"
       else
         case "$c" in
           1) start_all; ok "已重启" ;;
           2) svc_do stop sing-box; svc_do stop sbx-panel; ok "已停止" ;;
           3) start_all; ok "已启动" ;;
           4) if fw_apply; then ok "计数规则已重建"; else err "计数规则重建失败，请检查系统防火墙工具"; fi ;;
         esac
       fi ;;
    5|6) local name=sing-box
       [[ "$c" != 6 ]] || name=sbx-panel
       if [[ "$INIT_SYS" == systemd ]]; then
         if sbx_host_runtime; then journalctl -u "$name" -n 60 --no-pager; else echo "(未读取宿主机日志)"; fi
       else tail -n 60 "$ROOT/var/log/$name.log" 2>/dev/null || echo "(暂无日志)"; fi ;;
    0|"") return 0 ;;
    *) warn "无效选择" ;;
  esac
  pause
}

menu_host() {
  banner
  printf '%s分享地址%s\n\n' "$C_B" "$C_RESET"
  printf '  当前 IPv4: %s\n' "$(core_node get-host || echo '(未设置)')"
  local cur6; cur6=$(core_node get-host6)
  printf '  当前 IPv6: %s\n' "${cur6:-（无）}"
  echo
  info "重新探测中..."
  local v4 v6; v4=$(public_ip); v6=$(public_ip6)
  printf '  探测 IPv4: %s\n' "$v4"
  printf '  探测 IPv6: %s\n' "${v6:-（本机无可用公网 IPv6）}"
  echo
  printf '输入用于分享链接的域名或 IPv4 (回车用探测值 %s): ' "$v4"
  read -r h || true
  [[ -z "$h" ]] && h="$v4"
  core_node set-host "$h" >/dev/null
  ok "IPv4 分享地址已设为 $h"

  if [[ -n "$v6" ]]; then
    printf '是否用探测到的 IPv6 (%s) 作为分享地址? [Y/n] ' "$v6"
    read -r yn || true
    if [[ "${yn,,}" != "n" ]]; then
      core_node set-host6 "$v6" >/dev/null
      ok "IPv6 分享地址已设为 $v6（分享链接将附 IPv6 版本）"
    else
      core_node set-host6 "" >/dev/null
      info "已关闭 IPv6 分享链接"
    fi
  else
    core_node set-host6 "" >/dev/null
    info "本机未检测到可用公网 IPv6，分享链接仅提供 IPv4"
  fi
  pause
}

# 卸载备份路径（backup_sb_dir 写入）。
SB_BACKUP_PATH=""

# backup_sb_dir 把 $SB_DIR 打包到 $ROOT/var/backups，并把路径写入 SB_BACKUP_PATH。
#
# 为什么卸载要备份：SBX 是「叠加」在用户既有 sing-box 配置之上的——安装时
# ensure_sb_config 对已存在的 config.json 直接 return，nodes.RebuildConfig 也明确
# 保留用户的 dns/route/outbounds 等其它字段。直接 rm -rf 会把用户自有配置一起
# 抹掉且不可恢复，而 SBX 只应删除自己创建的东西。
#
# 备份失败时返回 1，调用方必须保留目录——宁可留残留，也不静默毁数据。
backup_sb_dir() {
  local bkdir="$ROOT/var/backups" dest mode stamp tmp archive parent name
  SB_BACKUP_PATH=""
  dest=$(
    umask 077 || { printf '%s\n' '无法设置备份权限掩码' >&2; exit 1; }
    if [[ -L "$bkdir" ]] || ! mkdir -p -- "$bkdir" 2>/dev/null || [[ ! -O "$bkdir" ]] \
      || ! mode=$(stat -c %a -- "$bkdir") || [[ ! "$mode" =~ ^[0-7]{3,4}$ ]] \
      || (( 8#$mode & 022 )); then
      printf '无法使用不安全或不可访问的备份目录：%s\n' "$bkdir" >&2
      exit 1
    fi
    if ! parent=$(dirname -- "$SB_DIR") || ! name=$(basename -- "$SB_DIR") \
      || ! stamp=$(date +%Y%m%d-%H%M%S); then
      printf '%s\n' '无法确定备份路径' >&2
      exit 1
    fi
    tmp=$(mktemp -d -- "$bkdir/.sbx-backup.XXXXXXXX") \
      || { printf '%s\n' '无法创建备份暂存目录' >&2; exit 1; }
    trap 'rm -rf -- "$tmp" || printf "无法清理备份暂存目录：%s\n" "$tmp" >&2' EXIT
    trap 'exit 1' HUP INT TERM
    archive="$tmp/archive.tar.gz"
    if ! touch -- "$archive" || ! chmod 600 -- "$archive" \
      || [[ $(stat -c %a -- "$archive") != 600 ]]; then
      printf '%s\n' '无法安全暂存备份（需要权限 600）' >&2
      exit 1
    fi
    if ! tar -czf - -C "$parent" -- "$name" > "$archive" || [[ ! -s "$archive" ]]; then
      printf '打包 sing-box 目录失败：%s\n' "$SB_DIR" >&2
      exit 1
    fi
    # 只发布完整归档；硬链接若遇到既有同名项（包括符号链接）会失败，不会覆盖。
    dest="$bkdir/sbx-sing-box-$stamp-${tmp##*.}.tar.gz"
    ln -T -- "$archive" "$dest" || { printf '无法发布备份文件：%s\n' "$dest" >&2; exit 1; }
    printf '%s\n' "$dest"
  ) || return 1
  SB_BACKUP_PATH="$dest"
  return 0
}

uninstall_all() {
  banner
  printf '%s卸载 SBX%s\n\n' "$C_B" "$C_RESET"
  echo "将会："
  echo "  · 停止并移除 sing-box / 面板 / 计数规则的服务"
  echo "  · 备份 $SB_DIR 到 $ROOT/var/backups 后删除（保留你原有的 sing-box 配置）"
  echo "  · 删除 $APP_DIR（含节点凭据与流量历史）与 $CMD_PATH"
  echo
  printf '%s确认卸载? 流量历史将一并删除 [y/N] %s' "$C_YEL" "$C_RESET"
  read -r yn || true
  [[ "${yn,,}" == "y" ]] || { echo "已取消"; pause; return 0; }

  if sbx_host_runtime; then
    upgrade_stop_consumers || die "无法停止 sing-box 或面板，拒绝清除访问规则"
    svc_do stop sbx-firewall || die "无法停止计数服务，已中止卸载"
    fw_clear || die "无法清除 SBX 防火墙规则，已中止卸载"
    # 旧版（≤ v3.0.8）留下的 SBX_IN/SBX_OUT 链也要清理。
    cleanup_legacy_backend
    case "$INIT_SYS" in
      systemd)
        systemctl disable sing-box sbx-panel sbx-firewall >/dev/null 2>&1 || true
        rm -f "$UNIT_DIR"/{sing-box,sbx-panel,sbx-firewall}.service
        systemctl daemon-reload ;;
      openrc)
        local s
        for s in sbx-panel sing-box sbx-firewall; do
          rc-update del "$s" default >/dev/null 2>&1 || true
          rm -f "$INIT_DIR/$s"
        done ;;
    esac
  else
    # 沙箱可能没有 init 管理器，也可能包含两种生成的定义。
    rm -f "$UNIT_DIR"/{sing-box,sbx-panel,sbx-firewall}.service \
      "$INIT_DIR"/{sing-box,sbx-panel,sbx-firewall}
  fi
  rm -rf "$APP_DIR" "$SB_BIN" "$CMD_PATH" "$CORE_BIN" "$BIN_DIR/libcronet.so"
  # sing-box 目录单独处理：可能含用户自有配置（见 backup_sb_dir 注释）。
  # 空目录（安装未完成）直接删；非空先备份，备份失败则保留。
  if [[ -d "$SB_DIR" ]] && [[ -n "$(ls -A "$SB_DIR" 2>/dev/null || true)" ]]; then
    if backup_sb_dir; then
      rm -rf "$SB_DIR"
      ok "已备份 sing-box 配置: $SB_BACKUP_PATH"
      info "确认不再需要可自行删除该备份文件"
    else
      warn "备份 sing-box 配置失败，已保留 $SB_DIR 未删除"
      info "请手工备份该目录后自行删除，或重新运行卸载"
    fi
  else
    rm -rf "$SB_DIR"
  fi
  # 清理安装期写入的 sysctl 持久化文件（conntrack 字节计费）
  rm -f -- "$CONNTRACK_SYSCTL_CONF" 2>/dev/null || true
  ok "已卸载"
  exit 0
}

pause() { printf '\n%s按回车继续...%s' "$C_DIM" "$C_RESET"; read -r _ || true; }

# ---------------------------------------------------------------- 主菜单
main_menu() {
  while :; do
    banner
    local nnum
    nnum=$(core_node count)
    printf '  节点: %s%s%s 个    sing-box: %s    面板: %s    %sv%s%s\n' \
      "$C_B" "$nnum" "$C_RESET" \
      "$(sb_running && echo "${C_GREEN}●${C_RESET}" || echo "${C_RED}●${C_RESET}")" \
      "$(panel_running && echo "${C_GREEN}●${C_RESET}" || echo "${C_RED}●${C_RESET}")" \
      "$C_DIM" "$APP_VERSION" "$C_RESET"
    printf '  面板: %s%s%s\n\n' "$C_CYAN" "$(panel_url)" "$C_RESET"
    echo "  1) 系统设置与运维"
    echo "  2) 检查更新"
    echo "  3) 卸载"
    echo "  0) 退出"
    echo
    printf '请选择: '
    read -r c || true
    case "$c" in
      1) menu_settings ;;
      2) do_update || warn "在线升级未完成"; pause ;;
      3) uninstall_all ;;
      0|"") clear 2>/dev/null || true; exit 0 ;;
      *) warn "无效选择"; sleep 1 ;;
    esac
  done
}

# ---------------------------------------------------------------- 流量日报

# notify_safe_value 拒绝会破坏参数传递的输入（控制字符，含换行）。
# 值本身仍会由 sbx-core 做格式白名单校验（含 webhook 域名锁定），两层分工明确。
notify_safe_value() {
  [[ "$1" =~ [[:cntrl:]] ]] && return 1
  return 0
}

# menu_notify_setup 收集 webhook / 密钥 / 时刻并写入配置。
#
# notify-config set already accepts a config file. Encode through stdin so
# neither the webhook nor the signing key appears in jq or sbx-core argv.
menu_notify_setup() {
  banner
  printf '%s配置飞书日报%s\n\n' "$C_B" "$C_RESET"
  echo "在飞书群里添加「自定义机器人」后会得到一条 webhook 地址，形如:"
  printf '  %shttps://open.feishu.cn/open-apis/bot/v2/hook/xxxxxxxx%s\n\n' "$C_DIM" "$C_RESET"

  command -v jq >/dev/null || { warn "请先安装 jq 才能安全写入飞书凭据；未修改配置"; return 0; }
  local hook secret at
  printf 'Webhook 地址: '
  IFS= read -rs hook || return 0
  echo
  hook="${hook//[[:space:]]/}"
  [[ -n "$hook" ]] || { warn "不能为空"; return 0; }
  notify_safe_value "$hook" || { warn "输入含控制字符，已取消"; return 0; }

  printf '签名密钥 secret（可选，直接回车表示不用签名）: '
  IFS= read -rs secret || return 0
  echo
  notify_safe_value "$secret" || { warn "输入含控制字符，已取消"; return 0; }

  printf '每天几点发送日报 [00:05]: '
  read -r at || true
  [[ -n "$at" ]] || at="00:05"
  notify_safe_value "$at" || { warn "输入含控制字符，已取消"; return 0; }

  if ! (
    # Do not echo the printf arguments in a traced shell.
    { set +x; } 2>/dev/null
    set -o pipefail
    umask 077
    local stage
    stage=$(mktemp -d "$APP_DIR/.notify-config.XXXXXXXX") || exit 1
    trap 'rm -rf -- "$stage"' EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM
    if ! printf '%s\n' "$hook" "$secret" "$at" | jq -Rn '[inputs] | if length == 3 then
      {enabled:true,type:"feishu",webhook:.[0],secret:.[1],daily_at:.[2]}
      else error("invalid notify input") end' > "$stage/notify.json"; then
      exit 1
    fi
    chmod 600 "$stage/notify.json" || exit 1
    "$CORE_BIN" notify-config set "$stage/notify.json"
  ); then
    warn "飞书配置未保存"
    return 0
  fi
  echo
  echo "接着选「发送测试推送」，确认飞书群里能收到卡片。"
}

menu_notify_time() {
  banner
  printf '%s修改日报发送时刻%s\n\n' "$C_B" "$C_RESET"
  printf '每天早上几点发送（24 小时制 HH:MM，如 00:05 / 09:00）: '
  local t; read -r t || true
  [[ -n "$t" ]] || { echo "已取消"; return 0; }
  "$CORE_BIN" notify-config set-time "$t"
}

menu_notify_toggle() {
  banner
  printf '%s启用 / 停用%s\n\n' "$C_B" "$C_RESET"
  local on="false"
  "$CORE_BIN" notify-config status 2>/dev/null | head -1 | grep -q "已启用" && on="true"
  if [[ "$on" == "true" ]]; then
    "$CORE_BIN" notify-config set-enabled false
  else
    # 重新启用不需要重新粘贴 webhook：配置本身就存着。
    # 若配置不完整，sbx-core 会拒绝并说明缺什么。
    "$CORE_BIN" notify-config set-enabled true
  fi
}

menu_notify_help() {
  banner
  printf '%s怎么配置飞书日报%s\n\n' "$C_B" "$C_RESET"
  echo "  1) 在飞书里建一个群（只有你自己也行），或用现有群"
  echo "  2) 群设置 → 群机器人 → 添加机器人 → 自定义机器人"
  echo "  3) 复制它给出的 Webhook 地址"
  echo "  4) 回到这里选「配置 / 修改」，粘贴地址即可"
  echo
  echo "  可选：在机器人设置里开启「签名校验」，把密钥一并填进来。"
  echo "        开启后即使 webhook 泄露，别人也无法往你的群发消息。"
  echo
  printf '%s推送什么%s: 每天一条卡片 = 前一天全天流量 + 全部历史账本。\n' "$C_YEL" "$C_RESET"
  echo "  服务器若在当天中途崩溃/断电，重启后会立刻补推一条「今日截至当前」。"
  echo
  printf '%s注意%s: 采集与推送无关——不配推送，流量统计照常运行。\n' "$C_YEL" "$C_RESET"
  echo "  但日报只留在飞书里，无法自动回灌数据库：整机损毁时历史数据不可恢复。"
}

menu_notify() {
  while :; do
    banner
    printf '%s流量日报配置%s\n\n' "$C_B" "$C_RESET"
    "$CORE_BIN" notify-config status 2>/dev/null | sed 's/^/  /'
    echo
    echo "  1) 配置 / 修改（飞书群机器人）"
    echo "  2) 修改发送时刻"
    echo "  3) 启用 / 停用"
    echo "  4) 查看当前配置（凭据已脱敏）"
    echo "  5) 发送测试推送"
    echo "  6) 怎么配置飞书机器人"
    echo "  0) 返回"
    echo
    printf '请选择: '
    local c; read -r c || true
    case "$c" in
      1) menu_notify_setup; pause ;;
      2) menu_notify_time; pause ;;
      3) menu_notify_toggle; pause ;;
      4) hr; "$CORE_BIN" notify-config show; hr; pause ;;
      5) hr; "$CORE_BIN" notify-test; hr; pause ;;
      6) menu_notify_help; pause ;;
      0|"") return 0 ;;
      *) warn "无效选择"; sleep 1 ;;
    esac
  done
}

menu_settings() {
  while :; do
    banner
    printf '%s系统设置与运维%s\n\n' "$C_B" "$C_RESET"
    echo "  1) 面板设置与统计维护（端口 / 间隔 / 监听 / 自检 / 清空）"
    echo "  2) 分享地址（域名 / IP）"
    echo "  3) 服务管理（重启 / 停止 / 日志）"
    echo "  4) 流量日报配置（飞书群机器人推送）"
    echo "  0) 返回"
    echo
    printf '请选择: '
    read -r c || true
    case "$c" in
      1) menu_panel_settings ;;
      2) menu_host ;;
      3) menu_service ;;
      4) menu_notify ;;
      0|"") return 0 ;;
      *) warn "无效选择" ;;
    esac
  done
}

# ---------------------------------------------------------------- 安装
install_self() {
  local source="${BASH_SOURCE[0]}"
  install -d -m 0755 "$BIN_DIR"
  # Fresh install and update execute from a verified regular staging file.
  [[ -f "$source" ]] || die "无法持久化已验证安装器"
  if [[ ! "$source" -ef "$SELF_PATH" ]]; then
    upgrade_replace "$source" "$SELF_PATH" 0755 || die "无法原子替换管理脚本"
  fi
  (
    local wrapper
    wrapper=$(mktemp "$BIN_DIR/.sbx-wrapper.XXXXXXXX") || exit 1
    trap 'rm -f -- "$wrapper"' EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM
    printf '#!/usr/bin/env bash\nexec bash %q "$@"\n' "$SELF_PATH" > "$wrapper" &&
      chmod 0755 "$wrapper" && mv -fT -- "$wrapper" "$CMD_PATH"
  ) || die "无法原子替换 sbx 命令"
}

# ---------------------------------------------------------------- 在线安装/升级
# Official release discovery happens exactly once per operation. Never fall back
# to a rolling download, even when the ref lookup is unavailable.
resolve_dist_sha() {
  local rev="${SBX_DIST_SHA:-}" refs
  if [[ -z "$rev" ]] && command -v git >/dev/null 2>&1; then
    refs=$(git ls-remote "https://github.com/$DIST_REPO.git" refs/heads/dist 2>/dev/null) || refs=""
    rev=$(awk '$2=="refs/heads/dist" {print $1}' <<< "$refs")
  fi
  if [[ -z "$rev" ]]; then
    refs=$(curl -q --globoff -fsS --proto =https -m 15 "https://api.github.com/repos/$DIST_REPO/git/ref/heads/dist") || return 1
    rev=$(sed -nE 's/.*"sha"[[:space:]]*:[[:space:]]*"([0-9a-f]{40})".*/\1/p' <<< "$refs")
  fi
  [[ "$rev" =~ ^[0-9a-f]{40}$ ]] || return 1
  printf '%s\n' "$rev"
}

set_dist_pin() {
  [[ "$1" =~ ^[0-9a-f]{40}$ ]] || die "分发提交必须为 40 位小写 Git SHA"
  DIST_SHA="$1"
  DIST_BASE="$OFFICIAL_RAW_REPO/$DIST_SHA"
}

fetch_dist_script() { # <private staging directory> [independent candidate anchor]
  local dir="$1" anchor="${2:-}" want count
  info "从官方 GitHub 分发提交 $DIST_SHA 拉取安装器..."
  curl -q --globoff -fsS --proto =https -m 60 -o "$dir/sbx.sh" "$DIST_BASE/sbx.sh" || die "无法下载固定提交的安装器"
  curl -q --globoff -fsS --proto =https -m 60 -o "$dir/sbx.sh.sha256" "$DIST_BASE/sbx.sh.sha256" || die "无法下载固定提交的脚本校验和"
  count=$(awk '$2=="sbx.sh" {c++} END {print c+0}' "$dir/sbx.sh.sha256")
  want=$(awk '$2=="sbx.sh" {print $1}' "$dir/sbx.sh.sha256")
  if [[ "$count" != 1 ]] || ! verify_remote_script "$dir/sbx.sh" "$want" "官方 sbx.sh.sha256"; then
    die "分发脚本校验失败，未改动现有安装"
  fi
  if [[ -n "$anchor" ]]; then
    verify_remote_script "$dir/sbx.sh" "$anchor" "候选脚本显式哈希" || die "候选脚本不匹配显式哈希，未改动现有安装"
  fi
  if ! bash -n "$dir/sbx.sh" || ! [[ "$(remote_version "$dir/sbx.sh")" =~ ^[0-9]+(\.[0-9]+)+$ ]]; then
    die "分发脚本语法或版本标记无效，未改动现有安装"
  fi
  grep -Fxq "DIST_PROTOCOL=\"$DIST_PROTOCOL\"" "$dir/sbx.sh" \
    || die "分发脚本不支持事务升级协议；请选择新发布提交"
  DIST_SCRIPT_SHA="${want,,}"
}

remote_version() {  # 从下载的脚本里提取 APP_VERSION
  grep -m1 '^APP_VERSION=' "$1" 2>/dev/null | sed -E 's/^APP_VERSION="?([^"]+)"?.*/\1/'
}

# Compare upstream sing-box versions using their numeric release base and
# final-vs-prerelease ordering (the published baseline is a final release).
sb_version_key() {  # <ver> → 输出四段数字
  local version="$1" rel=0 a b c
  local pattern='^v?([0-9]{1,9})\.([0-9]{1,9})(\.([0-9]{1,9}))?(-[[:alnum:]-]+(\.[[:alnum:]-]+)*)?(\+[[:alnum:]-]+(\.[[:alnum:]-]+)*)?$'
  [[ "$version" =~ $pattern ]] || return 1
  a=${BASH_REMATCH[1]} b=${BASH_REMATCH[2]} c=${BASH_REMATCH[4]:-0}
  if [[ -z "${BASH_REMATCH[5]}" ]]; then rel=1; fi
  printf '%s %s %s %s\n' "$((10#$a))" "$((10#$b))" "$((10#$c))" "$rel"
}
sb_version_ge() {
  local key i
  local -a ka kb
  key=$(sb_version_key "$1") || return 1
  read -r -a ka <<< "$key"
  key=$(sb_version_key "$2") || return 1
  read -r -a kb <<< "$key"
  for i in 0 1 2 3; do
    (( ka[i] > kb[i] )) && return 0
    (( ka[i] < kb[i] )) && return 1
  done
  return 0
}

# The candidate coordinates the entire transaction. The caller never replaces the
# installed script before the candidate has verified all managed services.
do_update() (
  local force="${1:-}" rev staging
  banner
  require_root
  [[ -z "$force" || "$force" == --force ]] || die "用法: sbx --update [--force]"
  rev=$(resolve_dist_sha) || die "无法解析官方 dist 提交，未改动现有安装"
  set_dist_pin "$rev"
  umask 077
  staging=$(mktemp -d) || die "无法创建升级暂存目录，未改动现有安装"
  trap 'rm -rf -- "$staging"' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  fetch_dist_script "$staging" "${SBX_UPDATE_SHA256:-}"
  SBX_SCRIPT_SHA256="$DIST_SCRIPT_SHA" bash "$staging/sbx.sh" --apply-update "$rev" "$force"
)

bootstrap_install() (
  local rev staging
  require_root
  rev=$(resolve_dist_sha) || die "无法解析官方 dist 提交，未开始安装"
  set_dist_pin "$rev"
  umask 077
  staging=$(mktemp -d) || die "无法创建安装暂存目录"
  trap 'rm -rf -- "$staging"' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  fetch_dist_script "$staging"
  SBX_SCRIPT_SHA256="$DIST_SCRIPT_SHA" bash "$staging/sbx.sh" --install-dist "$rev"
)

# Both services must remain up across consecutive samples. For the panel also
# probe the actual HTTP listener (not an external reverse proxy).
upgrade_health() {
  local want_sb="$1" want_panel="$2" addr="" listen port sample good=0 valid
  sbx_host_runtime || return 0
  if [[ "$want_panel" == 1 ]]; then
    listen=$(panel_get listen) || return 1
    port=$(panel_get port) || return 1
    [[ "$port" =~ ^[0-9]{1,5}$ ]] && valid_port "$port" || return 1
    case "$listen" in
      ''|0.0.0.0) listen=127.0.0.1 ;;
      ::) listen=::1 ;;
    esac
    [[ "$listen" =~ ^[a-zA-Z0-9_.:%-]+$ ]] || return 1
    addr="http://$(host_for_uri "$listen"):$port/healthz"
  fi
  for ((sample=0; sample<20; sample++)); do
    valid=1
    if [[ "$want_sb" == 1 ]]; then
      svc_do status sing-box || valid=0
    elif svc_do status sing-box; then
      valid=0
    fi
    if [[ "$want_panel" == 1 ]]; then
      if ! svc_do status sbx-panel || ! curl --noproxy '*' -fsS --max-time 2 -o /dev/null "$addr" 2>/dev/null; then
        valid=0
      fi
    elif svc_do status sbx-panel; then
      valid=0
    fi
    if [[ "$valid" == 1 ]]; then
      good=$((good + 1))
      [[ "$good" == 3 ]] && return 0
    else
      good=0
    fi
    sleep 0.5
  done
  return 1
}

upgrade_access_pending() {
  # Access-control recovery files belong to sbx-core, never to installer rollback.
  [[ -e "$APP_DIR/access-transition.nft" || -L "$APP_DIR/access-transition.nft" \
     || -e "$APP_DIR/node-transition.json" || -L "$APP_DIR/node-transition.json" ]]
}

upgrade_stop_consumers() {
  # Do not stop sbx-firewall: a previously installed stop hook may be destructive.
  svc_do stop sbx-panel && svc_do stop sing-box || return 1
  if [[ -z "${SBX_NO_SERVICE:-}" ]] && { svc_do status sbx-panel || svc_do status sing-box; }; then
    return 1
  fi
  return 0
}

upgrade_snapshot() {
  local item i
  for i in "${!UPGRADE_PATHS[@]}"; do
    item="${UPGRADE_PATHS[i]}"
    # NUL-separated paths, in backup slot order; absent files have no slot.
    printf '%s\0' "$item" >> "$UPGRADE_DIR/paths" || return 1
    if [[ -e "$item" || -L "$item" ]]; then
      cp -a -- "$item" "$UPGRADE_DIR/snapshot/$i" || return 1
    fi
  done
  printf 'sing-box=%s\nsbx-panel=%s\n' "$UPGRADE_SB_ACTIVE" "$UPGRADE_PANEL_ACTIVE" > "$UPGRADE_DIR/services"
}

upgrade_restore_path() {
  local backup="$1" item="$2" tmp
  if [[ ! -e "$backup" && ! -L "$backup" ]]; then
    # No slot means the path did not exist at snapshot time.
    rm -rf -- "$item"
    return $?
  fi
  # Stage on the destination filesystem without consuming the saved backup.
  # A rule cache directory needs removal before rename; binaries do not.
  tmp=$(mktemp -d "${item%/*}/.sbx-restore.XXXXXXXX") || return 1
  if ! cp -a -- "$backup" "$tmp/item" \
     || { [[ -d "$item" && ! -L "$item" ]] && ! rm -rf -- "$item"; } \
     || ! mv -fT -- "$tmp/item" "$item"; then
    rm -rf -- "$tmp" || true
    return 1
  fi
  rmdir -- "$tmp" || warn "无法清除回滚临时目录：$tmp"
}

upgrade_restore_files() {
  local item i rc=0
  for i in "${!UPGRADE_PATHS[@]}"; do
    item="${UPGRADE_PATHS[i]}"
    if ! upgrade_restore_path "$UPGRADE_DIR/snapshot/$i" "$item"; then
      err "未能恢复: $item"
      rc=1
    fi
  done
  return "$rc"
}

upgrade_restore() {
  # A failed Go commit may have left its own recovery journal/guard. Preserve
  # those exact files and the current binaries needed for explicit recovery.
  if upgrade_access_pending; then
    err "访问控制恢复未完成，保留隔离及升级备份；请先处理访问控制恢复"
    return 1
  fi
  upgrade_stop_consumers || return 1
  upgrade_restore_files || return 1
  if [[ "$INIT_SYS" == systemd ]] && sbx_host_runtime; then
    systemctl daemon-reload || return 1
  fi
  "$CORE_BIN" version >/dev/null && "$SB_BIN" version >/dev/null || return 1
  # Rebuild old runtime rules from restored configuration; never clear guard tables.
  fw_apply || return 1
  if [[ "$UPGRADE_SB_ACTIVE" == 1 ]]; then svc_do start sing-box || return 1; fi
  if [[ "$UPGRADE_PANEL_ACTIVE" == 1 ]]; then svc_do start sbx-panel || return 1; fi
  upgrade_health "$UPGRADE_SB_ACTIVE" "$UPGRADE_PANEL_ACTIVE"
}

upgrade_exit() {
  local rc="$1"
  trap - EXIT
  trap '' HUP INT TERM
  if [[ "$UPGRADE_COMMITTED" == 1 || "$UPGRADE_DIRTY" == 0 ]]; then
    if ! rm -rf -- "$UPGRADE_DIR"; then warn "无法清除升级工作目录：$UPGRADE_DIR"; fi
    if [[ "$rc" == 0 && "$UPGRADE_COMMITTED" == 1 ]]; then ok "升级完成 → v$APP_VERSION ($DIST_SHA)"; fi
  else
    [[ "$rc" != 0 ]] || rc=1
    if upgrade_restore; then
      err "升级失败；已恢复原文件并验证升级前的服务状态"
      rm -rf -- "$UPGRADE_DIR" || warn "无法清除升级备份：$UPGRADE_DIR"
    else
      err "升级失败，且无法确认回滚完成；保留备份：$UPGRADE_DIR"
    fi
  fi
  exit "$rc"
}

stage_upgrade_binaries() {
  local old_core="$CORE_BIN" old_sb="$SB_BIN" cur_sb
  local BIN_DIR="$UPGRADE_DIR/stage"
  local CORE_BIN="$BIN_DIR/sbx-core" SB_BIN="$BIN_DIR/sing-box"
  if [[ -f "$old_core" ]]; then cp -p -- "$old_core" "$CORE_BIN"; fi
  install_sbx_core
  cur_sb=$("$old_sb" version) || die "无法运行已安装的 sing-box"
  read -r _ _ cur_sb _ <<< "$cur_sb"
  sb_version_key "$cur_sb" >/dev/null || die "无法识别已安装 sing-box 的版本"
  if [[ -z "$SB_VERSION_PIN" ]] && ! sb_version_ge "$cur_sb" "$SB_VERSION_DEFAULT"; then
    info "sing-box 内核 $cur_sb 低于发布基线 $SB_VERSION_DEFAULT，随升级更新内核..."
    install_sing_box force stage
  fi
}

upgrade_preflight() {
  sbx_host_runtime || return 0
  # Host packages are outside the rollback boundary. Check APT state and only
  # refresh/install when an upgrade actually needs the missing dependency.
  if [[ "$PKG" == apt ]]; then
    apt-get --simulate check >/dev/null || die "APT 依赖预检失败；请先修复软件包状态，现有安装未改动"
  fi
  if ! command -v nft >/dev/null 2>&1; then
    [[ -z "${SBX_NO_SERVICE:-}" ]] || die "仅配置模式不会安装依赖；请先安装：$(pkg_install_hint nftables)"
    if [[ "$PKG" == apt ]]; then
      apt-get update -qq || die "刷新 APT 索引失败；未替换 SBX 文件"
    fi
    if ! pkg_install nftables || ! command -v nft >/dev/null 2>&1; then
      die "安装 nftables 失败；未替换 SBX 文件。请手动安装：$(pkg_install_hint nftables)"
    fi
  fi
  nft list tables >/dev/null || die "nftables 不可用；未改动现有安装"
}

upgrade_replace() ( # same-filesystem rename without truncating the running file
  local src="$1" dest="$2" mode="$3" tmp
  umask 077
  tmp=$(mktemp "${dest%/*}/.sbx-upgrade.XXXXXXXX") || exit 1
  trap 'rm -f -- "$tmp"' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  install -m "$mode" -- "$src" "$tmp" && mv -fT -- "$tmp" "$dest"
)

apply_update() {
  local force="$1" nodes nft_conf
  [[ -f "$PANEL_CONF" && -x "$CORE_BIN" && -x "$SB_BIN" ]] || die "未检测到完整 SBX 安装，无法升级"
  upgrade_access_pending && die "存在未完成的访问控制事务，拒绝升级"
  [[ ! -e "$BIN_DIR/.sing-box-transaction" && ! -L "$BIN_DIR/.sing-box-transaction" ]] \
    || die "存在未完成的内核升级，请先检查 $BIN_DIR/.sing-box-transaction"
  UPGRADE_DIR="$APP_DIR/.upgrade-transaction"
  mkdir -m 0700 -- "$UPGRADE_DIR" || die "无法创建升级目录（如有上次保留的备份，请先检查 $UPGRADE_DIR）"
  UPGRADE_DIRTY=0 UPGRADE_COMMITTED=0
  trap 'upgrade_exit "$?"' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  mkdir -- "$UPGRADE_DIR/stage" "$UPGRADE_DIR/snapshot"
  stage_upgrade_binaries
  if [[ "$force" != --force && ! -f "$UPGRADE_DIR/stage/sing-box" ]] \
     && cmp -s -- "${BASH_SOURCE[0]}" "$SELF_PATH" && cmp -s -- "$UPGRADE_DIR/stage/sbx-core" "$CORE_BIN"; then
    ok "安装器和后端已是固定分发提交 $DIST_SHA 的版本，无需升级"
    return 0
  fi
  upgrade_preflight
  nodes=$(panel_get nodes_file)
  nft_conf=$(panel_get nft_conf)
  [[ "$nodes" == /* && "$nft_conf" == /* ]] || die "无法确定节点或计数配置路径，拒绝升级"
  # The rules cache lives next to sing-box config; do not include SQLite or Go's
  # access recovery journals/backups in the installer snapshot.
  UPGRADE_PATHS=("$SELF_PATH" "$CMD_PATH" "$CORE_BIN" "$SB_BIN" "$BIN_DIR/libcronet.so"
    "$PANEL_CONF" "$nodes" "$nodes.candidate" "$SB_CONF" "$SB_CONF.candidate"
    "$nft_conf" "$APP_DIR/policy.nft" "$SB_DIR/rulesets")
  if [[ -z "${SBX_NO_SERVICE:-}" ]]; then
    case "$INIT_SYS" in
      systemd) UPGRADE_PATHS+=("$UNIT_DIR/sing-box.service" "$UNIT_DIR/sbx-panel.service" "$UNIT_DIR/sbx-firewall.service") ;;
      openrc) UPGRADE_PATHS+=("$INIT_DIR/sing-box" "$INIT_DIR/sbx-panel" "$INIT_DIR/sbx-firewall") ;;
      *) die "不支持的服务管理器，无法验证升级" ;;
    esac
  fi
  # Existing candidates can be evidence of an unfinished operation. Do not
  # stage over them or try to guess which generation of files is authoritative.
  [[ ! -e "$nodes.candidate" && ! -L "$nodes.candidate" && ! -e "$SB_CONF.candidate" && ! -L "$SB_CONF.candidate" ]] \
    || die "存在未提交的节点候选文件，拒绝升级"
  UPGRADE_SB_ACTIVE=0 UPGRADE_PANEL_ACTIVE=0
  if svc_do status sing-box; then UPGRADE_SB_ACTIVE=1; fi
  if svc_do status sbx-panel; then UPGRADE_PANEL_ACTIVE=1; fi
  # Replacing an aliased config or unit could change an unsnapshotted target.
  local item
  for item in "${UPGRADE_PATHS[@]}"; do
    [[ ! -L "$item" ]] || die "不支持以符号链接保存升级文件：$item"
  done
  upgrade_snapshot || die "无法完整保存升级前的文件，取消升级"
  printf '%s\n' "$DIST_SHA" > "$UPGRADE_DIR/generation" || die "无法记录升级目标，取消升级"
  UPGRADE_DIRTY=1
  upgrade_stop_consumers || die "无法停止现有服务，取消升级"
  upgrade_replace "$UPGRADE_DIR/stage/sbx-core" "$CORE_BIN" 0755 || die "无法替换 sbx-core"
  if [[ -f "$UPGRADE_DIR/stage/sing-box" ]]; then
    upgrade_replace "$UPGRADE_DIR/stage/sing-box" "$SB_BIN" 0755 || die "无法替换 sing-box"
  fi
  if [[ -f "$UPGRADE_DIR/stage/libcronet.so" ]]; then
    upgrade_replace "$UPGRADE_DIR/stage/libcronet.so" "$BIN_DIR/libcronet.so" 0644 || die "无法替换 libcronet.so"
  fi
  if [[ -f "$UPGRADE_DIR/stage/sing-box" ]]; then
    local kernel_version
    kernel_version=$("$SB_BIN" version) || die "升级后 sing-box 版本检查失败"
    read -r _ _ kernel_version _ <<< "$kernel_version"
    sb_version_key "$kernel_version" >/dev/null || die "升级后 sing-box 版本输出无效"
    "$SB_BIN" check -c "$SB_CONF" || die "升级后 sing-box 配置校验失败"
  fi
  "$CORE_BIN" config-migrate >/dev/null
  setup_services update
  # Managed ruleset/commit calls may access configured paths or touch nftables.
  if sbx_host_runtime; then
    core_node rulesets >/dev/null
    core_node sync >/dev/null
    commit_node >/dev/null
  fi
  if sbx_host_runtime; then
    if ! svc_do status sing-box; then
      svc_do start sing-box || die "无法启动 sing-box"
    fi
    fw_apply || die "无法应用计数规则"
    svc_do start sbx-panel || die "无法启动面板"
    upgrade_health 1 1 || die "升级后服务健康检查失败"
  fi
  install_self
  # The EXIT handler owns backup cleanup and emits the success message.
  UPGRADE_COMMITTED=1
}

do_install() {
  banner
  require_root
  detect_platform
  info "系统: $OS_FAMILY / 初始化: $INIT_SYS / 架构: $(sb_arch)"
  install_deps
  # 覆盖安装到「装过旧版但已不完整」的机器时，先清掉旧版 iptables 残留
  # （全新机器上是纯 no-op：没有 iptables.sh，也没有 SBX_IN/SBX_OUT 链）。
  cleanup_legacy_backend
  prepare_dirs
  install_sbx_core
  install_sing_box
  ensure_sb_config
  ensure_panel_conf
  install_self
  setup_services

  local host host6
  host="$(public_ip)"
  core_node set-host "$host" >/dev/null 2>&1 || true
  info "探测 IPv6 支持..."
  host6="$(public_ip6)"
  if [[ -n "$host6" ]]; then
    core_node set-host6 "$host6" >/dev/null 2>&1 || true
    ok "检测到公网 IPv6：$host6（分享链接将同时提供 IPv6 版本）"
  else
    core_node set-host6 "" >/dev/null 2>&1 || true
    info "未检测到可用的公网 IPv6，分享链接仅提供 IPv4 版本"
  fi

  # 启动服务前修掉可能让 sing-box DNS 全废的 resolv.conf 误判（见函数注释）
  fix_resolv_conf_misdetection
  start_all
  # 全新安装的计数规则应用失败必须让用户知道，不能静默带过
  fw_apply || warn "计数规则应用失败，流量统计暂不可用；可在菜单中重试"

  # 面板必须真的进入 active 才算安装成功。systemd 沙箱配置错误（例如
  # ReadWritePaths 指向不存在的路径）在老 systemd 上会让服务无限重启，
  # 而 `systemctl restart` 仍返回 0——不显式校验就会"安装完成"但面板是红的。
  if sbx_host_runtime && ! wait_service_active sbx-panel 10; then
    err "面板服务未能进入运行状态（当前: $(svc_do status sbx-panel 2>&1 || echo 未运行)）"
    err "  排查：journalctl -u sbx-panel -n 50 --no-pager"
    err "  若看到 status=226/NAMESPACE：说明服务沙箱路径与实际不符（多为路径不存在）"
    warn "其它组件已安装完成；面板修好后运行 sbx 即可，或重新执行安装脚本"
  fi

  local nnum
  nnum=$(core_node count)

  banner
  ok "安装完成"
  hr
  if [[ "$nnum" == "0" ]]; then
    printf '%s还没有任何节点。%s打开面板「配置」页并点击「添加节点」即可创建。\n' "$C_B" "$C_RESET"
  else
    printf '%s节点分享链接%s\n' "$C_B" "$C_RESET"
    core_node links --host "$host" || true
  fi
  show_panel_info
  printf '\n%s提示%s\n' "$C_B" "$C_RESET"
  printf '  · 随时运行 %ssbx%s 打开管理菜单\n' "$C_CYAN" "$C_RESET"
  printf '  · 首次使用请打开面板「配置」页添加节点\n'
  if [[ -n "$host6" ]]; then
    printf '  · 本机支持 IPv6，添加节点后会同时给出 IPv4 与 IPv6 两条分享链接\n'
  fi
  printf '  · 只向需要的来源开放节点端口；不要在云防火墙或安全组中公开明文面板端口。\n'
  echo
  if [[ ! -t 0 ]]; then
    printf '%s管道运行模式下不进入交互菜单，安装已完成。运行 sbx 打开菜单设置运维，添加节点请打开面板「配置」页。%s\n' "$C_DIM" "$C_RESET"
    return 0
  fi
  pause
  main_menu
}

# ---------------------------------------------------------------- 入口
main() {
  init_colors
  # Verify the invoking script independently from its downloaded candidate.
  validate_env || exit 1
  verify_script_integrity || exit 1
  case "${1:-}" in
    --apply-firewall) require_root; if sbx_host_runtime; then "$CORE_BIN" apply; fi; exit $? ;;
    --clear-firewall) require_root; if sbx_host_runtime; then "$CORE_BIN" clear; fi; exit $? ;;
    --panel-url) panel_url; exit 0 ;;
    --show) "$CORE_BIN" show; exit 0 ;;
    --update|update|upgrade) do_update "${2:-}"; exit $? ;;
    --apply-update|--install-dist)
      [[ -n "${SBX_SCRIPT_SHA256:-}" && -n "${2:-}" ]] || die "内部命令需要已验证的固定分发提交；请使用 --update"
      require_root
      detect_platform
      set_dist_pin "$2"
      if [[ "$1" == --apply-update ]]; then
        sbx_lock apply_update "${3:-}"
      else
        [[ ! -f "$PANEL_CONF" || ! -x "$SB_BIN" ]] || die "已安装 SBX，请使用 --update"
        do_install
      fi
      exit $? ;;
    --uninstall) require_root; detect_platform; uninstall_all ;;
    --version) echo "$APP_NAME v$APP_VERSION"; exit 0 ;;
    -h|--help)
      cat <<EOF
$APP_NAME v$APP_VERSION — sing-box 节点 + 流量面板（Go 后端，单二进制）

安装:   按项目 README 从固定提交下载并校验，再运行本地脚本
分发仓: https://github.com/$DIST_REPO/tree/dist
SBX_RAW_URL 仅为旧版保留，安装及升级不会从它下载。
菜单:   sbx
用法:   sbx [选项]

选项:
  --update           在线升级到最新版本（保留节点与流量历史）
  --update --force   强制重装固定分发版本
  --show             命令行查看流量统计
  --panel-url        输出面板访问地址
  --apply-firewall   重建流量计数规则
  --clear-firewall   移除流量计数规则
  --uninstall        卸载
  --version          版本信息
EOF
      exit 0 ;;
  esac

  require_root
  detect_platform
  # 已安装 → 菜单；未安装 → 安装
  if [[ -f "$PANEL_CONF" && -x "$SB_BIN" ]]; then
    main_menu
  else
    if [[ -n "${SBX_CORE_BIN:-}" && -x "$SBX_CORE_BIN" && -f "${BASH_SOURCE[0]}" ]]; then
      do_install  # local development/test installation
    else
      bootstrap_install
    fi
  fi
}

main "$@"
