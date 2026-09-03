#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="3.0.8"
RAW_URL="${SBX_RAW_URL:-https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh}"

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
# 二进制分发走仓库 dist 分支（rolling latest，与 Git Tag 无关）：
# raw.githubusercontent 对同一路径始终返回最新提交的产物，安装器无需感知版本号。
# 项目不使用 Git Tag / 版本 Release，因此不存在会失效的版本化下载链接。
RAW_BASE="${SBX_RAW_BASE:-https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/dist}"

OS_FAMILY="unknown"
INIT_SYS="unknown"
PKG=""

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

  local need=()
  command -v curl    >/dev/null 2>&1 || need+=(curl)
  command -v tar     >/dev/null 2>&1 || need+=(tar)
  command -v openssl >/dev/null 2>&1 || need+=(openssl)
  # jq：候选配置提交前的 route.final 引用完整性自检需要（见 sanitize_candidate_route）。
  # 缺失不阻断安装——自检会降级为告警，行为退回到修复前，不会更糟。
  command -v jq      >/dev/null 2>&1 || need+=(jq)
  ((${#need[@]})) && { info "安装: ${need[*]}"; pkg_install "${need[@]}"; }

  # 计数后端：优先 nftables
  if ! command -v nft >/dev/null 2>&1 && ! command -v iptables >/dev/null 2>&1; then
    info "安装计数后端 nftables"
    pkg_install nftables || pkg_install iptables \
      || die "无法安装 nftables/iptables，流量统计依赖其中之一"
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
SB_VERSION_DEFAULT="1.14.0-rc.1"
sb_expected_sha() {  # sb_expected_sha <filename> → 打印期望 sha256（无则输出空）
  awk -v n="$1" '$2==n{print $1}' <<'SBHASH'
342f6e3b4ab79abe470d1516b35dced9bc8dfe62dc43a459a53d97960108afeb  sing-box-1.14.0-rc.1-linux-amd64.tar.gz
98a5bd1f7bf5063f908461eb47ccb68d6df08571c62051f467c395a270a5e3c9  sing-box-1.14.0-rc.1-linux-arm64.tar.gz
a48e8b92e31dbc8fbec25e46feebefde4362024971ca645a222b0d98bcac4145  sing-box-1.14.0-rc.1-linux-armv7.tar.gz
d2083ea91f7637152b82639acc297a069675500b9f299560223196ceb19633a2  sing-box-1.14.0-rc.1-linux-armv6.tar.gz
7d551d6e766b886b9f439fdf1a4ebb1873cfb77ae2f9b03a66ab5e86c109ca5f  sing-box-1.14.0-rc.1-linux-386.tar.gz
aa99c75aa286d0c4e8808bd003f144a864aea5dd99a083314dddbacc50ae2772  sing-box-1.14.0-rc.1-linux-s390x.tar.gz
f227c4b2e85c6a8be5216a77476e55feffd9e01543c2a5b89d2d95061a924dd9  sing-box-1.14.0-rc.1-linux-riscv64.tar.gz
e057d850417b0f86b0da0c169adc552b7fd8b40012884435e9e7aaecf03aa9a8  sing-box-1.14.0-rc.1-linux-amd64-musl.tar.gz
3b0eda64906e7274a42b6829462ae910b1ccb3c89c0a2f01c65179c50e27b3d5  sing-box-1.14.0-rc.1-linux-arm64-musl.tar.gz
50c2e81fb0ac3281d8e9bb5224b45d9e2be5372d0a81c2cf7375a9891296de77  sing-box-1.14.0-rc.1-linux-armv7-musl.tar.gz
a9e642d8b2ea19626002193b8faf900e3365e9c1dcf81840534f89ef58c39221  sing-box-1.14.0-rc.1-linux-386-musl.tar.gz
55925652c1d6fdd50fc3baac40072becc22e12ac3f08fbef6219f3dcaabb6bec  sing-box-1.14.0-rc.1-linux-riscv64-musl.tar.gz
SBHASH
}

install_sing_box() {
  local force="${1:-}"
  # 正常安装：已安装即跳过；force（如 Snell 需要升级内核）则强制重装
  if [[ -z "$force" ]] && [[ -x "$SB_BIN" ]] && "$SB_BIN" version >/dev/null 2>&1; then
    ok "sing-box 已安装: $("$SB_BIN" version | head -1)"
    return 0
  fi
  if [[ -z "$force" ]] && [[ -n "${SBX_SB_BIN:-}" && -x "${SBX_SB_BIN}" ]]; then
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
  install -m 0755 "$found" "$SB_BIN"
  # 部分构建附带 libcronet.so（NaiveProxy 用），一并放到同目录
  local lib
  lib=$(find "$tmp" -type f -name 'libcronet.so' | head -1)
  [[ -n "$lib" ]] && install -m 0644 "$lib" "$BIN_DIR/libcronet.so" 2>/dev/null || true
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
  CORE_REPLACED=0   # 全局信号：本次是否真正替换了二进制（供 do_update 判断是否需重启）
  # 开发/测试：直接使用本地构建的二进制
  if [[ -n "${SBX_CORE_BIN:-}" && -x "${SBX_CORE_BIN}" ]]; then
    cat "$SBX_CORE_BIN" > "$CORE_BIN.tmp.$$" && chmod 0755 "$CORE_BIN.tmp.$$" \
      && mv -f "$CORE_BIN.tmp.$$" "$CORE_BIN" \
      || die "本地 sbx-core 拷贝失败；现有安装未被改动"
    CORE_REPLACED=1
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

  # 解析 dist 分支当前 commit sha，用 immutable revision 下载 binary 与 SHA256SUMS，
  # 避免两次下载之间 dist force-push 导致版本错位。解析失败回退 RAW_BASE
  # （仍有 SHA256 校验 fail-closed 兜底，错位只会失败、不会装错）。
  local base dist_sha
  dist_sha=""
  if command -v git >/dev/null 2>&1; then
    dist_sha=$(git ls-remote "https://github.com/k6nfmm7dbr-commits/sbx.git" refs/heads/dist 2>/dev/null | awk '{print $1}')
  else
    # 用轻量 /git/ref API（仅返回 sha，不带 diff），curl 完整下载到变量，
    # 再用 here-string 提取——避免 grep -m1 提前退出使上游进程 SIGPIPE
    # （`set -o pipefail` 下被误判失败）。
    local refs_json
    refs_json=$(curl -fsSL -m 15 "https://api.github.com/repos/k6nfmm7dbr-commits/sbx/git/ref/heads/dist" 2>/dev/null) || refs_json=""
    dist_sha=$(grep -m1 '"sha"' <<< "$refs_json" | sed -E 's/.*"sha"[[:space:]]*:[[:space:]]*"([0-9a-f]+)".*/\1/')
  fi
  if [[ -n "$dist_sha" ]]; then
    base="https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/$dist_sha"
  else
    base="$RAW_BASE"
  fi

  # 先下载 SHA256SUMS（小文件），与本地二进制做内容比较
  curl -fsSL -m 60 -o "$tmp/SHA256SUMS" "$(gh_url "$base/SHA256SUMS")" \
    || { cleanup; die "下载 SHA256SUMS 失败；现有安装未被改动"; }
  if [[ -x "$CORE_BIN" ]]; then
    local local_sum expect_sum
    local_sum=$(sha256_of "$CORE_BIN" 2>/dev/null || true)
    expect_sum=$(awk -v x="$name" '$2==x{print $1}' "$tmp/SHA256SUMS")
    if [[ -n "$local_sum" && -n "$expect_sum" && "$local_sum" == "$expect_sum" ]]; then
      cleanup
      ok "sbx-core 已是最新: $("$CORE_BIN" version 2>/dev/null | head -1)"
      return 0
    fi
  fi

  # 内容不一致或本地缺失 → 下载完整二进制并校验替换
  info "下载 sbx-core (${arch})..."
  curl -fsSL -m 300 -o "$dl" "$(gh_url "$base/${name}")" \
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
  mv -f "$dl" "$CORE_BIN"
  rm -rf "$tmp"
  if ! "$CORE_BIN" version >/dev/null 2>&1; then
    [[ -f "$CORE_BIN.bak" ]] && { mv -f "$CORE_BIN.bak" "$CORE_BIN"; warn "新二进制异常，已回滚"; }
    die "sbx-core 安装后自检失败"
  fi
  rm -f "$CORE_BIN.bak"
  CORE_REPLACED=1
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
    chmod 600 "$PANEL_CONF"
    return 0
  fi
  local token port
  token=$(rand_hex 16)
  port=$(pick_port)
  # 先以 0600 预创建（不受 umask 影响），避免含 token 的内容出现短暂暴露窗口
  install -m 0600 /dev/null "$PANEL_CONF"
  cat > "$PANEL_CONF" <<EOF
{
  "db": "$APP_DIR/traffic.db",
  "nodes_file": "$NODES_JSON",
  "nft_conf": "$APP_DIR/nft.conf",
  "ipt_script": "$APP_DIR/iptables.sh",
  "web_root": "$WEB_DIR",
  "backend": "auto",
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
  # 先停面板（停掉采集线程，避免其 repair() 自愈机制把刚删的表重建回来）
  svc_do stop sbx-panel >/dev/null 2>&1 || true
  "$CORE_BIN" clear >/dev/null 2>&1 || true
  # 兜底：直删计数表 + 策略 enforcement 表（sbx_policy），
  # 保证卸载后内核里不残留 quota/IP 限制的 drop 规则（残留到重启会误拦其它服务）。
  if command -v nft >/dev/null 2>&1; then
    nft delete table inet sbx_traffic >/dev/null 2>&1 || true
    nft delete table inet sbx_policy  >/dev/null 2>&1 || true
  fi
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
    flock 9 || { err "检测到另一个 sbx 节点操作正在进行，请稍后重试"; exit 75; }
    # 告知 sbx-core 本进程已持锁，Go 侧跳过自锁（否则会对自己死锁等待）
    export SBX_LOCK_HELD=1
    "$@"
  ) 9>"$SBX_LOCK"
}

# 通用提交流程：校验 → 备份 → 提交 config → 提交 nodes → 重启确认 → 才删备份
# v3.0.3 修复（P0）：
#   1) 备份在整个操作真正成功之前绝不删除——旧实现先删 .bak 再 restart，
#      restart 失败后的“回滚”实际上无备份可用，rollback 名存实亡；
#   2) 备份创建失败立即中止 mutation——绝不允许在无备份的情况下改动正式文件；
#   3) 原文件不存在属合法状态（全新安装），按 original_exists=false 记录，
#      回滚时恢复为“不存在”；但“存在却备份失败”必须中止。
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
  local cand="$SB_CONF.candidate"
  [[ -f "$cand" ]] || { warn "未生成候选配置"; return 1; }
  # check 之前先校正 route.final：check 不校验该引用，放过去就会在 restart 阶段炸
  sanitize_candidate_route "$cand"
  if ! "$SB_BIN" check -c "$cand" >/dev/null 2>&1; then
    warn "sing-box 配置校验失败，已回滚"
    "$SB_BIN" check -c "$cand" 2>&1 | head -5 >&2
    core_node rollback >/dev/null 2>&1 || true
    return 1
  fi
  # ---- 备份阶段：任一备份创建失败都必须立即中止，正式文件一个都不动 ----
  local conf_existed=0 nodes_existed=0
  if [[ -f "$SB_CONF" ]]; then
    conf_existed=1
    cp -f "$SB_CONF" "$SB_CONF.bak" || { warn "config 备份创建失败，已中止本次修改（正式文件未被改动）"; return 1; }
  fi
  if [[ -f "$NODES_JSON" ]]; then
    nodes_existed=1
    cp -f "$NODES_JSON" "$NODES_JSON.bak" || { warn "nodes 备份创建失败，已中止本次修改（正式文件未被改动）"; return 1; }
  fi

  restore_old() {
    core_node rollback >/dev/null 2>&1 || true
    # 逐项尽力恢复：单项失败不中断其余恢复动作，最后统一以非 0 返回
    if [[ "$conf_existed" == 1 ]]; then
      cp -f "$SB_CONF.bak" "$SB_CONF" || warn "config 回滚失败! 请手工从 $SB_CONF.bak 恢复"
    else
      rm -f "$SB_CONF"
    fi
    if [[ "$nodes_existed" == 1 ]]; then
      cp -f "$NODES_JSON.bak" "$NODES_JSON" || warn "nodes 回滚失败! 请手工从 $NODES_JSON.bak 恢复"
    else
      rm -f "$NODES_JSON"
    fi
    rm -f "$NODES_JSON.candidate"
  }

  # 提交 config + nodes 统一交给 sbx-core node commit：
  # 它用 rename + fsync 父目录 提交两个候选文件（v3.0.7 之前 config 由 shell
  # `mv -f` 提交，无目录 fsync，与 nodes.json 的耐久性不一致）。
  # config 先提交、nodes 后提交；nodes 提交失败绝不允许静默继续（否则两者状态分叉）。
  local cerr
  if ! cerr=$("$CORE_BIN" node commit 2>&1); then
    warn "配置/节点数据提交失败，正在回滚: $cerr"
    restore_old
    sb_restart || true   # 让 sing-box 回到原有效配置
    warn "已回滚到操作前状态（config 与 nodes 保持一致）"
    return 1
  fi
  # restart 失败时 .bak 必须仍在——这是 rollback 能真正完成的唯一保证
  if ! sb_restart; then
    warn "sing-box 启动失败，回滚配置与节点数据"
    restore_old
    if sb_restart; then
      warn "已回滚到操作前状态，旧配置已恢复运行（config 与 nodes 保持一致）"
    else
      warn "严重: 已恢复旧配置文件，但 sing-box 仍未运行！请检查 $SB_CONF 与服务日志后手动处理"
      warn "已回滚到操作前状态（config 与 nodes 保持一致）"
    fi
    return 1
  fi
  # 整个操作真正成功以后才允许删除备份
  rm -f "$SB_CONF.bak" "$NODES_JSON.bak" 2>/dev/null || true
  # 节点本身已创建成功；防火墙规则失败不能伪装成功，也不能回滚节点。
  # 返回码三态：0=完全成功，2=节点成功但流量规则失败（partial），1=失败。
  if fw_apply; then
    panel_running && svc_do restart sbx-panel || true
    return 0
  fi
  warn "节点已创建并可用，但流量统计规则应用失败，该节点流量统计暂不可用；可稍后在菜单重试"
  return 2
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
setup_services() {
  [[ -n "${SBX_NO_SERVICE:-}" ]] && { warn "已跳过服务注册（SBX_NO_SERVICE）"; return 0; }

  case "$INIT_SYS" in
    systemd)
      cat > /etc/systemd/system/sing-box.service <<EOF
[Unit]
Description=sing-box service
Documentation=https://sing-box.sagernet.org
After=network-online.target nss-lookup.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$SB_BIN run -C $SB_DIR
Restart=on-failure
RestartSec=3
LimitNOFILE=infinity
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW

[Install]
WantedBy=multi-user.target
EOF

      cat > /etc/systemd/system/sbx-firewall.service <<EOF
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

      cat > /etc/systemd/system/sbx-panel.service <<EOF
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
# 面板进程仍需 CAP_NET_ADMIN：sbx-core serve 内嵌 Collector，需执行 nft/iptables 采集；
# 还需读 /proc（连接数）、读写 /etc/sbx 与 /run/sbx（SQLite/状态文件）、绑定面板端口。
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
ProtectSystem=full
ReadWritePaths=/etc/sbx /run/sbx
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
# v3.0.6：面板需要 CAP_NET_RAW 兼容 iptables-legacy（Ubuntu 20.04 等）；
# root UID 在 systemd capability 沙箱下不等于完整 root，缺该能力时
# iptables-legacy 会报 filter table Permission denied。
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW

[Install]
WantedBy=multi-user.target
EOF
      systemctl daemon-reload
      systemctl enable sing-box sbx-firewall sbx-panel >/dev/null 2>&1
      ;;

    openrc)
      cat > /etc/init.d/sing-box <<EOF
#!/sbin/openrc-run
name="sing-box"
description="sing-box service"
command="$SB_BIN"
command_args="run -C $SB_DIR"
command_background=true
pidfile="/run/\${RC_SVCNAME}.pid"
output_log="/var/log/sing-box.log"
error_log="/var/log/sing-box.log"
depend() { need net; after sbx-firewall; }
EOF
      cat > /etc/init.d/sbx-firewall <<EOF
#!/sbin/openrc-run
name="sbx-firewall"
description="SBX traffic counters"
depend() { before sing-box sbx-panel; }
start() { $CORE_BIN apply; }
stop()  { $CORE_BIN clear; }
EOF
      cat > /etc/init.d/sbx-panel <<EOF
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
      chmod +x /etc/init.d/sing-box /etc/init.d/sbx-firewall /etc/init.d/sbx-panel
      rc-update add sbx-firewall default >/dev/null 2>&1
      rc-update add sing-box default >/dev/null 2>&1
      rc-update add sbx-panel default >/dev/null 2>&1
      ;;
  esac
  ok "服务已注册（开机自启）"
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
  local host port
  host=$(core_node get-host); [[ -z "$host" ]] && host="$(public_ip)"
  port=$(panel_get port)
  echo "http://$(host_for_uri "$host"):$port/"
}

show_panel_info() {
  hr
  printf '%s流量面板%s\n' "$C_B" "$C_RESET"
  printf '  地址: %s%s%s\n' "$C_CYAN" "$(panel_url)" "$C_RESET"
  printf '  令牌: %s%s%s\n' "$C_CYAN" "$(panel_get token)" "$C_RESET"
  printf '  状态: %s\n' "$(panel_running && echo "${C_GREEN}运行中${C_RESET}" || echo "${C_RED}未运行${C_RESET}")"
  printf '  后端: %s\n' "$(command -v nft >/dev/null 2>&1 && echo nftables || echo iptables)"
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

menu_traffic() {
  banner
  printf '%s流量统计%s\n' "$C_B" "$C_RESET"
  hr
  "$CORE_BIN" show
  hr
  printf '%s最近 14 天%s\n' "$C_B" "$C_RESET"
  "$CORE_BIN" daily 14
  hr
  show_panel_info
  pause
}

menu_panel_settings() {
  banner
  printf '%s面板设置%s\n\n' "$C_B" "$C_RESET"
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

uninstall_all() {
  banner
  printf '%s卸载 SBX%s\n\n' "$C_B" "$C_RESET"
  echo "将会："
  echo "  · 停止并移除 sing-box / 面板 / 计数规则的服务"
  echo "  · 删除 $APP_DIR 与 $SB_DIR"
  echo "  · 删除 $SB_BIN 与 $CMD_PATH"
  echo
  printf '%s确认卸载? 流量历史将一并删除 [y/N] %s' "$C_YEL" "$C_RESET"
  read -r yn || true
  [[ "${yn,,}" == "y" ]] || { echo "已取消"; pause; return 0; }

  svc_do stop sbx-panel || true
  svc_do stop sing-box || true
  svc_do stop sbx-firewall || true
  sleep 0.5   # 等 oneshot ExecStop 完全落地，避免与清理产生竞态
  fw_clear
  case "$INIT_SYS" in
    systemd)
      systemctl disable sing-box sbx-panel sbx-firewall >/dev/null 2>&1 || true
      rm -f /etc/systemd/system/{sing-box,sbx-panel,sbx-firewall}.service
      systemctl daemon-reload ;;
    openrc)
      for s in sbx-panel sing-box sbx-firewall; do
        rc-update del "$s" default >/dev/null 2>&1 || true
        rm -f "/etc/init.d/$s"
      done ;;
  esac
  rm -rf "$APP_DIR" "$SB_DIR" "$SB_BIN" "$CMD_PATH" "$CORE_BIN" "$BIN_DIR/libcronet.so"
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
    echo "  1) 添加节点"
    echo "  2) 节点管理"
    echo "  3) 流量统计"
    echo "  4) 系统设置"
    echo "  5) 检查更新"
    echo "  6) 卸载"
    echo "  0) 退出"
    echo
    printf '请选择: '
    read -r c || true
    case "$c" in
      1) menu_add_node ;;
      2) menu_nodes ;;
      3) menu_traffic ;;
      4) menu_settings ;;
      5) do_update; pause ;;
      6) uninstall_all ;;
      0|"") clear 2>/dev/null || true; exit 0 ;;
      *) warn "无效选择"; sleep 1 ;;
    esac
  done
}

menu_nodes() {
  while :; do
    banner
    printf '%s节点管理%s\n\n' "$C_B" "$C_RESET"
    core_node list
    echo
    echo "  1) 查看分享链接"
    echo "  2) 修改节点（端口 / SNI）"
    echo "  3) 删除节点"
    echo "  0) 返回"
    echo
    printf '请选择: '
    read -r c || true
    case "$c" in
      1) menu_show_links ;;
      2) menu_edit_node ;;
      3) menu_remove_node ;;
      0|"") return 0 ;;
      *) warn "无效选择" ;;
    esac
  done
}

menu_settings() {
  while :; do
    banner
    printf '%s系统设置%s\n\n' "$C_B" "$C_RESET"
    echo "  1) 面板设置（端口 / 间隔 / 监听 / 自检 / 清空）"
    echo "  2) 分享地址（域名 / IP）"
    echo "  3) 服务管理（重启 / 停止 / 日志）"
    echo "  0) 返回"
    echo
    printf '请选择: '
    read -r c || true
    case "$c" in
      1) menu_panel_settings ;;
      2) menu_host ;;
      3) menu_service ;;
      0|"") return 0 ;;
      *) warn "无效选择" ;;
    esac
  done
}

# ---------------------------------------------------------------- 安装
install_self() {
  install -d -m 0755 "$BIN_DIR"
  if [[ -f "${BASH_SOURCE[0]}" ]]; then
    install -m 0755 "${BASH_SOURCE[0]}" "$SELF_PATH" 2>/dev/null || true
  fi
  if [[ ! -f "$SELF_PATH" ]]; then
    curl -fsSL -m 60 -o "$SELF_PATH" "$(gh_url "$RAW_URL")" 2>/dev/null || true
    chmod +x "$SELF_PATH" 2>/dev/null || true
  fi
  cat > "$CMD_PATH" <<EOF
#!/usr/bin/env bash
exec bash "$SELF_PATH" "\$@"
EOF
  chmod +x "$CMD_PATH"
}

# ---------------------------------------------------------------- 在线升级
# 从 RAW_URL 拉取最新脚本 → 校验 → 替换本体 → 用新脚本重写内置资源并重启服务。
# 全程保留 nodes.json / panel.json / traffic.db，不动节点与历史流量。
remote_version() {  # 从下载的脚本里提取 APP_VERSION
  grep -m1 '^APP_VERSION=' "$1" 2>/dev/null | sed -E 's/^APP_VERSION="?([^"]+)"?.*/\1/'
}

ver_ge() {  # ver_ge A B  → A >= B ?（点分版本比较）
  [[ "$1" == "$2" ]] && return 0
  local a b i
  IFS=. read -ra a <<< "$1"
  IFS=. read -ra b <<< "$2"
  for ((i=0; i<${#a[@]} || i<${#b[@]}; i++)); do
    local x=${a[i]:-0} y=${b[i]:-0}
    ((10#$x > 10#$y)) && return 0
    ((10#$x < 10#$y)) && return 1
  done
  return 0
}

do_update() {
  local force="${1:-}"
  banner
  printf '%s在线升级%s\n\n' "$C_B" "$C_RESET"
  require_root
  detect_platform

  local tmp new_ver current_sha remote_sha
  tmp=$(mktemp)
  info "从 GitHub 拉取最新版本..."
  if ! curl -fsSL -m 60 -o "$tmp" "$(gh_url "$RAW_URL")"; then
    rm -f "$tmp"; die "下载失败，可设置 SBX_GH_PROXY 使用镜像后重试"
  fi
  # 基本完整性校验：语法 + 版本标记
  if ! bash -n "$tmp" 2>/dev/null; then
    rm -f "$tmp"; die "下载的脚本语法校验失败，已放弃升级（未改动现有安装）"
  fi
  if ! grep -q '^APP_VERSION=' "$tmp"; then
    rm -f "$tmp"; die "下载的脚本缺少版本标记，可能不是发布版，已放弃升级"
  fi

  new_ver=$(remote_version "$tmp")
  [[ -z "$new_ver" ]] && new_ver="unknown"
  # 版本号相同时，再用 sha256 比较脚本内容，避免开发者忘记升版本导致更新被误跳过。
  current_sha=$(sha256sum "$SELF_PATH" 2>/dev/null | awk '{print $1}')
  remote_sha=$(sha256sum "$tmp" 2>/dev/null | awk '{print $1}')
  local same_content="no"
  [[ -n "$current_sha" && "$current_sha" == "$remote_sha" ]] && same_content="yes"
  info "当前版本: $APP_VERSION    最新版本: $new_ver"

  if [[ "$same_content" == "yes" && "$force" != "--force" ]]; then
    rm -f "$tmp"
    # 脚本内容相同，但 dist 分支的 Go 二进制可能单独更新（rolling latest，
    # 版本号不随功能递增）。不能只看脚本/版本号——仍需按内容检查后端二进制。
    info "脚本已是最新，检查后端 sbx-core 是否同步..."
    CORE_REPLACED=0
    if ! install_sbx_core; then
      die "后端 sbx-core 检查/更新失败，已保留现有安装"
    fi
    if [[ "${CORE_REPLACED:-0}" == "1" ]]; then
      # 二进制内容有变化 → 刷新使用该二进制的面板与防火墙规则
      svc_do restart sbx-panel || svc_do start sbx-panel || true
      fw_apply || warn "计数规则应用失败，流量统计可能不准"
      ok "后端 sbx-core 已更新"
    else
      ok "已是最新版本，无需升级"
    fi
    printf '%s如需强制重装当前版本，运行: sbx --update --force%s\n' "$C_DIM" "$C_RESET"
    return 0
  fi
  if [[ "$new_ver" != "unknown" ]] && ver_ge "$APP_VERSION" "$new_ver" && [[ "$same_content" != "yes" ]]; then
    warn "版本号未递增但脚本内容不同，仍将更新（检测到远端代码变化）"
  fi

  # 备份当前本体（存在即必须备份成功，否则中止升级——旧脚本绝不能被无备份覆盖）
  if [[ -f "$SELF_PATH" ]]; then
    cp -f "$SELF_PATH" "$SELF_PATH.bak" || { rm -f "$tmp"; die "升级前备份创建失败，已中止（当前脚本未被改动）"; }
  fi
  cat "$tmp" > "$SELF_PATH"
  chmod +x "$SELF_PATH"
  rm -f "$tmp"
  ok "脚本已更新到 $new_ver，正在应用..."

  # 交给新脚本重写内置资源并重启（用内部标记，避免重复整套安装流程）
  if bash "$SELF_PATH" --apply-update; then
    ok "升级完成 → v$new_ver"
    [[ -t 0 ]] && { pause; exec bash "$SELF_PATH"; }
  else
    # 回滚到升级前版本；恢复本身失败绝不能吞掉（否则脚本停在半更新状态还报"已恢复"）
    warn "应用失败，回滚到升级前版本"
    if [[ -f "$SELF_PATH.bak" ]] && cat "$SELF_PATH.bak" > "$SELF_PATH" 2>/dev/null && chmod +x "$SELF_PATH" 2>/dev/null; then
      bash "$SELF_PATH" --apply-update >/dev/null 2>&1 || true
      die "升级未成功，已恢复原版本"
    else
      die "严重: 升级失败且回滚失败! 当前脚本可能处于半更新状态，请重新运行安装命令修复"
    fi
  fi
}

# 更新 sbx-core 二进制 + 重启服务（升级时由新脚本调用；保留所有用户数据）
apply_update() {
  require_root
  detect_platform
  [[ -f "$PANEL_CONF" ]] || die "未检测到已安装的 SBX，无法应用升级"
  install -d -m 0755 "$APP_DIR"
  info "更新 sbx-core 后端..."

  install_sbx_core                    # 原子替换 + SHA256 校验 + 失败回滚
  "$CORE_BIN" version >/dev/null 2>&1 || die "升级后 sbx-core 不可用，已中止（原二进制保留）"

  install_self                        # 刷新 /usr/local/bin/sbx 封装
  setup_services                      # 服务单元可能有更新
  # 节点 schema 或计数规则若有变化，重建一次（幂等，配置校验失败会保留旧配置）
  local sync_err=""
  if [[ -s "$NODES_JSON" ]]; then
    # sync 真实失败（nodes.json 损坏/不可写/candidate 生成失败）绝不能吞掉：
    # 继续执行会造成 config/nodes 不一致，必须报告并跳过本次配置迁移
    if ! core_node sync >/dev/null; then
      err "node sync 失败: nodes.json 可能损坏或 candidate 无法生成"
      info "已跳过本次升级的配置迁移；请修复 $NODES_JSON 后重新运行升级"
      sync_err="node sync 失败"
    elif [[ -f "$SB_CONF.candidate" ]]; then
      # 与 commit_node 同源：check 不校验 route.final 引用，先校正再校验
      sanitize_candidate_route "$SB_CONF.candidate"
      if "$SB_BIN" check -c "$SB_CONF.candidate" >/dev/null 2>&1; then
        # SB_CONF 含节点凭据：存在即必须备份成功，失败则中止迁移（正式配置不动）
        if [[ -f "$SB_CONF" ]] && ! cp -f "$SB_CONF" "$SB_CONF.upd-bak"; then
          warn "升级前配置备份创建失败，已中止本次配置迁移"
          rm -f "$SB_CONF.candidate"; core_node rollback >/dev/null 2>&1 || true
        elif "$CORE_BIN" node commit >/dev/null 2>&1; then
          # commit 现在一并提交 config 候选 + nodes 候选（rename + fsync）
          rm -f "$SB_CONF.upd-bak" 2>/dev/null || true
        else
          # 升级路径的提交失败同样必须回滚，避免 config/nodes 分叉。
          # 回滚恢复本身失败绝不能吞掉：那会留下 config/nodes 分叉还谎报"已恢复"。
          local rb_err=""
          cp -f "$SB_CONF.upd-bak" "$SB_CONF" 2>/dev/null || rb_err="config 恢复失败"
          rm -f "$SB_CONF.candidate" "$NODES_JSON.candidate"
          "$CORE_BIN" node rollback >/dev/null 2>&1 || [[ -n "$rb_err" ]] || rb_err="nodes 回滚失败"
          rm -f "$SB_CONF.upd-bak" 2>/dev/null || true
          if [[ -n "$rb_err" ]]; then
            err "升级回滚失败($rb_err)! config 与 nodes 可能不一致，请手工检查 $SB_CONF 与 $NODES_JSON"
            return 1
          fi
          warn "升级期节点提交失败，已恢复原配置"
        fi
      else
        rm -f "$SB_CONF.candidate"; core_node rollback >/dev/null 2>&1 || true
      fi
    fi
  fi
  svc_do restart sing-box || svc_do start sing-box || true
  local sb_ok=1 panel_ok=1
  svc_do status sing-box || sb_ok=0
  # 面板 restart 失败允许 start 兜底；两者都失败才算失败
  if ! svc_do restart sbx-panel; then
    svc_do start sbx-panel || true
  fi
  svc_do status sbx-panel || panel_ok=0
  if [[ "$sb_ok" == 1 && "$panel_ok" == 1 ]]; then
    if fw_apply; then
      ok "已重启 sing-box 与面板"
    else
      warn "已重启 sing-box 与面板，但计数规则应用失败（流量统计可能不准）"
    fi
  else
    [[ "$sb_ok" != 1 ]] && err "sing-box 重启失败，节点服务当前不可用！请查看日志: sbx 菜单 → 系统设置 → 查看日志"
    [[ "$panel_ok" != 1 ]] && err "面板服务重启失败，流量面板暂不可用！请查看日志: journalctl -u sbx-panel -n 60"
    [[ -n "$sync_err" ]] && err "$sync_err"
    return 1
  fi
  return 0
}

do_install() {
  banner
  require_root
  detect_platform
  info "系统: $OS_FAMILY / 初始化: $INIT_SYS / 架构: $(sb_arch)"
  install_deps
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

  start_all
  # 全新安装的计数规则应用失败必须让用户知道，不能静默带过
  fw_apply || warn "计数规则应用失败，流量统计暂不可用；可在菜单中重试"

  local nnum
  nnum=$(core_node count)

  banner
  ok "安装完成"
  hr
  if [[ "$nnum" == "0" ]]; then
    printf '%s还没有任何节点。%s在菜单里选「1) 添加节点」即可创建。\n' "$C_B" "$C_RESET"
  else
    printf '%s节点分享链接%s\n' "$C_B" "$C_RESET"
    core_node links --host "$host" || true
  fi
  show_panel_info
  printf '\n%s提示%s\n' "$C_B" "$C_RESET"
  printf '  · 随时运行 %ssbx%s 打开管理菜单\n' "$C_CYAN" "$C_RESET"
  printf '  · 首次使用请在菜单「1) 添加节点」创建你要的节点\n'
  if [[ -n "$host6" ]]; then
    printf '  · 本机支持 IPv6，添加节点后会同时给出 IPv4 与 IPv6 两条分享链接\n'
  fi
  printf '  · 面板需要令牌访问；如需仅本机访问，可在「面板设置」中切换\n'
  printf '  · 若服务器有云防火墙/安全组，请放行节点端口与面板端口 %s\n' "$(panel_get port)"
  echo
  if [[ ! -t 0 ]]; then
    printf '%s管道运行模式下不进入交互菜单，安装已完成。运行 sbx 打开菜单添加节点。%s\n' "$C_DIM" "$C_RESET"
    return 0
  fi
  pause
  main_menu
}

# ---------------------------------------------------------------- 入口
main() {
  init_colors
  case "${1:-}" in
    --apply-firewall) require_root; "$CORE_BIN" apply; exit $? ;;
    --clear-firewall) require_root; "$CORE_BIN" clear; exit $? ;;
    --panel-url) panel_url; exit 0 ;;
    --show) "$CORE_BIN" show; exit 0 ;;
    --links) core_node links --host "$(core_node get-host)"; exit 0 ;;
    --update|update|upgrade) do_update "${2:-}"; exit $? ;;
    --apply-update) apply_update; exit $? ;;   # 内部使用：升级时由新脚本调用
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
  --update --force   强制重装当前/最新版本
  --show             命令行查看流量统计
  --links            输出节点分享链接
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
    do_install
  fi
}

main "$@"
