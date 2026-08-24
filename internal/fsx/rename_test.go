package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// RenameAtomic 基本行为：内容完好、源文件消失、父目录持久化不报错。
func TestRenameAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "nodes.json.candidate")
	dst := filepath.Join(dir, "nodes.json")

	content := []byte("[{\"id\":7}]\n")
	if err := os.WriteFile(src, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameAtomic(src, dst); err != nil {
		t.Fatal(err)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil || string(got) != string(content) {
		t.Fatalf("目标内容错误: %q %v", got, rerr)
	}
	if _, serr := os.Stat(src); !os.IsNotExist(serr) {
		t.Errorf("源文件应已消失: %v", serr)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "nodes.json" {
		t.Errorf("目录残留异常")
	}
}

// 源文件不存在 → 真实 I/O 错误必须上抛，不能吞掉。
func TestRenameAtomicMissingSource(t *testing.T) {
	dir := t.TempDir()
	err := RenameAtomic(filepath.Join(dir, "nope"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Fatal("缺失源文件应返回错误")
	}
	if !os.IsNotExist(err) {
		t.Errorf("应为 NotExist 类错误: %v", err)
	}
}
