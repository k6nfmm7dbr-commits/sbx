package service

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
)

// ---- 测试基建 -------------------------------------------------------------
//
// 这些测试通过 PATH 桩控制 nftables/iptables provider 的行为：
// 绝不触碰真实内核防火墙，也不依赖宿主机是否安装 nft/iptables。
// panel.json 强制 backend，使 DetectBackend 走确定性分支。

const validNode = `[{"id":1,"name":"n1","type":"vless","port":443}]`

type testApp struct {
	appDir    string
	confPath  string
	nftConf   string
	iptScript string
	dbPath    string
	nodesPath string
	stubLog   string
	writeConf func(listen, token string)
}

// setupApp 建立隔离环境（SBX_DIR/SBX_CONF 指向临时目录）。
func setupApp(t *testing.T, backend string) *testApp {
	t.Helper()
	a := &testApp{appDir: t.TempDir()}
	a.confPath = filepath.Join(a.appDir, "panel.json")
	a.nftConf = filepath.Join(a.appDir, "nft.conf")
	a.iptScript = filepath.Join(a.appDir, "iptables.sh")
	a.dbPath = filepath.Join(a.appDir, "traffic.db")
	a.nodesPath = filepath.Join(a.appDir, "nodes.json")
	a.stubLog = filepath.Join(a.appDir, "stub.log")

	t.Setenv("SBX_DIR", a.appDir)
	t.Setenv("SBX_CONF", a.confPath)

	a.writeConf = func(listen, token string) {
		t.Helper()
		conf := `{"db":` + mustJSON(a.dbPath) +
			`,"nodes_file":` + mustJSON(a.nodesPath) +
			`,"nft_conf":` + mustJSON(a.nftConf) +
			`,"ipt_script":` + mustJSON(a.iptScript) +
			`,"backend":"` + backend + `","listen":"` + listen +
			`","port":18099,"token":"` + token + `","tz":"UTC"}`
		mustWriteFile(t, a.confPath, conf)
	}
	return a
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// installStubs 把桩目录插到 PATH 最前。每个桩把调用记录追加到 stub.log。
func (a *testApp) installStubs(t *testing.T, nftScript string) {
	t.Helper()
	binDir := filepath.Join(a.appDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkStub := func(name, body string) {
		p := filepath.Join(binDir, name)
		full := "#!/bin/sh\necho \"" + name + " $*\" >> \"" + a.stubLog + "\"\n" + body
		if err := os.WriteFile(p, []byte(full), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkStub("iptables", "exit 0")
	mkStub("ip6tables", "exit 0")
	mkStub("nft", nftScript)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// nftListJSON 构造 `nft -j list counters` 的合法输出。
func nftListJSON() string {
	return `{"nftables":[` +
		`{"counter":{"name":"sbx_epoch_111","bytes":0,"packets":0}},` +
		`{"counter":{"name":"sbx_n1_i","bytes":100,"packets":10}},` +
		`{"counter":{"name":"sbx_n1_o","bytes":50,"packets":5}}]}`
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return string(data)
}

func stubCalls(t *testing.T, a *testApp) []string {
	t.Helper()
	data, err := os.ReadFile(a.stubLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// seedBaseline 预置 counter_state 与 meta.epoch，返回读取函数供断言不变。
func seedBaseline(t *testing.T, dbPath string) func(t *testing.T) map[string]string {
	t.Helper()
	write := func() {
		db, err := database.Open(dbPath)
		if err != nil {
			t.Fatalf("打开 DB: %v", err)
		}
		defer db.Close()
		if _, err := db.Exec(`INSERT INTO meta(k,v) VALUES('epoch','12345')`); err != nil {
			t.Fatalf("seed meta: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO counter_state(name,last_bytes,last_pkts,updated_at)
			VALUES('sbx_n1_i@v4',100,10,1700000000000)`); err != nil {
			t.Fatalf("seed counter_state: %v", err)
		}
	}
	write()
	return func(t *testing.T) map[string]string {
		t.Helper()
		db, err := database.Open(dbPath)
		if err != nil {
			t.Fatalf("重开 DB: %v", err)
		}
		defer db.Close()
		out := map[string]string{}
		rows, err := db.Query(`SELECT 'meta:'||k, v FROM meta UNION ALL
			SELECT 'cs:'||name, last_bytes||':'||last_pkts FROM counter_state`)
		if err != nil {
			t.Fatalf("查询基线: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			out[k] = v
		}
		return out
	}
}

// ---- 场景 E：nodes.json 损坏 + Apply → 失败且一切原样 ----------------------

func TestApplyAbortsOnCorruptNodesJSON(t *testing.T) {
	a := setupApp(t, "nft")
	a.writeConf("127.0.0.1", "t")
	mustWriteFile(t, a.nodesPath, "{ invalid json")
	mustWriteFile(t, a.nftConf, "SENTINEL-NFT")
	mustWriteFile(t, a.iptScript, "SENTINEL-IPT")
	readBaseline := seedBaseline(t, a.dbPath)
	want := readBaseline(t)

	// provider 桩：任何真实调用都会留下痕迹——本场景必须零调用
	a.installStubs(t, "exit 2")

	if rc := Apply(); rc == 0 {
		t.Fatal("损坏 nodes.json 下 Apply 必须失败")
	}
	if got := readFileString(t, a.nftConf); got != "SENTINEL-NFT" {
		t.Fatalf("规则文件被重建: %q", got)
	}
	if got := readFileString(t, a.iptScript); got != "SENTINEL-IPT" {
		t.Fatalf("iptables 脚本被重建: %q", got)
	}
	if calls := stubCalls(t, a); len(calls) != 0 {
		t.Fatalf("防火墙被触碰: %v", calls)
	}
	got := readBaseline(t)
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("基线 %s 被改动: %q -> %q", k, v, got[k])
		}
	}
}

// ---- 场景 F：最终采样失败 → Apply 中止、计数器保留 -------------------------

func TestApplyAbortsWhenFinalSampleFails(t *testing.T) {
	a := setupApp(t, "nft")
	a.writeConf("127.0.0.1", "t")
	mustWriteFile(t, a.nodesPath, validNode)
	mustWriteFile(t, a.nftConf, "SENTINEL-NFT")
	mustWriteFile(t, a.iptScript, "SENTINEL-IPT")
	readBaseline := seedBaseline(t, a.dbPath)
	want := readBaseline(t)

	// list(-j) 失败(非 Lookup 类)；-f 成功（不应被执行到）
	nftScript := `if [ "$1" = "-j" ]; then echo "simulated provider failure" >&2; exit 2; fi` + "\nexit 0\n"
	a.installStubs(t, nftScript)

	if rc := Apply(); rc == 0 {
		t.Fatal("最终采样失败时 Apply 必须中止")
	}
	if got := readFileString(t, a.nftConf); got != "SENTINEL-NFT" {
		t.Fatalf("规则文件被重建: %q", got)
	}
	if calls := stubCalls(t, a); len(calls) != 1 || !strings.HasPrefix(calls[0], "nft -j") {
		t.Fatalf("只应有一次采样读取, 实际: %v", calls)
	}
	got := readBaseline(t)
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("计数基线 %s 被改动: %q -> %q", k, v, got[k])
		}
	}
}

// ---- 场景 G：LookupError（首次安装）→ 仍然允许 Apply -----------------------

func TestApplyAllowsLookupErrorFirstInstall(t *testing.T) {
	a := setupApp(t, "nft")
	a.writeConf("127.0.0.1", "t")
	mustWriteFile(t, a.nodesPath, validNode)

	// list 报“不存在”(rc=1 + does not exist → ErrLookup)；-f 正常生效
	nftScript := `if [ "$1" = "-j" ]; then echo "nft: table does not exist" >&2; exit 1; fi` + "\nexit 0\n"
	a.installStubs(t, nftScript)

	if rc := Apply(); rc != 0 {
		t.Fatal("首次安装(LookupError)必须允许 Apply")
	}
	got := readFileString(t, a.nftConf)
	if !strings.Contains(got, "sbx_n1_i") {
		t.Fatalf("规则未按节点生成: %q", got)
	}
	// 采样list + 应用-f 必须都发生过（首次安装走 nft 分支直接生效）
	calls := stubCalls(t, a)
	foundList, foundApply := false, false
	for _, c := range calls {
		if strings.HasPrefix(c, "nft -j") {
			foundList = true
		}
		if strings.HasPrefix(c, "nft -f") {
			foundApply = true
		}
	}
	if !foundList || !foundApply {
		t.Fatalf("Apply 流程不完整: %v", calls)
	}
}

// ---- 正向对照：采样成功 → Apply 全流程走通 ---------------------------------

func TestApplyHappyPath(t *testing.T) {
	a := setupApp(t, "nft")
	a.writeConf("127.0.0.1", "t")
	mustWriteFile(t, a.nodesPath, validNode)

	nftScript := `if [ "$1" = "-j" ]; then cat <<'JSON'
` + nftListJSON() + `
JSON
exit 0
fi
exit 0
`
	a.installStubs(t, nftScript)

	if rc := Apply(); rc != 0 {
		t.Fatal("正常路径 Apply 应成功")
	}
	if got := readFileString(t, a.nftConf); !strings.Contains(got, "sbx_n1_i") {
		t.Fatalf("规则未生成: %q", got)
	}
}

// ---- 场景 H：epoch 必须是纳秒级且连续两次不同（无 sleep）-------------------

var epochReTest = regexp.MustCompile(`sbx_epoch_(\d+)`)

func TestRulesEpochHighPrecisionUnique(t *testing.T) {
	a := setupApp(t, "ipt")
	a.writeConf("127.0.0.1", "t")
	// nodes.json 不存在 = 全新安装合法状态
	os.Remove(a.nodesPath)

	parseEpoch := func() uint64 {
		t.Helper()
		if _, err := Rules(); err != nil {
			t.Fatalf("Rules: %v", err)
		}
		m := epochReTest.FindStringSubmatch(readFileString(t, a.nftConf))
		if m == nil {
			t.Fatalf("nft.conf 缺少 epoch 标记: %q", readFileString(t, a.nftConf))
		}
		e, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			t.Fatalf("epoch 解析失败: %v", err)
		}
		return e
	}
	e1 := parseEpoch()
	e2 := parseEpoch()
	const nanoFloor = uint64(1e17) // 秒级时间戳 ~1.7e9；纳秒 ~1.75e18
	if e1 < nanoFloor {
		t.Fatalf("epoch 不是纳秒级: %d", e1)
	}
	if e1 == e2 {
		t.Fatalf("连续两次 Rules 得到相同 epoch: %d", e1)
	}

	// iptables 注释里的 epoch 同步检查（冒号格式 sbx:epoch:<n>，uint64 全范围）
	ipt := readFileString(t, a.iptScript)
	if regexp.MustCompile(`sbx[:_]epoch[:_](\d+)`).FindStringSubmatch(ipt) == nil {
		t.Fatalf("iptables.sh 缺少 epoch 注释: %q", ipt)
	}
}

// ---- 场景 D：公网监听 + 空 token 拒绝启动 ----------------------------------

func TestListenIsPublicMatrix(t *testing.T) {
	cases := []struct {
		listen string
		pub    bool
	}{
		{"0.0.0.0", true}, {"::", true}, {"192.0.2.10", true},
		{"", true}, {"example.internal", true},
		{"127.0.0.1", false}, {"127.8.8.8", false}, {"::1", false}, {"localhost", false},
	}
	for _, c := range cases {
		if got := listenIsPublic(c.listen); got != c.pub {
			t.Errorf("listenIsPublic(%q)=%v, 期望 %v", c.listen, got, c.pub)
		}
	}
}

// Serve 在拒绝路径上不得监听端口。
func assertNotListening(t *testing.T, port int) {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		conn.Close()
		t.Fatalf("端口 %d 仍在监听, 服务未被拒绝", port)
	}
}

func TestServeRefusesCorruptConfig(t *testing.T) {
	setupApp(t, "ipt") // 仅设置环境
	mustWriteFile(t, os.Getenv("SBX_CONF"), "{ invalid json")
	if rc := Serve(); rc != 1 {
		t.Fatalf("损坏 panel.json 时 serve 必须以非 0 退出, 得到 rc=%d", rc)
	}
	assertNotListening(t, 18099)
}

func TestServeRefusesPublicBindWithoutToken(t *testing.T) {
	for _, listen := range []string{"0.0.0.0", "::"} {
		a := setupApp(t, "ipt")
		a.writeConf(listen, "")
		if rc := Serve(); rc != 1 {
			t.Fatalf("listen=%s 空 token 必须 rc!=0, 得到 rc=%d", listen, rc)
		}
		assertNotListening(t, 18099)
	}
}
