#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="1.6.0"
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
  py_json add vless --port "$port" --name "$name" --uuid "$uuid" --sni "$sni" \
    --flow xtls-rprx-vision --private-key "$priv" --public-key "$pub" --short-id "$sid" >/dev/null
  commit_node && { ok "VLESS Reality 节点已添加"; show_links_for_last; }
}

add_ss() {
  local port name method pw
  port=$(prompt_port "Shadowsocks" "$(pick_port)")
  name=$(prompt_name "ss-$port")
  method="2022-blake3-aes-128-gcm"
  pw=$(rand_b64 16)
  py_json add shadowsocks --port "$port" --name "$name" --method "$method" --password "$pw" >/dev/null
  commit_node && { ok "Shadowsocks 2022 节点已添加"; show_links_for_last; }
}

add_hy2() {
  local port name sni pw
  port=$(prompt_port "Hysteria2" "$(pick_port)")
  name=$(prompt_name "hy2-$port")
  sni=$(prompt_sni www.bing.com)
  ensure_certs "$sni"
  pw=$(rand_hex 12)
  py_json add hysteria2 --port "$port" --name "$name" --password "$pw" --sni "$sni" >/dev/null
  commit_node && { ok "Hysteria2 节点已添加"; show_links_for_last; }
}

add_trojan() {
  local port name sni pw
  port=$(prompt_port "Trojan" 8443)
  name=$(prompt_name "trojan-$port")
  sni=$(prompt_sni www.bing.com)
  ensure_certs "$sni"
  pw=$(rand_hex 12)
  py_json add trojan --port "$port" --name "$name" --password "$pw" --sni "$sni" >/dev/null
  commit_node && { ok "Trojan 节点已添加"; show_links_for_last; }
}

add_tuic() {
  local port name sni pw uuid
  port=$(prompt_port "TUIC" "$(pick_port)")
  name=$(prompt_name "tuic-$port")
  sni=$(prompt_sni www.bing.com)
  ensure_certs "$sni"
  uuid=$(rand_uuid); pw=$(rand_hex 12)
  py_json add tuic --port "$port" --name "$name" --uuid "$uuid" --password "$pw" --sni "$sni" >/dev/null
  commit_node && { ok "TUIC 节点已添加"; show_links_for_last; }
}

add_vmess() {
  local port name uuid path
  port=$(prompt_port "VMess WebSocket" 8080)
  name=$(prompt_name "vmess-$port")
  uuid=$(rand_uuid); path="/$(rand_hex 4)"
  py_json add vmess --port "$port" --name "$name" --uuid "$uuid" --path "$path" >/dev/null
  commit_node && { ok "VMess WS 节点已添加"; show_links_for_last; }
}

add_anytls() {
  local port name sni pw
  port=$(prompt_port "AnyTLS" "$(pick_port)")
  name=$(prompt_name "anytls-$port")
  sni=$(prompt_sni www.bing.com)
  ensure_certs "$sni"
  pw=$(rand_hex 12)
  py_json add anytls --port "$port" --name "$name" --password "$pw" --sni "$sni" >/dev/null
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
    args+=(--port "$np")
  fi

  # 仅对支持 SNI 的类型询问
  case "$type" in
    vless|trojan|hysteria2|tuic|anytls)
      printf '新 SNI 伪装域名 (回车不改): '
      read -r ns || true
      if [[ -n "$ns" ]]; then
        [[ "$ns" =~ ^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || { warn "域名格式无效"; pause; return 1; }
        args+=(--sni "$ns")
      fi ;;
  esac

  # hysteria2 额外支持端口跳跃范围
  if [[ "$type" == "hysteria2" ]]; then
    printf '端口跳跃范围 如 20000-30000 (回车不改，输入 - 清空): '
    read -r nr || true
    if [[ "$nr" == "-" ]]; then args+=(--ports "")
    elif [[ -n "$nr" ]]; then args+=(--ports "$nr"); fi
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

  # src/panel.py (47138 bytes, sha256 215afa3a0f2ed085)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
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
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (16807 bytes, sha256 7a7ce74388ab57b4)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (5472 bytes, sha256 e6feb44e3d27baab)
  _sbx_unpack "$WEB_DIR/index.html" 0644 <<'_SBX_WEB_INDEX_HTML'
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
SiRXuBO9L+bke+p/sYsabmAVAAA=
_SBX_WEB_INDEX_HTML

  # web/login.html (676 bytes, sha256 9576018569a151c7)
  _sbx_unpack "$WEB_DIR/login.html" 0644 <<'_SBX_WEB_LOGIN_HTML'
H4sIAAAAAAAC/3VSMW8TMRjd+yuM5x6nbgznWwprYWBh9Pm+5Kz67MP+LmnYGCoaURQkWlWIVpUKlZjSqXRoIBK/hVwuU/4CvuSCYOCW7/z5+fl97zl68Pjp7vMXz56QDHMVb0VNIYrrLqOvsmB3jzY94KkvOSAnIuPWATJaYid4RDdtzXNgtCehXxiLlAijEbSH9WWK
GUuhJwUEq8U2kVqi5CpwgitgO9tkcy7oSGTC9MA2xChRQTz/eD/7fkp+3hEndTdIzAGpbl8v3owWF1fVxTQK17CtSEm9TywoRh0OFLgMwAvJLHTazkPhXMMbtvMkJh34ksoeEYo7x6gyXak9hJCoY2xO/GiZSRntNkxcoDSa0XAF8JBsJ/6PJL+zhhRxfXNX//gwO7yu
x9PF2fjX/Zf58Hg5OZqNh/Xnw+rs2/zksjp6v5wcz0Y385OvJBImhdglB1G4+iP16Hz27rS6vJ6fv11OhlFYtORSFyUSHBTe+cLr7xub0jYJNPugKSkUF5AZlYJl9G8FfpwSjTB5oQA9XpTW+ryCPzyrK9qvgXaMKJ2392UpLaSEW8kDxZPG7n94W21JiWh0K86VSS6R
xvX0kzdiY9IasjI7bNxukvFZNKVNJly/yd/u4YiFpAIAAA==
_SBX_WEB_LOGIN_HTML

  # web/app.js (23719 bytes, sha256 fdd484f4936c4df0)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
H4sIAAAAAAAC/+08a3MTV5bf/Ss63hq6BZIsmUcYv1KQMCE7hKQwmWTH5Uq1pLbVY7lb092y5clQxWOCzdMkgZBAGCATApNMMJOQ4GADVbv/ZMotyZ/4C3set7tvSy1jktTUfthUBbfu45xzz+uee+6jb6viFuqZqm4ZFcU/da759ZLyr6MXFf/89ebF68p/jirblMap
M82VT5TR3736dPVs4/IN/4uP10/cWXt8rfX9xz3KVsVfPOc/vLh+9Lr/8Mu11U/8sx+3vjzWPP7j09UrWK3kU0rjzEl/4WTj/C1/8W9QvP71J+uff9B6dLe19Hnr0cr6/KLSp1fNvoo5YzxdXQBQzfPz21pP/go9GpfuPV09BZjX5881r/yFG7q16WndmQPUX/mf3fEX
5ltnT/hX7/etPToPgBuXf2icOUW4+1OKf/evWHDpnv/N5ebFO4pj/LFmuN4ey5zWPdO2fuPo04biH7uCTY6uNlc/8k/fAXyt2ydbt0+tLZ8GOpor36wtn2s9+M5f/ITgbk8pZcOxldaTB2uPbjSuftt8+KTx2T/8z+41V+43Tn/RWPkAoDS/ukwDPttYWvSXb/s/fgfl
jfN3Wk+uQiFAbH4DCM6sLR9t/ONma+lB45/HCfqOFI6scfV+4+N7gBE4iiN/5Y3XlcbH84Cmdfo48ffs+rEn/vvnGpfut27eUUzLMpz9h18/oACr/JWH/tJqa/4+9Fy//FXzxDGA3Nej1lxDcT3HLHrqYE/PjO4oh9/47b6DyrBiGbPKW4cOjBq6Uyy/qQNXXK1iF4lH
WZdKU9lJw9NUz54yLDWl/PnPigpQEIjr6Z4BQN5TSvqcO6Bsz6UV13a80aJdNQYU6ALlalqx7JLxWmlAsWqVCrRgMQY/UfzBt6Nbk9hxe25aVY4wkkN7Dr66790De/buO0CoqBLatJ58AAgV0LD1D68DErW/HBT3K/69RRA/luZLQWle8b/4Oxa9GBa9GBRtz5VkmFCI
6HsmalYROaGA+mlV3SunlSrxKKW816MoSF4tYqJoEbLPdsxJ00oNQktzQtGCnm8U/mAUveyUMecGZdkJ29mnF8taiFCbAhRKTYiABQM/PG0qIGFsajw1qBwJwZNEU0ldhOjSLHTq4BhezbGUCcMDpLU0oCoCemS9ZWdcz3YMYEAq65UNS6LJ4WEzPieL4q+5yvDwsLIj
B/bulR17lpixz3FsR1PXVr5onkLn0bi0gLa19AB0FLS7+emK/+iSSpQwsBecrD2VAIHNw//in637txQV3FKAVfQV43Cyf3BtS6NC5MiRnp6+rUom/E9p3Fj1VxfBpORCsAyU4FsHXzs8ClIcU/eiKvyW/n2d/n2V/j1M/765Vx0fjDRiYtrbO+cZrmYxUyyAcLA2XTAc
LAEjyQ0KDTGhBgxjBhUFy2bLZsVQtBllZFjJ5/p3KFu2QJshpiNbMaxJr6xk0IG+B536uNGgYm7bBuIWMEsAbAb65HPce0TJKS+B3g8gXCyOleehPCfLfSbr2b8x60ZJK6WAqSqxlvCPmePIPnmYh8DIaZSh1khDh859roqEyV1e0ec0V+rhAhGgixWzaGg7U0CN2tHl
ZTCbKc2jXsEIURFeQeyeCw4SBgWDCEBqag6JLqFz2m/XHFdLpQSGTD/RNYD1crPXTauGZMsNOykndJumggHbllfWEGmeORKipH6JSA6b0788olCWm+BOLheXgeEWYzIbhfnCmtTQvsk7gwhVFUQH3soxqhUdQPWNbRka6VXH+ybTSuQlihIQcNdb0LFu0aerg2hFQ/Sr
4tGPEfoxyT966ccfazb97FV78ed/bP/1IHiiseI4uTqgNyLYs3XX06bdycgZQzgzrJTsYm3asDwc+76KgZ97514roRuEDux1jErWM+rey8BPqIZOAGYQS4sV3XUPmK6X1UvQxS3bs9yjWAG3ikKza55GkLLvelQT/AAo4G2DJhE/kB0xyI4xbc8YIXDlSBr8JwpbNjwE
BRRqZimteHVvk2M0S+F8AO3ABbQN9AUQJkHrYACUJvnNWHS00Lr7d/+L2xxVcYDEgQrHNRDRQLDE/0L01ulpDd01UNHfOzIIvruvTzFLSmZEea9Yc2CQugODSaN9yFoJXQ7bzIWogcQNAEdgx8zSeDD0FwziuVxFoQPgGQjB8N8BGewA/gMTf4z5UKRxm1RojuyIjSyX
I/foAzQoixCGsVNMmp5ZnNoHtGhMOUz34KhxbihBAMdUBnNr4qiE1zcnJrAmwJuBTxhTNI2+DgFIVi+4GrZMwURABdN6XcunlbAy6J8CL5PL5nI7UykGJMFmmEYFAkeu2jbM6LFLfz/KL/wPBNk4Ow9asX50FSKoMIrmyJcAyQwlHmkENcUzdqfewbJD8R/c8t9/kDBh
H8TZWi17XnWgr292djY7ux1ircm+fjCiPndmUpWmafi5r6JZEOynFd3zHDeuOqEdFR0DPKgwpYOj2sFRCFuhV2hOonMouSkUnCg0MMzaA99mASYYDNGogiK0aNI1UCOwq2dW96ERo1eVSQV3cNisasYMaGPZm64wqUFr/guBRXf/ZlbZV1HTbLQyGCZwg6K8q4tD2mYD
TFl7YgJG9bZZwpi23Fa83zAnyx6GomjP+f6gex1+sc6ZlhYqH4wIsJpA6Dugs7NKn9KfVnan0sqsaZXsWaaUMFF1BuoCgHOok2H3/4I6bANoB0kqc6Dju1MdrbZRi5ATrjdXMSCymkDTrOPsV62rg7FKz65C3VxQJ9tu2SwZKBf0KYiTuqU6mNnh1aWpyoKJ93VgxAyL
FKFAmAZhYSpQjnwwYKNeDXg4UbEhBqbPij2Jnfu44sDBfA64VwAPEbSt2rNaHqJM6A41ExQe9lGLALDrGQh5AvHmRVBIP/pF5Eg/dsKPnfAjn5M0l7puFdA6bHV/tCQWq25aGMN0wWvjtlXx+tEL4ao4cZYYfXPPod++i4vT3TmxzoQVzxREODpG6RAJkMdZWly/+aPy
Xq2aBouYtY5ELQ8HXhkWUNUBjL2xAXwES0pq9ZplkqfWKziqyAqx7s2aC8siAZlllgQ5aDFA/yJ0Mf+ECFBniNz1Lz+GUT9dvdK8eMd/9JF/6hzMq/7NrxsrNyPO0Lp+beW8vwiz6JnW7Q9hLsWV7N3v/dWj/u0z7Eol4j2nZgxGpRGLqCyaZXAFMkiLDMFcWk6kol7Z
Ko44eVRiqVVy9NlRbK+FTjvOtFHQEjG/wYBZwDwjNK/ebdyY9y+c98+f8d+/FQ4YwgnWFf/EYuPe/cb3xzApcuMBJ0uE3oKBUWgVkDoWER2tlcbRMSaJm2FYuEpHQNlaFaNjSZhYkgnqeEbs3xn2KwX9iBVtPaksE9XLvRM5a9UC2gAw81UsBjvGNBJIShaSWzYnPGZ/
TBqSJKTycKIr28TBrvMGIVDDme4Fag/87CBqCDNrIgISsN8OHT64t+05YD72Fo6YXTpAgvAWvNJ+aLtrR9ATOvwOJ0HhGiNkjlGqweIiCqCnYZ6R1hQhOizPorZWmf0USedT4dxhyWojxgACwDnqbXCOmkWrbGwd4qqYFszfxlxaKdoVG6LSCbNSkSMzVAhMf3WzL0tY
FveIJkUTdKNUH5RKcc7aDxRsh/81/NiF+hPxAVbiY0DJOFCKrEoFfUsYimkmLscO4HpMfV3FBV09XNLno2XgnFTI/Y+EIwE8sM6MRoOi59FyDWBRhzCjpZSGe2kxiWAP4NfbIYL9VJaLvn/fSyzjHsRDLO1V7KpeNL254d5cNr+jt29EIN0IVQDJsi2jF9OX9pTRAZeL
M7OoacO9+ezusAhF+QfbtIZ7HbtmlWLlRb0aFIekCO0iitjBKazLchylDkFAqcyYxuxeuw6DUXjobQzpVQQ9YV2vUqaIiYtEo6pjuIYzY+xxq0YR0yymLUY7ohJJ21gfVdRuXCKD2LRMhnQdfqLzT4VtatWoBTgyqV4dwqB4RE2Yuv2Fk82VvzSu/7N5+gf/6uO2WDvm
VF4FdlVBkZAlacWxZ900CNUT8TR+AXvoD3piWEMR7+JrTFY1zvcBAGGSaNoJjC6ZMwpFV8O9xnTVm+sdaVw5TrnEe41zd4f6oB7GFFuQoVrrM7pZkZ1S/+7AKYFoMbi3S0a7f9qVy4Vew63YnhhKlr6hvn8HhbpI2E7+PERhL30ejj73wmf/rkTfSHQx4wJ3upVxbWPQ
9OeQcJOEnVWG8PdH2cRZcl8Z7kR/DqUVsyy8CdFDf/Z2cbREQZKPdbr4WId8rCP52FzkY9EchsUqS8W1F+aShX2Ab0owkDQbx4DydloYxQAWOnYF08/m9KQqpsbQvU6ye53E+HQH/I38a+BHadTbkAvgS03k7CS4zR3C5wFZWb1aNazSyzDbljRBLdoNkUtaBrgnHbNE
ybNJymlj5lRVsDCTI0ergnDq+QHifFqZgy+YJ+r9A4E4QA5z8GsO6E9Fi3Yv4g9aQwyjXjddKKgPBNLcDSAGaBmyHawZ22d0q1i2HUyOwQDUMBxrt60wKUviDhnQMXgvFTi3QNlRTU1clEm6iYsLJ6a//eloYYcLDe4JKzmguj/SB6h+Vcd1xo5daYqMDugFo/IOFGTy
xq9pTYFYOvc7YK41ZbEW6yxXNAzGtRVnvW0QYe0ULBgbE14PaM3QVyY/nlbGQn+J5eI7Pz6egLQan6hndPQczlh1LDc+zote4iZn0UNWYDORWM/iWilH9rdVoQoxW2OSOxdM2ejxyrH1XlDTRTUdmA9kRamO4cBATYq4etWqY/3jEGsgBRmSU4ZWc/kUaY9sDOXQ2rBd
ZHBQ7gC4/khVj0gqWzYlpW2nRYVaNSAmE+lBiDtESVURTiAohsSbrpCjL4yg0WH62cl6pgfhMHg8J1tBxRHzV2FkqOCM/OvkB+RCQlV3KGTHFlT7UXstB+Wi3r+w0Lp7sxMAbSqxbyPaYHiYGNk3A3aFC3sDJiVNrUIk4Rk4fxiOKme7KdUYpm5E4mYwHOmG0DBh8IsB
qxg6QRPpii6+D0BIMjDdA7y6MsnjyZNThlMSrLwkacmaR4YDQwdRaQLKli1K93a4MnoxFZlbzDUUY4FxpbApj1msk8btZ6/Z7iunzVKpYkTuEjAW2jymULFOF1kpSMEyA6D4oc1Qk/YZ+RDD+rXPm7eXnhFQ7XEMXURTJMGfFU8xhNg6bZNB1fr8/PrVk2vL38B6vHnn
nH/0U//cd2urV/z5h83z93h53jXY6rL66x5ovRguBNsjnFwY4Ww60IIGPzsaEox7vjWnU8d/vXpHNOTlcNIikDCFZFGmXj4qiksJ0xbUBJZ8lszIPPXKALT/D7P+D4ZZtB++6ShrDIwCQhaMQBICkAmZxVLEA3q9VZmIRrdJnxi5xF0d45qIuO16sMhFXmtciOlgHnbk
OVMhCzocp0a2C5x4By032LtPaaD921iZgfZUV796JJ5yqb8xocmmJnNA08DMXDYF4DTCTrH3CbvPQfeZePdIR6XoTURoQXzGJyLioDADISV/uqV9hBUnRJMcwnbN0NBAE9M0WEPJHrk6mve7WBbSS4pQGpCSM1rIQOk4QMiVVNhONEts8HuVM18DQRpMpG/Q1WTz26GW
kyl0XsiiafY5SA1gc98IlsClyjkdUNx8dndUGGR10FApgaPG64p6NaoKyEIpk3BVr96eRwE2c5VTjydQqCduNBz/R+P7u43l5caNeeGNeau0m3+DWtd2MqIcPVmO3RgHx+jJcuzCQqZLDM4JqbczEndLg9kAwkanos9tJkYXTrQtMjdnE8JyAfU5w9XQRJAGpggTzHtR
AKY1+TLN/4egTpOctUOHGDRD2hXE7rRFh4ZOP4hWMNq3o34Tjl6UdxiDXW2cNYGnGsLlWAChgAXIsW4p3Jwk7dAI2Nb22CkTZJBFMBLN3gBgnHcdX6jG13Egm/gOsFrPI/vJ4EHBEur7N64X6oAL18CwolVBN/+LvlJaDoWLJZqwMGpJWiuJSghmJOf8TFUIFhvxUy4b
jQO9bbiTOthFyQXe5wq5G8vfNq5/9HT1SuPL463bC09XF/g8cHDOtGOPMaQZgtSS4YxyM01E3nSmNRscNQZ9JjrF8QV12vB0WHIXpwzyPJqbFT9oHlWtCZhWX6K/eqFiuHReKmjC7vi/HxDn3SzxETMGUOqyt4mjKaJU0T1mS/pc5KndrPcnPoTLEhMnZNSpqpmhA7fw
r6dXqCP9BsETQv7h1dPh+rdLf0qlRJ2f2V6kWDaFQa9UYvTBV0Qf/nhm75A67vqM1hJtidBDlmMHPKfsUmv6CvLTsXYTtu1BQw+1X+VcdOParcbDC6CB/sn3GzeWAzE9UzNUsyp+pES6X1Vad2/i8apP8bRV89vHzZt38ajV6eN8IB6Up7lyHdtc/sE/+7BNIfDgcYVC
DSgz8AAthdbQyV897i8vA4Wselw3EKoQ+jRRmhLH6oKffAoA7QSXc+4oWIeJ5wTj5aNGBT28u6F98uEAPOlPFwEUPve/sXEegDbBqQm2TOyFpxuCubBs6BWvjJY6kxXffKacShzo8u6UhRu1ePRNbPaLmM7ecFeUgYECiSOD8MGnPQ7izQEQKfYnAQQkALPtKRJsQS+1
WTSfWs54FLpLHZgpzasfNM7fhkU4BeUzkfB4dc7iI8jNb075j9/nYiG9riOo1mC0aipONV5gyHAN0k6cEic7VWQxhv6DPYJFjhdxkQyIt9kxdZjDY4nhJntgdgQdm6tCCC8BDDZw+OvVOVsazRrysWLRXjpxDCT96+hFNZgtYkjIDcRQ/GKghc+IgP9MumM+pmhbFvoY
b65q2BPAXCoQ3GVXQafI0VvEKwXULiAztVK1EywUbgw6aiCDR4eAw0pJx2GIyWlmB2sIBsenT/P48cIJ5b76wgs8/oUF/8cfWk+u8sWV1s2zYPv+539dn1/kSy58apSvrYAjEDpXAN3lM6JQALZArhj1bmw86Z4ESQL7jFlZPudpBXwPDeOPNcOZYx9lO3sqEDCPlXSY
YRF0BskfV5NAG7GdfxOpMipoZVKAE4eDqQtM5RA94dFNChmteMhIRwhNqztMrJRvSHBjFCJrUMIRXk0I3wrUpliNiT0FcpfrAjWNjnomIEIN2QwyaNYVGdY9Cxk5DcIjZywD9wTRrByvWuSS2iLahAagqZtCnE8aocCtbYxcUbqijuFOvJYSWMYdvB537dPG0WPrJ+74
Cyfx7Nz3x8BQ+vxzNxpLF/Fw2JXFIKJVQpPZcO6MT9jhaSCvYJfmNpr4SJupFapzUXdK7jObU6voFOmUQaEzh9LBhbDwWA4Z9HAYZfG1hBQ1lKxPTysFyctqBUqQ8FwivlG8GWgp1ehhTeAFRNIXwnz0DYQxKdtrdcn2WhJwSwaOSxp0S+HJzUlHt0obovHiaLwIZBx8
O3R2IHJIKvwSSaktxe85I0NeCZMpmC4b7s1v721L+beefNL49mbjs1PhjUL//QW8JOkW6kpr8TP/3KXGgxX/9I215aNry18N9XmlEfjHCU7KkLCfubHw3Fh4j0E6ixOkbqIEHWgITXsyF60sFuMNI40/yfHQB5hnGk0zaDIQfKFlBq0xcuaJj5Dh0UWyuca1m2CVraVb
TD9QjjdW/+eB0jh1NBwVbypirP5wCezWXz7hL/3IBru2fJ5teP3TC+ufX8MrtO9/x9McTpyXb3AtRH94JfTiCuG7ijdAPzvFLGEUGB5++hgv0gJNxz5cW36IV4N+pTBz157cbBxbAjh43/Tij40b873+xSW8g0Gde2X2uWXdMVBimpVWTOtlEKM8wbmsuokKGU5D2Ch2
cFkdooyvkH216PWOADeH+rA0EGdwsJx69wXWSFecUh0n1CjrQqkkWeOjPLU8PAqXGWhofR1gUQ1+JVHCe8xdwfPh3A4xkP7eAfcshHriESz7WvNf+XdPqGKHEWhRUXck/aAYm7whTjV8HxaxrK2cbly+xRH9/SVQIVq2qaxOeO2XV3abGln8KmTMFAu6QwfONJY2LQfp
vPtw70TFqA/ke8WOC81kxBg+ocY8wlNqI0Nm0IWTh1g9S5ihrs8cEYYreLAtQSG6AKYkdNETJ9RCdTmCoWWnY2NpTevVttgvEitdoeMcBMTBnvgFoW362aodMg+9nDQU9HwSP2muw2shveGZBStL10R4FMgJcpftMGSWFMtmVe6PgVTkhSR2JEAKCalNRyAktyi6b9xv
qBCSguFZrxIPYllQGE3zMcUwHA3as4EXRp4TD65QnhcXho6bxEfx2nPAxyhNgN4QbE0IKzw04pXCwHPjnriO7Ozr1TcppASsaEAShGeypJN272fQ7v1k2kUKQIaQSDt1i01TlLFJRhpGJEdSWdwS0lSRueiMT/5NzkP2FEhEb4ze9tqMZ4N4Yq6ByzdwMOwmu/ja53As
7S67gzbMQAviRppfL/mLf1O6u5tEqkYOv/ym8su5mmQcb73SgeMXcDObZRJ5nLgEf4ITyssR0yaw4lGJDs2KT1NFo1LJ0Omm3hEONTiyEDNUR9MZvVIzep/tcDakDQG5tUIbQ9AFRQvoTg9Ko1ZiXdj3RMvqTtcpsSqRqO7c4GjrJ3AjwYX9Utzwfgo3vE1yo61Ai0W9
IxDz4kUf8HAjSq4nutL7UpLiYTTZ6aGlawaEigPKDgqSHDUtKMKsfkrO+UvF4q548+sz/rnvWjdvrS2f5qy5v3C5R75lmbAfEWY93I2fBiBbdamXdBdWpzwQk8Jv2CgvhI8hBO8jSLW0rIxvHkkNuRzPhZklfgMjOt5lTkoJkc55Klrrsg8ZYIeCE8NgxNU/R8lbGEr2
XYSKux7wN5jrwnIqHQzL5Mny2WSoQ3aVSslS2L3xlGPyLmeX2Yt7jaiDcU3gVT6STRzfsoVIIthEP5ampDIuGYwpkBBP8FYFwOBe78XFJ7oqFVsvoaK8opuVOX6gI+ntg8a5u/7pG61HjxJuodODOeLxIwASvm5kOKbhBr+sAAcXxB8VQiKC3WeJu/jWkCo/eKWKh3hi
+9WpbFH34ulqhMG7d0Z22nBdfdJIBc9mxJCShSVi5CS2jA7bduKCzu3PceCZVsHO8JLzC8SkLDFIzn/LF4q62mQRvIyXob6ciAxBtalmeFIrfHSEnP1A8BKNgxvoAIHWngMK/UzThUyHtjT4SqaD2zxH2E2lIxhqY2kR5k9e9avRsZn+7TmR043fvSTxd7KA1aL99iQq
pFyfVgq14hTdMC5lxSeesNbipxLQQuSC8CwBpQx3hm6FjogMByZCj2txIuLFkkp+v70cn8FK8V4WPihD7qy9Tb6kRi3wtRNlIDqDEgiXDjc/Q7LBhkkpK07gxKVa7ZCqhzch6MQsbkDigRa84k5c4s1IPMYSFiXJkreo+FCzSicB3hlgLiWIUvIP7dIMzfonKjX2j3Q6
hPZv1Wv/3CWRYOyi4LkOBUffEXKk03mwqabD5+BYdfA74S2xUip4dIxtmlQe2w7KnoRmip/u6GLW2OleqZYIFg/PycqeTLJIgEcmi3QPxkw/yjTiVqAWqHewhclxwWDsWRQVbSFThmZAjfTg3ZhE0Hh4fCgK5hIMeDrZsvvLlHcEWYPEObsoe5idnAFs3v5QDaPAAanJ
yLCye9eOXE6Cwc/pKQNyAb+6x8vCp6sLFKUwYEygnhKT/fNItF39kowyKQII89NvOva06QIvDNeu4LQ3mKQK3VUXZnTxmiEioMArhu8Ih7obqHdo3m0qHo8/npMpbbHK2sMzays/dLtuUoC15qgxSW/YgNMpMOs22B4H3Z5UeI8cB4z9UIzJ2+SFwCwKCecFixWTDrHF
Qgfpss8kOkzcMZcujQQ3dqiu6+b9swmrIwPrCU+02JYqna5GwtvexOEGxYJWyCIuMNMxRDUeuyvHggh4q6JgYwOdiaJP8oHh+4AzqUHZlSZFowA6BEz61w1yuLtKZ6IkcxChQSppMzjWoA0b+Yxu2IRDQUyye2UQm1tcJelIuQOnkRC5B09T8SqgC9eklzC7z8HuDJ7O
ngJiyo6Bb+WwEzDqmNp6iV7LHKYFjFUE+G8deu1le7pqWwBAC57Q7OFjzBMVPJU7bTiDPeIto87hgecx/2Qk2EDsSTkJlnBR4e+NHpaLzZWxWajTx8hvhtBTGbvozTkcTduzABeW/NN31pbP82O9bW4FaHlNxJta8K4ZvmCXij0KJt4F4wd46f04//FXzfNLMC1IAJTG
0kXF2fMbBR/0vXDeX7zXuHrfX1xav3YDt0plVOEbM2mlf2c7MkDV+TLv0iK2nHYV+QlefFJn+XZPT2zx1e69kbPRQqld2WO2y+zrix4chjXj09Ur/TiX8hO/+G5ex3PH8bHFMVOgGWpv2SyVDFh9y/SA8Pr5xUDkMZ173iafJcnn4ugfn0h4R/mnEhFyjZUoFxLSvP4N
v8gsh9khNf7CAzrzdXX90hPR7Moix55c1Z0YMc93UBPF3pHf22QwlIrLtCc+EkHW1cfgBEJyw/HsovH8HHI39P7dhpPvsmjbeDHXMcxd0TDXb36/fu1zfpMXjK51+xgeNqA8G4uENSjy7J3ObcZ0zYJZMb25BC8urZs6mJFgYJJekVf6X+lqQDWnXAAA
_SBX_WEB_APP_JS

  # web/style.css (12305 bytes, sha256 ca853407a7b50008)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
DaL9Mm/zVXHMnFm25XcdIWzo/99MzOURMAAA
_SBX_WEB_STYLE_CSS
}

main "$@"
