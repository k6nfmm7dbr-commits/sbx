package nodes

import (
	"os"
	"path/filepath"
	"testing"
)

// strictPath 提供临时路径（不创建文件）。
func strictPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "nodes.json")
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPanelNodesStrictMissingIsFreshInstall(t *testing.T) {
	list, err := LoadPanelNodesStrict(strictPath(t))
	if err != nil {
		t.Fatalf("文件不存在应视为空列表(全新安装): %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("期望空列表, 得到 %d 个", len(list))
	}
}

func TestLoadPanelNodesStrictCorruptFails(t *testing.T) {
	p := strictPath(t)
	writeFixture(t, p, "{ invalid")
	if list, err := LoadPanelNodesStrict(p); err == nil {
		t.Fatalf("损坏文件必须报错, 得到 %v", list)
	}
	writeFixture(t, p, `{"nodes":"not-an-array"}`)
	if _, err := LoadPanelNodesStrict(p); err == nil {
		t.Fatal("结构错误必须报错")
	}
}

func TestLoadPanelNodesStrictValidWrapped(t *testing.T) {
	p := strictPath(t)
	writeFixture(t, p, `{"nodes":[{"id":1,"name":"a"},{"name":"no-id"},{"id":2}]}`)
	list, err := LoadPanelNodesStrict(p)
	if err != nil {
		t.Fatalf("合法包装不应报错: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("只保留含 id 的对象, 期望 2 得到 %d", len(list))
	}
}

// 防回归：nodes.json / state.json 必须保持 0600（含节点凭据）。
func TestSensitiveFilesStayPrivate(t *testing.T) {
	dir := t.TempDir()
	np := filepath.Join(dir, "nodes.json")
	sp := filepath.Join(dir, "state.json")

	if err := SaveNodesFile(np, []Node{{"id": int64(1), "port": "443"}}); err != nil {
		t.Fatalf("保存 nodes: %v", err)
	}
	if fi, err := os.Stat(np); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("nodes.json 权限必须为 0600, 得到 %v (%v)", fi.Mode(), err)
	}

	st := map[string]any{"next_node_id": int64(2)}
	if err := SaveState(sp, st); err != nil {
		t.Fatalf("保存 state: %v", err)
	}
	if fi, err := os.Stat(sp); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json 权限必须为 0600, 得到 %v (%v)", fi.Mode(), err)
	}
}
