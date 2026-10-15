package ruleset

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 清单 ----

// 清单里出现重复名字会让面板渲染出两个同名复选框，选中哪一个都只算一项，
// 用户会以为自己的选择丢了。
func TestCatalogHasNoDuplicates(t *testing.T) {
	seen := map[string]string{}
	for _, g := range Catalog() {
		for _, it := range g.Items {
			if it.Name == "" || it.Label == "" {
				t.Errorf("条目缺名字或标签: %#v", it)
			}
			// 名字会拼进 URL 与文件路径，必须是保守字符集。
			for _, r := range it.Name {
				ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
					(r >= '0' && r <= '9') || r == '_' || r == '-'
				if !ok {
					t.Errorf("规则集名含不安全字符: %q", it.Name)
				}
			}
			if prev, dup := seen[it.Name]; dup {
				t.Errorf("规则集 %q 重复出现（已在 %q）", it.Name, prev)
			}
			seen[it.Name] = g.Name
		}
	}
}

// 清单是按「是否需要代理才能访问」筛过的：国内站点本来就不需要走代理，
// 收进白名单只会让面板变长。生成器把排除名单一并产出，这里断言它确实生效了。
func TestCatalogExcludesMainlandAndPolicySets(t *testing.T) {
	for _, name := range catalogExcluded {
		if Known(name) {
			t.Errorf("规则集 %q 不应出现在清单里", name)
		}
	}
	for _, name := range []string{"Google", "TikTok", "Netflix", "Steam", "AI"} {
		if !Known(name) {
			t.Errorf("规则集 %q 应保留在清单里", name)
		}
	}
}

// AI 相关在上游是 8 个独立文件，清单里归并成一项：成员不能再单独出现，
// 否则面板上会同时有「AI」和「OpenAI」，选哪个都让人犹豫。
func TestAIItemMergesUpstreamSources(t *testing.T) {
	it, ok := itemOf("AI")
	if !ok {
		t.Fatal("AI 项应在清单里")
	}
	if len(it.Sources) < 2 {
		t.Fatalf("AI 项应合并多个上游文件，实得 %v", it.Sources)
	}
	dup := map[string]bool{}
	for _, s := range it.Sources {
		if dup[s] {
			t.Errorf("AI 的成员 %q 重复", s)
		}
		dup[s] = true
		if Known(s) {
			t.Errorf("AI 的成员 %q 不应再单独出现在清单里", s)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
		err  bool
	}{
		{"去重并保序", []string{"Google", "TikTok", "Google"}, []string{"Google", "TikTok"}, false},
		{"忽略空白", []string{" Google ", "", "  ", "TikTok"}, []string{"Google", "TikTok"}, false},
		{"空输入", nil, []string{}, false},
		{"未知名字拒绝", []string{"Google", "NotARealSet"}, nil, true},
		{"路径穿越拒绝", []string{"../../etc/passwd"}, nil, true},
		{"大小写敏感", []string{"google"}, nil, true},
	}
	for _, c := range cases {
		got, err := Validate(c.in)
		if c.err {
			if err == nil {
				t.Errorf("%s: 期望报错，实得 %v", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 意外错误 %v", c.name, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: 得到 %v，期望 %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: 得到 %v，期望 %v", c.name, got, c.want)
				break
			}
		}
	}
}

func TestKnownAndLabel(t *testing.T) {
	if !Known("Google") {
		t.Error("Google 应在清单内")
	}
	if Known("Nope") {
		t.Error("未知名字不应通过")
	}
	if Label("Google") == "" {
		t.Error("应有中文标签")
	}
}

func TestFilePathUnderDir(t *testing.T) {
	dir := Dir("/etc/sing-box/config.json")
	if dir != "/etc/sing-box/rulesets" {
		t.Errorf("目录推导错误: %s", dir)
	}
	if p := FilePath(dir, "Google"); p != "/etc/sing-box/rulesets/Google.json" {
		t.Errorf("规则集路径推导错误: %s", p)
	}
	if p := ListPath(dir, "Google"); p != "/etc/sing-box/rulesets/Google.list" {
		t.Errorf("离线导入路径推导错误: %s", p)
	}
}

// ---- 下载、转换与缓存 ----

// surgeBody 造一份可被解析的假规则文本。
func surgeBody(domainSuffix string) []byte {
	return []byte("# NAME: Fake\nDOMAIN-SUFFIX," + domainSuffix + "\nIP-CIDR,1.2.3.0/24,no-resolve\n")
}

func TestEnsureDownloadsAndCompiles(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if !strings.HasSuffix(r.URL.Path, "/Google/Google.list") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(surgeBody("google.com"))
	}))
	defer srv.Close()
	t.Setenv("SBX_RULESET_BASE", srv.URL)

	dir := t.TempDir()
	if err := Ensure(context.Background(), dir, []string{"Google"}); err != nil {
		t.Fatalf("首次下载应成功: %v", err)
	}

	// 落盘的是转换后的 sing-box 规则集，不是上游原文。
	raw, err := os.ReadFile(FilePath(dir, "Google"))
	if err != nil {
		t.Fatal(err)
	}
	var f sourceFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘内容应为合法 JSON: %v\n%s", err, raw)
	}
	if f.Version != sourceRuleVersion {
		t.Errorf("版本号应为 %d，实得 %d", sourceRuleVersion, f.Version)
	}
	if !containsDomain(f, "google.com") {
		t.Errorf("应包含上游的域名规则: %s", raw)
	}
	// 0644：sing-box 可能以非 root 运行，必须可读。
	st, _ := os.Stat(FilePath(dir, "Google"))
	if st.Mode().Perm() != 0o644 {
		t.Errorf("权限应为 0644，实得 %v", st.Mode().Perm())
	}

	// 第二次不应再发请求——缓存命中。
	before := hits
	if err := Ensure(context.Background(), dir, []string{"Google"}); err != nil {
		t.Fatal(err)
	}
	if hits != before {
		t.Errorf("已缓存时不应重复下载（请求数 %d → %d）", before, hits)
	}
}

// 手工放入的规则集必须被尊重：运维可能用内网镜像或自维护名单，
// 自动下载覆盖掉它们会造成难以察觉的配置漂移。
func TestEnsureKeepsManuallyPlacedFile(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(surgeBody("from-upstream.com"))
	}))
	defer srv.Close()
	t.Setenv("SBX_RULESET_BASE", srv.URL)

	dir := t.TempDir()
	mine := []byte("{\"version\":5,\"rules\":[{\"domain\":[\"mine.example\"]}]}\n")
	if err := os.WriteFile(FilePath(dir, "Google"), mine, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(context.Background(), dir, []string{"Google"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(FilePath(dir, "Google"))
	if string(got) != string(mine) {
		t.Error("不应覆盖已有的本地文件")
	}
	if hits != 0 {
		t.Error("已就绪时不应发起请求")
	}
}

// 离线导入：运维只需要放一份 Surge 文本，转换由 sbx 负责。
func TestEnsureCompilesManuallyPlacedList(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(surgeBody("from-upstream.com"))
	}))
	defer srv.Close()
	t.Setenv("SBX_RULESET_BASE", srv.URL)

	dir := t.TempDir()
	if err := os.WriteFile(ListPath(dir, "Google"), surgeBody("my-own-list.com"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(context.Background(), dir, []string{"Google"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(FilePath(dir, "Google"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "my-own-list.com") {
		t.Errorf("应使用手动放入的文本: %s", raw)
	}
	if hits != 0 {
		t.Error("有手动文本时不应访问上游")
	}
}

// AI 项由多个上游文件合并而成，任一文件缺失都应整体失败——
// 少一个来源意味着少放行一组域名，而用户以为自己勾的是「全部 AI」。
func TestEnsureMergesMultipleSources(t *testing.T) {
	sources := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sources = append(sources, r.URL.Path)
		_, _ = w.Write(surgeBody(strings.ToLower(strings.TrimSuffix(filepath.Base(filepath.Dir(r.URL.Path)), ".list")) + ".example"))
	}))
	defer srv.Close()
	t.Setenv("SBX_RULESET_BASE", srv.URL)

	dir := t.TempDir()
	if err := Ensure(context.Background(), dir, []string{"AI"}); err != nil {
		t.Fatalf("合并下载应成功: %v", err)
	}
	it, _ := itemOf("AI")
	if len(sources) != len(it.Sources) {
		t.Errorf("应逐个下载 %d 个来源，实发 %d 次请求", len(it.Sources), len(sources))
	}
	raw, _ := os.ReadFile(FilePath(dir, "AI"))
	var f sourceFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘内容应为合法 JSON: %v", err)
	}
	// 每个来源贡献一个域名，合并后应全部出现。
	for _, s := range it.Sources {
		if !containsDomain(f, strings.ToLower(s)+".example") {
			t.Errorf("合并结果缺少来源 %s 的规则", s)
		}
	}
}

// 下载失败必须整体失败：带着缺失的规则集去写配置会让 sing-box 起不来，
// 那是全节点一起挂。
func TestEnsureFailsOnBadContent(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		code int
	}{
		{"404", nil, http.StatusNotFound},
		{"错误页", []byte("<html>not found</html>"), http.StatusOK},
		{"空内容", []byte(""), http.StatusOK},
		// 能下载到、但里面没有任何 sing-box 表达得了的条件。
		{"无可表达规则", []byte("# 只有注释\nUSER-AGENT,OnlyThis\nIP-ASN,20473\n"), http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.code)
				if c.body != nil {
					_, _ = w.Write(c.body)
				}
			}))
			defer srv.Close()
			t.Setenv("SBX_RULESET_BASE", srv.URL)

			dir := t.TempDir()
			err := Ensure(context.Background(), dir, []string{"Google"})
			if err == nil {
				t.Fatal("应当失败")
			}
			if !strings.Contains(err.Error(), "Google") {
				t.Errorf("错误信息应指明是哪个规则集: %v", err)
			}
			// 失败的下载绝不能留下半成品文件，否则下次 Cached() 会误判为就绪。
			if _, statErr := os.Stat(FilePath(dir, "Google")); statErr == nil {
				t.Error("下载失败时不应留文件")
			}
		})
	}
}

// 名字非法时不应发起任何请求。
func TestEnsureRejectsUnknownBeforeNetwork(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()
	t.Setenv("SBX_RULESET_BASE", srv.URL)

	if err := Ensure(context.Background(), t.TempDir(), []string{"Evil"}); err == nil {
		t.Fatal("未知规则集应被拒绝")
	}
	if hits != 0 {
		t.Errorf("未知名字不应发起请求，实发 %d 次", hits)
	}
}

func TestEnsureEmptyIsNoop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-created")
	if err := Ensure(context.Background(), dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("没有规则集时不应创建目录")
	}
}

func TestCachedRejectsPartialFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(FilePath(dir, "Google"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Cached(dir, "Google") {
		t.Error("不是 JSON 对象的文件不应被视为已就绪")
	}
}

// 面板要展示四百多项的就绪状态，必须一次扫目录拿全，不能逐项开文件。
func TestCachedSetScansOnce(t *testing.T) {
	dir := t.TempDir()
	ready := []byte("{\"version\":5,\"rules\":[]}\n")
	if err := os.WriteFile(FilePath(dir, "A"), ready, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(FilePath(dir, "Empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// .list 是离线导入的源文本，不算「已就绪」。
	if err := os.WriteFile(ListPath(dir, "B"), surgeBody("b.com"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := CachedSet(dir)
	if !got["A"] {
		t.Error("A 应被报告为就绪")
	}
	if got["Empty"] {
		t.Error("空文件不应被报告为就绪")
	}
	if got["B"] {
		t.Error(".list 源文本不应被报告为就绪")
	}
	// 目录不存在时不应 panic，返回空集合即可。
	if len(CachedSet(filepath.Join(dir, "nope"))) != 0 {
		t.Error("目录不存在时应返回空集合")
	}
}

func containsDomain(f sourceFile, want string) bool {
	for _, r := range f.Rules {
		for _, d := range r.DomainSuffix {
			if d == want {
				return true
			}
		}
	}
	return false
}
