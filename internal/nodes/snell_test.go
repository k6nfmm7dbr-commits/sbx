package nodes

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- Snell v5 / v6 字段严格隔离 -------------------------------------------

func TestSnellBuildInboundV5(t *testing.T) {
	n := Node{"id": json.Number("1"), "type": "snell", "port": json.Number("12345"),
		"version": json.Number("5"), "psk": "testpsk", "obfs_mode": "none"}
	inbound, err := BuildInbound(n)
	if err != nil {
		t.Fatal(err)
	}
	if inbound["type"] != "snell" || inbound["version"] != int64(5) {
		t.Fatalf("type/version 异常: %+v", inbound)
	}
	if inbound["obfs_mode"] != "none" {
		t.Errorf("v5 必须有 obfs_mode=none: %+v", inbound)
	}
	if _, has := inbound["mode"]; has {
		t.Errorf("v5 绝不能出现 mode: %+v", inbound)
	}
}

func TestSnellBuildInboundV6(t *testing.T) {
	n := Node{"id": json.Number("1"), "type": "snell", "port": json.Number("12345"),
		"version": json.Number("6"), "psk": "testpsk", "mode": "default"}
	inbound, err := BuildInbound(n)
	if err != nil {
		t.Fatal(err)
	}
	if inbound["version"] != int64(6) {
		t.Fatalf("version 异常: %+v", inbound)
	}
	if inbound["mode"] != "default" {
		t.Errorf("v6 必须有 mode=default: %+v", inbound)
	}
	if _, has := inbound["obfs_mode"]; has {
		t.Errorf("v6 绝不能出现 obfs_mode: %+v", inbound)
	}
}

func TestSnellBuildInboundDefaults(t *testing.T) {
	// 缺 obfs_mode/mode 时自动补默认
	v5 := Node{"id": json.Number("1"), "type": "snell", "port": json.Number("1"),
		"version": json.Number("5"), "psk": "p"}
	if inbound, _ := BuildInbound(v5); inbound["obfs_mode"] != "none" {
		t.Errorf("v5 缺 obfs_mode 应补 none: %+v", inbound)
	}
	v6 := Node{"id": json.Number("1"), "type": "snell", "port": json.Number("1"),
		"version": json.Number("6"), "psk": "p"}
	if inbound, _ := BuildInbound(v6); inbound["mode"] != "default" {
		t.Errorf("v6 缺 mode 应补 default: %+v", inbound)
	}
}

// ---- PSK 生成 -------------------------------------------------------------

func TestSnellPSKLength(t *testing.T) {
	// 用 crypto/rand 生成 32 bytes hex（64 字符），满足 v6 的 12~255 bytes
	for i := 0; i < 5; i++ {
		psk, err := GenerateHex(32)
		if err != nil {
			t.Fatal(err)
		}
		if len([]byte(psk)) < 12 || len([]byte(psk)) > 255 {
			t.Errorf("PSK 长度 %d 不满足 12~255", len([]byte(psk)))
		}
		if len(psk) != 64 {
			t.Errorf("32 bytes hex 应为 64 字符, got %d", len(psk))
		}
	}
}

// ---- CLI add --------------------------------------------------------------

func TestSnellCLIAddVersionValidation(t *testing.T) {
	cli, _, _ := newTestCLI(t)
	// 非法 version
	if rc := cli.run([]string{"add", "snell", "--port", "12345", "--version", "7", "--psk", "x"}); rc == exitOK {
		t.Errorf("version=7 应拒绝, out=%s", cli.out())
	}
	if rc := cli.run([]string{"add", "snell", "--port", "12345", "--version", "5"}); rc == exitOK {
		t.Errorf("缺 psk 应拒绝, out=%s", cli.out())
	}
	// 合法 v5
	if rc := cli.run([]string{"add", "snell", "--port", "12345", "--version", "5", "--psk", "0123456789abcdef0123456789abcdef", "--obfs-mode", "none"}); rc != exitOK {
		t.Fatalf("v5 add 失败: %s", cli.err())
	}
}

// ---- 展示 / 分享 -----------------------------------------------------------

func TestSnellDisplayType(t *testing.T) {
	if got := DisplayType(Node{"type": "snell", "version": json.Number("5")}); got != "Snell v5" {
		t.Errorf("v5 DisplayType=%q", got)
	}
	if got := DisplayType(Node{"type": "snell", "version": json.Number("6")}); got != "Snell v6" {
		t.Errorf("v6 DisplayType=%q", got)
	}
	if got := DisplayType(Node{"type": "vless"}); got != "vless" {
		t.Errorf("其它协议 DisplayType 应原样: %q", got)
	}
}

func TestSnellLinkFor(t *testing.T) {
	s := &Store{AppDir: t.TempDir()}
	v5 := Node{"id": json.Number("1"), "type": "snell", "port": json.Number("12345"),
		"version": json.Number("5"), "psk": "psk123", "name": "my-snell"}
	link := s.LinkFor(v5, "1.2.3.4", "")
	if !strings.HasPrefix(link, "snell://psk123@1.2.3.4:12345#") {
		t.Errorf("Snell v5 分享链接异常: %q", link)
	}
	if !strings.Contains(link, "Snell v5") {
		t.Errorf("Snell v5 分享链接应标注版本: %q", link)
	}
	v6 := Node{"id": json.Number("1"), "type": "snell", "port": json.Number("12345"),
		"version": json.Number("6"), "psk": "psk123", "name": "my-snell"}
	if link := s.LinkFor(v6, "1.2.3.4", ""); !strings.Contains(link, "Snell v6") {
		t.Errorf("Snell v6 分享链接应标注版本: %q", link)
	}
}

// ---- 流量统计归属 ----------------------------------------------------------

func TestSnellProtocols(t *testing.T) {
	ps := Protocols(Node{"type": "snell"})
	if len(ps) != 2 || ps[0] != "tcp" || ps[1] != "udp" {
		t.Errorf("Snell 应为 tcp+udp 双栈: %v", ps)
	}
}
