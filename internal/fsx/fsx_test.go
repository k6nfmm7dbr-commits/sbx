package fsx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 基本写入：内容正确、权限正确、无临时文件残留。
func TestWriteFileAtomicBasic(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")

	err := WriteFileAtomic(target, []byte("hello-atomic"), 0o640)
	if err != nil {
		t.Fatal(err)
	}
	got, rerr := os.ReadFile(target)
	if rerr != nil || string(got) != "hello-atomic" {
		t.Fatalf("内容错误: %q %v", got, rerr)
	}
	st, serr := os.Stat(target)
	if serr != nil {
		t.Fatal(serr)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("权限 = %v, 期望 0640", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "out.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("目录应只含目标文件, got %v (临时文件未清理?)", names)
	}
}

// 覆盖已存在文件：内容替换 + 权限按本次调用强制生效。
func TestWriteFileAtomicReplaceExisting(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nodes.json")
	if werr := os.WriteFile(target, []byte(`["old"]`), 0o644); werr != nil {
		t.Fatal(werr)
	}

	newData := []byte("[\n  {\"id\": 1}\n]\n")
	if err := WriteFileAtomic(target, newData, 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(newData) {
		t.Errorf("覆盖后内容错误: %q", got)
	}
	st, _ := os.Stat(target)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("覆盖后权限应为 0600, got %v", st.Mode().Perm())
	}
}

// mode=0 时默认 0644。
func TestWriteFileAtomicDefaultMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	if err := WriteFileAtomic(target, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(target)
	if st.Mode().Perm() != 0o644 {
		t.Errorf("默认权限应为 0644, got %v", st.Mode().Perm())
	}
}

// 目标目录不存在 → 明确报错且不创建半成品。
func TestWriteFileAtomicMissingDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "no", "such", "dir", "f")
	err := WriteFileAtomic(target, []byte("x"), 0o644)
	if err == nil {
		t.Fatal("应对缺失目录报错")
	}
	if !strings.Contains(err.Error(), "创建临时文件失败") {
		t.Errorf("错误信息应说明创建临时文件失败: %v", err)
	}
}

// 并发写同一目标：最终内容为某一次完整写入（不撕裂），且无残留。
func TestWriteFileAtomicConcurrent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "c.json")
	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			payload := []byte(strings.Repeat(string(rune('a'+n)), 4096))
			_ = WriteFileAtomic(target, payload, 0o644)
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4096 {
		t.Fatalf("文件被撕裂: len=%d", len(got))
	}
	c := got[0]
	for _, b := range got {
		if b != c {
			t.Fatal("内容混合（非原子）")
		}
	}
}
