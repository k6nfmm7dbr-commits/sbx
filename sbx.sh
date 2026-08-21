#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="1.4.0"
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
  "interval": 5,
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

  # src/panel.py (43468 bytes, sha256 027f049aad8933ba)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
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
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (16807 bytes, sha256 7a7ce74388ab57b4)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (4628 bytes, sha256 39f728ae2768d890)
  _sbx_unpack "$WEB_DIR/index.html" 0644 <<'_SBX_WEB_INDEX_HTML'
H4sIAAAAAAAC/9VYS08cRxC+8ysmc/Z4eRyIo9m5kFwTS7Ei5dgz0+x0aKZXM72LyYk4EU8bbINFJMvgPKzEimSciDgOYPFfImZ2OeUvpLp73iy7PC3lwlJVXV9VV1VXV4/5wcefTdz58vYnmsenqTVkih+NIr9R17/2jIlPdcHDyIWfacyR5ngoCDGv6y0+aXyop2wf
TeO63iZ4pskCrmsO8zn2YdkMcblXd3GbONiQxA2N+IQTRI3QQRTXR25oqZ4xSXjdYW0cCGBOOMVWSPyGYbO7WvznN8cLa8fPfoyfHZo1JRwyKfGntADTuh7yWYpDD2Mw7wV4MuHcdMJQoNWSXdjMnU32hAPNoSgM6zpnTRsJo5pmuqSdsu0A+a7kAj9sIj8VuAyMELeu
AwzlniFpFBBkeMR1sQ+IQQvrllkTWgmAN1LdTGd/u/vqB3BtRFqugemqC8ozQwS55IgwLpiGjZwpDF5mCqihW//MbZRMl1UcypypTEFRZZXEFRU0HFhDkGZElCjEDicsC4WDAjdMNk+RLTIRz+13f1lM3UUBJw7FxfWJqLxTIVAIunW0vxxvvgCczsavEKcsMj112ohC
rOX2pprE4MxFs/CXI5rsqp9y2LIzbypJbjVBf/6RZtoVbCkQwHYpwT2qZMYXEOsnIRJRL5CCv2YtCd4lQtnZ3YEau1goEaXXFEiBfKkwCoD3F8To1Va8+eZ4bruzunCuCAaI4ysPngC9XPQkwvsLX3f5Xufe3/GT1+ctQJ+5ODxr/PIuJ1ThBvJB9c7Eba17uBWvvtDy
Dnfa9kCsmhs0vJOdDjTFZoaqHki+ITplHghv1Ip31kQPk40eGuloL+dD3NC1gMEtqDcCBikt9dHo55cQs+79b6Onu8XysFucg1sugl4OzQRwxnVrXIPlkEkp67t4bDjr/QwKYGz4zJq3hnXrVs/lpYjm/xbzBIMDN2YCBHUr+SJRiukiQmf10kI9ydEpYBQ3sF8It8oq
ycI6o4njYdaIdfR2+ejg+b8Hi9D+4sW/UnKpUu8nAdTpUBAr3XfvChCKLEHkd/dVlpBqOPHTPzp7hxcroQQBetfmbv9CglGngUVxTJeqo3v4aGw4Wpw/frw9oEQSgFFPKo1Gr6H835xNZ8SVOiOD6zBRGFcK42dWGBt2071ccfFS0sb/79qtXBge8ZPhVmzNUKQcpWF8
REGD+AbFk/wj1OLs5KR5LecgevidukLSGeYiRyFefRztrR29Xe1/DkKHNbGYu6HllU5CfH9JjaUDKi4DkHMTaKkJ7PxFx5FNcVJ0qZLkyeSIq9GQZHEnXL1yNC3nBEVSLknuY3hEeZYgO7/vR1srOfnbTrT2kySrmqlrfmtaHCdxrcINNXipChyMMamRntL1PtLo4aIM
4iBLKtinWUql632kZ7WkZsIqkHisOUyMjA+exzsbVRigC/kQ0lK+TC6ep4X8ClKcZK6erZmayHvv4sl0k4dZ6nLOsq7pkD54ksx5avD5frWzP186qiGmYCH3UNHVC2spXnmpgOT7Wa65aHMWZgY050oMJhnj+XcBQZ14dgsmIGeDfaH7IWU7bBviu0T6HaKGmqSG76ov
I6I3UwZJj3YOooU9beLzL2AGVZ4o4+LdrV7bQ9l2OEMhz9/4ilKtLuSIt7IXOLRsyBCjhOMs0WboBKTJtTBw6jpqNm9+JYtAcYW15JtITX0D+g+z3dVLFBIAAA==
_SBX_WEB_INDEX_HTML

  # web/login.html (676 bytes, sha256 9576018569a151c7)
  _sbx_unpack "$WEB_DIR/login.html" 0644 <<'_SBX_WEB_LOGIN_HTML'
H4sIAAAAAAAC/3VSMW8TMRjd+yuM5x6nbgznWwprYWBh9Pm+5Kz67MP+LmnYGCoaURQkWlWIVpUKlZjSqXRoIBK/hVwuU/4CvuSCYOCW7/z5+fl97zl68Pjp7vMXz56QDHMVb0VNIYrrLqOvsmB3jzY94KkvOSAnIuPWATJaYid4RDdtzXNgtCehXxiLlAijEbSH9WWK
GUuhJwUEq8U2kVqi5CpwgitgO9tkcy7oSGTC9MA2xChRQTz/eD/7fkp+3hEndTdIzAGpbl8v3owWF1fVxTQK17CtSEm9TywoRh0OFLgMwAvJLHTazkPhXMMbtvMkJh34ksoeEYo7x6gyXak9hJCoY2xO/GiZSRntNkxcoDSa0XAF8JBsJ/6PJL+zhhRxfXNX//gwO7yu
x9PF2fjX/Zf58Hg5OZqNh/Xnw+rs2/zksjp6v5wcz0Y385OvJBImhdglB1G4+iP16Hz27rS6vJ6fv11OhlFYtORSFyUSHBTe+cLr7xub0jYJNPugKSkUF5AZlYJl9G8FfpwSjTB5oQA9XpTW+ryCPzyrK9qvgXaMKJ2392UpLaSEW8kDxZPG7n94W21JiWh0K86VSS6R
xvX0kzdiY9IasjI7bNxukvFZNKVNJly/yd/u4YiFpAIAAA==
_SBX_WEB_LOGIN_HTML

  # web/app.js (18390 bytes, sha256 6adb26b36ceae1ba)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
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
bed13RD3h4qc/f8Fgx55O9ZHAAA=
_SBX_WEB_APP_JS

  # web/style.css (8198 bytes, sha256 7cd9be59bdbc35d7)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
_SBX_WEB_STYLE_CSS
}

main "$@"
