package nodes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// ---- 测试辅助 ----

func routeCfg(final string) map[string]any {
	if final == "" {
		final = "direct"
	}
	return map[string]any{
		"outbounds": []any{map[string]any{"type": "direct", "tag": "direct"}},
		"route":     map[string]any{"final": final},
	}
}

// mkNode 造一条节点记录；allow 为空表示不限制。
func mkNode(id string, allow ...string) Node {
	n := Node{
		"id":   json.Number(id),
		"type": "shadowsocks",
		"port": json.Number("1000" + id),
	}
	if len(allow) > 0 {
		items := make([]any, len(allow))
		for i, a := range allow {
			items[i] = a
		}
		n[AllowKey] = items
	}
	return n
}

func routeOf(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	route, ok := cfg["route"].(map[string]any)
	if !ok {
		t.Fatalf("route 不是对象: %#v", cfg["route"])
	}
	return route
}

func rulesOf(t *testing.T, cfg map[string]any) []any {
	t.Helper()
	route := routeOf(t, cfg)
	raw, exists := route["rules"]
	if !exists {
		return nil
	}
	rules, ok := raw.([]any)
	if !ok {
		t.Fatalf("rules 不是数组: %#v", raw)
	}
	return rules
}

func ruleAt(t *testing.T, cfg map[string]any, i int) map[string]any {
	t.Helper()
	rules := rulesOf(t, cfg)
	if i >= len(rules) {
		t.Fatalf("rules[%d] 不存在（共 %d 条）", i, len(rules))
	}
	m, ok := rules[i].(map[string]any)
	if !ok {
		t.Fatalf("rules[%d] 不是对象: %#v", i, rules[i])
	}
	return m
}

func strList(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("不是数组: %#v", v)
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 受限节点的规则布局，与 RulesPerLimitedNode 对应。
// 用常量而不是裸数字：规则构成变化时测试不需要跟着数第几条。
const (
	idxDNS         = 0
	idxProbeDomain = 1
	idxProbeSuffix = 2
	idxAllow       = RulesPerLimitedNode - 2
	idxReject      = RulesPerLimitedNode - 1
)

// ---- 白名单规则生成 ----

// 受限节点必须得到「先放行白名单、再兜底拒绝」两条规则，顺序不能反——
// reject 是无条件匹配，一旦排在前面白名单就永远命中不了。
func TestBuildRouteLimitedNodeGetsAllowThenReject(t *testing.T) {
	dir := t.TempDir()
	cfg := routeCfg("")
	list := []Node{mkNode("1"), mkNode("2", "Google", "TikTok")}

	if err := BuildRoute(cfg, list, dir); err != nil {
		t.Fatal(err)
	}
	rules := rulesOf(t, cfg)
	if len(rules) != RulesPerLimitedNode {
		t.Fatalf("受限节点应生成 %d 条规则（DNS / 探测×3 / 白名单 / 兜底拒绝），实得 %d: %#v",
			RulesPerLimitedNode, len(rules), rules)
	}

	dns := ruleAt(t, cfg, 0)
	if got := strList(t, dns["inbound"]); !eqStrings(got, []string{"sbx-n2"}) {
		t.Errorf("DNS 放行的 inbound 应为 [sbx-n2]，实得 %v", got)
	}
	if dns["action"] != "route" {
		t.Errorf("DNS 规则应为放行: %#v", dns)
	}
	if ports, ok := dns["port"].([]any); !ok || len(ports) != 1 || ports[0] != 53 {
		t.Errorf("DNS 放行应限定 53 端口，实得 %#v", dns["port"])
	}

	allow := ruleAt(t, cfg, idxAllow)
	if got := strList(t, allow["inbound"]); !eqStrings(got, []string{"sbx-n2"}) {
		t.Errorf("放行规则的 inbound 应为 [sbx-n2]，实得 %v", got)
	}
	if got := strList(t, allow["rule_set"]); !eqStrings(got, []string{"sbx-rs-Google", "sbx-rs-TikTok"}) {
		t.Errorf("rule_set 不对: %v", got)
	}
	if allow["action"] != "route" || allow["outbound"] != "direct" {
		t.Errorf("放行动作不对: %#v", allow)
	}

	reject := ruleAt(t, cfg, idxReject)
	if got := strList(t, reject["inbound"]); !eqStrings(got, []string{"sbx-n2"}) {
		t.Errorf("兜底规则的 inbound 应为 [sbx-n2]，实得 %v", got)
	}
	if reject["action"] != "reject" {
		t.Errorf("兜底规则必须是 reject: %#v", reject)
	}
	if _, has := reject["rule_set"]; has {
		t.Error("兜底规则不该带 rule_set（那是无条件拒绝）")
	}
}

// 放行目标跟随 route.final——用户把出站改成别的 tag 时，白名单放行也要跟着走，
// 否则会把流量从用户配置的中转上拽回来。
func TestBuildRouteAllowFollowsFinal(t *testing.T) {
	cfg := map[string]any{
		"outbounds": []any{
			map[string]any{"type": "direct", "tag": "direct"},
			map[string]any{"type": "socks", "tag": "upstream"},
		},
		"route": map[string]any{"final": "upstream"},
	}
	if err := BuildRoute(cfg, []Node{mkNode("3", "Google")}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := ruleAt(t, cfg, idxAllow)["outbound"]; got != "upstream" {
		t.Errorf("放行应指向 final，实得 %v", got)
	}
}

// 未配限制的节点不能被牵连——自用节点与员工节点共存于同一个 sing-box 进程，
// 这是整个功能的立足点。
func TestBuildRouteLeavesUnlimitedNodesAlone(t *testing.T) {
	cfg := routeCfg("")
	list := []Node{mkNode("1"), mkNode("2", "Google"), mkNode("3")}
	if err := BuildRoute(cfg, list, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for i, raw := range rulesOf(t, cfg) {
		m := raw.(map[string]any)
		for _, tag := range strList(t, m["inbound"]) {
			if tag != "sbx-n2" {
				t.Errorf("rules[%d] 影响到了未受限节点 %s", i, tag)
			}
		}
	}
	for _, raw := range routeOf(t, cfg)["rule_set"].([]any) {
		if raw.(map[string]any)["path"] == "" {
			t.Error("rule_set 缺 path")
		}
	}
}

// sbx 规则必须排在用户规则之前，否则用户那条更宽的规则会先把连接匹配走，
// 白名单形同虚设。这是安全属性，不是风格问题。
func TestBuildRouteManagedRulesPrecedeUserRules(t *testing.T) {
	cfg := routeCfg("")
	route := routeOf(t, cfg)
	userRule := map[string]any{"domain_suffix": []any{".example.com"}, "outbound": "direct"}
	route["rules"] = []any{userRule}

	if err := BuildRoute(cfg, []Node{mkNode("2", "Google")}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	rules := rulesOf(t, cfg)
	if len(rules) != RulesPerLimitedNode+1 {
		t.Fatalf("应为 %d 条 sbx 规则 + 1 条用户规则，实得 %d", RulesPerLimitedNode, len(rules))
	}
	for i := 0; i < RulesPerLimitedNode; i++ {
		// 用「有没有 inbound 条件」区分：sbx 生成的规则一律带 inbound
		// （这是限制只作用于该节点的关键），用户自己写的规则没有。
		if _, hasInbound := ruleAt(t, cfg, i)["inbound"]; !hasInbound {
			t.Errorf("sbx 规则应排在最前，但 rules[%d] 没有 inbound 条件（是用户规则）", i)
		}
	}
	if ruleAt(t, cfg, idxReject)["action"] != "reject" {
		t.Error("兜底拒绝应排在 sbx 段的最后")
	}
	last := ruleAt(t, cfg, RulesPerLimitedNode)
	if _, isUser := last["domain_suffix"]; !isUser {
		t.Errorf("用户规则应被完整保留在最后: %#v", last)
	}
}

// 重建必须幂等：反复保存同一节点不能把规则越堆越多。
func TestBuildRouteIdempotent(t *testing.T) {
	cfg := routeCfg("")
	list := []Node{mkNode("2", "Google")}
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := BuildRoute(cfg, list, dir); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(rulesOf(t, cfg)); n != RulesPerLimitedNode {
		t.Errorf("重复构建后规则数应恒为 %d，实得 %d", RulesPerLimitedNode, n)
	}
	if n := len(routeOf(t, cfg)["rule_set"].([]any)); n != 1 {
		t.Errorf("重复构建后 rule_set 数应恒为 1，实得 %d", n)
	}
}

// 改动白名单后，旧规则集定义必须被清掉，不能残留。
func TestBuildRouteReplacesPreviousSelections(t *testing.T) {
	cfg := routeCfg("")
	dir := t.TempDir()
	if err := BuildRoute(cfg, []Node{mkNode("2", "Google", "Netflix")}, dir); err != nil {
		t.Fatal(err)
	}
	if err := BuildRoute(cfg, []Node{mkNode("2", "TikTok")}, dir); err != nil {
		t.Fatal(err)
	}
	if got := strList(t, ruleAt(t, cfg, idxAllow)["rule_set"]); !eqStrings(got, []string{"sbx-rs-TikTok"}) {
		t.Errorf("应只剩新选择，实得 %v", got)
	}
	sets := routeOf(t, cfg)["rule_set"].([]any)
	if len(sets) != 1 || sets[0].(map[string]any)["tag"] != "sbx-rs-TikTok" {
		t.Errorf("rule_set 定义应只剩 TikTok: %#v", sets)
	}
}

// 解除限制后不能留下空的 rules/rule_set 键，否则每次保存都会产生无意义 diff。
func TestBuildRouteCleansUpWhenUnlimited(t *testing.T) {
	cfg := routeCfg("")
	dir := t.TempDir()
	if err := BuildRoute(cfg, []Node{mkNode("2", "Google")}, dir); err != nil {
		t.Fatal(err)
	}
	if err := BuildRoute(cfg, []Node{mkNode("2")}, dir); err != nil {
		t.Fatal(err)
	}
	route := routeOf(t, cfg)
	if _, has := route["rules"]; has {
		t.Errorf("解除限制后不该留 rules: %#v", route["rules"])
	}
	if _, has := route["rule_set"]; has {
		t.Errorf("解除限制后不该留 rule_set: %#v", route["rule_set"])
	}
}

// 用户自己建的 rule_set 不能被误删——只有 sbx-rs- 前缀的才归我们管。
func TestBuildRoutePreservesUserRuleSets(t *testing.T) {
	cfg := routeCfg("")
	route := routeOf(t, cfg)
	route["rule_set"] = []any{map[string]any{"tag": "my-own", "type": "remote", "format": "binary", "url": "https://example.com/x.srs"}}

	if err := BuildRoute(cfg, []Node{mkNode("2", "Google")}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	sets := routeOf(t, cfg)["rule_set"].([]any)
	if len(sets) != 2 {
		t.Fatalf("应保留用户项并追加 sbx 项，实得 %d", len(sets))
	}
	if sets[0].(map[string]any)["tag"] != "my-own" {
		t.Errorf("用户 rule_set 应原样保留在前: %#v", sets[0])
	}
	if sets[1].(map[string]any)["tag"] != "sbx-rs-Google" {
		t.Errorf("sbx rule_set 应追加在后: %#v", sets[1])
	}
}

// 节点记录被手工塞进非法名字时，不能把它拼进配置。
func TestAllowOfFiltersUnknownAndDedupes(t *testing.T) {
	n := Node{AllowKey: []any{"Google", "Google", "Evil", "", "  ", 42, "TikTok"}}
	got := AllowOf(n)
	if !eqStrings(got, []string{"Google", "TikTok"}) {
		t.Errorf("应过滤非法并去重，实得 %v", got)
	}
	if HasAllow(Node{AllowKey: []any{"Evil"}}) {
		t.Error("只有非法项时应视为无限制")
	}
	if HasAllow(Node{}) {
		t.Error("缺字段应视为无限制")
	}
}

// 老节点（没有 allow 字段）升级后行为必须完全不变。
func TestOldNodesHaveNoRouteImpact(t *testing.T) {
	cfg := routeCfg("")
	if err := BuildRoute(cfg, []Node{mkNode("1"), mkNode("2")}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	route := routeOf(t, cfg)
	if _, has := route["rules"]; has {
		t.Error("无受限节点时不应产生 rules")
	}
	if _, has := route["rule_set"]; has {
		t.Error("无受限节点时不应产生 rule_set")
	}
}

// ---- 与 RebuildConfig 的衔接 ----

// RebuildConfig 保留用户其它字段，只重建 sbx 管理的 inbounds 与白名单路由。
func TestRebuildConfigAppliesAllowAndKeepsUserConfig(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "config.json")
	// 模拟一份真实配置：带用户自建 inbound、自建出站、自建规则。
	seed := `{
  "log": {"level": "info"},
  "inbounds": [
    {"type": "socks", "tag": "user-custom", "listen": "127.0.0.1", "listen_port": 1080},
    {"type": "shadowsocks", "tag": "sbx-n9", "listen": "::", "listen_port": 19999,
     "method": "2022-blake3-aes-128-gcm", "password": "stale"}
  ],
  "outbounds": [{"type": "direct", "tag": "direct"}],
  "route": {
    "rules": [{"domain_suffix": [".internal"], "outbound": "direct"}],
    "final": "direct"
  },
  "experimental": {"cache_file": {"enabled": true, "path": "/tmp/cache.db"}}
}`
	if err := os.WriteFile(conf, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &Store{AppDir: dir, SBConf: conf, NodesFile: filepath.Join(dir, "nodes.json")}

	cfg, err := RebuildConfig(store, []Node{mkNode("1"), mkNode("2", "Google")})
	if err != nil {
		t.Fatal(err)
	}

	// 用户自建 inbound 保留，旧的 sbx inbound 被替换。
	tags := []string{}
	for _, raw := range cfg["inbounds"].([]any) {
		tags = append(tags, raw.(map[string]any)["tag"].(string))
	}
	if !eqStrings(tags, []string{"user-custom", "sbx-n1", "sbx-n2"}) {
		t.Errorf("inbounds 重建结果不对: %v", tags)
	}
	// 用户其它配置块原样保留。
	if _, ok := cfg["experimental"]; !ok {
		t.Error("experimental 应保留")
	}
	// 白名单路由生效，且用户规则仍在最后。
	rules := rulesOf(t, cfg)
	if len(rules) != RulesPerLimitedNode+1 {
		t.Fatalf("应为 %d 条 sbx + 1 条用户规则，实得 %d: %#v", RulesPerLimitedNode, len(rules), rules)
	}
	if _, isUser := ruleAt(t, cfg, RulesPerLimitedNode)["domain_suffix"]; !isUser {
		t.Error("用户规则应保留在最后")
	}
	// 规则集路径落在 sing-box 配置同级的 rulesets/ 下。
	sets := routeOf(t, cfg)["rule_set"].([]any)
	wantPath := filepath.Join(dir, "rulesets", "Google.json")
	if got := sets[0].(map[string]any)["path"]; got != wantPath {
		t.Errorf("规则集路径应为 %s，实得 %v", wantPath, got)
	}
}

// DNS 放行必须存在且排在白名单之前。
//
// 这是从真实故障里得来的：客户端把域名解析也走代理时，若 DNS 被兜底 reject 掉，
// 用户看到的是「这个节点什么网站都打不开」，而白名单里明明勾了服务——症状与
// 配置内容毫无关联，单靠面板无从判断。
func TestBuildRouteAlwaysAllowsDNSFirst(t *testing.T) {
	cfg := routeCfg("")
	list := []Node{mkNode("2", "Google"), mkNode("5", "Netflix", "Spotify")}
	if err := BuildRoute(cfg, list, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	rules := rulesOf(t, cfg)
	// 每个受限节点 RulesPerLimitedNode 条
	if len(rules) != 2*RulesPerLimitedNode {
		t.Fatalf("两个受限节点应生成 %d 条规则，实得 %d", 2*RulesPerLimitedNode, len(rules))
	}
	for i, tag := range []string{"sbx-n2", "sbx-n5"} {
		base := i * RulesPerLimitedNode
		dns := ruleAt(t, cfg, base)
		if got := strList(t, dns["inbound"]); !eqStrings(got, []string{tag}) {
			t.Errorf("节点 %s 的 DNS 规则 inbound 不对: %v", tag, got)
		}
		if ports, _ := dns["port"].([]any); len(ports) != 1 || ports[0] != 53 {
			t.Errorf("节点 %s 的 DNS 规则应限定 53: %#v", tag, dns["port"])
		}
		if ruleAt(t, cfg, base+idxReject)["action"] != "reject" {
			t.Errorf("节点 %s 的最后一条应为 reject", tag)
		}
	}
}

// 客户端的「延迟测试」是一次真实的 HTTP 请求（Clash 系打 gstatic、Surge 系打
// apple.com、Windows 打 msftconnecttest……）。白名单模式下这些请求会落到兜底
// reject 上，节点在客户端里就一直显示超时——用户看到的是「节点挂了」，而症状
// 与白名单里勾了什么毫无关系。所以探测目标必须内置放行，且排在拒绝之前。
func TestBuildRouteAlwaysAllowsConnectivityProbes(t *testing.T) {
	cfg := routeCfg("")
	// 白名单只勾一个与探测目标无关的服务，确保放行不是「顺带」的。
	if err := BuildRoute(cfg, []Node{mkNode("2", "Netflix")}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(rulesOf(t, cfg)) != RulesPerLimitedNode {
		t.Fatalf("只应有一个受限节点的规则，实得 %d 条", len(rulesOf(t, cfg)))
	}

	byDomain := ruleAt(t, cfg, idxProbeDomain)
	domains := strList(t, byDomain["domain"])
	for _, want := range []string{
		"www.gstatic.com", "cp.cloudflare.com", "www.apple.com", "www.msftconnecttest.com",
	} {
		if !slices.Contains(domains, want) {
			t.Errorf("探测域名里应含 %s（有客户端的默认延迟测试用的就是它），实得 %v", want, domains)
		}
	}
	if byDomain["action"] != "route" {
		t.Errorf("探测域名应是放行: %#v", byDomain)
	}
	if byDomain["outbound"] != "direct" {
		t.Errorf("探测放行应跟随 final: %#v", byDomain)
	}

	if got := strList(t, ruleAt(t, cfg, idxProbeSuffix)["domain_suffix"]); !slices.Contains(got, "gstatic.com") {
		t.Errorf("探测后缀里应含 gstatic.com，实得 %v", got)
	}

	// 两个维度必须各自成一条规则：sing-box 同一条规则里的字段是 AND 关系。
	for _, i := range []int{idxProbeDomain, idxProbeSuffix} {
		r := ruleAt(t, cfg, i)
		fields := 0
		for _, k := range []string{"domain", "domain_suffix"} {
			if _, has := r[k]; has {
				fields++
			}
		}
		if fields != 1 {
			t.Errorf("rules[%d] 混了 %d 个匹配维度（同一条规则内是 AND，会永远匹配不上）", i, fields)
		}
	}

	// 必须在兜底拒绝之前，否则永远不会命中。
	if !(idxProbeDomain < idxReject && idxProbeSuffix < idxReject) {
		t.Error("探测放行必须排在兜底拒绝之前")
	}

	// 这一条是踩过坑换来的：路由规则里一旦出现 ip_cidr，sing-box 会先把连接的
	// 域名解析掉、目标变成 IP，于是靠前的 domain 规则全部失配——白名单整体失效。
	// 所以探测放行只能走域名维度。（真实流量下复现过，见验收脚本第 4 节。）
	for i, raw := range rulesOf(t, cfg) {
		if _, has := raw.(map[string]any)["ip_cidr"]; has {
			t.Errorf("rules[%d] 带了 ip_cidr：它会让 sing-box 提前解析域名，导致域名规则失效", i)
		}
	}
}
