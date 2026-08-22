#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="1.8.0"
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
  [[ -f "$PANEL_CONF" ]] && return 0
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

panel_get() { python3 -c "
import json,sys
print(json.load(open('$PANEL_CONF')).get(sys.argv[1],''))
" "$1"; }

panel_set() { python3 -c "
import json,sys
p='$PANEL_CONF'
d=json.load(open(p))
k,v=sys.argv[1],sys.argv[2]
try: v=int(v)
except ValueError: pass
d[k]=v
json.dump(d,open(p,'w'),indent=2,ensure_ascii=False)
" "$1" "$2"; }

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
  cp -f "$SB_CONF" "$SB_CONF.bak" 2>/dev/null || true
  mv -f "$cand" "$SB_CONF"
  py_json commit >/dev/null
  if ! sb_restart; then
    warn "sing-box 启动失败，回滚配置"
    cp -f "$SB_CONF.bak" "$SB_CONF" 2>/dev/null || true
    py_json sync >/dev/null 2>&1 || true
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

add_hy2() {
  local port name sni pw
  port=$(prompt_port "Hysteria2" "$(pick_port)")
  name=$(prompt_name "hy2-$port")
  sni=$(prompt_sni www.bing.com)
  ensure_certs "$sni"
  pw=$(rand_hex 12)
  py_json add hysteria2 --port="$port" --name="$name" --password="$pw" --sni="$sni" >/dev/null
  commit_node && { ok "Hysteria2 节点已添加"; show_links_for_last; }
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

add_tuic() {
  local port name sni pw uuid
  port=$(prompt_port "TUIC" "$(pick_port)")
  name=$(prompt_name "tuic-$port")
  sni=$(prompt_sni www.bing.com)
  ensure_certs "$sni"
  uuid=$(rand_uuid); pw=$(rand_hex 12)
  py_json add tuic --port="$port" --name="$name" --uuid="$uuid" --password="$pw" --sni="$sni" >/dev/null
  commit_node && { ok "TUIC 节点已添加"; show_links_for_last; }
}

add_vmess() {
  local port name uuid path
  port=$(prompt_port "VMess WebSocket" 8080)
  name=$(prompt_name "vmess-$port")
  uuid=$(rand_uuid); path="/$(rand_hex 4)"
  py_json add vmess --port="$port" --name="$name" --uuid="$uuid" --path="$path" >/dev/null
  commit_node && { ok "VMess WS 节点已添加"; show_links_for_last; }
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
  last=$(python3 -c "
import json
d=json.load(open('$NODES_JSON'))
print(d[-1]['id'] if d else '')
")
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
  local panel_port token
  panel_port=$(panel_get port)
  token=$(panel_get token)

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
  local host port token
  host=$(py_json get-host); [[ -z "$host" ]] && host="$(public_ip)"
  port=$(panel_get port); token=$(panel_get token)
  echo "http://$(host_for_uri "$host"):$port/?token=$token"
}

show_panel_info() {
  hr
  printf '%s流量面板%s\n' "$C_B" "$C_RESET"
  printf '  地址: %s%s%s\n' "$C_CYAN" "$(panel_url)" "$C_RESET"
  printf '  令牌: %s\n' "$(panel_get token)"
  printf '  状态: %s\n' "$(panel_running && echo "${C_GREEN}运行中${C_RESET}" || echo "${C_RED}未运行${C_RESET}")"
  printf '  后端: %s\n' "$(command -v nft >/dev/null 2>&1 && echo nftables || echo iptables)"
  hr
}

# ---------------------------------------------------------------- 菜单动作
menu_add_node() {
  banner
  printf '%s添加节点%s\n\n' "$C_B" "$C_RESET"
  echo "  1) VLESS + Reality      (推荐，抗封锁)"
  echo "  2) Shadowsocks 2022     (轻量高速)"
  echo "  3) Hysteria2            (UDP，弱网优化)"
  echo "  4) Trojan               (自签证书)"
  echo "  5) TUIC v5              (UDP)"
  echo "  6) VMess + WebSocket    (可套 CDN)"
  echo "  7) AnyTLS"
  echo "  0) 返回"
  echo
  printf '请选择: '
  read -r c || true
  case "$c" in
    1) add_vless ;; 2) add_ss ;; 3) add_hy2 ;; 4) add_trojan ;;
    5) add_tuic ;; 6) add_vmess ;; 7) add_anytls ;;
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
  local info type sni port ports
  info=$(python3 -c "
import json
d=json.load(open('$NODES_JSON'))
n=next((x for x in d if str(x['id'])==str('$id')), None)
if n: print('%s\t%s\t%s\t%s' % (n['type'], n.get('sni',''), n.get('port',''), n.get('ports','')))
")
  [[ -z "$info" ]] && { warn "未找到节点 $id"; pause; return 1; }
  type=$(echo "$info" | cut -f1); sni=$(echo "$info" | cut -f2)
  port=$(echo "$info" | cut -f3); ports=$(echo "$info" | cut -f4)

  printf '\n节点类型: %s   当前端口: %s\n' "$type" "$port"
  [[ -n "$sni" ]] && printf '当前 SNI: %s\n' "$sni"
  [[ -n "$ports" ]] && printf '当前端口范围(跳跃): %s\n' "$ports"
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
    vless|trojan|hysteria2|tuic|anytls)
      printf '新 SNI 伪装域名 (回车不改): '
      read -r ns || true
      if [[ -n "$ns" ]]; then
        [[ "$ns" =~ ^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || { warn "域名格式无效"; pause; return 1; }
        args+=("--sni=$ns")
      fi ;;
  esac

  # hysteria2 额外支持端口跳跃范围
  if [[ "$type" == "hysteria2" ]]; then
    printf '端口跳跃范围 如 20000-30000 (回车不改，输入 - 清空): '
    read -r nr || true
    if [[ "$nr" == "-" ]]; then args+=("--ports=")
    elif [[ -n "$nr" ]]; then args+=("--ports=$nr"); fi
  fi

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
  echo "  2) 重新生成访问令牌"
  echo "  3) 修改采集间隔（当前 $(panel_get interval) 秒）"
  echo "  4) 仅本机访问 / 允许公网访问（当前 $(panel_get listen)）"
  echo "  5) 运行统计自检"
  echo "  6) 清空所有统计数据"
  echo "  0) 返回"
  echo
  printf '请选择: '
  read -r c || true
  case "$c" in
    1) printf '新端口: '; read -r p || true
       valid_port "$p" && { panel_set port "$p"; svc_do restart sbx-panel; ok "已改为 $p"; } || warn "无效端口" ;;
    2) panel_set token "$(rand_hex 16)"; svc_do restart sbx-panel; ok "新令牌: $(panel_get token)" ;;
    3) printf '采集间隔秒数 (1-60): '; read -r n || true
       [[ "$n" =~ ^[0-9]+$ ]] && ((n>=1 && n<=60)) && { panel_set interval "$n"; svc_do restart sbx-panel; ok "已改为 ${n}s"; } || warn "无效数值" ;;
    4) if [[ "$(panel_get listen)" == "127.0.0.1" ]]; then
         panel_set listen "0.0.0.0"; ok "已允许公网访问（务必保留令牌）"
       else
         panel_set listen "127.0.0.1"; ok "已限制为仅本机访问"
       fi
       svc_do restart sbx-panel ;;
    5) hr; python3 "$PANEL_PY" selftest; hr ;;
    6) printf '%s确认清空全部流量统计? 此操作不可恢复 [y/N] %s' "$C_YEL" "$C_RESET"
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
    nnum=$(python3 -c "
import json
try: print(len(json.load(open('$NODES_JSON'))))
except Exception: print(0)
")
    printf '  节点: %s%s%s 个    sing-box: %s    面板: %s    %sv%s%s\n' \
      "$C_B" "$nnum" "$C_RESET" \
      "$(sb_running && echo "${C_GREEN}●${C_RESET}" || echo "${C_RED}●${C_RESET}")" \
      "$(panel_running && echo "${C_GREEN}●${C_RESET}" || echo "${C_RED}●${C_RESET}")" \
      "$C_DIM" "$APP_VERSION" "$C_RESET"
    printf '  面板: %s%s%s\n\n' "$C_CYAN" "$(panel_url)" "$C_RESET"
    echo "  1) 添加节点"
    echo "  2) 修改节点（端口 / SNI）"
    echo "  3) 删除节点"
    echo "  4) 查看节点与分享链接"
    echo "  5) 流量统计"
    echo "  6) 面板设置"
    echo "  7) 服务管理"
    echo "  8) 设置分享地址（域名/IP）"
    echo "  9) 检查更新 / 升级"
    echo " 10) 卸载"
    echo "  0) 退出"
    echo
    printf '请选择: '
    read -r c || true
    case "$c" in
      1) menu_add_node ;;
      2) menu_edit_node ;;
      3) menu_remove_node ;;
      4) menu_show_links ;;
      5) menu_traffic ;;
      6) menu_panel_settings ;;
      7) menu_service ;;
      8) menu_host ;;
      9) do_update; pause ;;
      10) uninstall_all ;;
      0|"") clear 2>/dev/null || true; exit 0 ;;
      *) warn "无效选择"; sleep 1 ;;
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

  local tmp new_ver
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
  info "当前版本: $APP_VERSION    最新版本: $new_ver"

  if [[ "$new_ver" != "unknown" ]] && ver_ge "$APP_VERSION" "$new_ver" && [[ "$force" != "--force" ]]; then
    rm -f "$tmp"
    ok "已是最新版本，无需升级"
    printf '%s如需强制重装当前版本，运行: sbx --update --force%s\n' "$C_DIM" "$C_RESET"
    return 0
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
  write_payload                       # 覆盖 panel.py / nodes_tool.py / web，不动 nodes.json / panel.json / db
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
  nnum=$(python3 -c "
import json
try: print(len(json.load(open('$NODES_JSON'))))
except Exception: print(0)
")

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

  # src/panel.py (46797 bytes, sha256 314428d69b24636e)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
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
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (16807 bytes, sha256 7a7ce74388ab57b4)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (5675 bytes, sha256 03d05773c0f5cd09)
  _sbx_unpack "$WEB_DIR/index.html" 0644 <<'_SBX_WEB_INDEX_HTML'
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
_SBX_WEB_INDEX_HTML

  # web/login.html (676 bytes, sha256 9576018569a151c7)
  _sbx_unpack "$WEB_DIR/login.html" 0644 <<'_SBX_WEB_LOGIN_HTML'
H4sIAAAAAAAC/3VSMW8TMRjd+yuM5x6nbgznWwprYWBh9Pm+5Kz67MP+LmnYGCoaURQkWlWIVpUKlZjSqXRoIBK/hVwuU/4CvuSCYOCW7/z5+fl97zl68Pjp7vMXz56QDHMVb0VNIYrrLqOvsmB3jzY94KkvOSAnIuPWATJaYid4RDdtzXNgtCehXxiLlAijEbSH9WWK
GUuhJwUEq8U2kVqi5CpwgitgO9tkcy7oSGTC9MA2xChRQTz/eD/7fkp+3hEndTdIzAGpbl8v3owWF1fVxTQK17CtSEm9TywoRh0OFLgMwAvJLHTazkPhXMMbtvMkJh34ksoeEYo7x6gyXak9hJCoY2xO/GiZSRntNkxcoDSa0XAF8JBsJ/6PJL+zhhRxfXNX//gwO7yu
x9PF2fjX/Zf58Hg5OZqNh/Xnw+rs2/zksjp6v5wcz0Y385OvJBImhdglB1G4+iP16Hz27rS6vJ6fv11OhlFYtORSFyUSHBTe+cLr7xub0jYJNPugKSkUF5AZlYJl9G8FfpwSjTB5oQA9XpTW+ryCPzyrK9qvgXaMKJ2392UpLaSEW8kDxZPG7n94W21JiWh0K86VSS6R
xvX0kzdiY9IasjI7bNxukvFZNKVNJly/yd/u4YiFpAIAAA==
_SBX_WEB_LOGIN_HTML

  # web/app.js (22684 bytes, sha256 815e7b20755b62a2)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
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
_SBX_WEB_APP_JS

  # web/style.css (12008 bytes, sha256 5c025dc5b32366d5)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
_SBX_WEB_STYLE_CSS
}

main "$@"
