package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// preloadRuleSet 在测试环境预置规则集文件（转换后的 sing-box source 格式）。
//
// 真实环境里这一步由 ruleset.Ensure 联网下载并转换完成；测试里预置是为了让断言
// 聚焦在「配置生成是否正确」，而不是把网络拉进单元测试。
func preloadRuleSet(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("SBX_DIR"), "rulesets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := []byte(`{"version":5,"rules":[{"domain_suffix":["example.com"]}]}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, name+".json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readSBConf(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("SBX_SB_CONF"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func sbxRouteRules(t *testing.T, cfg map[string]any) []any {
	t.Helper()
	route, _ := cfg["route"].(map[string]any)
	if route == nil {
		return nil
	}
	rules, _ := route["rules"].([]any)
	return rules
}

func TestRuleSetsEndpointListsCatalog(t *testing.T) {
	ts, _, _, _, _ := newNodeCRUDTestServer(t)
	code, body := doJSON(t, ts, http.MethodGet, "/api/rulesets", "")
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %#v", code, body)
	}
	groups, _ := body["groups"].([]any)
	if len(groups) == 0 {
		t.Fatal("应返回分组清单")
	}
	found := false
	for _, raw := range groups {
		g, _ := raw.(map[string]any)
		items, _ := g["items"].([]any)
		if len(items) == 0 {
			t.Errorf("分组 %v 没有条目", g["name"])
		}
		for _, it := range items {
			m, _ := it.(map[string]any)
			if m["name"] == "Google" {
				found = true
				if m["label"] == "" || m["label"] == nil {
					t.Error("Google 应有中文标签")
				}
				if _, ok := m["cached"]; !ok {
					t.Error("应带 cached 状态")
				}
			}
		}
	}
	if !found {
		t.Error("清单里应有 Google")
	}
}

// isPort53 判断端口值是否为 53。
//
// 从 JSON 文件读回来的数字是 float64，而内存里构造的是 int——直接用 != 53 比较
// 会因类型不同恒为真（Go 的 interface 比较按动态类型判定）。
func isPort53(v any) bool {
	switch n := v.(type) {
	case float64:
		return n == 53
	case int:
		return n == 53
	case json.Number:
		return n.String() == "53"
	}
	return false
}

func TestCreateNodeWithAllowListWritesRoute(t *testing.T) {
	ts, nodesFile, _, _, _ := newNodeCRUDTestServer(t)
	preloadRuleSet(t, "Google")
	preloadRuleSet(t, "TikTok")

	code, created := doJSON(t, ts, http.MethodPost, "/api/nodes",
		`{"type":"shadowsocks","name":"员工节点","port":8388,"method":"2022-blake3-aes-128-gcm","allow":["Google","TikTok"]}`)
	if code != http.StatusOK {
		t.Fatalf("创建失败 %d: %#v", code, created)
	}
	node, _ := created["node"].(map[string]any)
	allow, _ := node["allow"].([]any)
	if len(allow) != 2 || allow[0] != "Google" || allow[1] != "TikTok" {
		t.Errorf("返回的 DTO 应带允许清单，实得 %#v", node["allow"])
	}

	// nodes.json 落盘
	raw, _ := os.ReadFile(nodesFile)
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil || len(list) != 1 {
		t.Fatalf("nodes.json 内容不对: %s (%v)", raw, err)
	}
	got, _ := list[0]["allow"].([]any)
	if len(got) != 2 {
		t.Errorf("nodes.json 应保存允许清单，实得 %#v", list[0]["allow"])
	}

	// sing-box 配置：两条规则 + 两个 rule_set 定义
	cfg := readSBConf(t)
	rules := sbxRouteRules(t, cfg)
	if len(rules) != 3 {
		t.Fatalf("应生成 3 条规则（DNS / 白名单 / 拒绝），实得 %d: %#v", len(rules), rules)
	}
	for i, r := range rules {
		m, _ := r.(map[string]any)
		inbound, _ := m["inbound"].([]any)
		if len(inbound) != 1 || inbound[0] != "sbx-n1" {
			t.Errorf("rules[%d] 应绑定到该节点，实得 %#v", i, inbound)
		}
	}
	dns, _ := rules[0].(map[string]any)
	if ports, _ := dns["port"].([]any); len(ports) != 1 || !isPort53(ports[0]) {
		t.Errorf("第一条应是 DNS 放行: %#v", dns)
	}
	allowRule, _ := rules[1].(map[string]any)
	if allowRule["action"] != "route" || allowRule["outbound"] != "direct" {
		t.Errorf("第二条应是白名单放行: %#v", allowRule)
	}
	last, _ := rules[2].(map[string]any)
	if last["action"] != "reject" {
		t.Errorf("最后一条应是兜底拒绝: %#v", last)
	}

	route, _ := cfg["route"].(map[string]any)
	sets, _ := route["rule_set"].([]any)
	if len(sets) != 2 {
		t.Fatalf("应有 2 个 rule_set 定义，实得 %d", len(sets))
	}
	set0, _ := sets[0].(map[string]any)
	if set0["type"] != "local" || set0["format"] != "source" || set0["tag"] != "sbx-rs-Google" {
		t.Errorf("rule_set 定义不对: %#v", set0)
	}
}

// 没有 allow 的节点不受任何影响——这是老行为不变的保证。
func TestCreateNodeWithoutAllowProducesNoRoute(t *testing.T) {
	ts, _, _, _, _ := newNodeCRUDTestServer(t)
	code, _ := doJSON(t, ts, http.MethodPost, "/api/nodes",
		`{"type":"shadowsocks","name":"自用","port":8388,"method":"2022-blake3-aes-128-gcm"}`)
	if code != http.StatusOK {
		t.Fatalf("创建失败 %d", code)
	}
	cfg := readSBConf(t)
	if rules := sbxRouteRules(t, cfg); len(rules) != 0 {
		t.Errorf("未配限制时不应产生规则: %#v", rules)
	}
	route, _ := cfg["route"].(map[string]any)
	if _, has := route["rule_set"]; has {
		t.Errorf("未配限制时不应产生 rule_set: %#v", route["rule_set"])
	}
}

func TestCreateNodeRejectsUnknownRuleSet(t *testing.T) {
	ts, nodesFile, _, _, _ := newNodeCRUDTestServer(t)
	code, body := doJSON(t, ts, http.MethodPost, "/api/nodes",
		`{"type":"shadowsocks","name":"坏配置","port":8388,"method":"2022-blake3-aes-128-gcm","allow":["Google","NotARealSet"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未知规则集应 400，实得 %d: %#v", code, body)
	}
	raw, _ := os.ReadFile(nodesFile)
	if string(raw) != "[]" {
		t.Errorf("非法输入不应落盘: %s", raw)
	}
}

// 规则集拿不到必须整次失败，且不留下任何改动。
// 若带着缺失的规则集写进配置，sing-box 会起不来——那是全节点一起挂。
func TestCreateNodeFailsClosedWhenRuleSetUnavailable(t *testing.T) {
	ts, nodesFile, restarts, _, _ := newNodeCRUDTestServer(t)
	// 指向一个必然连不上的地址，且不预置文件。
	t.Setenv("SBX_RULESET_BASE", "http://127.0.0.1:9")
	before := readSBConf(t)

	code, body := doJSON(t, ts, http.MethodPost, "/api/nodes",
		`{"type":"shadowsocks","name":"员工","port":8388,"method":"2022-blake3-aes-128-gcm","allow":["Google"]}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("规则集不可用时应 422，实得 %d: %#v", code, body)
	}
	if raw, _ := os.ReadFile(nodesFile); string(raw) != "[]" {
		t.Errorf("失败时不应改动 nodes.json: %s", raw)
	}
	after := readSBConf(t)
	if len(sbxRouteRules(t, after)) != 0 {
		t.Error("失败时不应写进 sing-box 配置")
	}
	if before["route"].(map[string]any)["final"] != after["route"].(map[string]any)["final"] {
		t.Error("失败时不应改动原有配置")
	}
	if *restarts != 0 {
		t.Errorf("失败时不应重启 sing-box（实重启 %d 次）", *restarts)
	}
}

// 编辑时用空数组显式解除限制；字段缺失则保持原样不动。
func TestEditNodeAllowListLifecycle(t *testing.T) {
	ts, _, _, _, _ := newNodeCRUDTestServer(t)
	preloadRuleSet(t, "Google")

	code, created := doJSON(t, ts, http.MethodPost, "/api/nodes",
		`{"type":"shadowsocks","name":"员工","port":8388,"method":"2022-blake3-aes-128-gcm","allow":["Google"]}`)
	if code != http.StatusOK {
		t.Fatalf("创建失败 %d", code)
	}
	id := created["node"].(map[string]any)["id"]

	// 只改端口、不带 allow → 限制保持
	code, _ = doJSON(t, ts, http.MethodPut, "/api/nodes/"+toID(t, id), `{"port":8389}`)
	if code != http.StatusOK {
		t.Fatalf("改端口失败 %d", code)
	}
	if n := len(sbxRouteRules(t, readSBConf(t))); n != 3 {
		t.Errorf("未提交 allow 时限制应保持，规则数 %d", n)
	}

	// 显式清空 → 解除
	code, _ = doJSON(t, ts, http.MethodPut, "/api/nodes/"+toID(t, id), `{"port":8389,"allow":[]}`)
	if code != http.StatusOK {
		t.Fatalf("解除限制失败 %d", code)
	}
	cfg := readSBConf(t)
	if n := len(sbxRouteRules(t, cfg)); n != 0 {
		t.Errorf("解除后不应保留规则: %#v", sbxRouteRules(t, cfg))
	}
	route, _ := cfg["route"].(map[string]any)
	if _, has := route["rule_set"]; has {
		t.Error("解除后不应保留 rule_set")
	}
}

func TestEditNodeRejectsUnknownRuleSet(t *testing.T) {
	ts, _, _, _, _ := newNodeCRUDTestServer(t)
	code, _ := doJSON(t, ts, http.MethodPost, "/api/nodes",
		`{"type":"shadowsocks","name":"员工","port":8388,"method":"2022-blake3-aes-128-gcm"}`)
	if code != http.StatusOK {
		t.Fatalf("创建失败 %d", code)
	}
	code, body := doJSON(t, ts, http.MethodPut, "/api/nodes/1", `{"allow":["Evil"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未知规则集应 400，实得 %d: %#v", code, body)
	}
}

func toID(t *testing.T, v any) string {
	t.Helper()
	switch n := v.(type) {
	case string:
		return n
	case float64:
		return strconv.FormatInt(int64(n), 10)
	default:
		t.Fatalf("无法识别的 id: %#v", v)
		return ""
	}
}
