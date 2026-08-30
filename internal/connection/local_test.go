package connection

import (
	"strings"
	"testing"
)

func TestLocalIPsIncludesLoopback(t *testing.T) {
	ips, err := LocalIPs()
	if err != nil {
		// iSH（iOS 用户态模拟）无 AF_NETLINK，net.InterfaceAddrs 直接失败。
		// 真实 Linux 内核不会走到这里；策略层对该错误已 fail-open（出站过滤降级）。
		if strings.Contains(err.Error(), "netlinkrib") ||
			strings.Contains(err.Error(), "address family not supported") {
			t.Skipf("当前环境不支持 netlink 查询接口地址: %v", err)
		}
		t.Fatal(err)
	}
	if !ips["127.0.0.1"] {
		t.Error("应含 IPv4 回环")
	}
	if !ips["::1"] {
		t.Error("应含 IPv6 回环")
	}
	if len(ips) < 2 {
		t.Errorf("本机地址集合过小: %v", ips)
	}
}
