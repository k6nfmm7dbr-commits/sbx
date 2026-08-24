package nodes

import (
	"os"
	"strings"
	"testing"
)

const corruptTruncated = "[\n  {\"id\": 1, \"type\": \"vless\", \"port\": 443},\n  {\"id\""
const corruptObject = `{"nodes": []}`
const corruptElement = `[1, 2, 3]`
const corruptGarbage = "这不是JSON{{{"

func TestLoadToolNodesStrict(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nodes.json"

	// 不存在 = 空列表（允许全新安装直接添加）
	got, err := LoadToolNodesStrict(path)
	if err != nil || len(got) != 0 {
		t.Fatalf("缺失文件应返回空列表: %v %v", got, err)
	}

	for name, content := range map[string]string{
		"截断":    corruptTruncated,
		"顶层对象":  corruptObject,
		"非对象元素": corruptElement,
		"乱码":    corruptGarbage,
	} {
		if werr := os.WriteFile(path, []byte(content), 0o600); werr != nil {
			t.Fatal(werr)
		}
		_, err := LoadToolNodesStrict(path)
		if err == nil {
			t.Errorf("%s: 应返回错误", name)
			continue
		}
		if !strings.Contains(err.Error(), "已拒绝修改") {
			t.Errorf("%s: 错误信息应说明拒绝修改, got %v", name, err)
		}
	}
	// 宽松读取对损坏文件仍返回 nil（只读路径兼容不变）
	if werr := os.WriteFile(path, []byte(corruptTruncated), 0o600); werr != nil {
		t.Fatal(werr)
	}
	if got := LoadToolNodes(path); got != nil {
		t.Errorf("宽松读取应保持旧行为(nil), got %v", got)
	}
}

// 修改类操作在损坏文件面前必须全部拒绝，且原文件字节完全不变。
func TestMutationsRefuseCorruptedNodesFile(t *testing.T) {
	corrupts := map[string]string{
		"截断":    corruptTruncated,
		"顶层对象":  corruptObject,
		"非对象元素": corruptElement,
	}

	ops := []struct {
		name string
		args []string
	}{
		{"add", []string{"add", "shadowsocks", "--port", "8399",
			"--method", "2022-blake3-aes-128-gcm", "--password", "pw"}},
		{"edit", []string{"edit", "1", "--port", "8500"}},
		{"remove", []string{"remove", "1"}},
		{"sync", []string{"sync"}},
	}

	for cname, content := range corrupts {
		for _, op := range ops {
			cli, store, _ := newTestCLI(t)
			seed := `[{"id":1,"type":"shadowsocks","port":8388,"method":"m","password":"p","name":"n1"}]`
			if oerr := os.WriteFile(store.NodesPath(), []byte(seed), 0o600); oerr != nil {
				t.Fatal(oerr)
			}
			if werr := os.WriteFile(store.NodesPath(), []byte(content), 0o600); werr != nil {
				t.Fatal(werr)
			}
			before, _ := os.ReadFile(store.NodesPath())

			rc := cli.run(op.args)
			if rc != exitErr {
				t.Errorf("[%s/%s] rc=%d, 期望 exitErr", cname, op.name, rc)
			}
			if msg := cli.err(); !strings.Contains(msg, "已拒绝修改") {
				t.Errorf("[%s/%s] 错误信息异常: %q", cname, op.name, msg)
			}
			after, rerr := os.ReadFile(store.NodesPath())
			if rerr != nil {
				t.Fatalf("[%s/%s] 原文件丢失: %v", cname, op.name, rerr)
			}
			if string(after) != string(before) {
				t.Errorf("[%s/%s] 原文件被改动!", cname, op.name)
			}
			// 不应留下候选文件（避免后续 commit 把残缺数据提升为正式）
			for _, cand := range []string{store.NodesPath() + ".candidate", store.SBConf + ".candidate"} {
				if _, serr := os.Stat(cand); serr == nil {
					t.Errorf("[%s/%s] 意外生成候选文件 %s", cname, op.name, cand)
				}
			}
		}
	}
}

// 文件缺失时的行为保持旧语义：add 可从零开始；remove 报“未找到”。
func TestMutationMissingFileSemantics(t *testing.T) {
	cli, store, _ := newTestCLI(t)
	rc := cli.run([]string{"remove", "1"})
	if rc != exitErr || !strings.Contains(cli.err(), "未找到节点") {
		t.Errorf("缺失文件 remove 应报未找到, rc=%d err=%q", rc, cli.err())
	}
	if _, serr := os.Stat(store.NodesPath()); serr == nil {
		t.Error("失败的 remove 不应创建 nodes.json")
	}

	rc = cli.run([]string{"add", "shadowsocks", "--port", "8388",
		"--method", "2022-blake3-aes-128-gcm", "--password", "pw"})
	if rc != exitOK {
		t.Fatalf("缺失文件 add 应成功, err=%s", cli.err())
	}
	cli.run([]string{"commit"})
	list, lerr := LoadToolNodesStrict(store.NodesPath())
	if lerr != nil || len(list) != 1 {
		t.Fatalf("添加后应存在 1 个节点: %v %v", list, lerr)
	}
}

// 只读命令在损坏文件上仍按宽松语义工作（不崩溃、不修改）。
func testReadOnlyToleratesCorruption(t *testing.T) {
	t.Helper()
	cli, store, _ := newTestCLI(t)
	if werr := os.WriteFile(store.NodesPath(), []byte(corruptTruncated), 0o600); werr != nil {
		t.Fatal(werr)
	}
	for _, args := range [][]string{
		{"list"}, {"count"}, {"last"},
		{"links"}, {"port-used", "1234"},
	} {
		cli.run(args) // 只要不 panic / 不写文件即可
		if _, serr := os.Stat(store.NodesPath() + ".candidate"); serr == nil {
			t.Fatalf("%v 只读操作不应产生候选文件", args)
		}
	}
}
