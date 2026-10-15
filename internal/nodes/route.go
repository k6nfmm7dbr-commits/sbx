package nodes

import (
	"fmt"
	"sort"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx/internal/ruleset"
)

// AllowKey 是节点记录里「只允许访问的规则集」的字段名。
//
// 缺省或空数组表示不限制——这正是本次改动之前所有节点的行为，
// 因此老节点升级后语义不变。
const AllowKey = "allow"

// ruleSetTagPrefix 是 sbx 写入 sing-box 配置的 rule_set 标签前缀。
// 重建时按此前缀清理旧项，用户自建的 rule_set 不受影响。
const ruleSetTagPrefix = "sbx-rs-"

// RouteSetTagPrefix 导出给测试与外部诊断使用。
const RouteSetTagPrefix = ruleSetTagPrefix

// RuleSetTagOf 返回某个规则集在 sing-box 配置中的 tag。
func RuleSetTagOf(name string) string { return ruleSetTagPrefix + name }

// AllowOf 读取节点的白名单规则集名，已去重并剔除非法值。
//
// 这里刻意「静默过滤」而不是报错：nodes.json 是 root 可编辑的本地文件，
// 若被手工写进非法名字，最坏后果也不该是「配置生成失败导致所有节点都改不了」。
// 真正的输入校验发生在面板 API 层，用户填错会在那里得到明确提示。
//
// 同时会把旧上游的名字翻译到当前清单（见 ruleset.MigrateAllow）：换上游后，
// 老节点里勾过的 OpenAI / xAI 这类名字仍按「最接近的选项」继续生效，
// 而不是悄悄失效。翻译是幂等的，因此这里反复调用不会让名单越来越短。
func AllowOf(n Node) []string {
	raw, ok := n[AllowKey]
	if !ok || raw == nil {
		return nil
	}
	var items []string
	switch v := raw.(type) {
	case []string:
		items = v
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				items = append(items, s)
			}
		}
	default:
		return nil
	}
	mapped, _ := ruleset.MigrateAllow(items)
	return mapped
}

// HasAllow 报告节点是否配了访问限制。
func HasAllow(n Node) bool { return len(AllowOf(n)) > 0 }

// isManagedInboundList 判断一条 route 规则的 inbound 是否完全由 sbx 节点组成。
//
// 这是识别「这条规则是 sbx 生成的」的唯一依据——sing-box 的 JSON 解析是严格的，
// 不允许未知字段，因此没法给自己生成的规则打标记。约定：凡是 inbound 全为
// sbx-n<数字> 的规则都归 sbx 所有，重建时清掉重来。
func isManagedInboundList(v any) bool {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return false
	}
	for _, e := range arr {
		s, ok := e.(string)
		if !ok || !isManagedTag(s) {
			return false
		}
	}
	return true
}

// limitedNodes 返回配了白名单的节点，按 id 升序以保证生成结果稳定（diff 友好）。
func limitedNodes(list []Node) []Node {
	var out []Node
	for _, n := range list {
		if HasAllow(n) {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return IDString(out[i]) < IDString(out[j])
	})
	return out
}

// neededRuleSets 汇总所有受限节点用到的规则集，按名字排序。
func neededRuleSets(list []Node) []string {
	set := map[string]bool{}
	for _, n := range list {
		for _, name := range AllowOf(n) {
			set[name] = true
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// BuildRoute 重建 route 中由 sbx 管理的部分：受限节点的白名单规则，以及它们
// 引用的 rule_set 定义。用户自己写的 rule_set 与 route 规则原样保留。
//
// 生成的规则形如（以配了 Google+TikTok 的节点为例）：
//
//	{ "inbound": ["sbx-n2"], "rule_set": ["sbx-rs-Google","sbx-rs-TikTok"],
//	  "action": "route", "outbound": "direct" }      ← 白名单内放行
//	{ "inbound": ["sbx-n2"], "action": "reject" }     ← 其余一律拒绝
//
// 两条都带 inbound 条件，因此只影响该节点——这是「一个自用、一个给员工」能
// 共存于同一个 sing-box 进程的关键。
//
// 顺序上 sbx 规则必须排在用户规则之前：白名单的语义是「除这些以外都不许」，
// 一旦被后面某条规则先匹配走（例如用户写的 geosite:cn → direct），限制就被绕过了。
func BuildRoute(cfg map[string]any, list []Node, ruleSetDir string) error {
	route, ok := cfg["route"].(map[string]any)
	if !ok {
		return fmt.Errorf("读取 sing-box 配置失败: route 必须是对象")
	}
	final, _ := route["final"].(string)
	if final == "" {
		final = "direct"
	}

	limited := limitedNodes(list)
	needed := neededRuleSets(list)

	// ---- rule_set：保留用户自建项，重建 sbx 项 ----
	ruleSets := make([]any, 0, len(needed))
	if raw, ok := route["rule_set"].([]any); ok {
		for _, it := range raw {
			m, isMap := it.(map[string]any)
			if !isMap {
				ruleSets = append(ruleSets, it)
				continue
			}
			if tag, _ := m["tag"].(string); strings.HasPrefix(tag, ruleSetTagPrefix) {
				continue
			}
			ruleSets = append(ruleSets, it)
		}
	}
	for _, name := range needed {
		ruleSets = append(ruleSets, map[string]any{
			"tag":    RuleSetTagOf(name),
			"type":   "local",
			"format": "source",
			"path":   ruleset.FilePath(ruleSetDir, name),
		})
	}

	// ---- rules：先放 sbx 白名单规则，再保留用户规则 ----
	userRules := make([]any, 0)
	if raw, ok := route["rules"].([]any); ok {
		for _, it := range raw {
			if m, isMap := it.(map[string]any); isMap && isManagedInboundList(m["inbound"]) {
				continue
			}
			userRules = append(userRules, it)
		}
	}
	managed := make([]any, 0, len(limited)*3)
	for _, n := range limited {
		tag := TagOf(IDString(n))
		allow := AllowOf(n)
		names := make([]any, len(allow))
		for i, name := range allow {
			names[i] = RuleSetTagOf(name)
		}
		// 1) 先放行 DNS。
		//
		// 相当多的客户端会把域名解析也交给代理；若不在白名单之前特判，DNS 查询
		// 会落到下面的兜底 reject 上，用户连"域名 → IP"这一步都做不到，表现为
		// 「这个节点什么网站都打不开」——而症状与白名单里勾了什么毫无关系，
		// 排查成本极高。DNS 查询本身不承载网页内容，放行不削弱限制意图。
		managed = append(managed, map[string]any{
			"inbound":  []any{tag},
			"port":     []any{53},
			"action":   "route",
			"outbound": final,
		})
		// 2) 白名单内的目标放行。
		managed = append(managed, map[string]any{
			"inbound":  []any{tag},
			"rule_set": names,
			"action":   "route",
			"outbound": final,
		})
		// 3) 其余一律拒绝。必须最后，reject 是无条件匹配。
		managed = append(managed, map[string]any{
			"inbound": []any{tag},
			"action":  "reject",
		})
	}

	// 清理空键，避免给没用到这些字段的配置留下无意义的 "rules": []。
	if len(ruleSets) == 0 {
		delete(route, "rule_set")
	} else {
		route["rule_set"] = ruleSets
	}
	if len(managed)+len(userRules) == 0 {
		delete(route, "rules")
	} else {
		route["rules"] = append(managed, userRules...)
	}
	return nil
}
