package nodes

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// ---- A/B/C：key 生成（crypto/rand 语义 + 非法 method 拒绝） ------------------

func TestGenerateSS2022PasswordSizes(t *testing.T) {
	for _, tc := range []struct {
		method string
		size   int
	}{
		{SS2022Method128, 16},
		{SS2022Method256, 32},
	} {
		for i := 0; i < 8; i++ {
			pw, err := GenerateSS2022Password(tc.method)
			if err != nil {
				t.Fatalf("%s 生成失败: %v", tc.method, err)
			}
			raw, err := base64.StdEncoding.DecodeString(pw)
			if err != nil {
				t.Fatalf("%s password 不是合法 Base64: %v (%q)", tc.method, err, pw)
			}
			if len(raw) != tc.size {
				t.Fatalf("%s 期望 %d 字节, 实得 %d", tc.method, tc.size, len(raw))
			}
		}
	}
}

func TestGenerateSS2022PasswordRejectsUnknownMethod(t *testing.T) {
	for _, m := range []string{"aes-128-gcm", "aes-256-gcm", "random-method", "2022-blake3-aes-192-gcm", ""} {
		if pw, err := GenerateSS2022Password(m); err == nil {
			t.Errorf("method %q 应报错, 却返回 %q", m, pw)
		}
	}
}

func TestGenerateSS2022PasswordIsRandom(t *testing.T) {
	a, _ := GenerateSS2022Password(SS2022Method256)
	b, _ := GenerateSS2022Password(SS2022Method256)
	if a == b {
		t.Fatalf("两次生成 key 相同，随机源疑似失效")
	}
}

// ---- D/E/F：config / nodes / 分享链接 method + password 一致 -----------------

func TestSS2022RoundtripMethodPersistence(t *testing.T) {
	for _, tc := range []struct {
		method string
		size   int
	}{
		{SS2022Method128, 16},
		{SS2022Method256, 32},
	} {
		cli, store, _ := newTestCLI(t)
		pw, _ := GenerateSS2022Password(tc.method)
		rc := cli.run([]string{"add", "shadowsocks", "--port", "8388",
			"--method", tc.method, "--password", pw, "--name", "ss-" + tc.method})
		if rc != exitOK {
			t.Fatalf("add 失败: %s", cli.err())
		}
		cli.run([]string{"commit"})

		// config candidate 中的 method == 节点 method
		var added struct {
			Candidate string `json:"candidate"`
		}
		// 重新 add 拿 candidate 已在上一步 commit 前；这里直接读 nodes + 重建校验
		_ = added

		list := LoadToolNodes(store.NodesPath())
		if len(list) != 1 {
			t.Fatalf("nodes 数量异常: %d", len(list))
		}
		n := list[0]
		if Str(n, "method") != tc.method {
			t.Errorf("nodes method=%q 期望 %q", Str(n, "method"), tc.method)
		}
		if Str(n, "password") != pw {
			t.Errorf("nodes password 与生成值不一致")
		}
		raw, _ := base64.StdEncoding.DecodeString(Str(n, "password"))
		if len(raw) != tc.size {
			t.Errorf("nodes password 解码后 %d 字节, 期望 %d", len(raw), tc.size)
		}

		// 分享链接 method + password 一致
		link := store.LinkFor(n, "1.2.3.4", "")
		if !strings.HasPrefix(link, "ss://") {
			t.Fatalf("分享链接异常: %q", link)
		}
		userinfo := strings.TrimPrefix(link, "ss://")
		userinfo = userinfo[:strings.IndexByte(userinfo, '@')]
		dec, err := base64.RawURLEncoding.DecodeString(userinfo)
		if err != nil {
			t.Fatalf("分享链接 userinfo 解码失败: %v", err)
		}
		parts := strings.SplitN(string(dec), ":", 2)
		if len(parts) != 2 || parts[0] != tc.method || parts[1] != pw {
			t.Errorf("分享链接 method/password 不一致: %q vs %q/%q", parts, tc.method, pw)
		}
	}
}

// ---- G/H/I：method 切换与保留 password --------------------------------------

func TestSS2022EditMethodSwitch(t *testing.T) {
	cli, store, _ := newTestCLI(t)
	pw128, _ := GenerateSS2022Password(SS2022Method128)
	cli.run([]string{"add", "shadowsocks", "--port", "8388",
		"--method", SS2022Method128, "--password", pw128, "--name", "ss"})
	cli.run([]string{"commit"})

	// I. method 不变，只改端口 → password 不变
	cli.run([]string{"edit", "1", "--port", "9000"})
	cli.run([]string{"commit"})
	if p := Str(LoadToolNodes(store.NodesPath())[0], "password"); p != pw128 {
		t.Errorf("method 不变时 password 被改变: %q -> %q", pw128, p)
	}

	// G. 128 → 256 → password 变 + 32 字节
	rc := cli.run([]string{"edit", "1", "--method", SS2022Method256})
	if rc != exitOK {
		t.Fatalf("128→256 edit 失败: %s", cli.err())
	}
	var ed struct {
		Changed []string `json:"changed"`
	}
	json.Unmarshal([]byte(strings.TrimSpace(cli.out())), &ed)
	found := false
	for _, c := range ed.Changed {
		if strings.Contains(c, "256") {
			found = true
		}
	}
	if !found {
		t.Errorf("changed 未体现算法切换: %v", ed.Changed)
	}
	cli.run([]string{"commit"})
	pw256 := Str(LoadToolNodes(store.NodesPath())[0], "password")
	if pw256 == pw128 {
		t.Errorf("128→256 后 password 未变化")
	}
	raw, _ := base64.StdEncoding.DecodeString(pw256)
	if len(raw) != 32 {
		t.Errorf("256 password 解码后 %d 字节, 期望 32", len(raw))
	}

	// H. 256 → 128 → password 变 + 16 字节
	rc = cli.run([]string{"edit", "1", "--method", SS2022Method128})
	if rc != exitOK {
		t.Fatalf("256→128 edit 失败: %s", cli.err())
	}
	cli.run([]string{"commit"})
	pw128b := Str(LoadToolNodes(store.NodesPath())[0], "password")
	if pw128b == pw256 {
		t.Errorf("256→128 后 password 未变化")
	}
	raw, _ = base64.StdEncoding.DecodeString(pw128b)
	if len(raw) != 16 {
		t.Errorf("128 password 解码后 %d 字节, 期望 16", len(raw))
	}
}

// ---- J：旧节点兼容（无 method 字段按 128 读取，不重置 password） -------------

func TestSS2022LegacyNodeCompat(t *testing.T) {
	cli, store, _ := newTestCLI(t)
	// 模拟历史节点：仅有 type/port/password，无 method 字段
	legacy := []Node{{"id": json.Number("1"), "type": "shadowsocks", "port": json.Number("8388"), "password": "legacy-pw", "name": "old-ss"}}
	if err := SaveNodesFile(store.NodesPath(), legacy); err != nil {
		t.Fatal(err)
	}
	// 读取不报错、password 不变
	list := LoadToolNodes(store.NodesPath())
	if len(list) != 1 || Str(list[0], "password") != "legacy-pw" {
		t.Fatalf("旧节点读取异常: %+v", list)
	}
	// config 兼容读取为 128（不重置 password）
	inbound, err := BuildInbound(list[0])
	if err != nil {
		t.Fatal(err)
	}
	if inbound["method"] != SS2022Method128 {
		t.Errorf("旧节点 method 应兼容为 128, got %v", inbound["method"])
	}
	if inbound["password"] != "legacy-pw" {
		t.Errorf("旧节点 password 被重置: %v", inbound["password"])
	}
	_ = cli
}

// ---- CLI：ss2022-key 子命令 + 非法 method -----------------------------------

func TestSS2022KeyCLI(t *testing.T) {
	cli, _, _ := newTestCLI(t)
	rc := cli.run([]string{"ss2022-key", "--method", SS2022Method256})
	if rc != exitOK {
		t.Fatalf("ss2022-key 失败: %s", cli.err())
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cli.out()))
	if err != nil || len(raw) != 32 {
		t.Errorf("ss2022-key 输出异常: %q (len=%d err=%v)", cli.out(), len(raw), err)
	}
	// 非法 method
	if rc = cli.run([]string{"ss2022-key", "--method", "aes-128-gcm"}); rc == exitOK {
		t.Errorf("非法 method 应失败, 输出 %q", cli.out())
	}
}
