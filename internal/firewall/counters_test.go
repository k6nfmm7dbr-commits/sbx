package firewall

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 本文件保留改写前的正则实现作为「参考实现」，与手写切分版逐一对照。
//
// 为什么不删掉它：计数器名一旦解析错误，后果是流量入账到错误的 scope
// 或被整条丢弃，而且不会有任何报错。这类改写必须有机器可验证的等价性，
// 不能只靠人眼看「差不多」。

var (
	refCounterRe = regexp.MustCompile(`^sbx[_:](n(\d+)|sys)[_:]([io])$`)
	refEpochRe   = regexp.MustCompile(`^sbx[_:]epoch[_:](\d+)$`)
)

func refParseCounterName(name string) (scope, direction string, ok bool) {
	base := name
	if i := strings.IndexByte(name, '@'); i >= 0 {
		base = name[:i]
	}
	m := refCounterRe.FindStringSubmatch(base)
	if m == nil {
		return "", "", false
	}
	if m[1] == "sys" {
		scope = "system"
	} else {
		scope = "node:" + m[2]
	}
	if m[3] == "i" {
		direction = "rx"
	} else {
		direction = "tx"
	}
	return scope, direction, true
}

func refParseEpochName(name string) (uint64, bool) {
	base := name
	if i := strings.IndexByte(name, '@'); i >= 0 {
		base = name[:i]
	}
	m := refEpochRe.FindStringSubmatch(base)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// counterNameCorpus 覆盖：两种分隔符、混合分隔符、@family 后缀、
// sys/节点/epoch/诊断计数器、畸形与前缀相似但不合法的输入。
var counterNameCorpus = []string{
	"sbx_n1_i", "sbx_n1_o", "sbx_n128_i", "sbx_n128_o",
	"sbx_sys_i", "sbx_sys_o",
	"sbx:n3:i@v4", "sbx:sys:o@v6",
	"sbx_n1:i", "sbx:sys_o", // 混合分隔符：原正则允许，必须保留
	"sbx_n01_i", "sbx_n0_o", "sbx_n9999999999_o",
	"sbx_epoch_3", "sbx:epoch:7@v4", "sbx_epoch_007",
	"sbx_epoch_18446744073709551615", "sbx_epoch_18446744073709551616", // 溢出边界
	CTActivateCounter,
	// 以下为必须拒绝的畸形输入
	"", "sbx", "sbx_", "sbx:", "random", "sbx_n1", "sbx_n1_",
	"sbx_n_i", "sbx_n1_x", "sbx_n1_ii", "sbx_n1_i_extra",
	"sbx__n1_i", "sbx_n1__i", "sbx_n1:_i", "sbx_n-1_i",
	"sbx_n1_i@", "sbx_sys_i@", "sbx_epoch_", "sbx_epoch_x",
	"sbx_epoch_1_2", "sbxepoch_1", "sbx_n１_i", // 全角数字不是 \d
	"sbx_n1_i@v4", "SBX_N1_I", "sbx_n1_I",
}

func TestParseCounterNameMatchesReference(t *testing.T) {
	for _, in := range counterNameCorpus {
		gs, gd, gok := ParseCounterName(in)
		ws, wd, wok := refParseCounterName(in)
		if gs != ws || gd != wd || gok != wok {
			t.Errorf("ParseCounterName(%q) = (%q,%q,%v)，参考实现 = (%q,%q,%v)",
				in, gs, gd, gok, ws, wd, wok)
		}
	}
}

func TestParseEpochNameMatchesReference(t *testing.T) {
	for _, in := range counterNameCorpus {
		gv, gok := ParseEpochName(in)
		wv, wok := refParseEpochName(in)
		if gv != wv || gok != wok {
			t.Errorf("ParseEpochName(%q) = (%v,%v)，参考实现 = (%v,%v)",
				in, gv, gok, wv, wok)
		}
	}
}

// TestSnapshotEpochAcrossSeparators：世代号来自快照遍历，混合形态也要能识别。
func TestSnapshotEpochAcrossSeparators(t *testing.T) {
	snap := Snapshot{
		"sbx_n1_i":     {100, 1},
		"sbx:epoch:42": {0, 0},
	}
	v, ok := SnapshotEpoch(snap)
	if !ok || v != 42 {
		t.Fatalf("SnapshotEpoch = (%v,%v)，期望 (42,true)", v, ok)
	}
}

func BenchmarkParseCounterName(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ParseCounterName(counterNameCorpus[i%len(counterNameCorpus)])
	}
}

func BenchmarkParseCounterNameReference(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		refParseCounterName(counterNameCorpus[i%len(counterNameCorpus)])
	}
}
