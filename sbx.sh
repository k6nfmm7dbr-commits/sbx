#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="1.7.0"
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

  # src/panel.py (45830 bytes, sha256 762e7951e899ed53)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
H4sIAAAAAAAC/9V9a3cUR5Lod/2KnPJoqYZWS+I13mZkjwyyrWsQLJJ3Z1bW9ml1l1Bbra52VbWQDNwjsAUCA8JjwJiXAYPN2IOEXyAkZM65P2VH1d365L9wIyIzq7Ie3RIDPnevdwdVV2VGRkZmPDIyMvKV37VXbKt9uFBqN0oTrDzljJqlbS2vsLbNbSxn5gulQ2lW
cUbaXsU3LZqmtdjDk23lbMkosv+evshsKNE2bE6y+pkTtRNPqj8fXzs1V1v+sj5/e+3GneqNZ78+nXVPzlRvLdaWvqn98Evt9jx8ql56+OvT0y0tvKA795X7y8fpFsY2M/hSPTdfvXGvunRhdXGJlQxnpFB0DItxKLyy+8V9gFsacbLDRcNmpey4kQd0KyUsWJ29zApl
8al+6lt3/urqk9Nrn/0CTf769Cw0wxhvuHb14+qVBY557bsFxOP6fdZ7gLnfn4BvtetfuvM33QefQwlodHXxvHt2hlA/u7p4rnpmpXrr8T+mj8Pz6tOHtfnP/zF9gvqwdurU2rWTgGP17Gn32hK0v/b5T2tXL9YXlt25y14P3CeP3ONX3XOX6g8/ch/Pu7Mnaz8tuGdu
uSe/cGfusf5/21twjDShy1jeKDpZ1sVyFYu1sWLWdpjyn1598JW7uJiIKRz9T69+NV39+RN39qGPyspf1649al87dc5dXkoy99yPVPeP1BCH6l67VX1w1x9vLHthAbrPh9n7CYPC6t987M5+Aa/qKytrH60Qga66C0/rp35aXbyDrc5exgp3z3EMJOGsSUC6ev2ce+Y2
ILW6fN6dv1OdfQxDU734CPCFMeGjwfQuVrt4H76tLp5ZfXqLo+gEqrtzn7q/THsQYut+AggmWlpWl2dWf7lR//kyO0AMwKq3TrmnTrpLnyFmOOlbCuNl03LYcNY2dm6Xv3ImTLhJp1gYlm9Gx7M5+fy+bZbks2nLJ8uQT/ZoxSkUvV8fFGG4t3k/K8Nly8wZtlfRnvIe
nVHLyCJvei8K4x7YilUEhFLlrGUbLSOWOc7yWcfAEkyUkL+TVI8mC3/80CyJKqOOU07ZhjUB/CRqvQFdf3tg4MBB44OKYTtvZ0v5omEl2YBEBj/2U5WWlu4DBzJ7eg/CeJh2CiRLwTJLqUOGo2v9b/wZv2hJprUbTq4dxImWaNm9v+/NzIHugbcb1MDvUAU+lbPOaOp9
s1DSRRsAiMRRCumtJRItfW8OZAa639jbA7A0AJ9xrOzISCGntfQeGMjsfru7ty/T24cfEXJvn/p+/7sD8gM8ai393fsO7O3JvNPTcyDT3wNY7OmH7zthrm7b2dHBtjD8F/97haFwWz4JvA9SYfXZjdqlL2pLX0NR9+Fc9fNHKAbvflm7uMx2AKO3b+vAf7fCh/ad8A9b
m/6ydv5U9doPtaVnJBn39LzZ/e7eAeo4tHiEJriWH9bSjYggepmCMokkL14y84adAflpNK5GZQTtZLURJwNTe6RJpREnRSVkFZC3GTtnwZ/GlaRMTtmjXr3DxnDGMs0mtaCEV3o4mxszSnkorGUrjqmJ18WC7RglfNuRov+TH3DewutXO17tEG8cc4yXlEUKqDImskV4
t1WW+RALdNuFbHv/aLZ0aDRbgNLHWlpa8sYIK5qH9M3ZBJfL4/YhnC5M41jbjqVPJtiIabFJViixLJdLwLkp28kblpU6bAGT69pgqz3EWu33ShprZTqyHhSwRvBB11r/0tY63taaZ61vp1v3pVv7of/YUiICbaRYsUf1hIdZNk/jpgvs8BnQyxdyjq7OJyEtrSmpXBg7
XHBGmVk2SrrHiUB7C1jOKHEzoEsjM0BLsKzNRvyasqFUpYxyRceZlEJU9BGBsDGZM8oOexOmYZ/pvAlaOt9jWablw0Caamsz52or86Ar3AdXQA0n2erKM5DTa8tX6vN33emnaUDGQy4AuYf+FEBuA2pGLNj6N19Vb15w735f/+keAjI4AMtwKlaJ8FeJSDyh41tBSVAB
Pp+k2eARrQCzsDOpoeWhpbVUKqUlNWeqjD8mYI7b8JPPvu3bt/FHGz5t7YD/2rbhv1CgWCiNicrHhlDLNBkWRGZQZeih5xgfGBg0B9ShiaWfX0sQZnCI3hRGWAEUv+1kSzlDR2hJmlUJv4JoAv9wqU2oAn6DQ7wts+LAdwEQOaSEHILlfSDBdkqiEQaahiHBsUIp2DOA
msqWgUB5vRQYUvgAIwp27Ev7D4CBpHbPLrF2eLhXvf7lywXfkhn4z8zu7t1vo946cqyF2rvFnA+JtGiOgoKZn679uAzGK5g9aNCQceken6t9s+xeu7k2PV2/jYZx/dkXq0vXAFUA4N6dW12+SxrobKD8/Fl35n7twenV5ceocbD1N7v37n2je/c7vsLJohS0pRQEUZoU
73JgJh36AOad+rJiVcY/wGI75RsslRkzRTEFJthJ2Ypa18kWykagCTQ1s8A5RqCYOTZlwot/VYHZhlkp0kvxYhgQHjOBudgf5KsxszgGhIRXO1I75Mt8ZZi6tV1Aqzg5+NUBnw+NO/Tkyf2M82GhNGKGpEL92UUgvFhNgFFJEwSMRi61VhcfuNdW4CXY/0x/d2D3llcT
MAy1p0vuw0/lyMJnXKScuQ/11QHCVcazG/WF4/XH9927f3NnT+Eq5f6PUAAakOICBRAMF6HFGQ/UV4IBf4U0WAI1TKGsJyRHY02QBocNS08gZ+la0cyBIoTOg4pxjHEtEZEHfWggKvWxnjdtI6W9L4NYlnO+8yFg64EJCDsyPNEERTpLq/M/4Xcv/PZKEQD5VkfA68my
cahgGanxrJMb1S3tv/TX0zAUR9/aN5B4XR/c0jaU0N/LH+lMbj2WgE/p1/EXPCde/z3QAhtIYvXehCqmxoNiyC4cKkErnfQpdcgyK2W9M8G6wDbYojGjaBusrTNQg7ohbW6d6m/2zXF91KxYdhcYJ7oEtzWBVkChVHGM4IdtNNodiYSPIDYYRJDgQYsql9N0UWdBIlAF
usJrFWCBbTo0aEGg0X6EO0D/huBGkfN0NWcfEFrVHy9xja1zcRfgGKADF3awImTEVGkxUInnRO9VgVpoomKVD1VlAr+4ECiZhzPEJaoYoDYC4kGyCHwB4gUJJ0DKNVgKQOoBzRX44nworTvHzGenMmhhKk2LKiG0YmxJLfEbaENy1cAi+SXrwX4YiH3daFeDjNt9sKd7
oIfx9Vzvm6xv/wDr+XNv/0A/GzdAeupEhzE20PPnAXbgYO++7oN/Ye/0/IUL9Al635LY1QyO8BsBbYHyAiAJN/5fPGR0jGSGp4AZWW/fQM9bPQcJZN+7e/cyYWuzDqVoecyx2bpFuQ2dz2SdJkXX6U4+WyhOiW7AlPH7ICHxpuwc2JXxn6xJ3vP1sHU2WM6a5N1fH97G
yimDwcAanUryziTWIYxjOtmiLSgT6H5kdP/nUmCdPtrZ8TK6PXknccpFkVPHv+Hob6znzzVSjh0zUL19e3r+HOpEIT+ZER3JQBf298luAQioSf44kon5YVzvlgxY3ypCEb0I6PykFVN+WOOGh2mDBTBm5AuWrUtXA/xARtfxB2gVY7IAnGqOdQ1YFaFLAAiAEr65lGwM
y3OPGSwzuraBvViwzWIWzY5M0Zgwil0o8z0IKQvk80g255jWlALtoHm4hbuO/qN7L6vP/w39wye/QLfw3KdgxXPHavXLe/WFO2yL9CqTcxgM9tWlv8Jaee3Ksnv75q9PrwlI1S8vgOVfvXxqdfkRV53oePr6RPXsifrKCi4iyPNZuzbvrlxCKIvnqhcXqmePIw5gca7d
+AJMV9StoGHJhoWljvvs8+r92+7TOe6nFctS3/mZsivlsgWLXl12bX/ZsIgc2SIt9ZNep/eAAkcnKr1WDEwkkzFp5CroHjlwsPst0ADvg5IGCJlxWEp2AX6aT9Fw0eGKPZXxBwTX15HC3D+lc/USXv+/fPXoOde529+9cL723cJLVpXIBFallMmN5/Wsdcj2J2XnDmmd
qCZ2Gaef51lOQVVRLZctAyWMDFQtQ21kAB+W+JsI2zDlFH/Iwfgk4Zft5HE9njfwhS78EeimMMrFbM5AN1ZZuq4aF9qgz0jg0LkVV3gIIGeOj6OnAG3VEayQZq02+tewh4MdQypchQYDvHM9k+WCZeRj4G8X8AUVPNljOCAKMsIlqcqf4eB6TDot+aKM/JYJaXFLQ3GY
L8BKI47GvavkKI1ZgFGRUC10uiru1bha3jdZle89pA6PFmBNREDVWrkky8D/47pJTK5BiRv6WokaHN5QYFVk5XDJ0xG07uMwDza/ccwDsFpacmBagX094rzBaZxWl8SiEN8Pg/V7plAqOJmMbhvFkSRTxou0IbxMCX8p9wbKeri5QXWU0t7Cnx2RxiM2mmY6WYQwzUGJ
J46BWK7fvu95NGF1Uz1zje01zbFKmWazXMRLosP0Ai1kWXGEb3tfJb9o1vaGgmZAycCP3v5HZHB+FxkceK/1mcAPuVGGbkWco6AVDdJV5HBDdHDi5k2DrwNJS6qf+Kh3Rtd0VrYAC1+lvzpyvnBDEFDcLCT0YWVro79HCy7hOISD0FdgPg6C6giRyt25gs0V2D6QgPSj
ITVzqicUbAFVsK3jUG6G0v/qBzsl4GaWeKly0+a+Pc/lAUQAtThOrlAzJzynkv3JeRrytEN9rMBLilmgRRbuOE656ICgyi6UKkaIP+1B2TJ6s1HcJXANrKOXQXyheQ2fOtALobwvI+M54ktguiEKADqOeOqMUKbAaBYnGPOmdkTfADiVL8vZghXmTGH7eezsUZRvaAUw
pLKoMaQ5SDPb5tZgCG8uERux5gj8i7WGggPB9x9oT53RrPUsgtv30WWhmWOaIja5nyh2GqtE4IXDtHkTljaGJxJ7hdSMlYtBkbq3t6+HO8hAgZZBBKCL7D17s/5efkviPXuL/Jva/Hr7e5vxgz08mR7sbvvPbNuHHW3/mkmn2oawxOb3Nrejm+FFBW4GJW6GvGJUcbhQ
ylqwxhvJjsPCNqAiItwEWh9ecVqoPJYbzRZov0FXt4CTLLDxGzPmcQJZ4qO1HabBL01M7oUHamIowomxUrchL8oeoP0V+mCxIsh37APaWHYZzGl8YeuJKOxxyQI4uMLxiYWj/jHBqONRGA1xJDYbw8UcCAW0OLO4B6p6JDulkAj6Lj1/ZQTcK8wom6CBqrdO1ecfunPf
rl2f5jrTPfejO7fAA27cuROri+erZ0+LqcDqx6+6F2bj+gQ4ARNlLcfGpYqOUQBpaiKtJeK7ilIQapHc60Bp9nwEGTNwZae12n/iQl8HWN6UjZLPMiaI52yST1A5KVqNlkXEoAAhhvXAkoXFICc9/e7E32RwhMUvt4LXE8CtNp+7tqeDAX8+ydeTwFHLKMKT5lgsQypM
TWypGIHAUxPbca2A73YqL3dqiUQ6TtMFrEmB+QYZLmIfiE7I3WziIl8iKWjHDBX1NcK6wqZQaL4B3ETPzLF1h8+PdCPtsvbZL57B+RI1qBLf8ZJ1qD0qlCfuyZXLxSmtiRaNdva30qNIIPQXxS3w5GaYt+7gHxGFuFUhbQOhqcARC+lmsWPw27kf3AvnWNtrTCfXW1JY
92DbJlj1yi334ce/gUuCYs8y6tKIb9R526bcHhlx0sIYGJ7MlP5YyL+WKbB25ZcpftlTtvcFn/luoJwMaXyfphrpwp8mtvOCaSiYNv80sZPKiuWaJMIma3LT0U3O5KYERYt6+5ESNfRQwSylzTFStbr2J5honQnpRwjvKUKDg5n0kF4ic+kotJ2g34MFcyiB+4gI0d96
Darc8OYqd812efuwMbuK8EVMJwq0SHOt4+tbdZ6KPuuaNRkAtY2DKkhAzqQmN5v4AJLGjBs+Ed957eTq4uXV5a+42qZhEHX+WHpNjALXuvgb5iDYBB69X5TUBJhojBQPklh0PGSTUN95X4nUvKt2KVu2R02HY67Ln6KzqKvGUEPJ9/6oIdYROo0FpKPReO9UoGi0hCfA
SxcEXjTyr09jgo1fMvfz5cdus1gkF4LuRaqmeJioIGs+a4yTZ5205UaXDJUy+sxSXimkeBcsgkVjimaKrC58SxO14/X79WdfrJ06Cyql/uxm9fw9IA0Pt2b8d235U4zDmL0GJdz5j9BvvvSsdv8TGT7OQdWnz6Ii0xMMLdUHF3iZ6ulv6rfP/mP6BAdVvfhkdXEJCqx9
fbn699s8qE2FCXhUP3pYnf6GNyd97Go31JAJ773QMfAtqqeCJW3HLGdGimSn+wPSM2EAf4SK0l6lgZYF7b/GfDTHcE9G1Zz0jRsTuHGJn/wRNUo2+pUBq7ChgW5A2b/ILnmo98pGjze0EeNGfmkReyE4I1ntzKPq9HG+s0JvfNQo1I82fSNWrHnYVmwgf7ehv2dvz+4B
HhTi7wAn/R3eNw/u3xfcUNYSqREDRFe2WNQjKB+xBrnTZSjNdHj2YWJ8n3yBkMEqImFkoTBC/I4pRMad8AzaaZxvxpL4OlspOnwfKtCxZv2a4PjTxvp/vN1zsIeNdb0OklUfSyZEN9AKjpqV5uFBbUIbIiMMmiAZK1BQ0EQPPTCuU8iNCUwpGAO3BYV0hdUk/CJ5GnS3
es/unZtrp+bcmXv1n76GdY/75RJw0eqzG2Dxws/q4kztwkmMdJu5v/bR/erFX5AzL8DqcXp18dvVpU/cM7eB31QeW11eXl25hJEns8vuT3+rLn209tEKLEHxxMK1m+7sQzxhsDhdX5kHjsWHUz/xOYWBXMs3+NkG/zDD7OXq0zl4UJtQ8ccd+S4lmCNmOuPJC3+Q1Nf+
gL3R81ZvH+vdt69nT2/3QI/WxOOJs0bafuRnJJqn0IsY6zwQ9km2NKXnUxPZYgV9DInndBCoyMbW1Hr7+nsODuAO8n4eskA7+RxRazLpTCbFpnlSbIon2L937323p19/Pen9X4Jp8dD391Gs8N7e3QM+3ATbs5+9e2AP7jv39wwwa7LLmtwC67NiJW/kU9Aqcya7HOUV
YNGoBYFdl/irguFoy838LidcQrwQIeiR4zl+RAOM2CBabUP04MgH0YL3WgqIKLzECw4MD5nQn2tUNjQm/z+Nx/+MofBDIFQmCZN/Q8SXIP4Z+jcikhfcESXUusQISLY9oIoAnzhF2qjSOMqqlqbUC0DSG+rvpB+AFaFsTM8HQTEy9Mjhv51DqL24lsbX6qpBStuhIIyI
t5i7QZsGXAaIpXYRtTbgM+EhvomgbWo8KWInx1h4Vkx0ecM/gbYAai2uoZOJDY6jjEriNgWYSX9kZFbAUxuLOWLVFPDu/fv29Q5ojTftgjRrGKvilU+sQ+SD+3m8bNzeZMDWxNXVyjxfbwVtTc/kCUa8SDWvWsmKJreFX52iQSmeU6GLnFoShrD/U+SVTai+9eqZa+7T
E+7iYu3i97D6wwigy0/cux+DpeTTkCZeV8PVcIvqus54pQl5z/rU6L1CJlxtoYPAnbvinr0MFTzHQf3Od9ylV392Cgyp6unp6vXTnssKLLDqlYXqZTzwqYSHG/ZoxqrARDKw13qEWcgRqSCofGo8/7GOP6VxswZ/+VBUklOAKLq3kVeD+JDJy+mhrCv8utzqCvrGLayZ
GS2I9VTAQc7jznXh5ydFGyNPgjOXvBF5zy0R9b/FOc95pQ26y8M+RNlWPuixHU6yIoUaIRG8MPP4PQ6MY5kikVAcTjcYIzWYiiZObWUe/Unuymfu6XPu9FPcJqKFQaQbecAlj7j4lIzz98tx2NLFOjcQrR6Aiuex4fd4dhL7R8tAeFMOdtMuEqsK4xvaE8sjaV0cQZ3J
j5448kEaF/K19+tYhITKiHQxAhXBGTHgmhl7mR9uWEDYMFSqvAFi8HrOeoCdEOCg/yCwOGy2LGziklCGtqkzI8y86eh+g39EPujqZK02TrxWQMd9/EP17GkQUnwNWlv+prb8gJ8Eoh1AX4RI1IPhON6ka9Y8a82DDP02dFBfNs7nfP32vfr8Q2zTh6nsw6OHKux6EYde
yXc0qXfy3drwlo88GZtkO9TYjsOjGCdE+24B51KqYGeg+bBQit1ho5o01Il19spiInAajK9HI2//y4vAiVQvkR9E0avxq+DDwMgh79ZrbFtH/Fo44gYrifje2IK+uqZNuFgyNAtDiicCKbJE/OEa4QcmS0A5AhvvKUwdzhYcXU6B3+LwCIU1/wZ7Tx9kuDcBJnISvS1y
XaK6wtD1iO8iHj/VKRagjvSQ4do8dh3MDV1+9oKbubzV19n+g3t6DrI3/kKunz09/bvZ3l4wYNECboldZwLOypKpmftwkI52WyHP4NBgOt3WyTdPmvVL7RPrf3efbsFyEhd9+IxLS2dSvhdLfW8pzUuIt5ICvoDV4okBHX+nh23iG1ab2FsH9797QBJmI1TSaTgFbSJ0
2RBN5BwRjo2cZ/3HEErSp4n3g486B6Y1wgidvAQDvbyx2B3z8LLQaqSpi4I4CauXUt48rM5d6UysXp+uP/t0dXG6Ov8zP2GKB4Kf/OjeOMWTSYhg1HY7IX2Raxev4q7G9ftQF/QWT1VRO/GkPk35bK4scHuK+0LxMMDxZ+7MOV6MZ7BZm75Vnb2ADT1bWfvuCvy7unJe
nj6of3JPBQwqCoz76nU8geDtQbnHr1c/PckT8Yizs39bqt++X7u7JFJgXP+29uU9ibHsbGEiqKqaaylONKghHoDUWHcrntOYYJvZ9kSL7wyPG/F93X/Goyasu5+NB1awWsQLXoQBs1Hakw983PeB47rCe0ergw51+1fUw/jP0AqP0vvQx9d4l3cItLdFw6PFYmIjXC6k
SzyfN1imv8ZeZ919e2jF3gXPHsfy6axwpsC4TZA8KbqwDq8GOENYwJawUds9UNwgtoSJKd8fi7JQf8/B3p7+zMHuvrd6+r0z7K/w6Kw00/Eg9dLXtW/+mmTV24/gbwJTWGEWK7FdcONe/dS3HhnUhC7eYWz2F1Zf+YlnrOK7ATi9ab2q1RfuAVyVBzWY6XIuAx7AVtWP
59A+OX5Vc+/+rd399L7GS1Lel6trJ75yv7+5unSeTZjFyrgBsKsLc5htgPJqVb/8vnbmkXtNZLICKMCNXv3q53fw6Cw08OS0OzdLGxi33LvfeDhVz5xxz11aXTnnnv8EKrJ33mi3PU7bMQ5EZvoOmGg7O5CZkv7qy52/SQkEru5gQDW+mQJyg/33yb9CYQaPHMa2DgSi
b+sQQDo9KK/Ac7Ru51al8tZRQmCryK2TBAiiNlQG0p1c++uXTarv5NV3etU7XxX1X2Hbmlb3pa9tWAWe+EPKXwsTa2C3QtKX5wxzL8xinjMaAHdpzp39HE9b0Rl+LpKrt2aqt57i4a1rj9zb37kzM3xC8PmGycCU+QZiUI5Gbfmz6s3rxBPZ0iGKyR6uYKQ1PpVNEBm4
Dhw8ouFfGN/6zyjHk3zlx9NcCb7hP44lWSqVGpKuBxDyte8WmMXdGdYkMBUHjy4aPolPzqjzWOdg2pFnJI6in9RDzhtSPG1m1fvfApcItXTiiUiktvBkbeUCLGpCakakB7t2Syik+9O1y3Ogk4BI1bN3QV0hn/38WKoZgv/D7er10yLZ3NWP8WTF/M9AYNbhwcPJfhxP
0tW+PQ7/4mb87AWtevY4rNJAdUG/NNRjx09hP+ceRjTOCI49iWqQMAHJokjhEqX/wfnBA3fKWZg5HikDtQahMLfJ+OIj1rHH9/N1LNEuxyQB/eVPvHaG/7B9jYjNKuVbhK/KQhwQYhtA9Gq1sU4F4obVB+jD9tcTm19HnTi8MTWi2IO+RukSKqWpQTisqhaOqSRrkves
kWIZzU6Qhw6pCxpjWBtK8J11viMhlEj8VnqZaCoy1Axz95XFic7XvMOoBQ01pJZvkUBRbJcMkuGolwvgyhw1nGGRgELZTUr15kwqTp1hdJEoYy7VpZAGaZx4vkhIe6TxRQO0KcXacKVQzGfsyvh41qJlEY+swX9E4IxqXlLaHuhQOA8SP0vIT1B3BQ1ojiLqe/oSNGG9
U5glPsUqJTqKUbJ1Ai7hhrfF1TbxnZiiR3zKR23qgP+UBnfd5dy6pr26hgE8eDQEoZRMNF6icTn7oWGZiPNzefZavFHI4Fkvf0JmDx3KSDpRhxF8QvnkkD8n9MlLtETUVmKpZIihEj1YGsQkS0P+/oM3Ikh9mt3CiPThU/NUDqdD4zIU84tTRC0SoYzCAjkukEq8gkAN
qzi5MhTFGQvVKnn5Q6nqEc/juuDYY+ou2dngElyk82Il5TgUHVQTLyjBF73g+a0CdAttc4psYIGqdLQqVEzkqBPF6FdcETtYxo6FxROKyXL0K64YDSqUOzIGcoefQECJ5Qc74tgdi1RyKEseVsputBIOOYqr0GsaWHifG6ThHEoGfP3uzFN3/kn182/A6AC1HlM1wyeB
Vz2uCJ8aUAQflCLHgnLCQz3oXvO4bXCM/NUKmWLKAWVEOYUyLTJbXyYo3jxmkuG9Kq/w4pyVFZ6KKyqVgq9lwWyALocNC7/rGh92Lr78t5TvMJo/isP4EJkAd7j98krKxAbrcKWwn7JR9/SN5/zkKaRGfE0kI8eiQfQqzFEjW3RGsSvDpln04TJ5/NtvyfeOqgDoRRAl
xY0aRSjAQjwmjxs5MTD4ZkQURocKgifHS/uiSh0OwZ3eFAx840zoTTvlG58iGZW5vcm3AX716/t87s3GjdRHXvdrh+QAincwQfQJsbpXgyS4XhA7mki5scDZLdJQWiIixBSQzguDjHRkDJipJGcYgVPHT0gh0VmJBcoiQmNCGB+gvWQAHSIhy0QhgYwKQyOxtQ40KiOg
Bc29YmHCWNfW8xazK8sYU0mLfMyUfOIJhorPfVub/5wv1YRDcYuIjvZSYNfuPOQWUjuXVTI8evWXa3yJiQutO5jL5H9vhY+n6yvz9YU7nmcRs3ueOgerMNaeLRfahYnKqp9crs7c4ejUF5a9/NDKyZFmRuoLWaKCIWkREc5V+U+YUP8PzB5Yh3iLjYCl00gbh2KBoro5
XufG61mBDyXyfkF+91N6vyCX/7Pa8qWpmQ1rmYZKZqM65p/V0hsQeUHxHrN8/Z8tHuPU7oThi86XvL2JudBFKvrf5ISNyL6uxydlF/Kd52/PwP9sHqujeXc3tHemOjQ1SbQXJOVPKfHSCyQomocy44ZtZw/Jo/kj406Sbcb8OoEDpLYdDIt7fM+deRw6fGGjhJLHfDB9
0LCZn4JnXCd10UHQQo4ySLVjro5deETZsg2nS+YLMiYdKxs+3hDMH8wh2o4VikjA9xi6A39SlD3ZS0MUPjcDOGYswy6bJRsVSd6IKwBiIg8joe3GGMOS0zbAV3rUk42U32uUDjl4CBb9HkWjRHirARcxVbO5UaMNAVgmJWwtmW22g5lym9X6c5uKYtt+Ci2weXW7VBhR
02OoYlYnWuOS4MixRHzkWaQxrBvCxf9q64mYU0A8e9PvYJa+3dO9R9tgFOcbFmZ0P1AoGyLR2G5+Tgi6dhCjYcJ5xgIoHcaUNyInO1FdOayC005MUHP4fT5Lu7Z2dIRzWPCZzOcwpZXJV8bLtk51RFhn1s4VCl10shhG2ShnQZaalt2la0mkPuootWXKUR89WE/6Kxyk
w/PZ82Q6SopfH1i24oyaVuFDQ3LbByqvUnUvkJM3HEkjQK9jTy8GzrofAnlaCgZaYeMSuhh6jna3wIo4nPso1FaxXkCVv2FkLQO6GBpI2SSWH/xDeiiMOX2Pr6J/YEcoOKiBupDHTpuDyZnmWMGI7dtu+hTqlHKQ1Ya+0ElW/fX0fx3d9Z69OUGXVCAeXfrgf+0a2pJA
+UFgIoF+MZk6ZJfUiz9SldIHFROmtXL+NTyX8K4SSvoCtM3kC4dAf+gEK8mHXJ1GpEoo8byYRvzEPhdy6caHj/wE9uKIvzUcm6FeSSE/IkKYw/Hd+/tjkicoLLi9YzuJMpH3gjJjgbRoLxezhYgCiUsJEMfXW3EnkSe8FwJd1Wz1xwu1i98HNVvezKD8CvMvwYRvb/UM
qPwp3kRyMIhA3uCQWkV64DKAMiwo8UsVxzuQnOepGCwuDbR2IR/afcb8wA4D5yHDH2DmBoIA5oQl5aHcD+NtAIe3c9P4Qy1uMEhwHtEo+zoKiGOxtI4Cplx27XQvTKGUNyZTo854UWuQ8YNaUmRbQKqFx9GbuoErRjxJOujfQoLLGjBzCiXReLLpuQnhiMBJhsWbz7GY
ecYnN+rWQFi4EMwbFVIy5Q3UoFh2VZTTmzg+h+/JYNEY+hFug1q/4bQJoUZZaDRfXLXau9gBoGhX+y62LzvZ1n0IdOSOf8VrLnaxtx2nvL9UnNrF+rPjRj8o2a692UltfZLy/1qDE5SLM2g2FD8eG1/qi50NDbk635rLqGZyaj1ZFRItO1C0IB7QlI07szL9znMIrgaT
Kl6ANZ6pwpx+LkYFEz31PlmP7bYzBWZUzrbD3EoiE02CgD2fncjyVDJhNLwm0FYUmh/bSLD3onOTp6zAHkHDYUgtLywGOB5FX4QmIiZ9AyJR4juqrVow5OHSEr+h9mp5bhkZkNfSWaFVSn4d7ViSbe/obN7eRo7oR7g0qE8U95/WiGkIz+BmtteQ8HKK32IBG7l3INom
+gA20iC5U1+4NXKVanGGzxRFc8Bs3LZzR1INdPQ0AJaRCmBbB1cBiZgcVNIr6VXku+QB1YHPsYeiAhOCWkw3jKdOHFu/wzyeKqbHPHDGw5GHNXi9G4/RcCH0grFaysgA5A2MhHAENSeA9BYpXmZ/Zm+g98Yk39uN9n6dYPO4AO0NRiqoYdS8TlTfxkaUKxcqoetbU9sU
p2Qd+RBqXxuKhbIFwSSVm9GswbEhZf/ISo0ZU5haIBSp3UxnkjbT3isJqJi7lW2hF0lPGUxEtFszi+OI54fZU7DLpl2gFSleMOc42dzoOHzZRZlxKdkM+s/kPXvQknZsI1ebxItZX9STjN3+XEloY0Hywx8ADSwLxZNTKMFIT0UubEvliqZteJfH5cbzXEWqic5UX6Cf
00fdTJFuaVJ1YjoJJ2Qk/47/3ofqPfNAlnzeCibPFlf7Jf2r/RLhCHBisiRd8icFotCA+np+EjKPsVEUiiLUT9c6t/6B2uok50y6U9XadJKm/uBr99MzacYT9Neufepe+Dsm+Llxs3r979XrS+71h+6N6dXFi9Xr39bnf8HDitR+UiQbWV1aWvtopQ7/T2mM+ZkxfpuK
FnM3H97FSUlBoxdt6oh8UlI2kDpdmKChyaOOmDzqQ4e2WkIX8eAVPLxnrNVOt+bpsl+R21JQLMlJh+fejUQIgLhNd24BeokHAe585567Vbt4H8P+Fx4TsZB2nDqriw+q136qnrvDa+F54NnL7swsHhZ4cKf6+Tf1Z9dq9z+JpjPkRzWJPimeUirD8xvZaiJGfvqJ2gNU
3AsL7pn7VCnd3s77xnhyfljOMJEuix93gBe2Lz9jex4f1JCM23ORszM6uClhnJqWMeGlhRfj+I4xNWxmrXwvgrEqZSfkaY/l8fWHWWnXykhRoJC24+Xvieze2/sbnPcarYxnS7o8xoOW6AgobEcv8VuxvC3cSqkg+PsN5Ot3CvRnH//zFv8zwP8cgD/BXYXssA1NsD+y
zo6t25mEhpoey8bnu29NbR2RHAOsjzWUzVrW3kXAFPFr4qaFKn0bSV315GTZQmkoD/fNn8Ug54BMHzUPB0V6yFj34SLTNAwW9S80ES1ylsEjlngYie5D9H7gLV7y8KVuD3pBQMAvNpkW4sn5ECy9ANQ2DY8OvBp419rW+arNWju3hv8h8BoPxcdxW10+A4j898lP1R+f
4Y/aTwsgYsUX+eMzuTfs7fDb4oZNbUhhso0gETzIV5JJvQbTnTuHQGGJOTooYnuG+KZ47IeYhClKIdwCja0tPlDtF6Wov1/KzzdosiW7UQfsxh1QCsQhbzdAPt5A8VYiI3wp0tW5ff1p7feZOrld+YdPID55ac7QZeL8Ca8GxydBA2Wq4OkumCyRhVEibtLEt9kSWg54
TMGpQm8CdKI3TuQNP83P/K9NyUfHwwX5Imdim1Awelw2cGhOzfQScwCWcsbQSkIxEhoCECHmLwAheIqtEYigqY7DKrQDX6QnRawdObi8w39RIzoWAy74nOhQKFNDNfrwMP/iTO1vSxqMpa4xvdVOIAS+lpfU90IRf4PT2GtXfqgd/8698wXHiSc9qF38ErTJb5YemMKY
KfYqHI5G50LdFTws5p69/I/p40ig8391l+b4M7Ck++QRJtDkVubZJbxfdVAvmsAchQQ/6yRjxsT1VthaF97FTE92V/AqZvkSC7yK/8R+7YRfafzC8eRoYBqbuVvttWuLayeWOSruL7PuzI9rF+d/fXrVy1jN9SU/geaenal/cs/9dAUviL1wlmuw1cW71Ru3OekDKQWj
8W922cj5AWq02skbagC5su8ERWVAGC4QseQgLyVVr5U9LFIHBMHY0X1l6bnLHg62QIv9Sf/ic0vm8rW0weR79tAWICJUosCcSXEyHX0+djDMTl6FgiCb3N+KCX/119ODbSL5L7+pFStH9q7HIywbTGpTNKN3F/i6qxD6uJUIUjQDzQCI16BoOItHklcfLSThh1q+E08R
QSX4d+eOHdt20CKUXkIF+TKc9h1pJYdRzvOEQseUbXrr73HDOkS7hwplBT4UNIflA9atqIB4cLz4i8G2ziF+u0Dobh//My7WlcK4MkLnZaC+gmu8j4SXbtA7cajfqYAE1icT/hTjtYa8i1nzIFEs0zFh6ROQKiU6ihec3XhPkje3Q1dxYXlSBBiLJmIto3EYg1BsKICh
Wn6IJLQXLFu98kvt7hKsfvHA4bm5+vw8CDf3+5t4uvfsaWhtmK79cOdvrn0xwxfdmGv06a36L5+535+gw75XASBjA7sPsNqPy/CTLplvn8B4rHbHMt/PltqzpSkHVCf7P48Ze3cPFdT/7d3e3QkoPorB3VYhu7XdqRRyvIw7d7Z6axY+2qPZvHnYNnNjdgum7DofIife
KA5yiB9B/vzR6uISXkP99T1+QaAEg3cJgjDjJ6pFfuW5b92V793PztHRaKICdaH+7Er1ygIh6RFJQyG3e39fX+bAwf0D+/3z2xp1FSPtOYlxh476HXzFiRB8xymivuMQPWrQJxwxDgJIE3yjUMaHIuaEfzU4Wkzxky869fwjRDFzj2aeQoPItFO+DYr59wrjKRNq3y+7
Nz/5lbLW1i+sYO4gnMr8DA36U2iYArcaE0/Fcc5vcR2BHzl+tb6wzNrx/r12wLD9CBA1Cf/bmQSy4v92HmPuyZnqrUWGhIc+0KX2Z4FVBG9wrU8shBmaSHu+ZDMFZinSeXfmzd69lEtA13yMxSwIvNgJq26Yz40r0ZxiwRdYCVvq6cf72nr73+7Zg7ulHZ2aOAoVaIIB
RzG1KM9DXLt1XF5Rj6X5RdDCukKXeJKNGUY5bGPRRWXhUQCUjuk7E6x6+VT1+t+9FB3V5dv1Rz8SGJ1SuuGF6OMJhievr//dvf5QjMjs58oR9vptmG9P3adzaWYXGUcKvWbAuFjbe7YdNNdapNqEYu3wGcjgnjvuzjyoP7vmzj7qPZBWf/L2Ah2i/oZ0nrgvCYkQuDBp
sDM9lA5kl3v8Y/3ZKUDevfuTGoRDELGSsGaCep+CmizME/JHtn1dOwM6RmTDOE8eqOPYpB3501bvaVsg/kxLa9IzTTDWaUeYPWKo+DitUyWyNUuklOoYzR9qOGWJ2xnS/HYGRL1zZyKybfHvGIMdE/0QaFdeIIotycmLk0KsPY287iNOly1exyEnt/rajZuYPeHaI56r
QMoIWBaA4nIXnmDB5aXad5+soqC5AboLOIxy0dxsBwaFJ9EIz8Tgnfcvl/mBeWg43NWOoZS4nVTXOpTAPkS/ca3OITl+ZBCLmye0jsBNlqJZjHrVUCl4IL3dCGpQXGgrKMXPrgxPEZPrxPS4Q2WHOR23F+5ehfUG53PGrwVGtqaLs6oLc/BN5WC0T2hRwuX0EYSf5kdl
jklCiXSVIl4J+YzuAAJcfUSeKyCwYawN8u3zxgNGZveIICoiuAH56Jv+wIlYZohseIdvVOMLOgS3RWw9yEjKgjeNo+eK4ha37Aip3UI+7R3tAUY7GjjeI18cOyZFKhSEUQlogKsfk0ElmICGbXZ1eYYbimARkLWJqWIufA3LSvLiKilpoCE0GGBFHeQYzm/AacBmUBxb
QVst0gpZmeu2Ap1e+Kl65XzUDHW/XMJcN2ikooKn4xLY8PRVd3HRPXfJY+67V+uPF2A5zHeQZBaTa7eqD+6CDMAEoHPnqlduadXrp7kIkCjzLimYa9jQlQVuYOD+1DfH0ZFACUr4YttvABOJLE6781drD05LPBkY5FBhbXq5/sunnlHjORvIn8PzCAkbJkbFDu4cCq/o
4ZNMBBti76AlgomVxofzWSaEe5q0SRcLWREJObwNgAYtlSjQGGGsBK3alXGCSxM8SbNfzWtccgLXgwSXnw18PwF2TbIcFiW2a3ZNAV+slvnSucEFBSUKRMiFDWl4L3KfGHal6AQFWtxpP7KQRdRGwNgP3O4KoAbFebuhQLoKHilKjK775Et6406eESogJCk25l1VFD77
ygVEAJAcaw4IC6wD6FhL8F44uqpDEWGIWawYcy+AEX6eH9JHYz4s0KCixxfebU+e+pDn8EpYNHAqDN4k5WGusAyV00CuuA4ZpUxpRPqyqYxIsqq6tGWaarRhREbomBTrnDqRVDxkK1IozCu/a6/YVrs9XCi14zWueN1rcLJprzCMA7eHJxky/pn73HUqNrs/eVY9/Yn7
+F7t6WWQGZG6/PJbvLtZOI69+5uTgawIT06AGEJxPTNbuz3PrzzB201OnnNnb619cddP2EcXDVXP3Fu7+EWorbxRNByDNWmyGXKgq9SyQ8K/za/rJVL5N4C15inzKk9/q3mXuGnKD1NrfsDWg7xFgi61Igj/nK0Ahq9N5bUPNudNJ4CjRHHg2EoLV1Paok6yYwgrpwg8
FFeU25Y7z4KR+nzZQc7VopkQcqmLvHnc4mvNt3FqeK6tWJfcUOTu8yOIDiGDZOMhT3zF0eL3EK/uBEAmBnSXrQIALYyApZoZO0x5uumvXRkZKZDXzN954mHchRK/OLxcwRiabRReVSiMyIug8yK4RiuosfC8qklVNPijVjb9yrasbMrKiaZDQHf0yllGvxJNipPWxPBi
sDhx5JACWBtpYBWcKdaa3wUDVyzkplg2hxbjLhoDn1KJBuA3eQEfNp4G0MR4bILakrTrISanEyW4kPOdsxofi+BJu7jZH/CRB7RmbBr3sFu3aRp30rKoGmS7AYUWBRLfSSAP//9Ib5EhW+11Iv0pRTa06k/TMJslfT4XVGtG92OaIri9L/Kt5CkvdpCKyehBRa3IDZvf
UrfYo1KxoE7BG1BbXoIyoYr3qz9eSmMDsh8peKZ7VY/mikbWCtbo7eviszJwLbZaYv+7A5Ei8C5QJgiTmqHbchPAyuonYKw3urTfd2qbQm+137+hsbbDrG0P6+078O4Aa3sf3vX2aWzra+15Y6K9VCkWm1QChLxa8Lyxam9Gm9jlf/3zcyDwZkyzIVBNsYJJ2oSeNHjP
R095mLZtgmPhtws4/EsnO3rUC/OKVJWjRzUbdrlPkGdD9Ol7nmHpFVOg05sETYqKge/0h75h4W6Jcds40ceAlUFbm3xSbggHXb3JM13WBVZAbQ+tH+wZePdgX9PWiQRt5sYrrIMuXnBb0DbQZFMIprapuRnWZP0Wvn9E7Es3M4XSQVPohdSSPaqo7BjKlem2hjYyYuix
ISFAYSEpWzairaiXnmqKO4LREC0+HAIte2NomS+CFsh+Mp6DAiYkYdimHN7Bq/3+SGe6jaTNMVw8bgpWYVyLJJgnjzwVsyvwTlzZznbt0sIQSLgkFBnjQ1DfNYIA5NycYEZu1GSa1HS/7whoN/bav2zdxYzJgsO2Qv1QHww7m9OG4u0BezRkDFDEVMW3BCImQNMsRC9k
H5h2Cu90BbvI1rsPHMjs6T2IR/QKlPqlCwObZaJz6UzlB9hgdYrL5hFaGR3GY32Y3qKAOYzFwbGQq3VEJD9ovJ5OxLek3Af/z7TVxMjySZAbHTfzse11mH/YsSMYjfr4B24mqQFMFI/ajv/o4hoV2ipMMnmXSz6h2qitLIaMcc3jzg/3SwRxlnc64ytlEtH8VGNwpS8E
Q9yfLrmzj3jiY8zl//fbGBRw5gwGvfMgrKsfiy5dWHGXPhOJv0+eE7dnURnP+3hZdNxd/JpfniVmHL+gGx0EMg9/feGkO/sd3W7qX9DlXji/unweU1UrN9rwPONQEt6vLt6hq02PwzOPT4r4MVV/fyh4WQ1bjl70EhPWvs5BGHGrCdIBc5TxSwfufl//6Z7uzi24z1Zq
l+4llJtOggydELePy0uU45IOyn15rxRm+xpRj3RZIHIz0IJFmb4qpQw0otMMwoUveopYeE4NBa8ByuHmT0fMHUDoanKXLuJt0dSpJHMf3qgvXPLvvdeoYRmcFVREfsc0WV5rEoMj2Aja5KJXZSPBW5dmtViCeODXpQoudGIZitu42kYp48fUKeRpSo3AeRGlvx6kZv1V
TkRIliZlo7L0c877mLsKvVmP8Z6jFadQTB0eLeRGaVACEUjhWcadehSyip3hDh1KFe+76ryRk2efSZvYMdI1tqXGI8fV7lBAGIeJ+c0yuSdn3fMn3bkf1MjX1Wc3ape+AKHWmNR41A2vkggJ0LVvYTl8nO+xeDc/1W7PY5558ouuLl5E7+idm2uXnknpNNz4svTgqQbo
7XDKvzIvMLojoJVxw2BY3Zdc9+IqCZ/PVRa9rIqifsUif2aWXyJPTgDOG7giaXD+CfGk68rBnjTy+ph/xJKjWhiJuxBwLHhMwFMI4RvHvHMcpPWwKQyM8vyR/A3FM1U/v6UFD+DRx3Q81hRe3eFl9+tQkrkR2v7V07w42kh2EeMatgnb1oDRy4cHgsB2xoLlFUJwRfe3
4eUUMJtEt0kdyp7zeHsBuE0gHqLeqW+rX02vTV+tPzulDC7uLy4s86huOg4orizQeMIOAviapARfISGbiOsRbvHCqIC/v1l98JW7uEicEmEV4pXxLIyGH8s/Ig1TfBa0wURtSJkpOwWPExgGIxUjJlGCr0gvPO6EBTk+dELMuxKBSnbJt0oMMIBEo1sPHSNVzmDRCWlZ
H486KdWDp5+ixfEQU6i4f64pWjx8zj58dASHjHrbOZSQkTyUxA4Go5P3u3N7DFw6PxGCq56pEDAbgPRPUAWBVoK6M85KCVTg8qAB7RVbM6YqF9YNqio6LW4IhBhuPOiqmI47YyEPPoiFG6UhTJWn2CDNl6M4/kdxVI/yMySDfUNHiQ5HlfXdUdnMUaI71MWTEUNDat47
idVWjJeBHmRI3mUy1I1MBvkkkxHd4EzT8n8BTA7wCwazAAA=
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (16807 bytes, sha256 7a7ce74388ab57b4)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (5296 bytes, sha256 bf22540ebc6d8946)
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

  # web/app.js (22164 bytes, sha256 99719805cb682b62)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
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
_SBX_WEB_APP_JS

  # web/style.css (11874 bytes, sha256 ebc989b9bf9ec7c2)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
_SBX_WEB_STYLE_CSS
}

main "$@"
