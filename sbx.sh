#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="1.2.0"
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
    py_json links "$id" --host "$(py_json get-host)"
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

  local host
  host="$(public_ip)"
  py_json set-host "$host" >/dev/null 2>&1 || true

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

  # src/panel.py (41389 bytes, sha256 fc738d9d30491276)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
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
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (15512 bytes, sha256 9eccf7ec74267e4b)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (4572 bytes, sha256 866941fe048cccd4)
  _sbx_unpack "$WEB_DIR/index.html" 0644 <<'_SBX_WEB_INDEX_HTML'
H4sIAAAAAAAC/9VYS2/cNhC++1eoOkdZPw5uAq0ubq9tgRYFeqQkesWaFhcidx335KaFn4mdxA5cIEicvtAGBeK0cNPUduD/Ulja9al/oUNS0kryev0O0IvXM+R88+RwKPu9Dz6e+OyLTz40AjFNnSFb/hgUhY26+VVgTXxkSh5GPvxMY4EML0ARx6JutsSk9b6ZsUM0
jetmm+CZJouEaXgsFDiEbTPEF0Hdx23iYUsRNwwSEkEQtbiHKK6P3DAyOWuSiLrH2jiSwIIIih1OwoblsjtG8ufXRwtrR09/SJ4e2DW9OGRTEk4ZEaZ1k4tZinmAMagPIjyZcm56nEu0WuqFy/zZ1CccGR5FnNdNwZoukkoNw/ZJO2O7EQp9xQU+b6IwW/AZKCF+3QQY
KgJL0SgiyAqI7+MQEKMWNh27JqVSgGCk6kxnb6v78nswbURproHqqgnaMksGuWSIVC6Zlou8KQxW5gKoYTr/zG2UVJdFPMq8qVxAU2WR1BQdNBw5Q5BmRPQSx54gLA+FhyKfp85T5MpMJHN73V8WM3NRJIhHcXF/ulT2VC5oBNM53FtONn8GnM7GrxCnPDJ9ZdqIQqyV
e1NNYgnmo1n4KxBNvRokzFtubk0lya0myM8/NGy3gq0WJLBbSnCfKpkJJcT6cYh0qR9IwV67lgbvEqHs7GxDjV0slIjSawqkRL5UGCXAuwti/PJZsvn6aG6rs7pwrghGSOArD54EvVz0FMK7C193+W7n7t/J41fnLcCQ+ZifNX69LidF4ZQVm9pJHsGy7mfQ4443N5CU
9g9VlSq+JZtjz/dg1Em212TbUr0deudoP3s5bphGxODiMxsRgyyWWmf80wsIU/feN/GTnWJFuC0hwCwfQfsGzwBn3HTGDdgOyVNrAzePDeftnkHOx4bPLHlr2HRu9d1eimjv32JqYFYQ1kyEoFQVX+ZGM31E6KxZ2mimOToBjOIGDgvh1lkleVhnDHki7BpxDt8sH+4/
/3d/ETpesvhXRi5VSvw4gD4QGmKl+/ZtAUKTJYjedX2VJaR7TPLkj87uwcVKKEWAdrW5M7iQYLppYFkc06Xq6B48HBuOF+ePHm2dUiIpwGighEbjV1D+r88mM+IrmZHT6zAVGNcC42cWGBv2M1+uuHgpaeP/d+1W7oiAhOk8K12zNKmmZ5gYUdQgoUXxpLiNWoIdHy6v
5RzED77Vt0Y2tlzkKCSrj+LdtcM3q4PPAfdYE8tRW94ZxZOQ3FvSk+gpFZcDqFEJpPTQdf6iE8ilOC26TEjxVHLkbWgpsuiJ0A8bw+hxoiKptqRXMLybAkeSnd/34mcrPfK37XjtR0VWJTPTwtZ0NpfDOJJJ9l1dH7AaP1hUkTlNk47gSZqy1fUBq2fVpGe7KpB8dHlM
jn73nyfbG1UYoAtBlqulJNhCPjMLSZOkPJ5CPz9zMZnM/hWRy6YPrMzkHsu5ppN3/3E6r+lp5rvVzt586fxxTEFDz0JNV2+hpWTlhQZS72C156IdV6o5peNWYjDJmOi97yV17PksmYCcD+iFloa0bt625PeF7HtCDTVJDd/RXzhkw6UMkh5v78cLu8bEp5/DYKkt0crl
+1m/modydwRDXPTe6prS/YsLJFr5Sxr6MGSIUSJwnmibexFpCoNHXt1EzebNL1URaK7Uln7bqOlvOf8Bf+V2btwRAAA=
_SBX_WEB_INDEX_HTML

  # web/login.html (676 bytes, sha256 9576018569a151c7)
  _sbx_unpack "$WEB_DIR/login.html" 0644 <<'_SBX_WEB_LOGIN_HTML'
H4sIAAAAAAAC/3VSMW8TMRjd+yuM5x6nbgznWwprYWBh9Pm+5Kz67MP+LmnYGCoaURQkWlWIVpUKlZjSqXRoIBK/hVwuU/4CvuSCYOCW7/z5+fl97zl68Pjp7vMXz56QDHMVb0VNIYrrLqOvsmB3jzY94KkvOSAnIuPWATJaYid4RDdtzXNgtCehXxiLlAijEbSH9WWK
GUuhJwUEq8U2kVqi5CpwgitgO9tkcy7oSGTC9MA2xChRQTz/eD/7fkp+3hEndTdIzAGpbl8v3owWF1fVxTQK17CtSEm9TywoRh0OFLgMwAvJLHTazkPhXMMbtvMkJh34ksoeEYo7x6gyXak9hJCoY2xO/GiZSRntNkxcoDSa0XAF8JBsJ/6PJL+zhhRxfXNX//gwO7yu
x9PF2fjX/Zf58Hg5OZqNh/Xnw+rs2/zksjp6v5wcz0Y385OvJBImhdglB1G4+iP16Hz27rS6vJ6fv11OhlFYtORSFyUSHBTe+cLr7xub0jYJNPugKSkUF5AZlYJl9G8FfpwSjTB5oQA9XpTW+ryCPzyrK9qvgXaMKJ2392UpLaSEW8kDxZPG7n94W21JiWh0K86VSS6R
xvX0kzdiY9IasjI7bNxukvFZNKVNJly/yd/u4YiFpAIAAA==
_SBX_WEB_LOGIN_HTML

  # web/app.js (18092 bytes, sha256 b567253a4681dddd)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
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
_SBX_WEB_APP_JS

  # web/style.css (8131 bytes, sha256 47d777d7932e1a7c)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
_SBX_WEB_STYLE_CSS
}

main "$@"
