package firewall

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

// loadRuleNodes 使用固定的节点集（与金标夹具 internal/firewall/testdata 对应）。
func loadRuleNodes() []nodes.Node {
	return []nodes.Node{
		{"id": json.Number("1"), "type": "vless", "port": json.Number("443")},
		{"id": json.Number("2"), "type": "shadowsocks", "port": json.Number("8388")},
	}
}

func TestParseCounterNameTable(t *testing.T) {
	cases := []struct {
		in         string
		scope, dir string
		ok         bool
	}{
		{"sbx_n3_i", "node:3", "rx", true},
		{"sbx_n3_o", "node:3", "tx", true},
		{"sbx_sys_i", "system", "rx", true},
		// 冒号形态 + @family 后缀：升级前由旧后端写入 counter_state 的历史键，
		// 仍必须能解析，否则老库里的基线会被当成未知计数器、导致重复入账。
		{"sbx:n3:i@v4", "node:3", "rx", true},
		{"sbx:sys:o@v6", "system", "tx", true},
		{"random", "", "", false},
		{"sbx_epoch_9", "", "", false},
		{CTActivateCounter, "", "", false},
	}
	for _, c := range cases {
		scope, dir, ok := ParseCounterName(c.in)
		if ok != c.ok || scope != c.scope || dir != c.dir {
			t.Errorf("ParseCounterName(%q)=(%q,%q,%v), want (%q,%q,%v)",
				c.in, scope, dir, ok, c.scope, c.dir, c.ok)
		}
	}
}

func TestParseEpochName(t *testing.T) {
	if v, ok := ParseEpochName("sbx_epoch_123"); !ok || v != 123 {
		t.Errorf("nft epoch: %v %v", v, ok)
	}
	// 历史冒号形态（旧 counter_state 里可能存在）
	if v, ok := ParseEpochName("sbx:epoch:7@v4"); !ok || v != 7 {
		t.Errorf("历史 epoch 形态: %v %v", v, ok)
	}
	if _, ok := ParseEpochName("sbx_n1_i"); ok {
		t.Error("非 epoch 计数器不应命中")
	}
}

func TestNftReadParseAndErrors(t *testing.T) {
	fakeOK := `{"nftables":[
		{"counter":{"name":"sbx_epoch_9","bytes":0,"packets":0}},
		{"counter":{"name":"sbx_n1_i","bytes":1000,"packets":7}}]}`

	old := runCmdFn
	defer func() { runCmdFn = old }()

	runCmdFn = func(ctx context.Context, args ...string) (int, string, string) {
		return 0, fakeOK, ""
	}
	ctx := context.Background()
	b := NewNft("")
	snap, err := b.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap["sbx_n1_i"] != [2]int64{1000, 7} {
		t.Errorf("解析错误: %v", snap)
	}
	if _, ok := snap["sbx_epoch_9"]; !ok {
		t.Error("快照应包含 epoch 计数器")
	}

	runCmdFn = func(ctx context.Context, args ...string) (int, string, string) {
		return 1, "", "No such file or directory"
	}
	if _, err := b.Read(ctx); !IsLookup(err) {
		t.Errorf("表缺失应为 LookupError, got %v", err)
	}
	runCmdFn = func(ctx context.Context, args ...string) (int, string, string) {
		return 2, "", "permission denied"
	}
	if _, err := b.Read(ctx); IsLookup(err) || err == nil {
		t.Errorf("权限错误应为普通错误, got %v", err)
	}
	runCmdFn = func(ctx context.Context, args ...string) (int, string, string) {
		return 0, `{"nftables":[]}`, ""
	}
	if _, err := b.Read(ctx); !IsLookup(err) {
		t.Errorf("空表应为 LookupError, got %v", err)
	}
}

// nftables-only：后端名恒为 nft，且构造器只有 NewNft。
func TestNftBackendNameIsFixed(t *testing.T) {
	if got := NewNft("").Name(); got != BackendName {
		t.Errorf("后端名应恒为 %q, got %q", BackendName, got)
	}
	if BackendName != "nft" {
		t.Errorf("BackendName 常量必须是 nft（API schema 兼容）, got %q", BackendName)
	}
}

// NftAvailable 是 nftables-only 架构的硬前置检查：命令缺失或 list 失败都算不可用，
// 调用方据此明确失败——绝不降级到其它后端。
func TestNftAvailable(t *testing.T) {
	oldRun, oldWhich := runCmdFn, whichFn
	defer func() { runCmdFn, whichFn = oldRun, oldWhich }()

	cases := []struct {
		name    string
		inPath  bool
		listRC  int
		wantOK  bool
		explain string
	}{
		{"命令存在且可列表", true, 0, true, "正常可用"},
		{"命令存在但列表失败", true, 1, false, "权限不足/内核不支持"},
		{"命令不存在", false, 0, false, "未安装 nftables"},
	}
	for _, c := range cases {
		whichFn = func(string) bool { return c.inPath }
		runCmdFn = func(ctx context.Context, args ...string) (int, string, string) {
			return c.listRC, "", ""
		}
		if got := NftAvailable(context.Background()); got != c.wantOK {
			t.Errorf("%s(%s): NftAvailable=%v want %v", c.name, c.explain, got, c.wantOK)
		}
	}
}

func TestRulesGolden(t *testing.T) {
	list := loadRuleNodes()
	wantNft, err := os.ReadFile(filepath.Join("testdata", "gen_nft.golden"))
	if err != nil {
		t.Fatal(err)
	}
	gotNft := GenNFT(list, 42)
	if gotNft != string(wantNft) {
		t.Errorf("GenNFT 与金标不一致:\n--- got ---\n%s\n--- want ---\n%s", gotNft, wantNft)
	}
	// 关键结构断言（防金标意外为空）
	if !strings.Contains(gotNft, "counter sbx_epoch_42") ||
		!strings.Contains(gotNft, "counter name sbx_n2_i") {
		t.Error("nft 规则缺少关键计数器")
	}
	if strings.Count(gotNft, "counter name sbx_n2_") != 4 {
		t.Errorf("ss 节点应生成 4 条规则(tcp+udp × in+out), got %d",
			strings.Count(gotNft, "counter name sbx_n2_"))
	}
	// nftables-only：规则里不得再出现任何 iptables 痕迹
	for _, bad := range []string{"iptables", "ip6tables", "SBX_IN", "SBX_OUT", "-A ", "-N "} {
		if strings.Contains(gotNft, bad) {
			t.Errorf("nft 规则不应包含 iptables 痕迹 %q:\n%s", bad, gotNft)
		}
	}
}

// 规则生成器只输出 nftables：GenNFT 是唯一入口，且只写一个文件。
func TestOnlyNFTRulesGenerated(t *testing.T) {
	script := GenNFT(loadRuleNodes(), 7)
	if !strings.HasPrefix(script, "#!/usr/sbin/nft -f") {
		t.Errorf("规则脚本必须是 nft 脚本, got 首行: %q",
			strings.SplitN(script, "\n", 2)[0])
	}
	if !strings.Contains(script, "table inet "+NFTTable) {
		t.Errorf("规则应操作 table inet %s", NFTTable)
	}
	// 绝不允许 flush 整个 ruleset（会误伤服务器已有防火墙）
	if strings.Contains(script, "flush ruleset") {
		t.Fatalf("规则绝不允许 flush ruleset:\n%s", script)
	}
	// delete 只针对 SBX 自己的表
	for _, line := range strings.Split(script, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "delete ") && !strings.Contains(l, NFTTable) {
			t.Errorf("delete 只能针对 SBX 自己的表, 出现: %q", l)
		}
	}
}

// IsMissingMsg 决定「删除时表本就不存在算成功」与「Read 失败是否可自愈」，
// 必须只对明确的「不存在」类消息为真——权限/语法错误绝不能被当成不存在，
// 否则 Collector 会误以为缺规则而反复 Repair、Clear 会把真实失败当成功。
func TestIsMissingMsg(t *testing.T) {
	for msg, want := range map[string]bool{
		"No such file or directory": true,
		"does not exist":            true,
		"no such table":             true,
		"no such chain":             true,
		"permission denied":         false,
		"operation not permitted":   false,
		"syntax error":              false,
	} {
		if got := IsMissingMsg(msg); got != want {
			t.Errorf("IsMissingMsg(%q)=%v want %v", msg, got, want)
		}
	}
}
