package nodes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const twoNodes = `[{"id":1,"type":"vless","port":443,"name":"a"},{"id":2,"type":"shadowsocks","port":8388,"name":"b"}]`
const threeNodes = `[{"id":1,"type":"vless","port":443},{"id":2,"type":"shadowsocks","port":8388},{"id":3,"type":"trojan","port":8443}]`

func writeNodes(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 缓存命中：内容不变时二次读取应返回同一切片（复用解析结果）。
func TestLoadStrictCacheHit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nodes.json")
	writeNodes(t, p, twoNodes)
	l1, err := LoadPanelNodesStrict(p)
	if err != nil || len(l1) != 2 {
		t.Fatalf("首次 err=%v n=%d", err, len(l1))
	}
	l2, err := LoadPanelNodesStrict(p)
	if err != nil || len(l2) != 2 {
		t.Fatalf("二次 err=%v n=%d", err, len(l2))
	}
	// 命中缓存应返回同一底层数组（&l1[0] == &l2[0]）。
	if &l1[0] != &l2[0] {
		t.Fatal("缓存未命中：两次返回不同切片")
	}
}

// 内容变化（mtime/size 变）必须失效重读，绝不返回旧内容。
func TestLoadStrictCacheInvalidateOnChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nodes.json")
	writeNodes(t, p, twoNodes)
	if l, _ := LoadPanelNodesStrict(p); len(l) != 2 {
		t.Fatalf("期望 2, got %d", len(l))
	}
	// 确保 mtime 变化（部分文件系统 mtime 粒度较粗，加 sleep 兜底 size 也变）。
	time.Sleep(10 * time.Millisecond)
	writeNodes(t, p, threeNodes)
	l, err := LoadPanelNodesStrict(p)
	if err != nil || len(l) != 3 {
		t.Fatalf("内容变化后应返回 3, got n=%d err=%v", len(l), err)
	}
}

// 同大小、同 mtime 的原子替换也必须失效：仅看 (mtime,size) 会把旧节点配置
// 缓存命中；os.SameFile 能识别 inode 已替换。
func TestLoadStrictCacheInvalidateOnSameMtimeSizeRename(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nodes.json")
	writeNodes(t, p, twoNodes)
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPanelNodesStrict(p); err != nil {
		t.Fatal(err)
	}

	updated := strings.Replace(twoNodes, `"name":"a"`, `"name":"z"`, 1)
	if len(updated) != len(twoNodes) {
		t.Fatal("fixture replacement must preserve byte length")
	}
	tmp := p + ".candidate"
	writeNodes(t, tmp, updated)
	if err := os.Chtimes(tmp, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Skip("filesystem cannot preserve mtime/size for same-size rename")
	}
	list, err := LoadPanelNodesStrict(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := DisplayName(list[0]); got != "z" {
		t.Fatalf("rename 后缓存返回陈旧内容: name=%q, want z", got)
	}
}

// 关键 fail-closed：文件从"好"变"坏"时必须返回 error，绝不用缓存的好结果掩盖。
func TestLoadStrictCacheNoStaleOnCorruption(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nodes.json")
	writeNodes(t, p, twoNodes)
	if _, err := LoadPanelNodesStrict(p); err != nil {
		t.Fatalf("首次应成功: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	writeNodes(t, p, `{ broken json`)
	l, err := LoadPanelNodesStrict(p)
	if err == nil {
		t.Fatalf("损坏后必须返回 error，绝不能用缓存掩盖；got list=%v", l)
	}
	// 再读一次仍应是 error（损坏态不入缓存）。
	if _, err := LoadPanelNodesStrict(p); err == nil {
		t.Fatal("损坏态不应被缓存为成功")
	}
}

// 损坏 → 修复后应恢复正常读取。
func TestLoadStrictCacheRecoverAfterFix(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nodes.json")
	writeNodes(t, p, `{ broken`)
	if _, err := LoadPanelNodesStrict(p); err == nil {
		t.Fatal("损坏应报错")
	}
	time.Sleep(10 * time.Millisecond)
	writeNodes(t, p, threeNodes)
	l, err := LoadPanelNodesStrict(p)
	if err != nil || len(l) != 3 {
		t.Fatalf("修复后应返回 3: n=%d err=%v", len(l), err)
	}
}

// 不存在的文件返回空列表且不报错（全新安装语义），不受缓存影响。
func TestLoadStrictCacheMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.json")
	l, err := LoadPanelNodesStrict(p)
	if err != nil || l != nil {
		t.Fatalf("不存在应返回 (nil,nil): n=%d err=%v", len(l), err)
	}
}
