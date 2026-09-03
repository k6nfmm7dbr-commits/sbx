package connection

import (
	"os"
	"path/filepath"
	"testing"
)

// 干净机器（无任何引用 ct 的 netfilter 规则）上，nf_conntrack 模块已加载、
// /proc/net/nf_conntrack 存在可读，但内核不建条目，内容恒为空。
// 此时必须报告「不可用 + Inactive」，让上层回退 /proc——否则在线 IP 恒为 0，
// 而连接数（读 /proc/net/tcp）正常，表现为「有连接数但在线 IP 是 0」。
func TestConntrackEmptyFileReportsInactive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nf_conntrack")
	if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadConntrack(p)
	if got.Available {
		t.Error("空 conntrack 表不得报告 Available=true(会导致在线 IP 恒为 0)")
	}
	if !got.Inactive {
		t.Error("空 conntrack 表应置 Inactive=true 以区分「模块缺失」")
	}
	if got.Partial || got.Err != nil {
		t.Errorf("空表不是读取失败: partial=%v err=%v", got.Partial, got.Err)
	}
}

// 只有空白行也算空表。
func TestConntrackWhitespaceOnlyIsInactive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nf_conntrack")
	if err := os.WriteFile(p, []byte("\n  \n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadConntrack(p)
	if got.Available || !got.Inactive {
		t.Errorf("仅空白行应视为未激活: available=%v inactive=%v", got.Available, got.Inactive)
	}
}

// 表里有条目但没有目标端口的流：conntrack 正常可用（Entries>0），
// 绝不能被误判成未激活——否则会回退 /proc 复活已断开的残留 socket。
func TestConntrackHasEntriesButNoRelevantFlow(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nf_conntrack")
	// 一条与节点端口无关的 SSH 流（TIME_WAIT 会被 ParseConntrack 过滤掉）
	line := "ipv4     2 tcp      6 100 TIME_WAIT src=1.2.3.4 dst=5.6.7.8 sport=1111 " +
		"dport=22 packets=1 bytes=1 src=5.6.7.8 dst=1.2.3.4 sport=22 dport=1111 " +
		"packets=1 bytes=1 [ASSURED] mark=0 zone=0 use=2\n"
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadConntrack(p)
	if !got.Available {
		t.Error("表内有条目时 conntrack 必须报告可用")
	}
	if got.Inactive {
		t.Error("表内有条目不得判为未激活(会错误回退 /proc 复活死连接)")
	}
	if got.Entries != 1 {
		t.Errorf("Entries 应为 1, got %d", got.Entries)
	}
	if len(got.Flows) != 0 {
		t.Errorf("TIME_WAIT 流应被过滤, got %d", len(got.Flows))
	}
}

// 文件不存在（模块未加载/精简系统）：不可用且 Inactive=false，与空表区分开。
func TestConntrackMissingFileNotInactive(t *testing.T) {
	got := ReadConntrack(filepath.Join(t.TempDir(), "nope"))
	if got.Available {
		t.Error("文件不存在应报告不可用")
	}
	if got.Inactive {
		t.Error("文件不存在属「模块缺失」，不应标记 Inactive")
	}
}
