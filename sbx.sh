#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="2.7.0"
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
  local host port token
  host=$(py_json get-host); [[ -z "$host" ]] && host="$(public_ip)"
  port=$(panel_get port); token=$(panel_get token)
  echo "http://$(host_for_uri "$host"):$port/?token=$token"
}

show_panel_info() {
  hr
  printf '%s流量面板%s\n' "$C_B" "$C_RESET"
  printf '  地址: %s%s%s\n' "$C_CYAN" "$(panel_url)" "$C_RESET"
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

  # src/panel.py (47118 bytes, sha256 6ac733870de98d8a)
  _sbx_unpack "$PANEL_PY" 0755 <<'_SBX_SRC_PANEL_PY'
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
_SBX_SRC_PANEL_PY

  # src/nodes_tool.py (16807 bytes, sha256 7a7ce74388ab57b4)
  _sbx_unpack "$APP_DIR/nodes_tool.py" 0755 <<'_SBX_SRC_NODES_TOOL_PY'
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
_SBX_SRC_NODES_TOOL_PY

  # web/index.html (2513 bytes, sha256 6e68607974abcec0)
  _sbx_unpack "$WEB_DIR/index.html" 0644 <<'_SBX_WEB_INDEX_HTML'
H4sIAAAAAAAC/61WzW4UORC+5ylMnzEtOHFw9wXOiMPuA7i7nbTB027Zngmzp5ADiAQSod0lCCnZhRVIq5WIVlpYUEBI+ygomZ+3wGW3e2YYmgS0h6Rd5fJXVV+Vy0POFTI3w5qh0vREukLggwSt1pLopxJfuRaBjtHCfnrMUJSXVGlmkqhvVvHlKKgr2mNJNOBsvZbK
RCiXlWGVNVvnhSmTgg14zrATzvOKG04F1jkVLLl4PpzCq9wkuRwwBbCGG8FSzas1nMlbaPTq9vTu7nT/2Wj/A4n95goRvLqJFBNJpM1QMF0yZp2Xiq02mgu51oAWNzlkshg2GTGFckG1TiJa19hropSUF7uc2h2Pw1S6YvOmvJpHANm6QogUfIB44enApeyxaN4OtEh6
U2usWW64bJF0zViBc6qKxqDBC9veGjsCbLS6plV68vJgtPd6uvHbeOcuiZ2KcBdB3RcazGJu/yzMAiQYlExJrKiZhegDGFDRtwc/bvyyfG7BsKZchTg+3nmIjt9sHb/7HZFsBt+vG6AsDdE15j9b8+3J+/cL5oVcr5YOzIKwGs+BrcEXCez3elQN8ZriixSmRNCMifSH
K9fR5MPBaOc5ib2GaKNkteZCuFlzbJu30k0IfmuZvYD249Xr6Pjdk8nh/lfRcL+oz4h4fLQ12nvuW68L08iCDu1/Q8VnqLpHhQjln5m2NeApWtqcYxz6pIHoim/8z+Hk5VMfH/rvX+SK2SJWsmAtd1A6NNnaHG++7crEujpbHmDYkQVsnZbDaW3TeeNgw00HmA2XUp/N
eOv1aOO2nQaXbKRMWCgXjDbU9LXjAHt1hKjiFLvck2i6cW+0/ac/7IHgbnrLtPOiGZoJOzsVtdkTJzhnzosTQe3mGzEKlmmg3C5BHP99dHKwPRP/OjzZ/aMV7X1o17ab2/XiVAFlDPBxcAWjdC4QECEbt4AvRNYxddwJIFa3Y2dO1VE2EJx+acQWlIvh0oztGLC+0F+p
8Ohwd+7+QYWd29al89YQHyDYrZyJUCJvfpbAIen/Le6TB7/6ujcJPN4ZH91ZatFTerOjKxcr9/0MkBheSXg9K9rmkUljZA9bjX8+s76V27ytGmemghcTFdRQx5F9KuBZDQ/J/c1m2ExfPJo+fWUfDgfRjbYA5dsnYD160WB5Gr8Ry1U0QO3tNFDhPgYoEtuz9hNYNZJq
01LppXALiM4Vrw3SKnc9cuGGuyFeC1DNT5rY/4D7BLGzCxzRCQAA
_SBX_WEB_INDEX_HTML

  # web/login.html (676 bytes, sha256 9576018569a151c7)
  _sbx_unpack "$WEB_DIR/login.html" 0644 <<'_SBX_WEB_LOGIN_HTML'
H4sIAAAAAAAC/3VSMW8TMRjd+yuM5x6nbgznWwprYWBh9Pm+5Kz67MP+LmnYGCoaURQkWlWIVpUKlZjSqXRoIBK/hVwuU/4CvuSCYOCW7/z5+fl97zl68Pjp7vMXz56QDHMVb0VNIYrrLqOvsmB3jzY94KkvOSAnIuPWATJaYid4RDdtzXNgtCehXxiLlAijEbSH9WWK
GUuhJwUEq8U2kVqi5CpwgitgO9tkcy7oSGTC9MA2xChRQTz/eD/7fkp+3hEndTdIzAGpbl8v3owWF1fVxTQK17CtSEm9TywoRh0OFLgMwAvJLHTazkPhXMMbtvMkJh34ksoeEYo7x6gyXak9hJCoY2xO/GiZSRntNkxcoDSa0XAF8JBsJ/6PJL+zhhRxfXNX//gwO7yu
x9PF2fjX/Zf58Hg5OZqNh/Xnw+rs2/zksjp6v5wcz0Y385OvJBImhdglB1G4+iP16Hz27rS6vJ6fv11OhlFYtORSFyUSHBTe+cLr7xub0jYJNPugKSkUF5AZlYJl9G8FfpwSjTB5oQA9XpTW+ryCPzyrK9qvgXaMKJ2392UpLaSEW8kDxZPG7n94W21JiWh0K86VSS6R
xvX0kzdiY9IasjI7bNxukvFZNKVNJly/yd/u4YiFpAIAAA==
_SBX_WEB_LOGIN_HTML

  # web/app.js (11851 bytes, sha256 5274c4bb408eb70c)
  _sbx_unpack "$WEB_DIR/app.js" 0644 <<'_SBX_WEB_APP_JS'
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
_SBX_WEB_APP_JS

  # web/style.css (5693 bytes, sha256 f5222591cad7d88b)
  _sbx_unpack "$WEB_DIR/style.css" 0644 <<'_SBX_WEB_STYLE_CSS'
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
_SBX_WEB_STYLE_CSS
}

main "$@"
