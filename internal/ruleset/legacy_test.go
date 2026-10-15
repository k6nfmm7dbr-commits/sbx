package ruleset

import "testing"

func equal(a, b []string) bool {
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

// 换上游后老节点里的名字必须仍有归宿：要么本身还在清单里，要么映射到最接近的项，
// 要么被明确丢弃——最坏的情况是「名字留着但悄悄不生效」，那会让用户以为限制还在。
func TestMigrateAllow(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    []string
		dropped []string
	}{
		{"已知名字原样保留", []string{"Google", "TikTok"}, []string{"Google", "TikTok"}, nil},
		{"旧名大小写修正", []string{"Github"}, []string{"GitHub"}, nil},
		{"AI 成员归并到 AI", []string{"OpenAI", "Claude", "Gemini", "Copilot"}, []string{"AI"}, nil},
		{"xAI 归到 Twitter", []string{"xAI"}, []string{"Twitter"}, nil},
		{"TronLink 归到 Crypto", []string{"TronLink"}, []string{"Crypto"}, nil},
		{"国内站点被丢弃", []string{"Google", "Bilibili", "WeChat"}, []string{"Google"}, []string{"Bilibili", "WeChat"}},
		{"乱值被丢弃", []string{"NotAThing"}, nil, []string{"NotAThing"}},
		{"映射结果去重", []string{"AI", "OpenAI", "Google", "Google"}, []string{"AI", "Google"}, nil},
		{"忽略空白", []string{" ", "\t", "Google"}, []string{"Google"}, nil},
	}
	for _, c := range cases {
		got, dropped := MigrateAllow(c.in)
		if !equal(got, c.want) {
			t.Errorf("%s: 映射结果 %v，期望 %v", c.name, got, c.want)
		}
		if !equal(dropped, c.dropped) {
			t.Errorf("%s: 丢弃名单 %v，期望 %v", c.name, dropped, c.dropped)
		}
	}
}

// 迁移会在每次读取节点时执行，必须幂等——否则反复保存会让白名单越来越短。
func TestMigrateAllowIsIdempotent(t *testing.T) {
	once, _ := MigrateAllow([]string{"Github", "OpenAI", "xAI", "Bilibili", "Google"})
	twice, dropped := MigrateAllow(once)
	if !equal(once, twice) {
		t.Errorf("二次映射结果变了: %v → %v", once, twice)
	}
	if len(dropped) != 0 {
		t.Errorf("二次映射不该再有丢弃项: %v", dropped)
	}
}

// 每个映射目标都必须是清单里的真名字，否则会被静默丢掉。
func TestLegacyAliasTargetsExist(t *testing.T) {
	for from, to := range legacyAliases {
		for _, name := range to {
			if !Known(name) {
				t.Errorf("别名 %q → %q 的目标不在清单里", from, name)
			}
		}
	}
}
