package ruleset

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// 上游 blackmatrix7/ios_rule_script 提供的是 Surge 的「一行一条」文本规则，
// sing-box 用的是自己的规则集格式。本文件负责两者的转换。
//
// 转换是**有损的**：IP-ASN / USER-AGENT / PROCESS-NAME / URL-REGEX 以及
// AND / OR 逻辑组合，在 sing-box 的规则集里都没有对应表达。这些行会被跳过并计数，
// 而不是硬翻译——在白名单场景下，翻译错了会**静默放行不该放行的目标**，
// 而少几条规则只是少放行几个域名。两害相权取其轻。
//
// 被跳过的统计会带出去记日志，用来回答「为什么某个站点没被放行」。

// sourceRuleVersion 是 sing-box source 规则集的版本号。
const sourceRuleVersion = 5

// sourceRule 是 sing-box source 格式里的一条规则。
//
// 每个维度必须**各自成一个 rule**：sing-box 中同一条规则内不同字段之间是 AND 关系，
// 把 domain 与 ip_cidr 放进同一条，会变成「域名匹配 且 IP 也匹配」——永远不成立。
// 空字段用 omitempty 省略，避免写出 "domain": null 这类无意义内容。
type sourceRule struct {
	Domain        []string `json:"domain,omitempty"`
	DomainSuffix  []string `json:"domain_suffix,omitempty"`
	DomainKeyword []string `json:"domain_keyword,omitempty"`
	IPCIDR        []string `json:"ip_cidr,omitempty"`
}

// sourceFile 是 sing-box source 规则集文件的完整结构。
type sourceFile struct {
	Version int          `json:"version"`
	Rules   []sourceRule `json:"rules"`
}

// domainSet 边解析边去重。用 map 而不是 slice：上游同一份文件里重复条目很常见，
// 合并多个文件（AI 归纳集就是这情况）时重复更多。
type domainSet struct {
	domain  map[string]struct{}
	suffix  map[string]struct{}
	keyword map[string]struct{}
	ipcidr  map[string]struct{}
	skipped map[string]int
}

func newDomainSet() *domainSet {
	return &domainSet{
		domain:  map[string]struct{}{},
		suffix:  map[string]struct{}{},
		keyword: map[string]struct{}{},
		ipcidr:  map[string]struct{}{},
		skipped: map[string]int{},
	}
}

// feed 处理一行。无法表达的指令记进 skipped 计数，不返回错误——
// 上游某一行用了新语法不该让整次保存失败。
func (d *domainSet) feed(raw string) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
		return
	}
	// 只切前两段：IP-CIDR 的第三段是 no-resolve 之类的修饰，直接丢弃。
	// 逻辑组合（`AND,((...),(...))`）的括号里还有逗号，会被切碎后走到 default 分支跳过。
	parts := strings.SplitN(line, ",", 3)
	if len(parts) < 2 {
		d.skip("MALFORMED")
		return
	}
	kind := strings.ToUpper(strings.TrimSpace(parts[0]))
	value := strings.TrimSpace(parts[1])
	if value == "" {
		d.skip(kind)
		return
	}
	switch kind {
	case "DOMAIN":
		d.domain[normalizeDomain(value)] = struct{}{}
	case "DOMAIN-SUFFIX":
		d.suffix[normalizeDomain(value)] = struct{}{}
	case "DOMAIN-KEYWORD":
		d.keyword[strings.ToLower(value)] = struct{}{}
	case "IP-CIDR", "IP-CIDR6":
		d.ipcidr[value] = struct{}{}
	default:
		d.skip(kind)
	}
}

func (d *domainSet) skip(kind string) { d.skipped[kind]++ }

// absorb 把另一份集合并入自己，用于把多个上游文件合成一个规则集。
func (d *domainSet) absorb(other *domainSet) {
	for k := range other.domain {
		d.domain[k] = struct{}{}
	}
	for k := range other.suffix {
		d.suffix[k] = struct{}{}
	}
	for k := range other.keyword {
		d.keyword[k] = struct{}{}
	}
	for k := range other.ipcidr {
		d.ipcidr[k] = struct{}{}
	}
	for k, v := range other.skipped {
		d.skipped[k] += v
	}
}

// build 产出排序后的 source 规则集。排序让同一份输入永远得到字节一致的输出，
// 既便于比对，也避免每次重新生成都产生无意义的文件改动。
func (d *domainSet) build() sourceFile {
	f := sourceFile{Version: sourceRuleVersion}
	if v := sortedKeys(d.domain); len(v) > 0 {
		f.Rules = append(f.Rules, sourceRule{Domain: v})
	}
	if v := sortedKeys(d.suffix); len(v) > 0 {
		f.Rules = append(f.Rules, sourceRule{DomainSuffix: v})
	}
	if v := sortedKeys(d.keyword); len(v) > 0 {
		f.Rules = append(f.Rules, sourceRule{DomainKeyword: v})
	}
	if v := sortedKeys(d.ipcidr); len(v) > 0 {
		f.Rules = append(f.Rules, sourceRule{IPCIDR: v})
	}
	return f
}

// ruleCount 返回可表达规则的总条数，用来判断一份文本是不是「什么都没解析出来」。
func (d *domainSet) ruleCount() int {
	return len(d.domain) + len(d.suffix) + len(d.keyword) + len(d.ipcidr)
}

// parseSurgeList 解析一份 Surge 规则文本。
func parseSurgeList(data []byte) (*domainSet, error) {
	set := newDomainSet()
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		set.feed(sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取规则文本失败: %w", err)
	}
	return set, nil
}

// marshalSourceFile 输出写入磁盘的内容：带缩进，方便运维直接阅读与手改。
func marshalSourceFile(f sourceFile) ([]byte, error) {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// normalizeDomain 统一域名写法：上游偶尔会写 `*.example.com` 或 `.example.com`，
// sing-box 的 domain / domain_suffix 不接受这两个前缀，必须去掉，否则规则静默失配。
func normalizeDomain(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	v = strings.TrimPrefix(v, "*.")
	v = strings.TrimPrefix(v, ".")
	return strings.TrimSuffix(v, ".")
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
