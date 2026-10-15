package nodes

import (
	"net"
	"strconv"
)

// clashFallbackPort 在无法探测空闲端口时的兜底。
const clashFallbackPort = 19090

// EnsureClashAPI 确保 sing-box 配置里有可用的 Clash API 段。
//
// 为什么需要它：SBX 要按「域名」统计流量，而 sing-box 只在连接表里同时暴露
// 「目标域名 + 字节数 + 属于哪个入站」。连接表通过 Clash API 读取。
//
// 安全：该接口具备控制能力（可切断连接、切换出站），因此**只监听回环地址**
// 并带随机密钥。sing-box 1.14 不支持 unix socket（实测确认），只能走本地 TCP。
//
// 若配置里已有 external_controller（用户自建，或上次由 SBX 生成），则原样沿用——
// 端口一变，采集端与 sing-box 之间就会出现一个无数据的窗口。
//
// 返回最终生效的监听地址与密钥，供诊断使用。
func EnsureClashAPI(cfg map[string]any) (addr, secret string) {
	exp, ok := cfg["experimental"].(map[string]any)
	if !ok {
		exp = map[string]any{}
		cfg["experimental"] = exp
	}
	ca, ok := exp["clash_api"].(map[string]any)
	if !ok {
		ca = map[string]any{}
		exp["clash_api"] = ca
	}
	if ctrl, ok := ca["external_controller"].(string); ok && ctrl != "" {
		s, _ := ca["secret"].(string)
		return ctrl, s
	}
	addr = "127.0.0.1:" + strconv.Itoa(pickFreePort())
	if h, err := GenerateHex(32); err == nil {
		secret = h
	}
	ca["external_controller"] = addr
	if secret != "" {
		ca["secret"] = secret
	}
	return addr, secret
}

// pickFreePort 让内核分配一个空闲端口。
//
// 这里存在极小的 TOCTOU 窗口（探测与 sing-box 真正监听之间），但比固定端口
// 撞上已有服务要好得多；真冲突时 sing-box 启动会失败，SBX 的提交链路会回滚。
func pickFreePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return clashFallbackPort
	}
	defer l.Close()
	if a, ok := l.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return clashFallbackPort
}
