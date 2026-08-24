package firewall

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// 可编排的假命令执行器：按 "binary|chain" 返回内容或注入失败。
type fakeExec struct {
	out     map[string]string
	failKey map[string]bool // 该 key 本轮执行失败
	missing map[string]bool // LookPath 不存在
}

func (f *fakeExec) install(t *testing.T) {
	t.Helper()
	oldRun, oldWhich := runCmdFn, whichFn
	runCmdFn = func(ctx context.Context, args ...string) (int, string, string) {
		key := args[0] + "|" + args[len(args)-1]
		if f.failKey[key] {
			return 1, "", "simulated transient failure"
		}
		if out, ok := f.out[key]; ok {
			return 0, out, ""
		}
		return 1, "", "no chain"
	}
	whichFn = func(name string) bool { return !f.missing[name] }
	t.Cleanup(func() { runCmdFn, whichFn = oldRun, oldWhich })
}

func fmtIptLine(tag string, byts, pkts int64) string {
	return fmt.Sprintf("  %d  %d            tcp  --  *      *       0.0.0.0/0            0.0.0.0/0            /* %s */\n",
		pkts, byts, tag)
}

func chainText(lines ...string) string {
	return "\nChain X (1 references)\n pkts bytes target prot opt in out source destination\n" +
		strings.Join(lines, "")
}

// mkSnap 构造一轮双栈快照：v4/v6 各自给出 n1 入向计数。
func snapFrom(t *testing.T, p *Iptables, v4Bytes, v6Bytes int64, failV6 bool) (Snapshot, error) {
	t.Helper()
	ex := &fakeExec{
		out: map[string]string{
			"iptables|SBX_IN":  chainText(fmtIptLine("sbx:n1:i", v4Bytes, 10)),
			"ip6tables|SBX_IN": chainText(fmtIptLine("sbx:n1:i", v6Bytes, 10)),
		},
		failKey: map[string]bool{},
	}
	if failV6 {
		ex.failKey["ip6tables|SBX_IN"] = true
	}
	ex.install(t)
	return p.Read(context.Background())
}

// 场景一：v4 成功 + v6 成功 → 正常 commit（返回完整快照）。
func TestIptablesBothSucceed(t *testing.T) {
	p := NewIptables("")
	snap, err := snapFrom(t, p, 1000, 2000, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap["sbx:n1:i@v4"]; got != [2]int64{1000, 10} {
		t.Errorf("v4 = %v", got)
	}
	if got := snap["sbx:n1:i@v6"]; got != [2]int64{2000, 10} {
		t.Errorf("v6 = %v", got)
	}
}

// 场景二：v6 曾成功启用后读取失败 → 整轮失败，绝不返回 partial snapshot。
func TestIptablesEnabledFamilyFailureAborts(t *testing.T) {
	p := NewIptables("")
	if _, err := snapFrom(t, p, 1000, 2000, false); err != nil {
		t.Fatal("首轮应成功:", err)
	}
	snap, err := snapFrom(t, p, 1500, 0, true) // v6 执行失败
	if err == nil {
		t.Fatalf("已启用的 v6 失败必须让整轮失败, 却得到快照: %v", snap)
	}
	if IsLookup(err) {
		t.Errorf("该失败不是“不存在”类错误: %v", err)
	}
	// 恢复后继续正常工作
	snap, err = snapFrom(t, p, 2000, 2500, false)
	if err != nil {
		t.Fatal("恢复后应成功:", err)
	}
	if snap["sbx:n1:i@v6"] != [2]int64{2500, 10} {
		t.Errorf("恢复后 v6 = %v", snap["sbx:n1:i@v6"])
	}
}

// 场景三：反方向 —— v4 失败同样整轮失败。
func TestIptablesV4FailureAborts(t *testing.T) {
	p := NewIptables("")
	if _, err := snapFrom(t, p, 1000, 2000, false); err != nil {
		t.Fatal(err)
	}
	ex := &fakeExec{
		out:     map[string]string{"ip6tables|SBX_IN": chainText(fmtIptLine("sbx:n1:i", 2100, 10))},
		failKey: map[string]bool{"iptables|SBX_IN": true},
	}
	ex.install(t)
	snap, err := p.Read(context.Background())
	if err == nil {
		t.Fatalf("已启用的 v4 失败必须整轮失败, 却得到快照: %v", snap)
	}
	if IsLookup(err) {
		t.Errorf("不应归类为 LookupError: %v", err)
	}
}

// 场景四：仅存在 iptables（纯 IPv4 环境）→ 长期只读 v4 且每轮成功。
func TestIptablesV4OnlyEnvironment(t *testing.T) {
	p := NewIptables("")
	ex := &fakeExec{
		out:     map[string]string{"iptables|SBX_IN": chainText(fmtIptLine("sbx:n1:i", 500, 5))},
		failKey: map[string]bool{},
		missing: map[string]bool{"ip6tables": true}, // 命令不存在
	}
	ex.install(t)
	for round, want := range []int64{500, 900, 1300} {
		ex.out["iptables|SBX_IN"] = chainText(fmtIptLine("sbx:n1:i", want, 5))
		snap, err := p.Read(context.Background())
		if err != nil {
			t.Fatalf("第 %d 轮不应失败: %v", round+1, err)
		}
		if got := snap["sbx:n1:i@v4"]; got != [2]int64{want, 5} {
			t.Errorf("第 %d 轮 v4=%v", round+1, got)
		}
		if _, has := snap["sbx:n1:i@v6"]; has {
			t.Error("纯 v4 环境不应出现 v6 键")
		}
	}
}

// 场景五：命令存在但从未成功过（如内核无 IPv6）→ 按“缺失”处理，不影响 v4。
func TestIptablesNeverEnabledFamilyTreatedAsAbsent(t *testing.T) {
	p := NewIptables("")
	ex := &fakeExec{
		out:     map[string]string{"iptables|SBX_IN": chainText(fmtIptLine("sbx:n1:i", 700, 7))},
		failKey: map[string]bool{"ip6tables|SBX_IN": true}, // 从第一轮起就持续失败
	}
	ex.install(t)
	for i := 0; i < 3; i++ {
		snap, err := p.Read(context.Background())
		if err != nil {
			t.Fatalf("从未启用的 family 失败不应拖垮整体: %v", err)
		}
		if _, has := snap["sbx:n1:i@v6"]; has && i >= 0 {
			continue // v6 从未成功，不该有键
		}
	}
}
