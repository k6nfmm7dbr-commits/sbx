package ruleset

import (
	"encoding/json"
	"strings"
	"testing"
)

// 上游规则文本的实际形态：注释行、空行、各种指令混在一起，
// 还有 sing-box 表达不了的条件。这里把「该收的收、该跳的跳」钉死。
func TestParseSurgeList(t *testing.T) {
	src := strings.Join([]string{
		"# NAME: Demo",
		"# AUTHOR: someone",
		"",
		"DOMAIN,Exact.Example.com",
		"DOMAIN-SUFFIX,example.com",
		"DOMAIN-SUFFIX,*.wild.example.com",
		"DOMAIN-SUFFIX,example.com", // 重复项
		"DOMAIN-KEYWORD,OpenAI",
		"IP-CIDR,1.2.3.0/24,no-resolve",
		"IP-CIDR6,2402:4e00::/32,no-resolve",
		// 以下都无法在 sing-box 的规则集里表达，应当跳过而不是硬翻
		"IP-ASN,20473,no-resolve",
		"USER-AGENT,Frodo*",
		"PROCESS-NAME,curl",
		"URL-REGEX,/\\d+/resolve",
		"AND,((USER-AGENT,Frodo*),(DOMAIN,119.29.29.29))",
		"OR,((IP-ASN,44907),(IP-ASN,59930))",
		"GEOIP,CN",
		// 畸形行
		"JUSTONE",
		"DOMAIN,",
	}, "\r\n")

	set, err := parseSurgeList([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	f := set.build()

	got := map[string][]string{}
	for _, r := range f.Rules {
		if len(r.Domain) > 0 {
			got["domain"] = r.Domain
		}
		if len(r.DomainSuffix) > 0 {
			got["domain_suffix"] = r.DomainSuffix
		}
		if len(r.DomainKeyword) > 0 {
			got["domain_keyword"] = r.DomainKeyword
		}
		if len(r.IPCIDR) > 0 {
			got["ip_cidr"] = r.IPCIDR
		}
	}

	want := map[string][]string{
		// 域名统一小写，`*.` 前缀去掉——sing-box 不接受这两种写法，
		// 留着会让规则静默失配。
		"domain":         {"exact.example.com"},
		"domain_suffix":  {"example.com", "wild.example.com"},
		"domain_keyword": {"openai"},
		"ip_cidr":        {"1.2.3.0/24", "2402:4e00::/32"},
	}
	for key, wantVals := range want {
		gotVals := got[key]
		if len(gotVals) != len(wantVals) {
			t.Errorf("%s: 得到 %v，期望 %v", key, gotVals, wantVals)
			continue
		}
		for i := range wantVals {
			if gotVals[i] != wantVals[i] {
				t.Errorf("%s: 得到 %v，期望 %v", key, gotVals, wantVals)
				break
			}
		}
	}

	// 跳过的条件要计数：排查「某个站点为什么没放行」时靠它。
	for _, kind := range []string{"IP-ASN", "USER-AGENT", "PROCESS-NAME", "URL-REGEX", "AND", "OR", "GEOIP", "DOMAIN"} {
		if set.skipped[kind] == 0 {
			t.Errorf("指令 %s 应被记为跳过", kind)
		}
	}
	if set.skipped["MALFORMED"] != 1 {
		t.Errorf("畸形行应被记为 MALFORMED，实得 %d", set.skipped["MALFORMED"])
	}
}

// sing-box 中同一条规则内不同字段是 AND 关系。domain 与 ip_cidr 若挤在一条里，
// 会变成「域名匹配 且 IP 也匹配」——永远不成立。必须各占一条。
func TestBuildKeepsFieldsInSeparateRules(t *testing.T) {
	set := newDomainSet()
	set.feed("DOMAIN,a.com")
	set.feed("IP-CIDR,1.2.3.0/24")
	set.feed("DOMAIN-SUFFIX,b.com")
	set.feed("DOMAIN-KEYWORD,kw")

	f := set.build()
	if len(f.Rules) != 4 {
		t.Fatalf("四个维度应产出四条规则，实得 %d 条: %#v", len(f.Rules), f.Rules)
	}
	for i, r := range f.Rules {
		n := 0
		if len(r.Domain) > 0 {
			n++
		}
		if len(r.DomainSuffix) > 0 {
			n++
		}
		if len(r.DomainKeyword) > 0 {
			n++
		}
		if len(r.IPCIDR) > 0 {
			n++
		}
		if n != 1 {
			t.Errorf("第 %d 条规则混了 %d 个字段: %#v", i, n, r)
		}
	}
	if f.Version != sourceRuleVersion {
		t.Errorf("版本号应为 %d，实得 %d", sourceRuleVersion, f.Version)
	}
}

// 合并（AI 归纳集就是这条路）要去重，并且产出可被 json 序列化的结构。
func TestAbsorbMergesAndDedupes(t *testing.T) {
	a := newDomainSet()
	a.feed("DOMAIN-SUFFIX,openai.com")
	a.feed("IP-ASN,1")
	b := newDomainSet()
	b.feed("DOMAIN-SUFFIX,openai.com")
	b.feed("DOMAIN-SUFFIX,claude.ai")
	b.feed("IP-ASN,2")

	a.absorb(b)
	f := a.build()

	var suffix []string
	for _, r := range f.Rules {
		suffix = append(suffix, r.DomainSuffix...)
	}
	if len(suffix) != 2 || suffix[0] != "claude.ai" || suffix[1] != "openai.com" {
		t.Errorf("合并去重后应只剩两个域名，实得 %v", suffix)
	}
	if a.skipped["IP-ASN"] != 2 {
		t.Errorf("跳过的计数应累加，实得 %d", a.skipped["IP-ASN"])
	}
	if _, err := json.Marshal(f); err != nil {
		t.Errorf("产出的结构应可序列化: %v", err)
	}
}

// 空规则集是危险的静默状态（勾了它，节点除 DNS 外什么都不放行），
// 调用方要靠 ruleCount 把它拦下来。
func TestRuleCountDetectsEmpty(t *testing.T) {
	set := newDomainSet()
	set.feed("# 只有注释")
	set.feed("")
	set.feed("USER-AGENT,OnlyThis")
	if set.ruleCount() != 0 {
		t.Errorf("没有任何可表达规则时应为 0，实得 %d", set.ruleCount())
	}
}

func TestNormalizeDomain(t *testing.T) {
	cases := map[string]string{
		"Example.COM":   "example.com",
		"*.example.com": "example.com",
		".example.com":  "example.com",
		" example.com ": "example.com",
		"example.com.":  "example.com",
	}
	for in, want := range cases {
		if got := normalizeDomain(in); got != want {
			t.Errorf("normalizeDomain(%q) = %q，期望 %q", in, got, want)
		}
	}
}
