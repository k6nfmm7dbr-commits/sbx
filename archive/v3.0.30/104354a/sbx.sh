#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="3.0.30"
RAW_URL="${SBX_RAW_URL:-https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/dist/sbx.sh}"

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

OS_FAMILY="unknown"
INIT_SYS="unknown"
PKG=""

# >>> env-validation（CI/tests/env_validation_test.sh 提取本区块做一致性测试）
# 环境变量格式校验（审计项「Shell 注入」）。
#
# 背景：安装器**全文件无 eval**，所有变量展开都带引号，用户数据不进 shell 解析
# （外部命令一律以参数列表传入）。但 SBX_* 环境变量仍会影响行为，因此做格式
# 白名单校验——目的不是"防 shell 注入"（没有那个入口），而是：
#   - 让配置错误立刻失败并说清原因，而不是中途以奇怪方式出错；
#   - 阻止控制字符/引号/反引号/换行这类会让日志与下游命令困惑的取值；
#   - SBX_GH_PROXY 仅加速大文件下载，不承载信任锚；SHA256SUMS 与升级脚本哈希
#     均直连官方 RAW 源，避免镜像同时替换文件与清单形成自证式校验。
#   - 这仍依赖 GitHub 官方仓库与账号安全；若官方源被攻破，哈希校验本身无法识别，
#     后续可通过独立签名密钥增加第二层信任锚。
validate_env() {
  local bad=0

  # SBX_GH_PROXY：必须是 http(s):// 前缀的 URL，且不含空白/控制字符/引号类字符。
  if [[ -n "${SBX_GH_PROXY:-}" ]]; then
    case "$SBX_GH_PROXY" in
      *[[:space:]]*|*[\`\$\"\'\\]*|*\;*|*\|*|*\<*\>*)
        err "SBX_GH_PROXY 含非法字符（空白/引号/反引号/美元符/分号/竖线/尖括号）"
        bad=1 ;;
    esac
    if [[ "$bad" == 0 && ! "${SBX_GH_PROXY}" =~ ^https?://[^[:space:]]+$ ]]; then
      err "SBX_GH_PROXY 必须以 http:// 或 https:// 开头，例如 https://ghfast.top/"
      bad=1
    fi
  fi

  # SBX_RAW_BASE 仅用于大文件字节流（binary），绝不作为 SHA256SUMS / sbx.sh.sha256 信任源。
  if [[ -n "${SBX_RAW_BASE:-}" ]]; then
    case "$SBX_RAW_BASE" in
      *[[:space:]]*|*[\`\$\"\'\\]*|*\;*|*\|*|*\<*\>*)
        err "SBX_RAW_BASE 含非法字符"
        bad=1 ;;
    esac
    if [[ "$bad" == 0 && ! "$SBX_RAW_BASE" =~ ^https?://[^[:space:]]+$ ]]; then
      err "SBX_RAW_BASE 必须是 http(s) URL"
      bad=1
    fi
  fi

  # SBX_ROOT：仅用于测试/沙箱安装的目录前缀，必须是干净的绝对路径。
  if [[ -n "${SBX_ROOT:-}" ]]; then
    case "$SBX_ROOT" in
      /*) ;;
      *) err "SBX_ROOT 必须是绝对路径（以 / 开头）: $SBX_ROOT"; bad=1 ;;
    esac
    if [[ "$bad" == 0 && "$SBX_ROOT" == *".."* ]]; then
      err "SBX_ROOT 不得包含 .. : $SBX_ROOT"
      bad=1
    fi
    if [[ "$bad" == 0 && ! "${SBX_ROOT}" =~ ^/[A-Za-z0-9._/-]*$ ]]; then
      err "SBX_ROOT 含非法字符: $SBX_ROOT"
      bad=1
    fi
  fi

  # SBX_SCRIPT_SHA256：设置时必须是 64 位十六进制。
  if [[ -n "${SBX_SCRIPT_SHA256:-}" && ! "${SBX_SCRIPT_SHA256}" =~ ^[0-9a-fA-F]{64}$ ]]; then
    err "SBX_SCRIPT_SHA256 必须是 64 位十六进制 sha256"
    bad=1
  fi

  if [[ -n "${SBX_UPDATE_SHA256:-}" && ! "$SBX_UPDATE_SHA256" =~ ^[0-9a-fA-F]{64}$ ]]; then
    err "SBX_UPDATE_SHA256 必须是 64 位十六进制 sha256"
    bad=1
  fi
  if [[ -n "${SBX_DIST_SHA:-}" && ! "$SBX_DIST_SHA" =~ ^[0-9a-f]{40}$ ]]; then
    err "SBX_DIST_SHA 必须是 40 位小写 Git SHA"
    bad=1
  fi

  # SBX_SB_VERSION：设置时必须是纯版本号（会拼进下载 URL）。
  if [[ -n "${SBX_SB_VERSION:-}" && ! "${SBX_SB_VERSION}" =~ ^[0-9]+(\.[0-9]+)*(-[A-Za-z0-9.]+)?$ ]]; then
    err "SBX_SB_VERSION 格式非法（应形如 1.14.0 或 1.14.3）: $SBX_SB_VERSION"
    bad=1
  fi

  [[ "$bad" == 0 ]] || return 1
  return 0
}
# <<< env-validation

# >>> script-integrity（CI/tests/script_integrity_test.sh 提取本区块做一致性测试）
# 脚本自校验（审计项「sbx.sh 远程脚本执行风险」）。
#
# 用法（README「安全安装方式」）：
#   1) 下载脚本与官方哈希：
#        curl -fsSLO <RAW_BASE>/sbx.sh
#        curl -fsSLO <RAW_BASE>/sbx.sh.sha256
#   2) 本地校验并执行：
#        sha256sum -c sbx.sh.sha256 && bash sbx.sh
#   3) 或者在运行本地文件时自校验：
#        SBX_SCRIPT_SHA256=<官方公布值> bash sbx.sh
#
# 未提供 SBX_SCRIPT_SHA256 时保持原有的一键安装便捷性（不校验），
# 但 README 明确标注该方式的风险，不再作为唯一推荐。
verify_script_integrity() {
  [[ -n "${SBX_SCRIPT_SHA256:-}" ]] || return 0
  local want self actual
  want="$(printf '%s' "$SBX_SCRIPT_SHA256" | tr 'A-F' 'a-f')"

  # Streams cannot be replayed safely for self-verification. Download a file
  # before invoking Bash if an independent SHA is required.
  self="${BASH_SOURCE[0]:-$0}"
  if [[ ! -f "$self" || ! -r "$self" ]]; then
    err "无法读取自身脚本（$self）用于完整性校验"
    err "请改用：curl -fsSLO <URL> && SBX_SCRIPT_SHA256=<哈希> bash sbx.sh"
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
  [[ -t 1 ]] && clear 2>/dev/null || true
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
  elif [[ -n "${SBX_NO_SERVICE:-}" ]]; then
    INIT_SYS="none"
  else
    die "未找到 systemd 或 OpenRC，无法安装服务"
  fi
}

pkg_install() {
  case "$PKG" in
    apk) apk add --no-cache "$@" >/dev/null 2>&1 || apk add --no-cache "$@" ;;
    apt) DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" >/dev/null 2>&1 \
         || DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" ;;
    dnf) dnf install -y "$@" >/dev/null 2>&1 || dnf install -y "$@" ;;
    yum) yum install -y "$@" >/dev/null 2>&1 || yum install -y "$@" ;;
  esac
}

install_deps() {
  info "检查依赖..."
  [[ "$PKG" == "apt" ]] && { apt-get update -qq >/dev/null 2>&1 || true; }

  # 依赖分「必需 / 可选」两类，各自独立处理。
  #
  # 为什么必须分开（真实故障）：早期实现把 curl/tar/openssl/jq 混在一次
  # `pkg_install` 调用里，任一失败就被 `set -e` 整体中断——与代码里
  # 「jq 缺失不阻断安装」的承诺自相矛盾。真机上出现过：Debian 11（已 EOL）
  # 的 security 源被移除，apt 装 jq 报 404，整个安装器当场退出，
  # 用户看到的是"装个 jq 失败就装不下去了"。
  #
  # 包管理器安装命令（按发行版给出可照抄的下一步；内联在函数内以便
  # tests/installer_flow_test.sh 整体提取本函数后仍可独立运行）。
  local pm_hint
  case "$PKG" in
    apt) pm_hint="apt-get install -y" ;;
    dnf) pm_hint="dnf install -y" ;;
    yum) pm_hint="yum install -y" ;;
    apk) pm_hint="apk add" ;;
    *)   pm_hint="请用本发行版的包管理器安装" ;;
  esac

  # ---- 必需依赖：缺失则无法下载/解包，装不上必须中止 ----
  local required=()
  command -v curl >/dev/null 2>&1 || required+=(curl)
  command -v tar  >/dev/null 2>&1 || required+=(tar)
  if ((${#required[@]})); then
    info "安装必需依赖: ${required[*]}"
    pkg_install "${required[@]}" || true   # 失败留给下面的复检统一报错
  fi
  local missing_req=()
  for c in curl tar; do
    command -v "$c" >/dev/null 2>&1 || missing_req+=("$c")
  done
  if ((${#missing_req[@]})); then
    err "必需依赖安装失败: ${missing_req[*]}"
    err "请先手动安装后重试：  $pm_hint ${missing_req[*]}"
    err "若 apt 报 404 Not Found：该发行版很可能已 EOL、软件源被移除"
    err "（例如 Debian 11 bullseye 需把源指向 archive.debian.org 后再 update）"
    die "依赖缺失，已中止安装（未做任何改动）"
  fi

  # ---- 可选依赖：缺失只降级，绝不阻断安装 ----
  #   · jq      ：候选配置提交前的 route.final 引用完整性自检（见
  #               sanitize_candidate_route）；缺失时自检降级为告警
  #   · openssl ：sha256sum 不可用时的 sha256 兜底实现
  local optional=()
  command -v jq      >/dev/null 2>&1 || optional+=(jq)
  command -v openssl >/dev/null 2>&1 || optional+=(openssl)
  if ((${#optional[@]})); then
    info "安装可选依赖: ${optional[*]}"
    if ! pkg_install "${optional[@]}"; then
      local still=()
      for c in "${optional[@]}"; do
        command -v "$c" >/dev/null 2>&1 || still+=("$c")
      done
      if ((${#still[@]})); then
        warn "可选依赖未能安装: ${still[*]}（不影响安装，相关功能自动降级）"
        warn "  常见原因：发行版已 EOL，软件源被移除（apt 报 404 Not Found）"
        warn "  如需补装：  $pm_hint ${still[*]}"
        warn "  已 EOL 的 Debian（如 11 bullseye）需先把源指向 archive.debian.org"
      fi
    fi
  fi

  # 供应链校验能力：sha256sum 或 openssl 至少要有一个，否则无法校验下载产物。
  # 这是 fail-closed 的硬要求——没有校验能力就绝不继续安装。
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v openssl >/dev/null 2>&1; then
    err "既没有 sha256sum 也没有 openssl，无法校验下载产物的完整性"
    err "请安装 coreutils 或 openssl 后重试：  $pm_hint coreutils"
    die "缺少校验工具，已中止安装（未做任何改动）"
  fi

  # 计数/策略后端：nftables 是唯一后端（v3.0.9 起 nftables-only）。
  # 不存在回退：装不上或装完仍不可用一律中止安装，绝不静默降级到其它后端。
  if ! command -v nft >/dev/null 2>&1; then
    info "安装 nftables（SBX 的唯一防火墙后端）"
    pkg_install nftables || true
  fi
  if ! command -v nft >/dev/null 2>&1; then
    # 安装命令按发行版给出，让用户能直接照抄（而不是只说"没找到 nft"）。
    # 刻意内联在 install_deps 内：本函数会被 tests/installer_flow_test.sh
    # 整体提取做隔离测试，依赖外部 helper 会导致提取后的代码不可运行。
    local nft_hint
    case "$PKG" in
      apt) nft_hint="apt-get install -y nftables" ;;
      dnf) nft_hint="dnf install -y nftables" ;;
      yum) nft_hint="yum install -y nftables" ;;
      apk) nft_hint="apk add nftables" ;;
      *)   nft_hint="请用本发行版的包管理器安装 nftables" ;;
    esac
    err "未找到 nft 命令：SBX 依赖 nftables（流量统计 / 节点暂停 / IP 限制 / 限速均由其实现）"
    err "SBX 只支持 nftables 这一种防火墙后端，不存在其它后端降级。请先安装后重试："
    err "  $nft_hint"
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
  local cur=""
  cur=$(sysctl -n net.netfilter.nf_conntrack_acct 2>/dev/null || true)
  if [[ "$cur" == "1" ]]; then
    return 0
  fi
  # 模块可能尚未加载：加载失败不算错误（内核可能已内建或稍后由 nft 触发）
  modprobe nf_conntrack 2>/dev/null || true
  if sysctl -w net.netfilter.nf_conntrack_acct=1 >/dev/null 2>&1; then
    info "已开启 conntrack 字节计费（nf_conntrack_acct=1）"
  else
    warn "无法开启 nf_conntrack_acct，在线 IP 判活将降级为「ESTABLISHED 即在线」"
    return 0
  fi
  # 持久化，重启后仍生效
  if [[ -d /etc/sysctl.d ]]; then
    printf '# 由 sbx 写入：面板在线 IP/连接判活依赖 conntrack 字节计费\nnet.netfilter.nf_conntrack_acct = 1\n' \
      > /etc/sysctl.d/99-sbx-conntrack.conf 2>/dev/null \
      || warn "写入 /etc/sysctl.d/99-sbx-conntrack.conf 失败（重启后需手工设置）"
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
  expect=$(awk -v x="$name" '$2==x{print $1}' "$sums")
  got=$(sha256_of "$f")
  if [[ -z "$expect" || "$expect" != "$got" ]]; then
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

port_busy() {
  local p="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -Hlnt "sport = :$p" 2>/dev/null | grep -q . && return 0
    ss -Hlnu "sport = :$p" 2>/dev/null | grep -q . && return 0
  elif command -v netstat >/dev/null 2>&1; then
    netstat -lnt 2>/dev/null | grep -qE "[:.]${p}[[:space:]]" && return 0
    netstat -lnu 2>/dev/null | grep -qE "[:.]${p}[[:space:]]" && return 0
  fi
  core_node port-used "$p" >/dev/null 2>&1
}

pick_port() {
  local p
  for _ in $(seq 1 60); do
    p=$(( (RANDOM % 40000) + 20000 ))
    port_busy "$p" || { echo "$p"; return 0; }
  done
  echo 34567
}

public_ip() {
  local ip=""
  # 强制 IPv4：用 -4 + 纯 IPv4 探测端点，避免返回 IPv6 地址
  for u in "https://api.ipify.org" "https://ipv4.icanhazip.com" "https://ifconfig.me/ip" "https://4.ipw.cn"; do
    ip=$(curl -4 -fsSL -m 8 "$u" 2>/dev/null | tr -d '[:space:]') || true
    # 只接受合法 IPv4
    [[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] && { echo "$ip"; return 0; }
    ip=""
  done
  # 兜底：本机默认路由的 IPv4 源地址
  ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}') || true
  [[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] && { echo "$ip"; return 0; }
  # 最后再试本机第一个非回环 IPv4
  ip=$(ip -4 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1) || true
  echo "${ip:-127.0.0.1}"
}

# 探测公网 IPv6：能通过 IPv6 访问外网才算"可用"，否则视为无
public_ip6() {
  local ip=""
  for u in "https://api6.ipify.org" "https://ipv6.icanhazip.com" "https://6.ipw.cn"; do
    ip=$(curl -6 -fsSL -m 8 "$u" 2>/dev/null | tr -d '[:space:]') || true
    # 合法 IPv6：含冒号且只有十六进制/冒号，排除链路本地 fe80::
    if [[ "$ip" == *:* && "$ip" =~ ^[0-9A-Fa-f:]+$ && "${ip,,}" != fe80:* ]]; then
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

install_sing_box() {
  local force="${1:-}"
  # 已安装：正常跳过。但若版本低于发布基线 SB_VERSION_DEFAULT（如老机器
  # 停在 1.14.0-rc.1），随面板安装/升级一并把内核带上去——用户显式设置了
  # SB_VERSION_PIN 时尊重其选择，不自动动内核。
  if [[ -x "$SB_BIN" ]] && "$SB_BIN" version >/dev/null 2>&1; then
    local cur_sb
    cur_sb=$("$SB_BIN" version 2>/dev/null | awk 'NR==1{print $3}')
    if [[ -n "$cur_sb" && -z "$force" && -z "${SB_VERSION_PIN:-}" ]] \
       && ! sb_version_ge "$cur_sb" "$SB_VERSION_DEFAULT"; then
      info "sing-box 内核 $cur_sb 低于发布基线 $SB_VERSION_DEFAULT，自动升级..."
      force=1
    fi
    if [[ -z "$force" ]]; then
      ok "sing-box 已安装: $("$SB_BIN" version | head -1)"
      return 0
    fi
  elif [[ -z "$force" ]] && [[ -n "${SBX_SB_BIN:-}" && -x "${SBX_SB_BIN}" ]]; then
    install -d -m 0755 "$BIN_DIR"
    cat "$SBX_SB_BIN" > "$SB_BIN"; chmod 0755 "$SB_BIN"
    ok "使用本地 sing-box: $("$SB_BIN" version | head -1)"
    return 0
  fi
  local ver arch suffix tmp url name expected
  ver="${SB_VERSION_PIN:-$SB_VERSION_DEFAULT}"
  arch="$(sb_arch)"
  # Alpine 用 musl 构建，其余用默认（静态）构建
  suffix=""
  [[ "$OS_FAMILY" == "alpine" ]] && { case "$arch" in amd64|arm64|armv7|386|riscv64) suffix="-musl";; esac; }
  name="sing-box-${ver}-linux-${arch}${suffix}.tar.gz"

  tmp=$(mktemp -d)
  url="https://github.com/SagerNet/sing-box/releases/download/v${ver}/${name}"
  info "下载 sing-box v${ver} (${arch}${suffix})"
  if ! curl -fsSL -m 300 -o "$tmp/sb.tar.gz" "$(gh_url "$url")"; then
    # musl 包不存在时退回默认包
    if [[ -n "$suffix" ]]; then
      name="sing-box-${ver}-linux-${arch}.tar.gz"
      url="https://github.com/SagerNet/sing-box/releases/download/v${ver}/${name}"
      warn "musl 包不可用，改用默认构建"
      curl -fsSL -m 300 -o "$tmp/sb.tar.gz" "$(gh_url "$url")" \
        || { rm -rf "$tmp"; die "下载 sing-box 失败，可设置 SBX_GH_PROXY 使用镜像"; }
    else
      rm -rf "$tmp"; die "下载 sing-box 失败，可设置 SBX_GH_PROXY 使用镜像"
    fi
  fi
  # 供应链校验：sha256 必须与项目 pin 的期望一致（官方无逐文件 checksum）。
  expected=$(sb_expected_sha "$name")
  if [[ -z "$expected" ]]; then
    rm -rf "$tmp"
    die "sing-box ${ver} 无校验哈希，拒绝安装（供应链保护）；请使用默认版本或反馈维护者补充"
  fi
  [[ "$(sha256_of "$tmp/sb.tar.gz")" == "$expected" ]] \
    || { rm -rf "$tmp"; die "sing-box 校验失败，已中止安装"; }

  tar xzf "$tmp/sb.tar.gz" -C "$tmp" || { rm -rf "$tmp"; die "解压失败"; }
  local found
  found=$(find "$tmp" -type f -name sing-box | head -1)
  [[ -n "$found" ]] || { rm -rf "$tmp"; die "压缩包中未找到 sing-box"; }

  # 升级前备份旧二进制（存在时），供失败回滚
  if [[ -x "$SB_BIN" ]]; then
    cp -f "$SB_BIN" "$SB_BIN.bak" || { rm -rf "$tmp"; die "旧 sing-box 备份失败，已中止升级"; }
  fi
  install -d -m 0755 "$BIN_DIR"
  # 原子替换（与 install_sbx_core 的 tmp+mv 同一口径）：install 原地截断写入，
  # 中断/掉电会留下损坏的 sing-box 且 .bak 无人恢复；同目录临时文件 + rename
  # 保证任意时刻 $SB_BIN 都是完整可执行体。失败时旧文件分毫未动。
  local sb_tmp="$BIN_DIR/.sing-box.tmp.$$"
  cp -f "$found" "$sb_tmp" && chmod 0755 "$sb_tmp" && mv -f "$sb_tmp" "$SB_BIN" \
    || { rm -f "$sb_tmp"; rm -rf "$tmp"; die "sing-box 安装失败；现有安装未被改动"; }
  # 部分构建附带 libcronet.so（NaiveProxy 用），一并放到同目录
  local lib
  lib=$(find "$tmp" -type f -name 'libcronet.so' | head -1)
  if [[ -n "$lib" ]]; then
    install -m 0644 "$lib" "$BIN_DIR/libcronet.so" || { rm -rf "$tmp"; die "无法安装 libcronet.so"; }
  fi
  rm -rf "$tmp"

  if ! "$SB_BIN" version >/dev/null 2>&1; then
    [[ -f "$SB_BIN.bak" ]] && { mv -f "$SB_BIN.bak" "$SB_BIN"; warn "新 sing-box 异常，已回滚旧版本"; }
    die "sing-box 无法运行（架构或 libc 不匹配）"
  fi
  rm -f "$SB_BIN.bak"
  ok "sing-box 安装完成: $("$SB_BIN" version | head -1)"
}

# sb_supports_snell 判断当前 sing-box 是否支持 Snell（>= 1.14.0）。
sb_supports_snell() {
  local v
  v=$("$SB_BIN" version 2>/dev/null | head -1 | sed -nE 's/.*version v?([0-9][0-9.]*).*/\1/p')
  [[ -n "$v" ]] && ver_ge "$v" "1.14.0"
}

# ---------------------------------------------------------------- sbx-core 安装
install_sbx_core() {
  install -d -m 0755 "$BIN_DIR"
  # 开发/测试：直接使用本地构建的二进制
  if [[ -n "${SBX_CORE_BIN:-}" && -x "${SBX_CORE_BIN}" ]]; then
    cat "$SBX_CORE_BIN" > "$CORE_BIN.tmp.$$" && chmod 0755 "$CORE_BIN.tmp.$$" \
      && mv -f "$CORE_BIN.tmp.$$" "$CORE_BIN" \
      || die "本地 sbx-core 拷贝失败；现有安装未被改动"
    ok "使用本地 sbx-core: $("$CORE_BIN" version 2>/dev/null | head -1)"
    return 0
  fi
  # 更新判断基于「代码内容」而非版本号：dist 是 rolling latest，版本号不随
  # 功能递增，二进制内容会变而版本号不变。因此先取 SHA256SUMS 与本地二进制做
  # 内容比较——一致则跳过，不一致才下载替换。绝不能用「版本号相等」判断跳过
  # （那会导致旧二进制永远无法刷新）。

  local arch name tmp dl rc=0
  arch="$(sb_arch)"
  name="sbx-core-linux-${arch}"
  tmp=$(mktemp -d)
  # 临时文件放在目标目录内：最后的 mv 是同文件系统 rename，保证原子性
  dl="$BIN_DIR/.sbx-core.dl.$$"
  cleanup() { rm -rf "$tmp" "$dl" 2>/dev/null || true; }

  # Only binary bytes can be mirrored. The official immutable manifest
  # authenticates those bytes, even if a custom mirror serves a rolling URL.
  local base="${SBX_RAW_BASE:-$DIST_BASE}"
  [[ "$DIST_SHA" =~ ^[0-9a-f]{40}$ && -n "$DIST_BASE" ]] \
    || { cleanup; die "缺少固定分发提交，拒绝下载 sbx-core"; }
  curl -fsSL -m 60 -o "$tmp/SHA256SUMS" "$DIST_BASE/SHA256SUMS" \
    || { cleanup; die "从固定官方提交下载 SHA256SUMS 失败"; }
  if [[ -x "$CORE_BIN" ]]; then
    local local_sum expect_sum expect_count
    local_sum=$(sha256_of "$CORE_BIN" 2>/dev/null || true)
    expect_sum=$(awk -v x="$name" '$2==x{print $1}' "$tmp/SHA256SUMS")
    expect_count=$(awk -v x="$name" '$2==x{c++} END{print c+0}' "$tmp/SHA256SUMS")
    if [[ "$expect_count" == 1 && -n "$local_sum" && "$local_sum" == "$expect_sum" ]] \
       && [[ "$(core_version_of "$CORE_BIN")" == "$APP_VERSION" ]]; then
      cleanup
      ok "sbx-core 已是最新: $("$CORE_BIN" version 2>/dev/null | head -1)"
      return 0
    fi
  fi

  # 内容不一致或本地缺失 → 下载完整二进制并校验替换
  info "下载 sbx-core (${arch})..."
  curl -fsSL -m 300 -o "$dl" "$(gh_url "${base%/}/${name}")" \
    || { cleanup; die "下载 sbx-core 失败（可设置 SBX_GH_PROXY 使用镜像）；现有安装未被改动"; }
  verify_core_checksum "$dl" "$tmp/SHA256SUMS" "$name" \
    || { cleanup; die "sbx-core 校验失败；现有安装未被改动"; }

  chmod 0755 "$dl"
  # 替换前自检 1：新二进制必须能在本机执行
  "$dl" version >/dev/null 2>&1 \
    || { cleanup; die "sbx-core 自检失败（架构或 libc 不匹配）；现有安装未被改动"; }
  # 替换前自检 2：candidate 版本必须与安装器 APP_VERSION 严格一致（fail-closed）。
  # dist 分支是 rolling latest，若 CDN 尚未刷新或发布链异常，candidate 可能是旧版本——
  # 即使 SHA256 与 checksums 自洽也必须拒绝，防止脚本与后端版本错位。
  local cand_ver
  cand_ver=$(core_version_of "$dl")
  if [[ "$cand_ver" != "$APP_VERSION" ]]; then
    cleanup
    die "sbx-core 版本不匹配，已拒绝安装（现有安装未被改动）
  期望: $APP_VERSION    实际: ${cand_ver:-无法解析}
  提示: dist 分支产物可能尚未同步，请稍后重试或反馈维护者"
  fi

  # 原子替换；保留旧版备份以备极端回滚（备份失败则中止，旧二进制绝不能被无备份覆盖）
  if [[ -x "$CORE_BIN" ]]; then
    cp -f "$CORE_BIN" "$CORE_BIN.bak" || { cleanup; die "旧 sbx-core 备份创建失败，已中止安装（现有安装未被改动）"; }
  fi
  mv -f "$dl" "$CORE_BIN" || { cleanup; die "无法替换 sbx-core"; }
  rm -rf "$tmp"
  if ! "$CORE_BIN" version >/dev/null 2>&1; then
    [[ -f "$CORE_BIN.bak" ]] && { mv -f "$CORE_BIN.bak" "$CORE_BIN"; warn "新二进制异常，已回滚"; }
    die "sbx-core 安装后自检失败"
  fi
  rm -f "$CORE_BIN.bak"
  ok "sbx-core 安装完成: $("$CORE_BIN" version | head -1)"
}

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
    chmod 600 "$PANEL_CONF"
    return 0
  fi
  local token port
  token=$(rand_hex 16)
  port=$(pick_port)
  # 先以 0600 预创建（不受 umask 影响），避免含 token 的内容出现短暂暴露窗口
  install -m 0600 /dev/null "$PANEL_CONF"
  # listen 默认 0.0.0.0（与 internal/config 的 defaultConf() 一致）：面板自带
  # 随机强令牌认证。明文 HTTP 下 Cookie 携带令牌，公网部署建议在「面板设置」
  # 切回 127.0.0.1 并配置 HTTPS 反代，或启用 secure_cookie。
  cat > "$PANEL_CONF" <<EOF
{
  "db": "$APP_DIR/traffic.db",
  "nodes_file": "$NODES_JSON",
  "nft_conf": "$APP_DIR/nft.conf",
  "web_root": "$WEB_DIR",
  "listen": "0.0.0.0",
  "port": $port,
  "token": "$token",
  "interval": 2,
  "tz": "Asia/Shanghai"
}
EOF
  chmod 600 "$PANEL_CONF"
  ok "面板配置已生成（端口 $port）"
}

panel_get() { "$CORE_BIN" config-get "$1"; }
panel_set() { "$CORE_BIN" config-set "$1" "$2"; }

# ---------------------------------------------------------------- 服务管理
svc_do() {  # svc_do <start|stop|restart|enable|status> <name>
  local action="$1" name="$2"
  [[ -n "${SBX_NO_SERVICE:-}" ]] && return 0
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
  [[ -s "$CERT_DIR/cert.pem" && -s "$CERT_DIR/key.pem" ]] && return 0
  info "生成自签证书 (CN=$sni)"
  local out
  out=$("$SB_BIN" generate tls-keypair "$sni" -m 1200 2>/dev/null) || die "生成证书失败"
  # awk 拆分 PEM 块
  printf '%s\n' "$out" | awk '
    /-----BEGIN CERTIFICATE-----/ {inc=1}
    inc {print > "'"$CERT_DIR"'/cert.pem.part"}
    /-----END CERTIFICATE-----/ {inc=0}
    /-----BEGIN PRIVATE KEY-----/ {ink=1}
    ink {print > "'"$CERT_DIR"'/key.pem.part"}
    /-----END PRIVATE KEY-----/ {ink=0}
  '
  [[ -s "$CERT_DIR/cert.pem.part" && -s "$CERT_DIR/key.pem.part" ]] \
    || die "证书解析失败"
  mv -f "$CERT_DIR/cert.pem.part" "$CERT_DIR/cert.pem"
  mv -f "$CERT_DIR/key.pem.part" "$CERT_DIR/key.pem"
  chmod 600 "$CERT_DIR/key.pem"; chmod 644 "$CERT_DIR/cert.pem"
  ok "证书就绪"
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
    rm -f "$APP_DIR/iptables.sh" \
      && info "已删除旧版残留文件 $APP_DIR/iptables.sh" \
      || warn "删除 $APP_DIR/iptables.sh 失败（可手工删除）"
  fi

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
  local label="$1" def="$2" p
  while :; do
    printf '%s端口 (%s，回车用 %s，q 取消): %s' "$C_DIM" "$label" "$def" "$C_RESET" >&2
    read -r p || { echo; return 1; }
    [[ "$p" == "q" || "$p" == "Q" ]] && { echo; return 1; }
    p="${p:-$def}"
    if ! valid_port "$p"; then warn "端口需在 1-65535"; continue; fi
    if core_node port-used "$p" >/dev/null 2>&1; then warn "端口 $p 已被其它节点使用"; continue; fi
    if port_busy "$p"; then
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

prompt_sni() {
  local def="${1:-www.microsoft.com}" s
  printf '%s伪装域名 SNI (回车用 %s，q 取消): %s' "$C_DIM" "$def" "$C_RESET" >&2
  read -r s || { echo; return 1; }
  [[ "$s" == "q" || "$s" == "Q" ]] && { echo; return 1; }
  s="${s:-$def}"
  [[ "$s" =~ ^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || { warn "域名格式无效，使用 $def"; s="$def"; }
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
SBX_LOCK="${SBX_LOCK:-/run/lock/sbx.lock}"

# sbx_lock <func> [args...]：在排他锁内执行函数；获取失败报错并返回非 0。
sbx_lock() {
  mkdir -p "$(dirname "$SBX_LOCK")" 2>/dev/null || true
  (
    command -v flock >/dev/null || die "缺少 flock；请先安装提供 flock 的软件包"
    flock 9 || { err "检测到另一个 sbx 节点操作正在进行，请稍后重试"; exit 75; }
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
# 约束：纯读改候选文件，绝不触碰正式配置；jq 缺失或任何异常都只告警并放行，
#       让后续 check/restart/回滚链路照原样兜底（fail-open：不比修复前更糟）。
sanitize_candidate_route() {
  local cand="$1" verb tag tmp
  [[ -f "$cand" ]] || return 0
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
    warn "候选配置 route.final 自检失败（JSON 解析异常），已跳过自检"
    return 0
  }
  case "$verb" in
    ok) return 0 ;;
    set|del) ;;
    *) warn "候选配置 route.final 自检结果异常，已跳过自检"; return 0 ;;
  esac
  # 阶段二：确实存在悬空引用才改写候选文件（正式配置始终不受影响）
  tmp="$cand.route-fix"
  if ! jq '
      (.outbounds // []) as $obs
    | (($obs | map(select(.type? == "direct")) | first | .tag?) // null) as $d
    | if $d != null then .route.final = $d else del(.route.final) end
  ' "$cand" > "$tmp" 2>/dev/null; then
    rm -f "$tmp"
    warn "候选配置 route.final 改写失败，已跳过自检"
    return 0
  fi
  [[ -s "$tmp" ]] || { rm -f "$tmp"; warn "候选配置 route.final 改写结果为空，已跳过自检"; return 0; }
  chmod 600 "$tmp" 2>/dev/null || true
  mv -f "$tmp" "$cand" || { rm -f "$tmp"; warn "候选配置 route.final 写回失败，已跳过自检"; return 0; }
  if [[ "$verb" == "set" ]]; then
    tag=$(jq -r '.route.final // "?"' "$cand" 2>/dev/null)
    info "已校正候选配置 route.final → ${tag}（原值指向不存在的出站 tag）"
  else
    info "已移除候选配置悬空的 route.final（无可用 direct 出站）"
  fi
  return 0
}

commit_node() {
  # Preparation and commit share FD 9. The backend owns validation, access
  # isolation, transactional backups, sing-box restart and recovery.
  if [[ -f "$SB_CONF.candidate" ]]; then
    sanitize_candidate_route "$SB_CONF.candidate"
  fi
  core_node commit
}
# <<< commit-node-flow

add_vless() {
  local port name sni uuid kp priv pub sid
  port=$(prompt_port "VLESS Reality" 443) || return 0
  name=$(prompt_name "vless-$port") || return 0
  sni=$(prompt_sni www.microsoft.com) || return 0
  uuid=$(rand_uuid)
  kp=$("$SB_BIN" generate reality-keypair)
  priv=$(echo "$kp" | awk '/PrivateKey/{print $2}')
  pub=$(echo "$kp" | awk '/PublicKey/{print $2}')
  sid=$(rand_hex 8)
  sbx_lock do_add_vless "$port" "$name" "$sni" "$uuid" "$priv" "$pub" "$sid"
  local rc=$?
  case $rc in
    0) ok "VLESS Reality 节点已添加"; show_links_for_last ;;
    2) warn "节点已创建，但流量统计规则应用失败"; show_links_for_last ;;
    *) warn "节点添加失败" ;;
  esac
}
do_add_vless() {
  core_node add vless --port="$1" --name="$2" --uuid="$4" --sni="$3" \
    --flow xtls-rprx-vision --private-key="$5" --public-key="$6" --short-id="$7" >/dev/null || return 1
  commit_node
}

add_ss() {
  local port name method pw
  method=$(prompt_ss2022_method) || return 0
  port=$(prompt_port "Shadowsocks 2022" "$(pick_port)") || return 0
  name=$(prompt_name "ss-$port") || return 0
  pw=$(core_node ss2022-key --method="$method") || { warn "生成 Shadowsocks 2022 密钥失败"; return 1; }
  sbx_lock do_add_ss "$port" "$name" "$method" "$pw"
  local rc=$?
  case $rc in
    0) ok "Shadowsocks 2022 节点已添加"; show_links_for_last ;;
    2) warn "节点已创建，但流量统计规则应用失败"; show_links_for_last ;;
    *) warn "节点添加失败" ;;
  esac
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
  sbx_lock do_add_trojan "$port" "$name" "$sni" "$pw"
  local rc=$?
  case $rc in
    0) ok "Trojan 节点已添加"; show_links_for_last ;;
    2) warn "节点已创建，但流量统计规则应用失败"; show_links_for_last ;;
    *) warn "节点添加失败" ;;
  esac
}
do_add_trojan() {
  core_node add trojan --port="$1" --name="$2" --password="$4" --sni="$3" >/dev/null || return 1
  commit_node
}

add_anytls() {
  local port name sni pw
  port=$(prompt_port "AnyTLS" "$(pick_port)") || return 0
  name=$(prompt_name "anytls-$port") || return 0
  sni=$(prompt_sni www.bing.com) || return 0
  ensure_certs "$sni"
  pw=$(rand_hex 12)
  sbx_lock do_add_anytls "$port" "$name" "$sni" "$pw"
  local rc=$?
  case $rc in
    0) ok "AnyTLS 节点已添加"; show_links_for_last ;;
    2) warn "节点已创建，但流量统计规则应用失败"; show_links_for_last ;;
    *) warn "节点添加失败" ;;
  esac
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
    if ! install_sing_box force; then
      err "sing-box 内核升级失败，已中止创建 Snell"
      return 1
    fi
    # 升级后校验：配置通过 + 服务真实运行
    "$SB_BIN" check -c "$SB_CONF" >/dev/null 2>&1 \
      || { err "升级后配置校验失败，已中止创建 Snell（原配置未动）"; return 1; }
    if ! svc_do restart sing-box; then
      err "升级后 sing-box 服务启动失败，已中止创建 Snell"
      return 1
    fi
    sb_supports_snell || { err "升级后 sing-box 仍不支持 Snell，已中止创建"; return 1; }
    ok "sing-box 已升级到 $("$SB_BIN" version | head -1)"
  fi

  local port name psk extra
  port=$(prompt_port "Snell v$ver" "$(pick_port)") || return 0
  name=$(prompt_name "snell-v$ver-$port") || return 0
  psk=$("$CORE_BIN" secret hex 32) || { warn "生成 PSK 失败"; return 1; }
  extra=()
  if [[ "$ver" == 5 ]]; then
    extra=(--obfs-mode="none")
  else
    extra=(--mode="default")
  fi

  sbx_lock do_add_snell "$ver" "$port" "$name" "$psk" "${extra[@]}"
  local rc=$?
  case $rc in
    0) ok "Snell v$ver 节点已添加"; show_links_for_last ;;
    2) warn "节点已创建，但流量统计规则应用失败"; show_links_for_last ;;
    *) warn "节点添加失败" ;;
  esac
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
fix_resolv_conf_misdetection() {
  # systemd-resolved 正在运行 → 不做任何改动
  if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet systemd-resolved 2>/dev/null; then
    return 0
  fi

  # 路径可用环境变量覆盖：测试需要在临时目录上验证，且 CI 不能碰真实 /etc。
  # 生产默认值即真实路径，不设这些变量时行为完全一致。
  local fixed=0 head="${SBX_RESOLVCONF_HEAD:-/etc/resolvconf/resolv.conf.d/head}"
  local resolv="${SBX_RESOLV_CONF:-/etc/resolv.conf}"
  # 1) 源头：resolvconf 的 head 模板（改了它，重启/网络变化后也不会再生成回来）
  if [[ -f "$head" ]] && grep -q '127\.0\.0\.53' "$head"; then
    sed -i '/127\.0\.0\.53/d' "$head" && fixed=1
  fi
  # 2) 当前生效的文件：跟随符号链接改真实文件
  local real
  real="$(readlink -f "$resolv" 2>/dev/null || true)"
  [[ -n "$real" && -f "$real" ]] || real="$resolv"
  if [[ -f "$real" ]] && grep -q '127\.0\.0\.53' "$real"; then
    # 仅当它**不是**真正的 nameserver 行时才删
    if ! grep -qE '^[[:space:]]*nameserver[[:space:]]+127\.0\.0\.53([[:space:]]|$)' "$real"; then
      sed -i '/127\.0\.0\.53/d' "$real" && fixed=1
    fi
  fi

  if [[ "$fixed" == 1 ]]; then
    # 让 resolvconf 用新模板重新生成（失败也不影响：文件本身已经改好了）
    if command -v resolvconf >/dev/null 2>&1; then
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
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
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
      systemctl daemon-reload
      if [[ "$mode" != update ]]; then systemctl enable sing-box sbx-firewall sbx-panel >/dev/null 2>&1; fi
      ;;

    openrc)
      cat > "$INIT_DIR"/sing-box <<EOF
#!/sbin/openrc-run
name="sing-box"
description="sing-box service"
command="$SB_BIN"
command_args="run -C $SB_DIR"
command_background=true
pidfile="/run/\${RC_SVCNAME}.pid"
output_log="/var/log/sing-box.log"
error_log="/var/log/sing-box.log"
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
pidfile="/run/\${RC_SVCNAME}.pid"
export SBX_CONF="$PANEL_CONF"
output_log="/var/log/sbx-panel.log"
error_log="/var/log/sbx-panel.log"
depend() { need net; after sbx-firewall; }
EOF
      chmod +x "$INIT_DIR/sing-box" "$INIT_DIR/sbx-firewall" "$INIT_DIR/sbx-panel"
      if [[ "$mode" != update ]]; then
        rc-update add sbx-firewall default >/dev/null 2>&1
        rc-update add sing-box default >/dev/null 2>&1
        rc-update add sbx-panel default >/dev/null 2>&1
      fi
      ;;
  esac
  if [[ "$mode" == update ]]; then ok "服务定义已刷新（保留开机自启设置）";
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
  # 默认只监听回环：此时用公网 IP 拼地址是错的（用户点开必然连不上），
  # 必须显示 127.0.0.1，并让用户知道外网访问需要在「面板设置」里显式切换。
  if [[ "$listen" == "127.0.0.1" || "$listen" == "localhost" || "$listen" == "::1" ]]; then
    echo "http://127.0.0.1:$port/"
    return 0
  fi
  host=$(core_node get-host); [[ -z "$host" ]] && host="$(public_ip)"
  echo "http://$(host_for_uri "$host"):$port/"
}

show_panel_info() {
  hr
  printf '%s流量面板%s\n' "$C_B" "$C_RESET"
  printf '  地址: %s%s%s\n' "$C_CYAN" "$(panel_url)" "$C_RESET"
  printf '  令牌: %s%s%s\n' "$C_CYAN" "$(panel_get token)" "$C_RESET"
  printf '  状态: %s\n' "$(panel_running && echo "${C_GREEN}运行中${C_RESET}" || echo "${C_RED}未运行${C_RESET}")"
  # 后端恒为 nftables（nftables-only）；这里只报告它当前是否真的可用。
  printf '  后端: %s\n' "$(nft list tables >/dev/null 2>&1 \
    && echo "nftables" || echo "${C_RED}nftables 不可用${C_RESET}")"
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
  case "$c" in
    1) add_vless ;; 2) add_ss ;; 3) add_trojan ;; 4) add_anytls ;; 5) menu_snell ;;
    0|"") return 0 ;;
    *) warn "无效选择" ;;
  esac
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
  sbx_lock do_remove_node "$id"
  local rc=$?
  case $rc in
    0) ok "节点 $id 已删除" ;;
    2) warn "节点已删除，但流量统计规则应用失败" ;;
    *) warn "删除失败" ;;
  esac
  if [[ $rc -ne 1 ]]; then
    printf '%s该节点的历史流量数据保留在数据库中。要一并清除吗? [y/N] %s' "$C_DIM" "$C_RESET"
    read -r yn || true
    [[ "${yn,,}" == "y" ]] && { "$CORE_BIN" reset "node:$id"; }
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
  info=$(core_node info "$id")
  [[ -z "$info" ]] && { warn "未找到节点 $id"; pause; return 1; }
  type=$(echo "$info" | cut -f1); sni=$(echo "$info" | cut -f2)
  port=$(echo "$info" | cut -f3)

  local disp_type="$type"
  if [[ "$type" == "snell" ]]; then
    disp_type="Snell v$(echo "$info" | cut -f4)"
  fi
  printf '\n节点类型: %s   当前端口: %s\n' "$disp_type" "$port"
  [[ -n "$sni" ]] && printf '当前 SNI: %s\n' "$sni"
  echo

  local args=("$id")

  printf '新端口 (回车不改): '
  read -r np || true
  if [[ -n "$np" ]]; then
    valid_port "$np" || { warn "端口无效"; pause; return 1; }
    if core_node port-used "$np" >/dev/null 2>&1; then
      # 允许改成自己当前端口（等于没改）
      [[ "$np" != "$port" ]] && { warn "端口 $np 已被其它节点占用"; pause; return 1; }
    fi
    port_busy "$np" && [[ "$np" != "$port" ]] && {
      printf '%s端口 %s 已被系统其它进程监听，仍要使用? [y/N] %s' "$C_YEL" "$np" "$C_RESET"
      read -r yn || true; [[ "${yn,,}" == "y" ]] || { pause; return 1; }
    }
    args+=("--port=$np")
  fi

  # 仅对支持 SNI 的类型询问
  case "$type" in
    vless|trojan|anytls)
      printf '新 SNI 伪装域名 (回车不改): '
      read -r ns || true
      if [[ -n "$ns" ]]; then
        [[ "$ns" =~ ^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || { warn "域名格式无效"; pause; return 1; }
        args+=("--sni=$ns")
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
            args+=("--method=$new_method")
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
        args+=("--psk=$new_psk")
      fi ;;
  esac

  if [[ ${#args[@]} -le 1 ]]; then echo "未做任何修改"; pause; return 0; fi

  local out rc
  out=$(sbx_lock do_edit_node "${args[@]}" 2>/dev/null)
  rc=$?
  if [[ $rc -eq 0 ]]; then
    ok "节点 $id 已更新：$(printf '%s' "$out" | tr ',' '\n' | grep -o '端口→[0-9]*\|SNI→[^"]*\|PSK 已更新' | paste -sd' ' - || echo '完成')"
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
    warn "$out"
  fi
  pause
}
do_edit_node() {
  local out
  out=$("$CORE_BIN" node edit "$@" 2>&1) || { echo "$out" >&2; return 1; }
  commit_node
  local rc=$?
  echo "$out"
  return $rc
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
       valid_port "$p" && { panel_set port "$p"; svc_do restart sbx-panel; ok "已改为 $p"; } || warn "无效端口" ;;
    2) printf '采集间隔秒数 (1-60): '; read -r n || true
       [[ "$n" =~ ^[0-9]+$ ]] && ((n>=1 && n<=60)) && { panel_set interval "$n"; svc_do restart sbx-panel; ok "已改为 ${n}s"; } || warn "无效数值" ;;
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
    1) start_all; ok "已重启" ;;
    2) svc_do stop sing-box; svc_do stop sbx-panel; ok "已停止" ;;
    3) start_all; ok "已启动" ;;
    4) if fw_apply; then ok "计数规则已重建"; else err "计数规则重建失败，请检查系统防火墙工具"; fi ;;
    5) if [[ "$INIT_SYS" == "systemd" ]]; then journalctl -u sing-box -n 60 --no-pager
       else tail -n 60 /var/log/sing-box.log 2>/dev/null || echo "(暂无日志)"; fi ;;
    6) if [[ "$INIT_SYS" == "systemd" ]]; then journalctl -u sbx-panel -n 60 --no-pager
       else tail -n 60 /var/log/sbx-panel.log 2>/dev/null || echo "(暂无日志)"; fi ;;
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
  local bkdir="$ROOT/var/backups" dest
  dest="$bkdir/sbx-sing-box-$(date +%Y%m%d-%H%M%S).tar.gz"
  mkdir -p "$bkdir" 2>/dev/null || return 1
  if ! tar -czf "$dest" -C "$(dirname "$SB_DIR")" "$(basename "$SB_DIR")" 2>/dev/null; then
    rm -f "$dest" 2>/dev/null || true
    return 1
  fi
  chmod 600 "$dest" 2>/dev/null || true
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

  upgrade_stop_consumers || die "无法停止 sing-box 或面板，拒绝清除访问规则"
  svc_do stop sbx-firewall || die "无法停止计数服务，已中止卸载"
  fw_clear || die "无法清除 SBX 防火墙规则，已中止卸载"
  # 若这台机器曾装过旧版（≤ v3.0.8），把它当年创建的 iptables 链一并清掉，
  # 否则卸载后 SBX_IN/SBX_OUT 会残留在内核里直到重启。
  cleanup_legacy_backend
  case "$INIT_SYS" in
    systemd)
      systemctl disable sing-box sbx-panel sbx-firewall >/dev/null 2>&1 || true
      rm -f "$UNIT_DIR"/{sing-box,sbx-panel,sbx-firewall}.service
      systemctl daemon-reload ;;
    openrc)
      for s in sbx-panel sing-box sbx-firewall; do
        rc-update del "$s" default >/dev/null 2>&1 || true
        rm -f "$INIT_DIR/$s"
      done ;;
  esac
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
  rm -f /etc/sysctl.d/99-sbx-conntrack.conf 2>/dev/null || true
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
      2) do_update; pause ;;
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
# 刻意把"拼配置"交给 sbx-core 的 notify-config enable（按位置收参数），
# 而不是在 shell 里拼 JSON：shell 拼 JSON 极易写出坏结构，而且报错极难查。
menu_notify_setup() {
  banner
  printf '%s配置飞书日报%s\n\n' "$C_B" "$C_RESET"
  echo "在飞书群里添加「自定义机器人」后会得到一条 webhook 地址，形如:"
  printf '  %shttps://open.feishu.cn/open-apis/bot/v2/hook/xxxxxxxx%s\n\n' "$C_DIM" "$C_RESET"

  local hook secret at
  printf 'Webhook 地址: '
  read -r hook || true
  hook="${hook//[[:space:]]/}"
  [[ -n "$hook" ]] || { warn "不能为空"; return 0; }
  notify_safe_value "$hook" || { warn "输入含控制字符，已取消"; return 0; }

  printf '签名密钥 secret（可选，直接回车表示不用签名）: '
  read -r secret || true
  notify_safe_value "$secret" || { warn "输入含控制字符，已取消"; return 0; }

  printf '每天几点发送日报 [00:05]: '
  read -r at || true
  [[ -n "$at" ]] || at="00:05"

  local rc=0
  "$CORE_BIN" notify-config enable "$hook" "$secret" "$at" || rc=$?
  [[ $rc -eq 0 ]] || return 0
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
  local source="${BASH_SOURCE[0]}" wrapper
  install -d -m 0755 "$BIN_DIR"
  # Fresh install and update execute from a verified regular staging file.
  [[ -f "$source" ]] || die "无法持久化已验证安装器"
  if [[ ! "$source" -ef "$SELF_PATH" ]]; then
    upgrade_replace "$source" "$SELF_PATH" 0755
  fi
  wrapper=$(mktemp "$BIN_DIR/.sbx-wrapper.XXXXXXXX")
  printf '#!/usr/bin/env bash\nexec bash %q "$@"\n' "$SELF_PATH" > "$wrapper"
  chmod 0755 "$wrapper"
  mv -fT -- "$wrapper" "$CMD_PATH"
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
    refs=$(curl -fsSL -m 15 "https://api.github.com/repos/$DIST_REPO/git/ref/heads/dist") || return 1
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
  curl -fsSL -m 60 -o "$dir/sbx.sh" "$DIST_BASE/sbx.sh" || die "无法下载固定提交的安装器"
  curl -fsSL -m 60 -o "$dir/sbx.sh.sha256" "$DIST_BASE/sbx.sh.sha256" || die "无法下载固定提交的脚本校验和"
  count=$(awk '$2=="sbx.sh" {c++} END {print c+0}' "$dir/sbx.sh.sha256")
  want=$(awk '$2=="sbx.sh" {print $1}' "$dir/sbx.sh.sha256")
  [[ "$count" == 1 ]] && verify_remote_script "$dir/sbx.sh" "$want" "官方 sbx.sh.sha256" \
    || die "分发脚本校验失败，未改动现有安装"
  if [[ -n "$anchor" ]]; then
    verify_remote_script "$dir/sbx.sh" "$anchor" "候选脚本显式哈希" || die "候选脚本不匹配显式哈希，未改动现有安装"
  fi
  bash -n "$dir/sbx.sh" && [[ "$(remote_version "$dir/sbx.sh")" =~ ^[0-9]+(\.[0-9]+)+$ ]] \
    || die "分发脚本语法或版本标记无效，未改动现有安装"
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
  local base="$1" rel=0
  if [[ "$base" == *-* ]]; then base="${base%%-*}"; else rel=1; fi
  local a b c
  IFS=. read -r a b c <<< "$base"
  echo "$((10#$a)) $((10#$b)) $((10#${c:-0})) $rel"
}
sb_version_ge() {
  local ka kb
  read -ra ka <<< "$(sb_version_key "$1")"
  read -ra kb <<< "$(sb_version_key "$2")"
  local i
  for i in 0 1 2 3; do
    (( ${ka[i]} > ${kb[i]} )) && return 0
    (( ${ka[i]} < ${kb[i]} )) && return 1
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
  staging=$(mktemp -d)
  trap 'rm -rf -- "$staging"' EXIT
  fetch_dist_script "$staging" "${SBX_UPDATE_SHA256:-}"
  SBX_SCRIPT_SHA256="$DIST_SCRIPT_SHA" bash "$staging/sbx.sh" --apply-update "$rev" "$force"
)

bootstrap_install() (
  local rev staging
  require_root
  rev=$(resolve_dist_sha) || die "无法解析官方 dist 提交，未开始安装"
  set_dist_pin "$rev"
  staging=$(mktemp -d)
  trap 'rm -rf -- "$staging"' EXIT
  fetch_dist_script "$staging" "${SBX_SCRIPT_SHA256:-}"
  SBX_SCRIPT_SHA256="$DIST_SCRIPT_SHA" bash "$staging/sbx.sh" --install-dist "$rev"
)

# Both services must remain up across consecutive samples. For the panel also
# probe the actual HTTP listener (not an external reverse proxy).
upgrade_health() {
  local want_sb="$1" want_panel="$2" addr="" listen port sample good=0 valid
  [[ -n "${SBX_NO_SERVICE:-}" ]] && return 0
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
    printf '%s\0' "$item" >> "$UPGRADE_DIR/paths"
    if [[ -e "$item" || -L "$item" ]]; then
      cp -a -- "$item" "$UPGRADE_DIR/snapshot/$i"
    fi
  done
  printf 'sing-box=%s\nsbx-panel=%s\n' "$UPGRADE_SB_ACTIVE" "$UPGRADE_PANEL_ACTIVE" > "$UPGRADE_DIR/services"
}

upgrade_restore_files() {
  local item backup i tmp
  for i in "${!UPGRADE_PATHS[@]}"; do
    item="${UPGRADE_PATHS[i]}"
    backup="$UPGRADE_DIR/snapshot/$i"
    if [[ -e "$backup" || -L "$backup" ]]; then
      # Stage the replacement on the destination filesystem. Rule caches can
      # be directories; unlike binaries they need a remove before rename.
      tmp=$(mktemp -d "${item%/*}/.sbx-restore.XXXXXXXX") || return 1
      if ! cp -a -- "$backup" "$tmp/item" \
         || { [[ -d "$item" && ! -L "$item" ]] && ! rm -rf -- "$item"; } \
         || ! mv -fT -- "$tmp/item" "$item"; then
        rm -rf -- "$tmp" || true
        err "未能恢复: $item"
        return 1
      fi
      rmdir -- "$tmp" || return 1
    else
      rm -rf -- "$item" || return 1
    fi
  done
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
  if [[ "$INIT_SYS" == systemd && -z "${SBX_NO_SERVICE:-}" ]]; then
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
  if [[ "$rc" == 0 || "$UPGRADE_DIRTY" == 0 ]]; then
    if ! rm -rf -- "$UPGRADE_DIR"; then warn "无法清除升级工作目录：$UPGRADE_DIR"; fi
    if [[ "$UPGRADE_COMMITTED" == 1 ]]; then ok "升级完成 → v$APP_VERSION ($DIST_SHA)"; fi
  elif upgrade_restore; then
    err "升级失败；已恢复原文件并验证升级前的服务状态"
    rm -rf -- "$UPGRADE_DIR" || warn "无法清除升级备份：$UPGRADE_DIR"
  else
    err "升级失败，且无法确认回滚完成；保留备份：$UPGRADE_DIR"
    rc=1
  fi
  exit "$rc"
}

stage_upgrade_binaries() (
  local old_core="$CORE_BIN" old_sb="$SB_BIN" cur_sb
  BIN_DIR="$UPGRADE_DIR/stage"
  CORE_BIN="$BIN_DIR/sbx-core"
  SB_BIN="$BIN_DIR/sing-box"
  if [[ -f "$old_core" ]]; then cp -p -- "$old_core" "$CORE_BIN"; fi
  install_sbx_core
  cur_sb=$("$old_sb" version | awk 'NR==1 {sub(/^v/, "", $3); print $3}')
  [[ "$cur_sb" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?$ ]] || die "无法识别已安装 sing-box 的版本"
  if [[ -z "$SB_VERSION_PIN" ]] && ! sb_version_ge "$cur_sb" "$SB_VERSION_DEFAULT"; then
    info "sing-box 内核 $cur_sb 低于发布基线 $SB_VERSION_DEFAULT，随升级更新内核..."
    install_sing_box force
  fi
)

upgrade_replace() { # same-filesystem rename without truncating the running file
  local src="$1" dest="$2" mode="$3" tmp
  tmp=$(mktemp "${dest%/*}/.sbx-upgrade.XXXXXXXX") || return 1
  if ! install -m "$mode" -- "$src" "$tmp" || ! mv -fT -- "$tmp" "$dest"; then
    rm -f -- "$tmp"
    return 1
  fi
}

apply_update() {
  local force="$1" nodes nft_conf
  [[ -f "$PANEL_CONF" && -x "$CORE_BIN" && -x "$SB_BIN" ]] || die "未检测到完整 SBX 安装，无法升级"
  upgrade_access_pending && die "存在未完成的访问控制事务，拒绝升级"
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
  command -v nft >/dev/null 2>&1 || die "升级需要 nftables；请先安装 nftables"
  nft list tables >/dev/null 2>&1 || die "nftables 不可用；未改动现有安装"
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
  upgrade_snapshot
  printf '%s\n' "$DIST_SHA" > "$UPGRADE_DIR/generation"
  UPGRADE_DIRTY=1
  upgrade_stop_consumers || die "无法停止现有服务，取消升级"
  upgrade_replace "$UPGRADE_DIR/stage/sbx-core" "$CORE_BIN" 0755
  if [[ -f "$UPGRADE_DIR/stage/sing-box" ]]; then
    upgrade_replace "$UPGRADE_DIR/stage/sing-box" "$SB_BIN" 0755
  fi
  if [[ -f "$UPGRADE_DIR/stage/libcronet.so" ]]; then
    upgrade_replace "$UPGRADE_DIR/stage/libcronet.so" "$BIN_DIR/libcronet.so" 0644
  fi
  "$CORE_BIN" config-migrate >/dev/null
  setup_services update
  core_node rulesets >/dev/null
  core_node sync >/dev/null
  # Commit is the managed node transaction (includes checking/access/restart).
  # Explicit start is only needed if a no-op commit didn't start sing-box.
  commit_node >/dev/null
  if [[ -z "${SBX_NO_SERVICE:-}" ]] && ! svc_do status sing-box; then
    svc_do start sing-box || die "无法启动 sing-box"
  fi
  fw_apply || die "无法应用计数规则"
  svc_do start sbx-panel || die "无法启动面板"
  upgrade_health 1 1 || die "升级后服务健康检查失败"
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
  if ! wait_service_active sbx-panel 10; then
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
  # 默认回环，因此这里不能暗示"公网是默认"。外网访问必须由用户显式切换监听地址，
  # 切换时应同时配置 HTTPS 反代或 secure_cookie——否则 Cookie 会以明文跨网络传输。
  if [[ "$(panel_get listen)" == "127.0.0.1" || "$(panel_get listen)" == "localhost" || "$(panel_get listen)" == "::1" ]]; then
    printf '  · 面板默认仅本机可访问（127.0.0.1）；如需外网访问，请在「面板设置」中切换监听地址，并配置 HTTPS 反代或 secure_cookie\n'
    printf '  · 若服务器有云防火墙/安全组，只需放行节点端口；面板端口 %s 无需对公网放行\n' "$(panel_get port)"
  else
    printf '  · 面板已配置为对外监听（%s）；请确保已设置强令牌，并建议置于 HTTPS 反代之后或启用 secure_cookie\n' "$(panel_get listen)"
    printf '  · 若服务器有云防火墙/安全组，请放行节点端口与面板端口 %s\n' "$(panel_get port)"
  fi
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
    --apply-firewall) require_root; "$CORE_BIN" apply; exit $? ;;
    --clear-firewall) require_root; "$CORE_BIN" clear; exit $? ;;
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

安装:   bash <(curl -fsSL $RAW_URL)
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
