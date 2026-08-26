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
	var out []nodes.Node
	data, err := os.ReadFile(filepath.Join("testdata", "gen_nft.golden"))
	if err != nil {
		return nil
	}
	_ = data
	// 节点集固定：vless:443 + shadowsocks:8388（与生成脚本一致）
	out = append(out,
		nodes.Node{"id": json.Number("1"), "type": "vless", "port": json.Number("443")},
		nodes.Node{"id": json.Number("2"), "type": "shadowsocks", "port": json.Number("8388")})
	return out
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
		{"sbx:n3:i@v4", "node:3", "rx", true},
		{"sbx:sys:o@v6", "system", "tx", true},
		{"random", "", "", false},
		{"sbx_epoch_9", "", "", false},
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
	if v, ok := ParseEpochName("sbx:epoch:7@v4"); !ok || v != 7 {
		t.Errorf("ipt epoch: %v %v", v, ok)
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

const chainHeader = " pkts bytes target     prot opt in     out     source               destination\n"

var chainInFixture = "\nChain SBX_IN (1 references)\n" + chainHeader +
	"    5  1000            tcp  --  *      *       0.0.0.0/0            0.0.0.0/0            /* sbx:n1:i */\n" +
	"    2  300             tcp  --  *      *       0.0.0.0/0            0.0.0.0/0            /* sbx:epoch:9 */\n"

var chainOutV4 = "\nChain SBX_OUT (1 references)\n" + chainHeader +
	"    8  2000            udp  --  *      *       0.0.0.0/0            0.0.0.0/0            /* sbx:n1:o */\n"

var chainOutV6 = "\nChain SBX_OUT (1 references)\n" + chainHeader +
	"    1   70             udp  --  *      *       ::/0                 ::/0                 /* sbx:n1:o */\n"

func TestIptablesReadAggregation(t *testing.T) {
	oldRun, oldWhich := runCmdFn, whichFn
	defer func() { runCmdFn, whichFn = oldRun, oldWhich }()

	binary := ""
	runCmdFn = func(ctx context.Context, args ...string) (int, string, string) {
		binary = args[0]
		if len(args) >= 4 && args[3] == IptChainIn {
			return 0, chainInFixture, ""
		}
		if len(args) >= 4 && args[3] == IptChainOut {
			if binary == "ip6tables" {
				return 0, chainOutV6, ""
			}
			return 0, chainOutV4, ""
		}
		return 1, "", "no chain"
	}
	whichFn = func(name string) bool { return name == "iptables" }

	p := NewIptables("")
	snap, err := p.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap["sbx:n1:i@v4"]; got != [2]int64{1000, 5} {
		t.Errorf("v4 rx 聚合错误: %v", got)
	}
	if got := snap["sbx:n1:o@v4"]; got != [2]int64{2000, 8} {
		t.Errorf("v4 tx 聚合错误: %v", got)
	}
	if _, ok := snap["sbx:epoch:9"]; !ok {
		t.Error("epoch 标记应存在")
	}
	if _, ok := snap["sbx:n1:o@v6"]; ok {
		t.Error("ip6tables 缺席时不应有 v6 键")
	}

	whichFn = func(name string) bool { return true }
	snap, err = p.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap["sbx:n1:o@v6"]; got != [2]int64{70, 1} {
		t.Errorf("v6 tx 聚合错误: %v", got)
	}

	whichFn = func(string) bool { return false }
	if _, err := p.Read(context.Background()); !IsLookup(err) {
		t.Errorf("无可用二进制应为 LookupError, got %v", err)
	}
}

func TestDetectBackend(t *testing.T) {
	oldRun, oldWhich := runCmdFn, whichFn
	defer func() { runCmdFn, whichFn = oldRun, oldWhich }()

	cases := []struct {
		forced    string
		nftInPath bool
		nftListRC int
		iptInPath bool
		want      string
	}{
		{"nft", false, 0, false, "nft"},
		{"iptables", true, 0, false, "iptables"},
		{"auto", true, 0, false, "nft"},
		{"auto", true, 1, true, "iptables"}, // nft list tables 失败
		{"auto", false, 0, true, "iptables"},
		{"auto", false, 0, false, "nft"}, // 都缺失仍默认 nft
	}
	for i, c := range cases {
		whichFn = func(name string) bool {
			if name == "nft" {
				return c.nftInPath
			}
			if name == "iptables" {
				return c.iptInPath
			}
			return false
		}
		runCmdFn = func(ctx context.Context, args ...string) (int, string, string) {
			return c.nftListRC, "", ""
		}
		if got := DetectBackend(c.forced); got != c.want {
			t.Errorf("case %d: DetectBackend(%q)=%q, want %q", i, c.forced, got, c.want)
		}
	}
}

func TestRulesGolden(t *testing.T) {
	nodes := loadRuleNodes()
	wantNft, err := os.ReadFile(filepath.Join("testdata", "gen_nft.golden"))
	if err != nil {
		t.Fatal(err)
	}
	gotNft := GenNFT(nodes, 42)
	if gotNft != string(wantNft) {
		t.Errorf("GenNFT 与 Python 金标不一致:\n--- got ---\n%s\n--- want ---\n%s", gotNft, wantNft)
	}
	wantIpt, err := os.ReadFile(filepath.Join("testdata", "gen_iptables.golden"))
	if err != nil {
		t.Fatal(err)
	}
	gotIpt := GenIPTables(nodes, 42)
	if gotIpt != string(wantIpt) {
		t.Errorf("GenIPTables 与 Python 金标不一致:\n--- got ---\n%s\n--- want ---\n%s",
			gotIpt, wantIpt)
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
	if !strings.Contains(gotIpt, "sbx:epoch:42") {
		t.Error("iptables 规则缺少 epoch 注释")
	}
}
