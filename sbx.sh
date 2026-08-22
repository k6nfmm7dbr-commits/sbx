#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="1.5.0"
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

  # src/panel.py (44708 bytes, sha256 b0490108aeed96db)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
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
HCeZjGgGHzQt/w1fhbISpK4AAA==
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (16807 bytes, sha256 7a7ce74388ab57b4)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (5301 bytes, sha256 f4df3a70dc77729f)
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

  # web/app.js (23294 bytes, sha256 9ae92e10854823c3)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
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
_SBX_WEB_APP_JS

  # web/style.css (12218 bytes, sha256 b683782eb5f28b8b)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
bfldIoQN/R+dyl/Vui8AAA==
_SBX_WEB_STYLE_CSS
}

main "$@"
