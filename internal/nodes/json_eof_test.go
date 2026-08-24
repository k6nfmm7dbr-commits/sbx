package nodes

import (
	"os"
	"strings"
	"testing"
)

func TestStrictJSONTrailingData(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nodes.json"

	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"空数组", `[]`, false},
		{"空数组+换行", "[]\n", false},
		{"空数组+空白", "[]  \n\t ", false},
		{"合法数组+换行", "[{\"id\":1,\"type\":\"vless\",\"port\":443}]\n", false},
		{"trailing garbage", "[] garbage", true},
		{"第二个对象", "[] {}", true},
		{"数组后跟对象", "[{\"id\":1}] {\"a\":1}", true},
		{"数组后跟数组", "[] []", true},
	}
	for _, c := range cases {
		if werr := os.WriteFile(path, []byte(c.content), 0o600); werr != nil {
			t.Fatal(werr)
		}
		list, err := LoadToolNodesStrict(path)
		if c.wantErr {
			if err == nil {
				t.Errorf("[%s] 应报错, 却得到 %d 个节点", c.name, len(list))
			}
			continue
		}
		if err != nil {
			t.Errorf("[%s] 不应报错: %v", c.name, err)
		}
	}

	// 宽松读取保持旧行为：DecodeJSON 只取第一段，不校验尾部（只读路径兼容）
	if werr := os.WriteFile(path, []byte("[] garbage"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	if got := LoadToolNodes(path); got == nil || len(got) != 0 {
		t.Logf("宽松读取对 trailing garbage 返回: %v（兼容保留）", got)
	}
}

// nodes.json = "[] garbage" 时，修改类操作必须拒绝且原文件逐字节不变。
func TestMutationRefusesTrailingGarbage(t *testing.T) {
	cli, store, _ := newTestCLI(t)
	content := "[] garbage\n"
	if werr := os.WriteFile(store.NodesPath(), []byte(content), 0o600); werr != nil {
		t.Fatal(werr)
	}

	for _, op := range [][]string{
		{"add", "shadowsocks", "--port", "8388",
			"--method", "2022-blake3-aes-128-gcm", "--password", "pw"},
		{"edit", "1", "--port", "8500"},
		{"remove", "1"},
		{"sync"},
	} {
		rc := cli.run(op)
		if rc != exitErr {
			t.Errorf("%v: rc=%d, 期望 exitErr", op, rc)
		}
		if msg := cli.err(); !strings.Contains(msg, "已拒绝修改") {
			t.Errorf("%v: 错误信息异常: %q", op, msg)
		}
		after, rerr := os.ReadFile(store.NodesPath())
		if rerr != nil || string(after) != content {
			t.Errorf("%v: 原文件被改动: %q (%v)", op, string(after), rerr)
		}
	}
}
