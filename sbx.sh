#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="2.5.2"
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

  # src/panel.py (47326 bytes, sha256 6546505f30b287a4)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
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
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (16807 bytes, sha256 7a7ce74388ab57b4)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (4606 bytes, sha256 8227c68756bf960a)
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

  # web/app.js (15090 bytes, sha256 af7c0d4671e0add2)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
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
_SBX_WEB_APP_JS

  # web/style.css (13605 bytes, sha256 4ccc3c7f73702d70)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
_SBX_WEB_STYLE_CSS
}

main "$@"
