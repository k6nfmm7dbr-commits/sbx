#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="2.8.0"
RAW_URL="${SBX_RAW_URL:-https://raw.githubusercontent.com/k6nfmm7dbr-commits/sbx/main/sbx.sh}"

# SBX_ROOT 仅用于测试/沙箱安装（把整套目录挪到前缀下），正常安装留空
ROOT="${SBX_ROOT:-}"
APP_DIR="$ROOT/etc/sbx"
WEB_DIR="$APP_DIR/web"
BIN_DIR="$ROOT/usr/local/bin"
SB_BIN="$BIN_DIR/sing-box"
SB_DIR="$ROOT/etc/sing-box"
SB_CONF="$SB_DIR/config.json"
PANEL_PY="$APP_DIR/panel.py"
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
die()  { printf '%s[x]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }
hr()   { printf '%s%s%s\n' "$C_DIM" "────────────────────────────────────────────────" "$C_RESET"; }

banner() {
  [[ -t 1 ]] && clear 2>/dev/null || true
  printf '%s%s' "$C_CYAN" "$C_B"
  cat <<'EOF'
   ___ ___  __  __
  / __| _ )\ \/ /   sing-box 节点 + 精确流量面板
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
  command -v python3 >/dev/null 2>&1 || need+=(python3)
  ((${#need[@]})) && { info "安装: ${need[*]}"; pkg_install "${need[@]}"; }

  # 计数后端：优先 nftables
  if ! command -v nft >/dev/null 2>&1 && ! command -v iptables >/dev/null 2>&1; then
    info "安装计数后端 nftables"
    pkg_install nftables || pkg_install iptables \
      || die "无法安装 nftables/iptables，流量统计依赖其中之一"
  fi
  command -v python3 >/dev/null 2>&1 || die "python3 安装失败（本脚本的 JSON 处理全部依赖它）"
  ok "依赖就绪"
}

# ---------------------------------------------------------------- 通用工具
rand_hex() { openssl rand -hex "$1"; }
rand_b64() { openssl rand -base64 "$1" | tr -d '\n'; }
rand_uuid() {
  if [[ -r /proc/sys/kernel/random/uuid ]]; then cat /proc/sys/kernel/random/uuid
  else "$SB_BIN" generate uuid 2>/dev/null || python3 -c 'import uuid;print(uuid.uuid4())'; fi
}

py_json() { python3 "$APP_DIR/nodes_tool.py" "$@"; }

valid_port() { [[ "$1" =~ ^[0-9]+$ ]] && ((10#$1 >= 1 && 10#$1 <= 65535)); }

port_busy() {
  local p="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -Hlnt "sport = :$p" 2>/dev/null | grep -q . && return 0
    ss -Hlnu "sport = :$p" 2>/dev/null | grep -q . && return 0
  elif command -v netstat >/dev/null 2>&1; then
    netstat -lnt 2>/dev/null | grep -qE "[:.]$p[[:space:]]" && return 0
    netstat -lnu 2>/dev/null | grep -qE "[:.]$p[[:space:]]" && return 0
  fi
  py_json port-used "$p" >/dev/null 2>&1
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

sb_latest_version() {
  local v
  v=$(curl -fsSL -m 20 "https://api.github.com/repos/SagerNet/sing-box/releases/latest" 2>/dev/null \
      | grep -m1 '"tag_name"' | sed -E 's/.*"v?([^"]+)".*/\1/') || true
  [[ -n "$v" ]] && { echo "$v"; return 0; }
  echo "1.13.19"   # API 不可用时的保底版本
}

install_sing_box() {
  if [[ -x "$SB_BIN" ]] && "$SB_BIN" version >/dev/null 2>&1; then
    ok "sing-box 已安装: $("$SB_BIN" version | head -1)"
    return 0
  fi
  if [[ -n "${SBX_SB_BIN:-}" && -x "${SBX_SB_BIN}" ]]; then
    install -d -m 0755 "$BIN_DIR"
    cat "$SBX_SB_BIN" > "$SB_BIN"; chmod 0755 "$SB_BIN"
    ok "使用本地 sing-box: $("$SB_BIN" version | head -1)"
    return 0
  fi
  local ver arch suffix tmp url
  ver="${SB_VERSION_PIN:-$(sb_latest_version)}"
  arch="$(sb_arch)"
  # Alpine 用 musl 构建，其余用默认（静态）构建
  suffix=""
  [[ "$OS_FAMILY" == "alpine" ]] && { case "$arch" in amd64|arm64|armv7|386|riscv64) suffix="-musl";; esac; }

  tmp=$(mktemp -d)
  url="https://github.com/SagerNet/sing-box/releases/download/v${ver}/sing-box-${ver}-linux-${arch}${suffix}.tar.gz"
  info "下载 sing-box v${ver} (${arch}${suffix})"
  if ! curl -fsSL -m 300 -o "$tmp/sb.tar.gz" "$(gh_url "$url")"; then
    # musl 包不存在时退回默认包
    if [[ -n "$suffix" ]]; then
      url="https://github.com/SagerNet/sing-box/releases/download/v${ver}/sing-box-${ver}-linux-${arch}.tar.gz"
      warn "musl 包不可用，改用默认构建"
      curl -fsSL -m 300 -o "$tmp/sb.tar.gz" "$(gh_url "$url")" \
        || { rm -rf "$tmp"; die "下载 sing-box 失败，可设置 SBX_GH_PROXY 使用镜像"; }
    else
      rm -rf "$tmp"; die "下载 sing-box 失败，可设置 SBX_GH_PROXY 使用镜像"
    fi
  fi
  tar xzf "$tmp/sb.tar.gz" -C "$tmp" || { rm -rf "$tmp"; die "解压失败"; }
  local found
  found=$(find "$tmp" -type f -name sing-box | head -1)
  [[ -n "$found" ]] || { rm -rf "$tmp"; die "压缩包中未找到 sing-box"; }
  install -d -m 0755 "$BIN_DIR"
  install -m 0755 "$found" "$SB_BIN"
  # 部分构建附带 libcronet.so（NaiveProxy 用），一并放到同目录
  local lib
  lib=$(find "$tmp" -type f -name 'libcronet.so' | head -1)
  [[ -n "$lib" ]] && install -m 0644 "$lib" "$BIN_DIR/libcronet.so" 2>/dev/null || true
  rm -rf "$tmp"

  "$SB_BIN" version >/dev/null 2>&1 || die "sing-box 无法运行（架构或 libc 不匹配）"
  ok "sing-box 安装完成: $("$SB_BIN" version | head -1)"
}

# ---------------------------------------------------------------- 目录与基础配置
prepare_dirs() {
  install -d -m 0755 "$APP_DIR" "$WEB_DIR" "$SB_DIR"
  install -d -m 0700 "$CERT_DIR"
  [[ -f "$NODES_JSON" ]]  || echo '[]' > "$NODES_JSON"
  [[ -f "$STATE_JSON" ]]  || echo '{}' > "$STATE_JSON"
}

ensure_sb_config() {
  [[ -f "$SB_CONF" ]] && return 0
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
  ok "已生成 sing-box 基础配置"
}

ensure_panel_conf() {
  if [[ -f "$PANEL_CONF" ]]; then
    # 保留/补齐面板查看令牌。
    python3 - "$PANEL_CONF" <<'PY'
import json, os, sys, secrets
p=sys.argv[1]
try:
 d=json.load(open(p))
 if not d.get('token'): d['token']=secrets.token_hex(16)
 tmp=p+'.clean';json.dump(d,open(tmp,'w'),indent=2,ensure_ascii=False);os.replace(tmp,p);os.chmod(p,0o600)
except Exception: pass
PY
    return 0
  fi
  local token port
  token=$(rand_hex 16)
  port=$(pick_port)
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

panel_get() { python3 "$PANEL_PY" config-get "$1"; }
panel_set() { python3 "$PANEL_PY" config-set "$1" "$2"; }

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
  python3 - "$CERT_DIR" <<PY
import sys
d = sys.argv[1]
s = """$out"""
c0 = s.find('-----BEGIN CERTIFICATE')
c1 = s.find('-----END CERTIFICATE-----') + len('-----END CERTIFICATE-----')
k0 = s.find('-----BEGIN PRIVATE KEY')
k1 = s.find('-----END PRIVATE KEY-----') + len('-----END PRIVATE KEY-----')
assert c0 >= 0 and k0 >= 0, "keypair parse failed"
open(d + '/cert.pem', 'w').write(s[c0:c1] + '\n')
open(d + '/key.pem', 'w').write(s[k0:k1] + '\n')
PY
  chmod 600 "$CERT_DIR/key.pem"; chmod 644 "$CERT_DIR/cert.pem"
  ok "证书就绪"
}

# ---------------------------------------------------------------- 流量规则
fw_apply() {
  python3 "$PANEL_PY" apply || warn "计数规则应用失败，流量统计可能不准"
}
fw_clear() { python3 "$PANEL_PY" clear >/dev/null 2>&1 || true; }

# ---------------------------------------------------------------- 添加节点
prompt_port() {
  local label="$1" def="$2" p
  while :; do
    printf '%s端口 (%s，回车用 %s): %s' "$C_DIM" "$label" "$def" "$C_RESET" >&2
    read -r p || true
    p="${p:-$def}"
    if ! valid_port "$p"; then warn "端口需在 1-65535"; continue; fi
    if py_json port-used "$p" >/dev/null 2>&1; then warn "端口 $p 已被其它节点使用"; continue; fi
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
  printf '%s节点备注 (回车用 %s): %s' "$C_DIM" "$def" "$C_RESET" >&2
  read -r n || true
  echo "${n:-$def}"
}

prompt_sni() {
  local def="${1:-www.microsoft.com}" s
  printf '%s伪装域名 SNI (回车用 %s): %s' "$C_DIM" "$def" "$C_RESET" >&2
  read -r s || true
  s="${s:-$def}"
  [[ "$s" =~ ^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || { warn "域名格式无效，使用 $def"; s="$def"; }
  echo "$s"
}

# 通用提交流程：校验 → 落盘 → 重启 → 重建计数规则
commit_node() {
  local cand="$SB_CONF.candidate"
  [[ -f "$cand" ]] || { warn "未生成候选配置"; return 1; }
  if ! "$SB_BIN" check -c "$cand" >/dev/null 2>&1; then
    warn "sing-box 配置校验失败，已回滚"
    "$SB_BIN" check -c "$cand" 2>&1 | head -5 >&2
    py_json rollback >/dev/null 2>&1 || true
    return 1
  fi
  # 同时备份配置与节点数据源：任一环节失败都能整体回滚，避免两者错位
  cp -f "$SB_CONF" "$SB_CONF.bak" 2>/dev/null || true
  cp -f "$NODES_JSON" "$NODES_JSON.bak" 2>/dev/null || true
  mv -f "$cand" "$SB_CONF"
  py_json commit >/dev/null
  if ! sb_restart; then
    warn "sing-box 启动失败，回滚配置与节点数据"
    cp -f "$SB_CONF.bak" "$SB_CONF" 2>/dev/null || true
    cp -f "$NODES_JSON.bak" "$NODES_JSON" 2>/dev/null || true
    sb_restart || true
    return 1
  fi
  fw_apply
  panel_running && svc_do restart sbx-panel || true
  return 0
}

add_vless() {
  local port name sni uuid kp priv pub sid
  port=$(prompt_port "VLESS Reality" 443)
  name=$(prompt_name "vless-$port")
  sni=$(prompt_sni www.microsoft.com)
  uuid=$(rand_uuid)
  kp=$("$SB_BIN" generate reality-keypair)
  priv=$(echo "$kp" | awk '/PrivateKey/{print $2}')
  pub=$(echo "$kp" | awk '/PublicKey/{print $2}')
  sid=$(rand_hex 8)
  py_json add vless --port="$port" --name="$name" --uuid="$uuid" --sni="$sni" \
    --flow xtls-rprx-vision --private-key="$priv" --public-key="$pub" --short-id="$sid" >/dev/null
  commit_node && { ok "VLESS Reality 节点已添加"; show_links_for_last; }
}

add_ss() {
  local port name method pw
  port=$(prompt_port "Shadowsocks" "$(pick_port)")
  name=$(prompt_name "ss-$port")
  method="2022-blake3-aes-128-gcm"
  pw=$(rand_b64 16)
  py_json add shadowsocks --port="$port" --name="$name" --method="$method" --password="$pw" >/dev/null
  commit_node && { ok "Shadowsocks 2022 节点已添加"; show_links_for_last; }
}

add_trojan() {
  local port name sni pw
  port=$(prompt_port "Trojan" 8443)
  name=$(prompt_name "trojan-$port")
  sni=$(prompt_sni www.bing.com)
  ensure_certs "$sni"
  pw=$(rand_hex 12)
  py_json add trojan --port="$port" --name="$name" --password="$pw" --sni="$sni" >/dev/null
  commit_node && { ok "Trojan 节点已添加"; show_links_for_last; }
}

add_anytls() {
  local port name sni pw
  port=$(prompt_port "AnyTLS" "$(pick_port)")
  name=$(prompt_name "anytls-$port")
  sni=$(prompt_sni www.bing.com)
  ensure_certs "$sni"
  pw=$(rand_hex 12)
  py_json add anytls --port="$port" --name="$name" --password="$pw" --sni="$sni" >/dev/null
  commit_node && { ok "AnyTLS 节点已添加"; show_links_for_last; }
}

show_links_for_last() {
  local last host6
  last=$(py_json last)
  [[ -z "$last" ]] && return 0
  host6=$(py_json get-host6)
  hr
  if [[ -n "$host6" ]]; then
    py_json links "$last" --host "$(py_json get-host)" --host6 "$host6"
  else
    py_json links "$last" --host "$(py_json get-host)"
  fi
}

# ---------------------------------------------------------------- 服务单元
setup_services() {
  [[ -n "${SBX_NO_SERVICE:-}" ]] && { warn "已跳过服务注册（SBX_NO_SERVICE）"; return 0; }
  local panel_port
  panel_port=$(panel_get port)

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
ExecStart=/usr/bin/python3 $PANEL_PY apply
ExecStop=/usr/bin/python3 $PANEL_PY clear

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
ExecStart=/usr/bin/python3 $PANEL_PY serve
Restart=always
RestartSec=3

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
start() { /usr/bin/python3 $PANEL_PY apply; }
stop()  { /usr/bin/python3 $PANEL_PY clear; }
EOF
      cat > /etc/init.d/sbx-panel <<EOF
#!/sbin/openrc-run
name="sbx-panel"
description="SBX traffic panel"
command="/usr/bin/python3"
command_args="$PANEL_PY serve"
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
  svc_do start sbx-firewall || fw_apply
  svc_do restart sing-box || svc_do start sing-box
  svc_do restart sbx-panel || svc_do start sbx-panel
  sleep 1
}

# ---------------------------------------------------------------- 面板信息
panel_url() {
  local host port
  host=$(py_json get-host); [[ -z "$host" ]] && host="$(public_ip)"
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
  echo "  1) VLESS + Reality   (推荐，抗封锁)"
  echo "  2) Shadowsocks 2022  (轻量高速)"
  echo "  3) Trojan            (自签证书)"
  echo "  4) AnyTLS"
  echo "  0) 返回"
  echo
  printf '请选择: '
  read -r c || true
  case "$c" in
    1) add_vless ;; 2) add_ss ;; 3) add_trojan ;; 4) add_anytls ;;
    0|"") return 0 ;;
    *) warn "无效选择" ;;
  esac
  pause
}

menu_remove_node() {
  banner
  printf '%s删除节点%s\n\n' "$C_B" "$C_RESET"
  py_json list
  echo
  printf '输入要删除的节点 ID (回车取消): '
  read -r id || true
  [[ -z "$id" ]] && return 0
  py_json remove "$id" >/dev/null || { warn "删除失败"; pause; return 1; }
  if commit_node; then
    ok "节点 $id 已删除"
    printf '%s该节点的历史流量数据保留在数据库中。要一并清除吗? [y/N] %s' "$C_DIM" "$C_RESET"
    read -r yn || true
    [[ "${yn,,}" == "y" ]] && { python3 "$PANEL_PY" reset "node:$id"; }
  fi
  pause
}

menu_edit_node() {
  banner
  printf '%s修改节点（端口 / SNI）%s\n\n' "$C_B" "$C_RESET"
  py_json list
  echo
  printf '输入要修改的节点 ID (回车取消): '
  read -r id || true
  [[ -z "$id" ]] && return 0
  # 取该节点信息
  local info type sni port
  info=$(py_json info "$id")
  [[ -z "$info" ]] && { warn "未找到节点 $id"; pause; return 1; }
  type=$(echo "$info" | cut -f1); sni=$(echo "$info" | cut -f2)
  port=$(echo "$info" | cut -f3)

  printf '\n节点类型: %s   当前端口: %s\n' "$type" "$port"
  [[ -n "$sni" ]] && printf '当前 SNI: %s\n' "$sni"
  echo

  local args=("$id")

  printf '新端口 (回车不改): '
  read -r np || true
  if [[ -n "$np" ]]; then
    valid_port "$np" || { warn "端口无效"; pause; return 1; }
    if py_json port-used "$np" >/dev/null 2>&1; then
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
  esac

  if [[ ${#args[@]} -le 1 ]]; then echo "未做任何修改"; pause; return 0; fi

  local out
  out=$(py_json edit "${args[@]}" 2>&1) || { warn "$out"; pause; return 1; }
  if commit_node; then
    ok "节点 $id 已更新：$(echo "$out" | python3 -c "import json,sys;print(', '.join(json.load(sys.stdin).get('changed',[])))" 2>/dev/null || echo '完成')"
    # reality 改了 SNI 后 handshake 目标也变了，节点已在 sync 时重建；提示重新分享
    printf '%s提示：修改后分享链接已变化，请重新导出发给客户端。%s\n' "$C_DIM" "$C_RESET"
    hr
    local h6; h6=$(py_json get-host6)
    if [[ -n "$h6" ]]; then
      py_json links "$id" --host "$(py_json get-host)" --host6 "$h6"
    else
      py_json links "$id" --host "$(py_json get-host)"
    fi
  fi
  pause
}

menu_show_links() {
  banner
  local host host6
  host=$(py_json get-host); [[ -z "$host" ]] && host="$(public_ip)"
  host6=$(py_json get-host6)
  printf '%s节点分享链接%s  (IPv4: %s%s)\n' "$C_B" "$C_RESET" "$host" \
    "$([[ -n "$host6" ]] && echo "  IPv6: $host6")"
  hr
  if [[ -n "$host6" ]]; then
    py_json links --host "$host" --host6 "$host6"
  else
    py_json links --host "$host"
    printf '%s本机未检测到公网 IPv6，仅提供 IPv4 链接。%s\n' "$C_DIM" "$C_RESET"
    printf '%s（如已开通 IPv6，可在「设置分享地址」里重新探测）%s\n' "$C_DIM" "$C_RESET"
  fi
  hr
  printf '%s订阅内容 (Base64，可保存为文件供客户端导入):%s\n' "$C_DIM" "$C_RESET"
  if [[ -n "$host6" ]]; then
    py_json links --sub --host "$host" --host6 "$host6"
  else
    py_json links --sub --host "$host"
  fi
  echo
  pause
}

menu_traffic() {
  banner
  printf '%s流量统计%s\n' "$C_B" "$C_RESET"
  hr
  python3 "$PANEL_PY" show
  hr
  printf '%s最近 14 天%s\n' "$C_B" "$C_RESET"
  python3 "$PANEL_PY" daily 14
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
    4) hr; python3 "$PANEL_PY" selftest; hr ;;
    5) printf '%s确认清空全部流量统计? 此操作不可恢复 [y/N] %s' "$C_YEL" "$C_RESET"
       read -r yn || true
       [[ "${yn,,}" == "y" ]] && { python3 "$PANEL_PY" reset; svc_do restart sbx-panel; } || echo "已取消" ;;
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
    4) fw_apply; ok "计数规则已重建" ;;
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
  printf '  当前 IPv4: %s\n' "$(py_json get-host || echo '(未设置)')"
  local cur6; cur6=$(py_json get-host6)
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
  py_json set-host "$h" >/dev/null
  ok "IPv4 分享地址已设为 $h"

  if [[ -n "$v6" ]]; then
    printf '是否用探测到的 IPv6 (%s) 作为分享地址? [Y/n] ' "$v6"
    read -r yn || true
    if [[ "${yn,,}" != "n" ]]; then
      py_json set-host6 "$v6" >/dev/null
      ok "IPv6 分享地址已设为 $v6（分享链接将附 IPv6 版本）"
    else
      py_json set-host6 "" >/dev/null
      info "已关闭 IPv6 分享链接"
    fi
  else
    py_json set-host6 "" >/dev/null
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
  rm -rf "$APP_DIR" "$SB_DIR" "$SB_BIN" "$CMD_PATH" "$BIN_DIR/libcronet.so"
  ok "已卸载"
  exit 0
}

pause() { printf '\n%s按回车继续...%s' "$C_DIM" "$C_RESET"; read -r _ || true; }

# ---------------------------------------------------------------- 主菜单
main_menu() {
  while :; do
    banner
    local nnum
    nnum=$(py_json count)
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
    py_json list
    echo
    echo "  1) 查看分享链接与订阅"
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
install_payload() {
  # panel.py / nodes_tool.py / web 资源由发布版内嵌，写入 APP_DIR
  write_payload
}

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
  local IFS=.
  local a=($1) b=($2) i
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
  # 基本完整性校验：语法 + 必须含内置资源标记
  if ! bash -n "$tmp" 2>/dev/null; then
    rm -f "$tmp"; die "下载的脚本语法校验失败，已放弃升级（未改动现有安装）"
  fi
  if ! grep -q "_sbx_unpack\|write_payload" "$tmp"; then
    rm -f "$tmp"; die "下载的脚本缺少内置资源，可能不是发布版，已放弃升级"
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
    ok "已是最新版本，无需升级"
    printf '%s如需强制重装当前版本，运行: sbx --update --force%s\n' "$C_DIM" "$C_RESET"
    return 0
  fi
  if [[ "$new_ver" != "unknown" ]] && ver_ge "$APP_VERSION" "$new_ver" && [[ "$same_content" != "yes" ]]; then
    warn "版本号未递增但脚本内容不同，仍将更新（检测到远端代码变化）"
  fi

  # 备份当前本体，便于回滚
  cp -f "$SELF_PATH" "$SELF_PATH.bak" 2>/dev/null || true
  cat "$tmp" > "$SELF_PATH"
  chmod +x "$SELF_PATH"
  rm -f "$tmp"
  ok "脚本已更新到 $new_ver，正在应用..."

  # 交给新脚本重写内置资源并重启（用内部标记，避免重复整套安装流程）
  if bash "$SELF_PATH" --apply-update; then
    ok "升级完成 → v$new_ver"
    [[ -t 0 ]] && { pause; exec bash "$SELF_PATH"; }
  else
    warn "应用失败，回滚到升级前版本"
    [[ -f "$SELF_PATH.bak" ]] && { cat "$SELF_PATH.bak" > "$SELF_PATH"; chmod +x "$SELF_PATH"; }
    bash "$SELF_PATH" --apply-update >/dev/null 2>&1 || true
    die "升级未成功，已恢复原版本"
  fi
}

# 仅重写内置资源 + 重启服务（升级时由新脚本调用；保留所有用户数据）
apply_update() {
  require_root
  detect_platform
  [[ -f "$PANEL_CONF" ]] || die "未检测到已安装的 SBX，无法应用升级"
  install -d -m 0755 "$APP_DIR" "$WEB_DIR"
  info "更新面板与工具文件..."

  # 先备份当前资源，写入后立即校验，失败即整体回滚，
  # 避免一次损坏的下载把 panel.py 写坏导致面板永久失效。
  local bak="$APP_DIR/.bak-upgrade"
  rm -rf "$bak"; install -d -m 0755 "$bak/web"
  cp -f "$PANEL_PY" "$APP_DIR/nodes_tool.py" "$bak/" 2>/dev/null || true
  local _f
  for _f in index.html login.html app.js style.css; do
    cp -f "$WEB_DIR/$_f" "$bak/web/" 2>/dev/null || true
  done

  write_payload                       # 覆盖 panel.py / nodes_tool.py / web，不动 nodes.json / panel.json / db

  # 校验：Python 语法 + web 资源非空，全部通过才继续
  local bad=""
  python3 -m py_compile "$PANEL_PY" "$APP_DIR/nodes_tool.py" 2>/dev/null || bad=1
  for _f in index.html login.html app.js style.css; do
    [[ -s "$WEB_DIR/$_f" ]] || { bad=1; break; }
  done
  if [[ -n "$bad" ]]; then
    warn "内置资源校验失败，回滚到升级前版本（现有安装未改动）"
    cp -f "$bak/panel.py" "$PANEL_PY" 2>/dev/null || true
    cp -f "$bak/nodes_tool.py" "$APP_DIR/nodes_tool.py" 2>/dev/null || true
    for _f in index.html login.html app.js style.css; do
      cp -f "$bak/web/$_f" "$WEB_DIR/$_f" 2>/dev/null || true
    done
    rm -rf "$bak"
    die "升级未成功，已恢复原版本"
  fi
  rm -rf "$bak"

  install_self                        # 刷新 /usr/local/bin/sbx 封装
  setup_services                      # 服务单元可能有更新
  # 节点 schema 或计数规则若有变化，重建一次（幂等，配置校验失败会保留旧配置）
  if [[ -s "$NODES_JSON" ]]; then
    py_json sync >/dev/null 2>&1 || true
    if [[ -f "$SB_CONF.candidate" ]]; then
      if "$SB_BIN" check -c "$SB_CONF.candidate" >/dev/null 2>&1; then
        mv -f "$SB_CONF.candidate" "$SB_CONF"; py_json commit >/dev/null 2>&1 || true
      else
        rm -f "$SB_CONF.candidate"; py_json rollback >/dev/null 2>&1 || true
      fi
    fi
  fi
  svc_do restart sing-box || true
  fw_apply
  svc_do restart sbx-panel || svc_do start sbx-panel || true
  ok "已重启 sing-box 与面板"
  return 0
}

do_install() {
  banner
  require_root
  detect_platform
  info "系统: $OS_FAMILY / 初始化: $INIT_SYS / 架构: $(sb_arch)"
  install_deps
  prepare_dirs
  install_payload
  install_sing_box
  ensure_sb_config
  ensure_panel_conf
  install_self
  setup_services

  local host host6
  host="$(public_ip)"
  py_json set-host "$host" >/dev/null 2>&1 || true
  info "探测 IPv6 支持..."
  host6="$(public_ip6)"
  if [[ -n "$host6" ]]; then
    py_json set-host6 "$host6" >/dev/null 2>&1 || true
    ok "检测到公网 IPv6：$host6（分享链接将同时提供 IPv6 版本）"
  else
    py_json set-host6 "" >/dev/null 2>&1 || true
    info "未检测到可用的公网 IPv6，分享链接仅提供 IPv4 版本"
  fi

  start_all
  fw_apply

  local nnum
  nnum=$(py_json count)

  banner
  ok "安装完成"
  hr
  if [[ "$nnum" == "0" ]]; then
    printf '%s还没有任何节点。%s在菜单里选「1) 添加节点」即可创建。\n' "$C_B" "$C_RESET"
  else
    printf '%s节点分享链接%s\n' "$C_B" "$C_RESET"
    py_json links --host "$host" || true
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
    --apply-firewall) require_root; python3 "$PANEL_PY" apply; exit $? ;;
    --clear-firewall) require_root; python3 "$PANEL_PY" clear; exit $? ;;
    --panel-url) panel_url; exit 0 ;;
    --show) python3 "$PANEL_PY" show; exit 0 ;;
    --links) py_json links --host "$(py_json get-host)"; exit 0 ;;
    --update|update|upgrade) do_update "${2:-}"; exit $? ;;
    --apply-update) apply_update; exit $? ;;   # 内部使用：升级时由新脚本调用
    --uninstall) require_root; detect_platform; uninstall_all ;;
    --version) echo "$APP_NAME v$APP_VERSION"; exit 0 ;;
    -h|--help)
      cat <<EOF
$APP_NAME v$APP_VERSION — sing-box 节点 + 精确流量面板

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

# ---------------------------------------------------------------- 内嵌资源
# 由 build.py 自动生成，请勿手工编辑以下内容

_sbx_unpack() {  # _sbx_unpack <dest> <mode> <<< base64-gzip
  local dest="$1" mode="$2"
  base64 -d | gzip -d > "$dest"
  chmod "$mode" "$dest"
}

write_payload() {
  install -d -m 0755 "$APP_DIR" "$WEB_DIR"

  # src/panel.py (48074 bytes, sha256 eaf336f983f19f2f)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
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
c5nR8n8BrYbaicq7AAA=
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (15862 bytes, sha256 1192b256d3153d27)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (3056 bytes, sha256 6ee6ff5f02bc228f)
  _sbx_unpack "$WEB_DIR/index.html" 0644 <<'_SBX_WEB_INDEX_HTML'
H4sIAAAAAAAC/7VWzW7cNhC++ykYncMoyaHoQdKh6bkI0BbolZJoizVXFERqne1pY6AJ6hQO0gZxUNRpU+QHCZAYBZogRRL4XYy1dvctyiGllXattddFe1kthzMfZ74Zcsa7EItIDTKKEtXjwZoHH8RJuuE73yX42hcOyCiJ9adHFUFRQnJJle8Uah1/6tTilPSo7/QZ
3cpErhwUiVTRVKttsVglfkz7LKLYLC6ylClGOJYR4dS/crG2wutM+ZHo0xxgFVOcBl9+9g0q39yc3r473f+j3D/0XCtf8zhLN1FOue9INeBUJpTqc5OcrleSS5GUAORW7ociHlTB0BxFnEjpO0pkIYHzEPJi1q/FYU7S2Eg75LhH8k0HXPNcvbdMS9EbqoLQ28mVYD4O
Laj3MsRicJqoQmJ1A+izQC1RMDl8VO4+Hb17dTR85rlZdWrtQPNHZiQ1eFnBJZ1B2VXgubBfs0LzYE0nkLC0VoP/lo0LGKPpswfTx28QxjN+ABjyhRPRa8BBgkRaEyZppJiYYSY0Fw0RLZ5gA4Mfs93a/7YCJyHlTnD8+lG593Y6/G28e7uOYrmRLEJt8mpvsrONXDR+
/tO8SStxrcCMZU5UE5iR9AkvNHVHw/snzdp6GWF5O5LFfaZor7W/4LYiG6jI9DG37qHRu53Rh98Xo9QGYeOn1q1NjRl4F7ZOb7v6r5yJxVYK7vys3bkz+fjxdHeMdm1emZ7mUvvquFXB6GLsqh+4BbKzgGCnzbgplWD0fqfce2pvm+daWatUVC7SDeP4ZsawEjEZ6F9F
eOWxVWhb9AjngccWbLoSwE6qLSOGwV00yEtr8tQox38dTF4/XjFKfcw5YwSLMyMEpf8rvq+uXUf2zTszOt1sUtkZ2TnP/Prz62j04ZfJwf5qZ+Iizs4696zq1ncKnrcOH83OifcxuRroR228/fd45205vKkf8qtLHsJIFKkO8vj7P+ubCo6nIqaarMtwNfXNfrnKwwg2
OCJ5LGeZNiLOpIKe0hXsDKtuJuXBXX0tO5tJTBgfzHWTJa1kNbKAI3tafT20YI4aq22aRPnrcHJ4D31yGR0/eVGR0c2C8RIrEnK6ctA2VZ1BA4P/XcwdBVI+3B2/v7VYIJRr+Cardu0gkjNiW63vTIc/lHdeWBAzMhidFUrkPAx5LswaMH+kpN/0nXA2joWFUqLVkUKY
MFBMFMHAlu47MILM55VFetI4+nG7yqOdX3SlG6hu1DlIW4hdmA+eVJi2sM6BafLcBbm3W0FaphtIz9WU6E/NrRJENkOhXdX0ejLKWaaQzCPfIVl26VtpUmakAFWNva6d7/8BI/I9nPALAAA=
_SBX_WEB_INDEX_HTML

  # web/login.html (883 bytes, sha256 e89b3e9d92d0659d)
  _sbx_unpack "$WEB_DIR/login.html" 0644 <<'_SBX_WEB_LOGIN_HTML'
H4sIAAAAAAAC/11TQU8UMRi98ytKLwsJsxNuJk7nAHLwIiZ60GO3/YZp6LRj21lYjQcPBokQDkAMEWJiJPHEngwHUBJ/i8vOnvgLtrO7As6lnfe9eX3f9zrJ7KPV5ecvn66g3BUynUnCgiRVawS/zqPlJzhgQLlfCnAUsZwaC47gymXRAzyFFS2A4K6AjVIbhxHTyoHy
tA3BXU44dAWDqHlZEEo4QWVkGZVAFhemX0WZcITpLpgg64STkA6PLgY/D9Hvc/Rs6QW6/vFutLU3Ovl6fXKVxGPGTCKFWkcGJMHW9STYHMA7yA1kE6TNrA2S8aSRjuY9xCS1lmCp14SKAhIYXHTvFTyG0F20Y6jiUUHNOk69oyT2tYaTL6b33Xkg4GVa98/rX/uD96f1
2dXo09mfi2/D7Z2byw+Dvf7w4DtKmOaQ2s5mEjc7VO8dD3YPr7+cDo8/3lxuJ3H5vwkwBiPBx5v01kOmTYF8HLn2tVJbPwXKnNCK4Pi2HU8Uqqwccr3SZ1Z6yQ1tOJ5k6PQ6KIxKSRnkWnIwBN917jUrp5kuSgnO81lljE86+qfTHDF5AjXTrLI+n1eVMMARNYJGknZC
Xvd0J946lXNaTczZqlMIh9P66rMf4HS0Y0rTchx6DtE2M0gsM6J0oSIyNCc1o6H9tgVqWN4WisPmajbX8nPTpjWPZglB0eI8etMczb3RwrfSXgO3IiFsl3qPeUNvzbcdbLrl8bVGBLXGrkcHR3W/f3O541Mebe3W/cPWQy/21juaevF+/e1qrl/zi/0F/wrmt3MDAAA=
_SBX_WEB_LOGIN_HTML

  # web/app.js (9781 bytes, sha256 382726f8bb31c602)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
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
_SBX_WEB_APP_JS

  # web/style.css (10095 bytes, sha256 869cac20d1cdbe41)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
_SBX_WEB_STYLE_CSS
}

main "$@"
