package nodes

import (
	"os"
	"testing"
)

// cmdCommit 现在一并提交 sing-box 候选配置（rename+fsync），与 nodes.json 同语义。
func TestCommitMovesBothCandidates(t *testing.T) {
	cli, store, _ := newTestCLI(t)

	confCand := store.SBConf + ".candidate"
	nodesCand := store.NodesPath() + ".candidate"
	if err := os.WriteFile(confCand, []byte(`{"inbounds":["NEW"]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodesCand, []byte(`[{"id":1}]`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if rc := cli.run([]string{"commit"}); rc != exitOK {
		t.Fatalf("commit rc=%d err=%s", rc, cli.err())
	}

	gotConf, _ := os.ReadFile(store.SBConf)
	if string(gotConf) != `{"inbounds":["NEW"]}`+"\n" {
		t.Fatalf("config 未提交为候选内容: %q", gotConf)
	}
	gotNodes, _ := os.ReadFile(store.NodesPath())
	if string(gotNodes) != `[{"id":1}]`+"\n" {
		t.Fatalf("nodes 未提交为候选内容: %q", gotNodes)
	}
	if _, err := os.Stat(confCand); !os.IsNotExist(err) {
		t.Fatal("config 候选文件未清理")
	}
	if _, err := os.Stat(nodesCand); !os.IsNotExist(err) {
		t.Fatal("nodes 候选文件未清理")
	}
}

// 仅存在 nodes 候选（无 config 候选）时 commit 仍只提交 nodes，不报错。
func TestCommitNodesOnly(t *testing.T) {
	cli, store, _ := newTestCLI(t)
	nodesCand := store.NodesPath() + ".candidate"
	if err := os.WriteFile(nodesCand, []byte(`[{"id":2}]`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := cli.run([]string{"commit"}); rc != exitOK {
		t.Fatalf("commit rc=%d err=%s", rc, cli.err())
	}
	got, _ := os.ReadFile(store.NodesPath())
	if string(got) != `[{"id":2}]`+"\n" {
		t.Fatalf("nodes 未提交: %q", got)
	}
}

// 经 shell 锁内调用（SBX_LOCK_HELD=1）时必须跳过自锁，避免自死锁。
func TestCommitSkipsLockWhenHeld(t *testing.T) {
	t.Setenv("SBX_LOCK_HELD", "1")
	cli, store, _ := newTestCLI(t)
	if err := os.WriteFile(store.NodesPath()+".candidate", []byte(`[{"id":3}]`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := cli.run([]string{"commit"}); rc != exitOK {
		t.Fatalf("锁内 commit rc=%d err=%s", rc, cli.err())
	}
}
