package traffic

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/nodes"
)

func writeNodesFile(path string, list ...nodes.Node) error {
	arr := make([]any, len(list))
	for i, n := range list {
		arr[i] = map[string]any(n)
	}
	data, err := json.MarshalIndent(arr, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// loadGoldenSummary 读 Python reference 生成的 summary/live 形状样本。
func loadGoldenSummary(t *testing.T) (summary, live map[string]any) {
	t.Helper()
	data, err := os.ReadFile("testdata/golden/summary_live.json")
	if err != nil {
		t.Fatalf("读取金标失败（先运行 tests/gen_goldens.py）: %v", err)
	}
	var doc struct {
		Summary map[string]any `json:"summary"`
		Live    map[string]any `json:"live"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Summary, doc.Live
}

// assertJSONShape 递归比较两棵 JSON 树：键集合必须完全一致；
// 字符串精确相等；数值在 1e-6 容差内相等；null 对 null。
func assertJSONShape(t *testing.T, path string, got, want any) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("%s: 类型不符 got %T want object", path, got)
		}
		if len(g) != len(w) {
			var extra, missing []string
			for k := range w {
				if _, ok := g[k]; !ok {
					missing = append(missing, k)
				}
			}
			for k := range g {
				if _, ok := w[k]; !ok {
					extra = append(extra, k)
				}
			}
			t.Fatalf("%s: 键集合不符 extra=%v missing=%v", path, extra, missing)
		}
		for k, wv := range w {
			assertJSONShape(t, path+"."+k, g[k], wv)
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			t.Fatalf("%s: 类型不符 got %T want array", path, got)
		}
		if len(g) != len(w) {
			t.Fatalf("%s: 数组长度 %d != %d", path, len(g), len(w))
		}
		for i := range w {
			assertJSONShape(t, fmt.Sprintf("%s[%d]", path, i), g[i], w[i])
		}
	case string:
		g, ok := got.(string)
		if !ok || g != w {
			t.Fatalf("%s: got %#v want %#v", path, got, want)
		}
	case bool:
		g, ok := got.(bool)
		if !ok || g != w {
			t.Fatalf("%s: got %#v want %#v", path, got, want)
		}
	case nil:
		if got != nil {
			t.Fatalf("%s: got %#v want null", path, got)
		}
	case float64:
		g, ok := got.(float64)
		if !ok {
			t.Fatalf("%s: 类型不符 got %T want number", path, got)
		}
		if math.Abs(g-w) > 1e-6*math.Max(1, math.Abs(w)) {
			t.Fatalf("%s: 数值 %v != %v", path, g, w)
		}
	default:
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %#v want %#v", path, got, want)
		}
	}
}

func toJSONAny(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSummaryLiveGoldenFixture 与 Python build_summary/build_live 输出对拍。
func TestSummaryLiveGoldenFixture(t *testing.T) {
	wantSummary, wantLive := loadGoldenSummary(t)

	// 固定包级时钟：now 与 Python reference 完全一致
	orig := TimeNow
	fixed := time.Unix(int64(wantSummary["now"].(float64)), 0)
	TimeNow = func() time.Time { return fixed }
	t.Cleanup(func() { TimeNow = orig })

	env := newEnv(t, 2)
	if err := writeNodesFile(env.cfg.NodesFile,
		nodes.Node{"id": int64(1), "type": "vless", "port": int64(443), "name": "测试节点"},
		nodes.Node{"id": int64(2), "type": "shadowsocks", "port": int64(8388), "name": "ss-2"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES" +
			"('2023-11-14','node:1',1000,2000,10,20)," +
			"('2023-11-14','node:2',300,400,3,4)," +
			"('2023-11-14','system',50,60,5,6)," +
			"('2023-11-15','node:1',111,222,1,2)",
		"INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES" +
			"('node:1',9999,8888,99,88),('node:2',777,666,7,6),('system',55,44,5,4)",
	} {
		if _, err := env.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	nowS := int64(wantSummary["now"].(float64))
	latest := nowS - 1
	for _, ins := range []string{
		fmt.Sprintf("INSERT OR REPLACE INTO samples(ts,scope,rx,tx,duration_ms,valid) VALUES(%d,'node:1',1000,2000,2000,1)", latest),
		fmt.Sprintf("INSERT OR REPLACE INTO samples(ts,scope,rx,tx,duration_ms,valid) VALUES(%d,'node:2',300,400,2000,1)", latest-2),
	} {
		if _, err := env.db.Exec(ins); err != nil {
			t.Fatal(err)
		}
	}

	s, err := BuildSummary(env.cfg, env.db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	l, err := BuildLive(env.cfg, env.db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}

	assertJSONShape(t, "summary", toJSONAny(t, s), wantSummary)
	assertJSONShape(t, "live", toJSONAny(t, l), wantLive)
}

// TestQRateStale 验证过期样本返回空速率（前端回落为未知）。
func TestQRateStale(t *testing.T) {
	env := newEnv(t, 2)
	env.push(map[string][2]int64{"sbx_n1_i": {0, 0}, "sbx_n1_o": {0, 0}})
	env.tickOK()
	env.advance(2000)
	env.push(map[string][2]int64{"sbx_n1_i": {100, 10}, "sbx_n1_o": {50, 5}})
	env.tickOK()

	r, err := QRate(env.db.DB, 2, env.now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(r) == 0 {
		t.Fatal("新鲜样本应有速率")
	}
	stale := env.now().Add(30 * time.Second).Unix()
	r2, err := QRate(env.db.DB, 2, stale)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2) != 0 {
		t.Errorf("过期样本应返回空速率, got %+v", r2)
	}
}

// TestBuildSummaryAggregatesExcludeSystem 校验聚合口径。
func TestBuildSummaryAggregatesExcludeSystem(t *testing.T) {
	orig := TimeNow
	TimeNow = func() time.Time { return time.Date(2023, 11, 13, 10, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { TimeNow = orig })

	env := newEnv(t, 2)
	if err := writeNodesFile(env.cfg.NodesFile,
		nodes.Node{"id": int64(1), "type": "vless", "port": int64(443), "name": "n1"},
		nodes.Node{"id": int64(2), "type": "shadowsocks", "port": int64(8388)}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2023-11-13','node:1',10,20,1,2)",
		"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2023-11-13','system',1000,2000,10,20)",
		"INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('node:1',7,8,1,2)",
		"INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('system',700,800,7,8)",
	} {
		if _, err := env.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s, err := BuildSummary(env.cfg, env.db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Today.Rx != 10 || s.Today.Tx != 20 {
		t.Errorf("today 聚合不应包含 system: %+v", s.Today)
	}
	if s.SystemToday.Rx != 1000 || s.SystemTotal.Rx != 700 {
		t.Errorf("system 行独立输出: today=%+v total=%+v", s.SystemToday, s.SystemTotal)
	}
	if len(s.Nodes) != 2 || s.Nodes[0].Name != "n1" {
		t.Errorf("节点列表异常: %+v", s.Nodes)
	}
	if s.Nodes[0].ConnsUDP != nil {
		t.Error("vless conns_udp 应为 null")
	}
	live, err := BuildLive(env.cfg, env.db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Nodes) != 2 || live.Nodes[0].ID == nil {
		t.Errorf("live 节点异常: %+v", live.Nodes)
	}
	if live.Healthy {
		t.Error("无采集器时 healthy 应为 false（对齐 collector=None）")
	}
}
