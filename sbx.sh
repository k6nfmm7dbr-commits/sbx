#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="2.0.0"
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

  # src/panel.py (53874 bytes, sha256 a4a6c717d0d54de4)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
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
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (16807 bytes, sha256 7a7ce74388ab57b4)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (5536 bytes, sha256 9f5696993abb9020)
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

  # web/app.js (25165 bytes, sha256 31bf6080a1f46dee)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
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
5bElw/C/4xJ6iE1iAAA=
_SBX_WEB_APP_JS

  # web/style.css (13329 bytes, sha256 1e77025bc868263d)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
_SBX_WEB_STYLE_CSS
}

main "$@"
