#!/usr/bin/env bash
# =============================================================================
#  SBX — sing-box 节点搭建 + 精确流量统计面板
#  一键安装:  bash <(curl -fsSL <RAW_URL>)
#  管理菜单:  sbx
# =============================================================================
set -Eeuo pipefail

APP_NAME="SBX"
APP_VERSION="1.0.0"
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
  for u in "https://api64.ipify.org" "https://ifconfig.me/ip" "https://icanhazip.com"; do
    ip=$(curl -fsSL -m 8 "$u" 2>/dev/null | tr -d '[:space:]') || true
    [[ -n "$ip" ]] && { echo "$ip"; return 0; }
  done
  ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}') || true
  echo "${ip:-127.0.0.1}"
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
  printf '  探测: %s\n\n' "$(public_ip)"
  printf '输入用于分享链接的域名或 IP (回车用探测值): '
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
    printf '  节点: %s%s%s 个    sing-box: %s    面板: %s\n' \
      "$C_B" "$nnum" "$C_RESET" \
      "$(sb_running && echo "${C_GREEN}●${C_RESET}" || echo "${C_RED}●${C_RESET}")" \
      "$(panel_running && echo "${C_GREEN}●${C_RESET}" || echo "${C_RED}●${C_RESET}")"
    printf '  面板: %s%s%s\n\n' "$C_CYAN" "$(panel_url)" "$C_RESET"
    echo "  1) 添加节点"
    echo "  2) 删除节点"
    echo "  3) 查看节点与分享链接"
    echo "  4) 流量统计"
    echo "  5) 面板设置"
    echo "  6) 服务管理"
    echo "  7) 设置分享地址（域名/IP）"
    echo "  8) 卸载"
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
      8) uninstall_all ;;
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
    --uninstall) require_root; detect_platform; uninstall_all ;;
    --version) echo "$APP_NAME v$APP_VERSION"; exit 0 ;;
    -h|--help)
      cat <<EOF
$APP_NAME v$APP_VERSION — sing-box 节点 + 精确流量面板

安装:   bash <(curl -fsSL $RAW_URL)
菜单:   sbx
用法:   sbx [选项]

选项:
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

  # src/panel.py (38227 bytes, sha256 a1b01316c12c7403)
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
