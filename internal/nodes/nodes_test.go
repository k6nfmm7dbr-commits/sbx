package nodes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- PyQuote / 编码 -------------------------------------------------------

func TestPyQuote(t *testing.T) {
	cases := []struct {
		in, safe, want string
	}{
		{"abcXYZ019._~-", "", "abcXYZ019._~-"},
		{"a b", "/", "a%20b"},
		{"a/b", "/", "a/b"},
		{"a/b", "", "a%2Fb"},
		{"p@ss w:rd/+", "/", "p%40ss%20w%3Ard/%2B"},
		{"测试", "", "%E6%B5%8B%E8%AF%95"},
	}
	for _, c := range cases {
		if got := PyQuote(c.in, c.safe); got != c.want {
			t.Errorf("PyQuote(%q,%q)=%q, want %q", c.in, c.safe, got, c.want)
		}
	}
	if got := PyQuotePlus("a b+c"); got != "a+b%2Bc" {
		t.Errorf("PyQuotePlus: %q", got)
	}
}

// ---- 分享链接：与 Python reference 金标对拍 --------------------------------

type linkCase struct {
	Name     string `json:"name"`
	Node     Node   `json:"node"`
	Host     string `json:"host"`
	Suffix   string `json:"suffix"`
	Expected string `json:"expected"`
}

func TestLinksGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "link_fixtures.json"))
	if err != nil {
		t.Fatalf("读取链接金标失败（internal/nodes/testdata/link_fixtures.json 静态快照）: %v", err)
	}
	var doc struct {
		Cases []linkCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	store := &Store{AppDir: dir, SBConf: filepath.Join(dir, "config.json")}

	for _, tc := range doc.Cases {
		got := store.LinkFor(tc.Node, tc.Host, tc.Suffix)
		if got != tc.Expected {
			t.Errorf("%s:\n  got  %s\n  want %s", tc.Name, got, tc.Expected)
		}
	}
}

func TestURIHost(t *testing.T) {
	if URIHost("2001:db8::1") != "[2001:db8::1]" || URIHost("[x]:1") != "[x]:1" ||
		URIHost("1.2.3.4") != "1.2.3.4" {
		t.Error("URIHost 括号规则错误")
	}
}

// ---- inbound / rebuild：与 Python 结构对拍（语义比较，键序无关） --------------

func normalize(t *testing.T, v any) any {
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

func TestInboundGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "inbound_fixtures.json"))
	if err != nil {
		t.Fatalf("读取 inbound 金标失败: %v", err)
	}
	var doc struct {
		InboundsBuilt []map[string]any `json:"inbounds_built"`
		RebuildResult map[string]any   `json:"rebuild_result"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	list := []Node{
		{"id": json.Number("1"), "type": "vless", "port": json.Number("443"),
			"uuid": strings.Repeat("u", 16), "sni": "www.microsoft.com",
			"private_key": "PK1", "short_id": "sid", "flow": "xtls-rprx-vision", "name": "v1"},
		{"id": json.Number("2"), "type": "shadowsocks", "port": json.Number("8388"),
			"method": "2022-blake3-aes-128-gcm", "password": "pw2"},
		{"id": json.Number("3"), "type": "trojan", "port": json.Number("8443"),
			"password": "pw3", "sni": "www.bing.com",
			"cert": "/etc/sbx/certs/cert.pem", "key": "/etc/sbx/certs/key.pem"},
		{"id": json.Number("4"), "type": "anytls", "port": json.Number("9443"),
			"password": "pw4", "sni": "example.org",
			"cert": "/etc/sbx/certs/cert.pem", "key": "/etc/sbx/certs/key.pem"},
	}
	for i, n := range list {
		got, err := BuildInbound(n)
		if err != nil {
			t.Fatal(err)
		}
		want := doc.InboundsBuilt[i]
		if !reflectDeepEqual(normalize(t, got), normalize(t, want)) {
			t.Errorf("inbound[%d] 与 Python 不一致:\n got %s\nwant %s",
				i, mustJSON(got), mustJSON(want))
		}
	}

	// rebuild_config：保留用户自定义 inbound/outbound/route
	dir := t.TempDir()
	store := &Store{AppDir: dir, SBConf: filepath.Join(dir, "config.json")}
	sbBase := `{
	  "log": {"level": "warn"},
	  "inbounds": [
	    {"type": "direct", "tag": "user-custom-in", "listen": "127.0.0.1", "listen_port": 1080},
	    {"type": "vless", "tag": "sbx-n99", "listen": "::", "listen_port": 1}
	  ],
	  "outbounds": [{"type": "block", "tag": "block"}],
	  "route": {"rules": [{"inbound": "user-custom-in", "outbound": "block"}]}
	}`
	if err := os.WriteFile(store.SBConf, []byte(sbBase), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := RebuildConfig(store, list)
	if err != nil {
		t.Fatal(err)
	}
	if !reflectDeepEqual(normalize(t, cfg), normalize(t, doc.RebuildResult)) {
		t.Errorf("rebuild_config 与 Python 不一致:\n got %s\nwant %s",
			mustJSON(cfg), mustJSON(doc.RebuildResult))
	}
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func reflectDeepEqual(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

// ---- NextID 单调不回收 ------------------------------------------------------

func TestNextIDMonotonic(t *testing.T) {
	dir := t.TempDir()
	store := &Store{AppDir: dir, SBConf: filepath.Join(dir, "c.json")}

	nid, err := NextID(store, nil)
	if err != nil || nid != 1 {
		t.Fatalf("首个 id=%d err=%v, want 1", nid, err)
	}
	list := []Node{{"id": json.Number("1"), "type": "vless", "port": json.Number("443")}}
	if nid, _ = NextID(store, list); nid != 2 {
		t.Fatalf("第二个 id=%d, want 2", nid)
	}
	// 删除节点后不复用
	if nid, _ = NextID(store, nil); nid != 3 {
		t.Fatalf("删除后 id=%d, want 3(不得复用)", nid)
	}
	// 手工大 id 兜底
	big := []Node{{"id": json.Number("10"), "type": "vless", "port": json.Number("80")}}
	if nid, _ = NextID(store, big); nid != 11 {
		t.Fatalf("大 id 兜底 id=%d, want 11", nid)
	}
}

// ---- CLI 全流程 ------------------------------------------------------------

func newTestCLI(t *testing.T) (*CLI, *Store, string) {
	dir := t.TempDir()
	store := &Store{AppDir: dir, SBConf: filepath.Join(dir, "sing-box", "config.json")}
	if err := os.MkdirAll(filepath.Dir(store.SBConf), 0o755); err != nil {
		t.Fatal(err)
	}
	base := `{"log":{"level":"warn"},"inbounds":[],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}`
	if err := os.WriteFile(store.SBConf, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{Store: store, Stdout: &strings.Builder{}, Stderr: &strings.Builder{}}
	return cli, store, dir
}

func (c *CLI) out() string { return c.Stdout.(*strings.Builder).String() }
func (c *CLI) err() string { return c.Stderr.(*strings.Builder).String() }

// run 每次清空输出缓冲后执行，避免断言被历史输出干扰。
func (c *CLI) run(args []string) int {
	c.Stdout = &strings.Builder{}
	c.Stderr = &strings.Builder{}
	return c.Run(args)
}

// runGetInfo 执行 info 子命令并返回输出内容（含 method 字段）。
func (c *CLI) runGetInfo(id string) string {
	c.run([]string{"info", id})
	return c.out()
}

func TestCLIRoundtrip(t *testing.T) {
	cli, store, _ := newTestCLI(t)

	rc := cli.run([]string{"add", "shadowsocks", "--port", "8388",
		"--method", "2022-blake3-aes-128-gcm", "--password", "pw", "--name", "ss-1"})
	if rc != exitOK {
		t.Fatalf("add rc=%d err=%s", rc, cli.err())
	}
	var added struct {
		ID             json.Number `json:"id"`
		Candidate      string      `json:"candidate"`
		NodesCandidate string      `json:"nodes_candidate"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(cli.out())), &added); err != nil {
		t.Fatalf("add 输出解析失败: %v (%s)", err, cli.out())
	}
	if _, err := os.Stat(added.Candidate); err != nil {
		t.Error("候选配置未生成")
	}
	if _, err := os.Stat(added.NodesCandidate); err != nil {
		t.Error("候选 nodes 未生成")
	}
	if _, err := os.Stat(store.NodesPath()); !os.IsNotExist(err) {
		t.Error("commit 前 nodes.json 不应存在")
	}

	if rc = cli.run([]string{"commit"}); rc != exitOK || strings.TrimSpace(cli.out()) != "ok" {
		t.Fatalf("commit 失败 rc=%d", rc)
	}
	if rc = cli.run([]string{"count"}); rc != exitOK || strings.TrimSpace(cli.out()) != "1" {
		t.Fatalf("count=%s", cli.out())
	}
	if rc = cli.run([]string{"last"}); rc != exitOK || strings.TrimSpace(cli.out()) != "1" {
		t.Fatalf("last=%s", cli.out())
	}

	// 端口占用判断
	if rc = cli.run([]string{"port-used", "8388"}); rc != exitOK {
		t.Error("port-used 8388 应返回 0")
	}
	if rc = cli.run([]string{"port-used", "12345"}); rc != exitErr {
		t.Error("port-used 空闲端口应返回 1")
	}

	// edit：端口冲突检测
	cli2, _, _ := newTestCLI(t)
	_ = cli2
	cli.Stdout = &strings.Builder{}
	cli.Stderr = &strings.Builder{}
	if rc = cli.run([]string{"add", "trojan", "--port", "8443", "--password", "x",
		"--sni", "a.com"}); rc != exitOK {
		t.Fatalf("第二个 add 失败: %s", cli.err())
	}
	cli.run([]string{"commit"})
	rc = cli.run([]string{"edit", "2", "--port", "8388"})
	if rc != exitErr || !strings.Contains(cli.err(), "已被节点") {
		t.Errorf("端口冲突应报错, rc=%d err=%s", rc, cli.err())
	}
	// edit：ss 无 SNI
	rc = cli.run([]string{"edit", "1", "--sni", "b.com"})
	if rc != exitErr || !strings.Contains(cli.err(), "没有 SNI 可改") {
		t.Errorf("ss 改 SNI 应被拒绝, rc=%d err=%s", rc, cli.err())
	}
	// edit 成功
	rc = cli.run([]string{"edit", "1", "--port", "8399"})
	if rc != exitOK {
		t.Fatalf("edit 失败: %s", cli.err())
	}
	var edited struct {
		ID      int      `json:"id"`
		Changed []string `json:"changed"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(cli.out())), &edited)
	if len(edited.Changed) != 1 || edited.Changed[0] != "端口→8399" {
		t.Errorf("changed 文案异常: %v", edited.Changed)
	}

	// info（edit 只写候选，需先 commit 生效——与 shell 流程一致）
	cli.run([]string{"commit"})
	if rc = cli.run([]string{"info", "1"}); rc != exitOK {
		t.Fatal("info 失败")
	}
	if cli.out() != "shadowsocks\t\t8399\t2022-blake3-aes-128-gcm\n" {
		t.Errorf("info 输出异常: %q", cli.out())
	}

	// remove
	rc = cli.run([]string{"remove", "2"})
	if rc != exitOK {
		t.Fatalf("remove 失败: %s", cli.err())
	}
	cli.run([]string{"commit"})
	cli.run([]string{"count"})
	if strings.TrimSpace(cli.out()) != "1" {
		t.Errorf("删除后 count 应为 1, got %s", cli.out())
	}

	// links 输出格式
	if rc = cli.run([]string{"links", "1", "--host", "5.6.7.8"}); rc != exitOK {
		t.Fatal("links 失败")
	}
	lines := strings.Split(strings.TrimRight(cli.out(), "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "### ss-1 (shadowsocks, 端口 ") ||
		!strings.HasPrefix(lines[1], "ss://") {
		t.Errorf("links 格式异常: %q", cli.out())
	}

	// rollback 清理候选
	cli.run([]string{"sync"})
	if rc = cli.run([]string{"rollback"}); rc != exitOK {
		t.Fatal("rollback 失败")
	}
	if _, err := os.Stat(store.SBConf + ".candidate"); !os.IsNotExist(err) {
		t.Error("候选配置应已删除")
	}
}

func TestCLIUsageErrors(t *testing.T) {
	cli, _, _ := newTestCLI(t)
	if rc := cli.run([]string{"add", "vmess", "--port", "1"}); rc != exitUsage {
		t.Errorf("不支持类型应 usage 错误, got %d", rc)
	}
	if rc := cli.run([]string{"add", "vless"}); rc != exitUsage {
		t.Errorf("缺 --port 应 usage 错误, got %d", rc)
	}
	if rc := cli.run([]string{"remove", "99"}); rc != exitErr ||
		!strings.Contains(cli.err(), "未找到节点") {
		t.Errorf("remove 不存在节点应报错, got %d %s", rc, cli.err())
	}
}
