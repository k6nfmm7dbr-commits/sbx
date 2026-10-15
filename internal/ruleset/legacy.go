package ruleset

import "strings"

// legacyAliases 把旧上游（Repcz/Tool）用过的规则集名映射到当前清单。
//
// 换上游（Repcz/Tool → blackmatrix7/ios_rule_script）之后，有几个名字对不上：
//
//   - Github：新上游写作 GitHub，而名字要用来拼文件名，必须完全一致
//   - OpenAI / Claude：新清单把它们并入了「AI」归纳集，不再单独提供
//   - xAI：新上游没有这个规则集，但 Grok 的域名（grok.com）在 Twitter 里
//   - TronLink：新上游没有，最接近的是加密相关合集
//   - Bilibili / WeChat / ChinaDomain / ChinaIP：新清单按约定不再收录国内站点
//
// 映射发生在**读取时**（见 nodes.AllowOf），而不是去改写 nodes.json：
// 节点配置文件是 root 可编辑的本地文件，让读取端做一次幂等翻译，
// 比在启动流程里改用户的文件安全得多。用户下次保存节点时，映射结果
// 会随面板上的勾选自然写回。
//
// AI 归纳集的成员（OpenAI / Claude / Gemini …）由 init 自动补进来，
// 不在这里手写——它们是「被归并」而不是「被改名」，加一个成员不该再改这份表。
var legacyAliases = map[string][]string{
	"Github":      {"GitHub"},
	"xAI":         {"Twitter"},
	"TronLink":    {"Crypto"},
	"Bilibili":    {},
	"WeChat":      {},
	"ChinaDomain": {},
	"ChinaIP":     {},
}

func init() {
	// AI 归纳集的成员在清单里不再单独提供。老节点若勾过其中某个名字，
	// 一律映射到 AI，而不是让它静默失效。
	it, ok := itemOf("AI")
	if !ok {
		return
	}
	for _, s := range it.Sources {
		if _, exists := legacyAliases[s]; !exists {
			legacyAliases[s] = []string{"AI"}
		}
	}
}

// MigrateAllow 把一组（可能来自旧上游的）规则集名翻译成当前清单里的名字。
//
// 返回映射后的名字（已按清单去重）与被丢弃的名字。丢弃发生在两种情况下：
// 名字既不在清单里、也没有映射（说明是手工写进 nodes.json 的乱值），
// 或者它映射到空集（例如国内站点在新清单里不再收录）。
//
// 已知名字原样保留，因此这个函数是幂等的——反复调用不会越传越少。
func MigrateAllow(names []string) (mapped []string, dropped []string) {
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" || seen[name] || !Known(name) {
			return
		}
		seen[name] = true
		mapped = append(mapped, name)
	}
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if Known(name) {
			add(name)
			continue
		}
		repl, ok := legacyAliases[name]
		if !ok || len(repl) == 0 {
			dropped = append(dropped, name)
			continue
		}
		for _, r := range repl {
			add(r)
		}
	}
	return mapped, dropped
}
