package nodes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 符号链接逃逸（审计项「路径遍历」要求覆盖符号链接攻击）。
//
// 结论：**写入不会跟随符号链接**。SaveNodesFile 走 fsx.WriteFileAtomic
// （同目录临时文件 + rename），rename 替换的是链接本身，链接指向的目标文件
// 分毫不动。这条性质必须锁住——一旦有人把原子写改成 os.WriteFile，
// 攻击者只要能让 nodes.json 变成指向 /etc/shadow 的符号链接，就能借 root 权限
// 覆写任意文件。
func TestSaveNodesFileDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "nodes.json")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("环境不支持符号链接: %v", err)
	}

	list := []Node{{"id": json.Number("1"), "type": "vless", "port": json.Number("443")}}
	if err := SaveNodesFile(link, list); err != nil {
		t.Fatal(err)
	}

	// 1) 被指向的目标文件绝不能被改写
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("符号链接目标被改写! victim=%q", got)
	}
	// 2) 链接本身应被替换为普通文件（不再指向 victim）
	st, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t.Error("写入后 nodes.json 仍是符号链接，存在逃逸风险")
	}
	// 3) 内容确实是新写入的节点
	loaded, err := LoadToolNodesStrict(link)
	if err != nil {
		t.Fatalf("写入结果不可读: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("节点数应为 1, got %d", len(loaded))
	}
}
