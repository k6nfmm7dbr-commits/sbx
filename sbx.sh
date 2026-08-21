#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="1.1.0"
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

# 探测公网 IPv6（仅在用户主动要 v6 时使用）
public_ip6() {
  local ip=""
  for u in "https://api6.ipify.org" "https://ipv6.icanhazip.com"; do
    ip=$(curl -6 -fsSL -m 8 "$u" 2>/dev/null | tr -d '[:space:]') || true
    [[ "$ip" == *:* ]] && { echo "$ip"; return 0; }
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
  local last
  last=$(python3 -c "
import json
d=json.load(open('$NODES_JSON'))
print(d[-1]['id'] if d else '')
")
  [[ -n "$last" ]] && { hr; py_json links "$last" --host "$(py_json get-host)"; }
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

menu_show_links() {
  banner
  local host
  host=$(py_json get-host); [[ -z "$host" ]] && host="$(public_ip)"
  printf '%s节点分享链接%s  (地址: %s)\n' "$C_B" "$C_RESET" "$host"
  hr
  py_json links --host "$host"
  hr
  printf '%s订阅内容 (Base64，可保存为文件供客户端导入):%s\n' "$C_DIM" "$C_RESET"
  py_json links --sub --host "$host"
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
  printf '  当前: %s\n' "$(py_json get-host || echo '(未设置，自动探测)')"
  printf '  IPv4: %s\n' "$(public_ip)"
  local v6; v6=$(public_ip6)
  [[ -n "$v6" ]] && printf '  IPv6: %s\n' "$v6"
  echo
  printf '输入用于分享链接的域名或 IP (回车用 IPv4 探测值): '
  read -r h || true
  [[ -z "$h" ]] && h="$(public_ip)"
  py_json set-host "$h" >/dev/null
  ok "已设置为 $h"
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
    echo "  2) 删除节点"
    echo "  3) 查看节点与分享链接"
    echo "  4) 流量统计"
    echo "  5) 面板设置"
    echo "  6) 服务管理"
    echo "  7) 设置分享地址（域名/IP）"
    echo "  8) 检查更新 / 升级"
    echo "  9) 卸载"
    echo "  0) 退出"
    echo
    printf '请选择: '
    read -r c || true
    case "$c" in
      1) menu_add_node ;;
      2) menu_remove_node ;;
      3) menu_show_links ;;
      4) menu_traffic ;;
      5) menu_panel_settings ;;
      6) menu_service ;;
      7) menu_host ;;
      8) do_update; pause ;;
      9) uninstall_all ;;
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

  local host
  host="$(public_ip)"
  py_json set-host "$host" >/dev/null 2>&1 || true

  # 首次安装：自动建一个 VLESS Reality 节点，开箱即用
  local nnum
  nnum=$(python3 -c "
import json
try: print(len(json.load(open('$NODES_JSON'))))
except Exception: print(0)
")
  if [[ "$nnum" == "0" ]]; then
    info "创建默认 VLESS Reality 节点"
    local uuid kp priv pub sid port
    port=443
    port_busy 443 && port=$(pick_port)
    uuid=$(rand_uuid)
    kp=$("$SB_BIN" generate reality-keypair)
    priv=$(echo "$kp" | awk '/PrivateKey/{print $2}')
    pub=$(echo "$kp" | awk '/PublicKey/{print $2}')
    sid=$(rand_hex 8)
    py_json add vless --port "$port" --name "reality-$port" --uuid "$uuid" \
      --sni www.microsoft.com --flow xtls-rprx-vision \
      --private-key "$priv" --public-key "$pub" --short-id "$sid" >/dev/null
    if "$SB_BIN" check -c "$SB_CONF.candidate" >/dev/null 2>&1; then
      mv -f "$SB_CONF.candidate" "$SB_CONF"
      py_json commit >/dev/null
    else
      warn "默认节点配置校验失败，已跳过"
      py_json rollback >/dev/null 2>&1 || true
    fi
  fi

  start_all
  fw_apply

  banner
  ok "安装完成"
  hr
  printf '%s节点分享链接%s\n' "$C_B" "$C_RESET"
  py_json links --host "$host" || true
  show_panel_info
  printf '\n%s提示%s\n' "$C_B" "$C_RESET"
  printf '  · 随时运行 %ssbx%s 打开管理菜单\n' "$C_CYAN" "$C_RESET"
  printf '  · 面板需要令牌访问；如需仅本机访问，可在「面板设置」中切换\n'
  printf '  · 若服务器有云防火墙/安全组，请放行节点端口与面板端口 %s\n' "$(panel_get port)"
  echo
  if [[ ! -t 0 ]]; then
    printf '%s管道运行模式下不进入交互菜单，安装已完成。%s\n' "$C_DIM" "$C_RESET"
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

  # src/panel.py (38548 bytes, sha256 f390483c08178371)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
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
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (13574 bytes, sha256 9344220f88289225)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (4050 bytes, sha256 e8a0e6fc696f435b)
  _sbx_unpack "$WEB_DIR/index.html" 0644 <<'_SBX_WEB_INDEX_HTML'
H4sIAAAAAAAC/71X3W/cRBB/z19h/Fz3kvahVPL5JfAKSCAkHtf25rxk4z159y4NT6GgfLZJ26RqJdSmLSCokBqQCpReqv4vKOe7PPEvMLNr++zL5fLZvuQyM57ffO3M7rgffPTp9BdfffaxFak57k24+GNxEjfq9jeRM/2JjTxKQviZo4pYQUQSSVXdbqkZ50M7Z8dk
jtbtNqPzTZEo2wpErGgMn82zUEX1kLZZQB1NXLJYzBQj3JEB4bQ+dcnK9ZwZpuqBaNMEgRVTnHqSxQ3HFzes9M9vD5Y3Dx49Sx+9dWtGOOFyFs9aCeV1W6oFTmVEKZiPEjqTcS4HUiJaLYvCF+FCFhNNrIATKeu2Ek2foFHLckPWztl+QuJQc4EvmyTOBaEAIyys2wDD
VeRomiSMOBELQxoDYtKitufWUCsDiKaGg+l1dvovnoJrU9pyDUwPu2A8czDJFUfQODIdnwSzFLwsFEjD9v5d3K6YrqoEXASzhYKhqiqZKyZpNPEmoMyEGZGkgWKiSEVAklBmwXPiYyXSxU7/l5XcXZIoFnBa/j4TVSNFgUGwvf3OWvrgZ8Dpbf8KeSoyM1KnTTjkWoc3
22SOEiFZgL+K8Cyqccqy5RfeDBW51QT9pbuW6w9hawEC+5UCjzgl8zFCbB2GyESjQEr+urUseedIZe/lLpyxs6WScP6OEonI50ojAry/JHZfPE4f/HWwuNPbWD5VBhOi6IUnD0HPlz2N8P7S11+72bv5T3r/99MewFiEVJ40f4Mph6rQZeWhdlREIDbzDGbc4eEGmuj/
xLBRzXdwOA5ij6546e4mji0922F2Xhnlr6QN20oEXHx2IxFQxcro7P70HNLUv/Vd94eX5RPht5QCt0IC4xsiA5xrtnfNgs+heFo29uOrk8W4F1Dzq5Mn1rw+aXvXR35eyejg33Jp4K2gnPmEwFHVfKyNYYaE8QW78qGd1egIME4bNC6l21SVFWmdt7Aj3Brz9l+t7e89
+W9vBSZeuvJ3Tq4OHfHDAKYhDMR6/82bEoQhKxCD6/oij1B50oD9/tu7FlZrZeng3o52oHSqym0esTh7knDWpo4mDz0DTl0mxDqmShcb/Z3vzazIL6uz9FC6ca/7enP/1cb4NpKBaFJ8YOGkKHdHemvVvD+OaZACQF+QoGWu2tP3iSI+p1kBciXN05XAGehoshyJMs9Z
yxpwkjKpP8kGL7yWIw/J3h+d7uP1AfnbbnfzR00Oa+auxa25/DUGl1CuOVK6NUbavbOiM3OcJZPBoyzl0q0x0pNaMn02DIRP7UDghX/7Sbq7PQwDdCnJKK0UwVW4XJSKhiQ2jDJLR6GGxRx9Igrd7FmduzxgvavOu30/u6XNHfZwo9dZqk4bysHCwENDV1vvYHE1XX9u
gPT2o7856/RBM6ebPjNCqMFWh9ShpQmZgFw8y0rjkRjbsu3gVplvkTXSZDV6w+y1eEVwAUXv7u51l19b059/Cc8J44kxjluT2ZUminCUIFINNjRDmfklFVGtYn+CcQsVEpwpWhTalUHCmsqSSVC3SbN5+Wt9CAwXrWUbbc1s8P8D0SWiC9IPAAA=
_SBX_WEB_INDEX_HTML

  # web/login.html (676 bytes, sha256 9576018569a151c7)
  _sbx_unpack "$WEB_DIR/login.html" 0644 <<'_SBX_WEB_LOGIN_HTML'
H4sIAAAAAAAC/3VSMW8TMRjd+yuM5x6nbgznWwprYWBh9Pm+5Kz67MP+LmnYGCoaURQkWlWIVpUKlZjSqXRoIBK/hVwuU/4CvuSCYOCW7/z5+fl97zl68Pjp7vMXz56QDHMVb0VNIYrrLqOvsmB3jzY94KkvOSAnIuPWATJaYid4RDdtzXNgtCehXxiLlAijEbSH9WWK
GUuhJwUEq8U2kVqi5CpwgitgO9tkcy7oSGTC9MA2xChRQTz/eD/7fkp+3hEndTdIzAGpbl8v3owWF1fVxTQK17CtSEm9TywoRh0OFLgMwAvJLHTazkPhXMMbtvMkJh34ksoeEYo7x6gyXak9hJCoY2xO/GiZSRntNkxcoDSa0XAF8JBsJ/6PJL+zhhRxfXNX//gwO7yu
x9PF2fjX/Zf58Hg5OZqNh/Xnw+rs2/zksjp6v5wcz0Y385OvJBImhdglB1G4+iP16Hz27rS6vJ6fv11OhlFYtORSFyUSHBTe+cLr7xub0jYJNPugKSkUF5AZlYJl9G8FfpwSjTB5oQA9XpTW+ryCPzyrK9qvgXaMKJ2392UpLaSEW8kDxZPG7n94W21JiWh0K86VSS6R
xvX0kzdiY9IasjI7bNxukvFZNKVNJly/yd/u4YiFpAIAAA==
_SBX_WEB_LOGIN_HTML

  # web/app.js (16649 bytes, sha256 5360d7da748ae077)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
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
AZ2RDNsJQQAA
_SBX_WEB_APP_JS

  # web/style.css (8131 bytes, sha256 47d777d7932e1a7c)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
_SBX_WEB_STYLE_CSS
}

main "$@"
