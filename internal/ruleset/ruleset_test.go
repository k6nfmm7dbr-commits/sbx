package ruleset

import (
	"context"
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
	p := FilePath(dir, "Google")
	if p != "/etc/sing-box/rulesets/Google.srs" {
		t.Errorf("路径推导错误: %s", p)
	}
}

// ---- 下载与缓存 ----

// srsBlob 造一个满足最小校验的假规则集。
func srsBlob(n int) []byte {
	b := make([]byte, n)
	copy(b, srsMagic)
	b[3] = 2
	for i := 4; i < n; i++ {
		b[i] = byte(i)
	}
	return b
}

func TestEnsureDownloadsAndCaches(t *testing.T) {
	blob := srsBlob(256)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if !strings.HasSuffix(r.URL.Path, "/Google.srs") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(blob)
	}))
	defer srv.Close()
	t.Setenv("SBX_RULESET_BASE", srv.URL+"/Rules")

	dir := t.TempDir()
	if err := Ensure(context.Background(), dir, []string{"Google"}); err != nil {
		t.Fatalf("首次下载应成功: %v", err)
	}
	got, err := os.ReadFile(FilePath(dir, "Google"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(blob) {
		t.Error("落盘内容与下载内容不一致")
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

// 手工放入的规则集必须被尊重：运维可能用自建镜像或自维护名单，
// 自动下载覆盖掉它们会造成难以察觉的配置漂移。
func TestEnsureKeepsManuallyPlacedFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(srsBlob(128))
	}))
	defer srv.Close()
	t.Setenv("SBX_RULESET_BASE", srv.URL)

	dir := t.TempDir()
	mine := srsBlob(200)
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
		{"过小", []byte("SRS"), http.StatusOK},
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

func TestCachedFalseForPartialFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(FilePath(dir, "Google"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Cached(dir, "Google") {
		t.Error("过小的文件不应被视为已就绪")
	}
}
