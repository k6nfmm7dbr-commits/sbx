package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
)

func newNodeCRUDTestServer(t *testing.T) (*httptest.Server, string, *int, *int, *Server) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SBX_DIR", dir)
	t.Setenv("SBX_LOCK", filepath.Join(dir, "sbx.lock"))
	t.Setenv("SBX_SB_CONF", filepath.Join(dir, "sing-box.json"))
	fakeSB := filepath.Join(dir, "sing-box")
	if err := os.WriteFile(fakeSB, []byte("#!/bin/sh\n[ \"$1\" = check ] || exit 0\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SBX_SB_BIN", fakeSB)
	nodesFile := filepath.Join(dir, "nodes.json")
	writeTemp(t, nodesFile, "[]")
	writeTemp(t, os.Getenv("SBX_SB_CONF"), `{"inbounds":[],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}`)
	db, err := database.Open(filepath.Join(dir, "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DB: db.Path(), NodesFile: nodesFile, NftConf: filepath.Join(dir, "nft.conf"), Listen: "127.0.0.1", Port: 8080, Token: testToken, Interval: 2, TZ: "UTC"}
	srv, _ := New(cfg, db, nil, nil)
	restarts, applies := new(int), new(int)
	srv.SetNodeMutationHooks(func(context.Context) error { *restarts++; return nil }, func(context.Context) error { *applies++; return nil })
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { ts.Close(); _ = db.Close() })
	return ts, nodesFile, restarts, applies, srv
}

func doJSON(t *testing.T, ts *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp.StatusCode, got
}

func TestPanelNodeCRUDAndShareLinks(t *testing.T) {
	ts, nodesFile, restarts, applies, srv := newNodeCRUDTestServer(t)
	code, created := doJSON(t, ts, http.MethodPost, "/api/nodes", `{"type":"shadowsocks","name":"面板节点","port":8388,"method":"2022-blake3-aes-128-gcm"}`)
	if code != http.StatusOK {
		t.Fatalf("create status=%d body=%v", code, created)
	}
	node, ok := created["node"].(map[string]any)
	if !ok || node["id"] != float64(1) {
		t.Fatalf("created node not returned: %#v", created)
	}
	if *restarts != 1 || *applies != 1 {
		t.Fatalf("post-commit hooks not called once: restart=%d apply=%d", *restarts, *applies)
	}

	code, listBody := doJSON(t, ts, http.MethodGet, "/api/nodes", "")
	list, ok := listBody["nodes"].([]any)
	if code != http.StatusOK || !ok || len(list) != 1 {
		t.Fatalf("list nodes failed: %d %#v", code, listBody)
	}
	entry := list[0].(map[string]any)
	for _, secret := range []string{"password", "uuid", "private_key", "public_key", "psk"} {
		if _, exists := entry[secret]; exists {
			t.Fatalf("secret %s leaked in node list: %#v", secret, entry)
		}
	}

	code, links := doJSON(t, ts, http.MethodGet, "/api/nodes/1/links", "")
	if code != http.StatusOK || !strings.HasPrefix(links["ipv4"].(string), "ss://") {
		t.Fatalf("share link unavailable: %d %#v", code, links)
	}

	code, edited := doJSON(t, ts, http.MethodPut, "/api/nodes/1", `{"port":9443}`)
	if code != http.StatusOK {
		t.Fatalf("edit failed: %d %#v", code, edited)
	}
	var saved []map[string]any
	b, _ := os.ReadFile(nodesFile)
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0]["port"] != float64(9443) {
		t.Fatalf("edit did not commit port: %s", b)
	}

	for _, q := range []string{
		"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2026-10-01','node:1',1,2,0,0)",
		"INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('node:1',1,2,0,0)",
		"INSERT INTO samples(ts,scope,rx,tx,duration_ms,valid) VALUES(1,'node:1',1,2,1000,1)",
	} {
		if _, err := srv.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	code, deleted := doJSON(t, ts, http.MethodDelete, "/api/nodes/1?clear_history=1", "")
	if code != http.StatusOK || deleted["deleted"] != "1" || deleted["history_cleared"] != true || deleted["cumulative_preserved"] != true {
		t.Fatalf("delete with history clear failed: %d %#v", code, deleted)
	}
	for _, table := range []string{"daily", "samples"} {
		var count int
		if err := srv.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE scope='node:1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("clear_history left %d rows in %s", count, table)
		}
	}
	var totalsCount int
	if err := srv.db.QueryRow("SELECT COUNT(*) FROM totals WHERE scope='node:1'").Scan(&totalsCount); err != nil {
		t.Fatal(err)
	}
	if totalsCount != 1 {
		t.Fatalf("cumulative totals must be preserved, count=%d", totalsCount)
	}
	b, _ = os.ReadFile(nodesFile)
	if string(bytes.TrimSpace(b)) != "[]" {
		t.Fatalf("nodes not empty after delete: %s", b)
	}
	if *restarts != 3 || *applies != 3 {
		t.Fatalf("each mutation should restart/apply: %d/%d", *restarts, *applies)
	}
}

// 编辑已有 shadowsocks 节点时 method 必须被忽略：加密算法和凭据是绑定的，
// 客户端早就按旧算法配好了，改了会表现为「节点突然连不上」且查不出原因。
// 面板前端在编辑模式下已经不提交 method，这里守的是直接打 API 的场景。
func TestPanelNodeEditIgnoresMethodChange(t *testing.T) {
	ts, nodesFile, _, _, _ := newNodeCRUDTestServer(t)
	code, created := doJSON(t, ts, http.MethodPost, "/api/nodes",
		`{"type":"shadowsocks","name":"ss","port":8388,"method":"2022-blake3-aes-128-gcm"}`)
	if code != http.StatusOK {
		t.Fatalf("create status=%d body=%v", code, created)
	}

	code, edited := doJSON(t, ts, http.MethodPut, "/api/nodes/1",
		`{"port":9443,"method":"chacha20-ietf-poly1305"}`)
	if code != http.StatusOK {
		t.Fatalf("edit failed: %d %#v", code, edited)
	}
	var saved []map[string]any
	b, _ := os.ReadFile(nodesFile)
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 {
		t.Fatalf("expected 1 node, got %d", len(saved))
	}
	if saved[0]["method"] != "2022-blake3-aes-128-gcm" {
		t.Fatalf("edit must not change method, got %v", saved[0]["method"])
	}
	if saved[0]["port"] != float64(9443) {
		t.Fatalf("other fields should still apply, port=%v", saved[0]["port"])
	}
}

func TestPanelNodeDuplicatePortRecovery(t *testing.T) {
	ts, nodesFile, _, _, _ := newNodeCRUDTestServer(t)
	writeTemp(t, nodesFile, `[
	  {"id":1,"name":"first","type":"shadowsocks","port":443,"method":"2022-blake3-aes-128-gcm","password":"secret-one"},
	  {"id":2,"name":"second","type":"shadowsocks","port":443,"method":"2022-blake3-aes-128-gcm","password":"secret-two"}
	]`)

	// 管理列表应在保留 fail-closed 的同时展示历史冲突及对应节点 ID，供用户修复。
	code, got := doJSON(t, ts, http.MethodGet, "/api/nodes", "")
	if code != http.StatusOK {
		t.Fatalf("duplicate-port nodes should remain manageable: status=%d body=%#v", code, got)
	}
	list, ok := got["nodes"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("expected two visible nodes, got %#v", got)
	}
	for i, wantPeer := range []float64{2, 1} {
		entry := list[i].(map[string]any)
		peers, ok := entry["port_conflict_with"].([]any)
		if !ok || len(peers) != 1 || peers[0] != wantPeer {
			t.Fatalf("node conflict diagnosis missing: node=%#v", entry)
		}
		if _, leaked := entry["password"]; leaked {
			t.Fatalf("node secrets leaked in recovery list: %#v", entry)
		}
	}
	if code, _ := doJSON(t, ts, http.MethodGet, "/api/nodes/1/links", ""); code != http.StatusConflict {
		t.Fatalf("conflicting node must not get a share link, status=%d", code)
	}

	// 编辑其中一个重复节点端口后，最终候选通过严格验证并正常提交。
	code, edited := doJSON(t, ts, http.MethodPut, "/api/nodes/2", `{"port":30012}`)
	if code != http.StatusOK {
		t.Fatalf("repair edit failed: status=%d body=%#v", code, edited)
	}
	code, got = doJSON(t, ts, http.MethodGet, "/api/nodes", "")
	if code != http.StatusOK {
		t.Fatalf("repaired list failed: status=%d body=%#v", code, got)
	}
	list = got["nodes"].([]any)
	for _, raw := range list {
		if _, remains := raw.(map[string]any)["port_conflict_with"]; remains {
			t.Fatalf("conflict marker remains after repair: %#v", raw)
		}
	}
}

func TestPanelNodeMutationRollsBackWhenSingBoxRestartFails(t *testing.T) {
	ts, nodesFile, _, _, srv := newNodeCRUDTestServer(t)
	confPath := os.Getenv("SBX_SB_CONF")
	beforeNodes, err := os.ReadFile(nodesFile)
	if err != nil {
		t.Fatal(err)
	}
	beforeConf, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	restarts := 0
	srv.SetNodeMutationHooks(func(context.Context) error { restarts++; return errors.New("simulated restart failure") }, func(context.Context) error { return nil })
	code, _ := doJSON(t, ts, http.MethodPost, "/api/nodes", `{"type":"shadowsocks","port":8388}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("restart failure should report rollback, status=%d", code)
	}
	afterNodes, err := os.ReadFile(nodesFile)
	if err != nil {
		t.Fatal(err)
	}
	afterConf, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeNodes, afterNodes) || !bytes.Equal(beforeConf, afterConf) {
		t.Fatalf("failed restart did not restore files: nodes=%s config=%s", afterNodes, afterConf)
	}
	if restarts != 2 {
		t.Fatalf("expected failed new restart plus old-config restart, got %d", restarts)
	}
}

func TestConcurrentPanelNodeAddsDoNotLoseUpdates(t *testing.T) {
	ts, nodesFile, _, _, _ := newNodeCRUDTestServer(t)
	const workers = 6
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"type":"shadowsocks","name":"node-%d","port":%d,"method":"2022-blake3-aes-128-gcm"}`, i, 20000+i)
			code, result := doJSON(t, ts, http.MethodPost, "/api/nodes", body)
			if code != http.StatusOK {
				errs <- fmt.Errorf("create %d: status=%d body=%v", i, code, result)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	b, err := os.ReadFile(nodesFile)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != workers {
		t.Fatalf("concurrent CRUD lost nodes: got %d want %d: %s", len(got), workers, b)
	}
	ids, ports := map[float64]bool{}, map[float64]bool{}
	for _, n := range got {
		id, iok := n["id"].(float64)
		port, pok := n["port"].(float64)
		if !iok || !pok || ids[id] || ports[port] {
			t.Fatalf("duplicate or invalid id/port after concurrent adds: %#v", got)
		}
		ids[id], ports[port] = true, true
	}
}

func TestPanelNodeCreationGeneratesSecretsAndTLSFilesServerSide(t *testing.T) {
	ts, nodesFile, _, _, _ := newNodeCRUDTestServer(t)
	fakeSB := os.Getenv("SBX_SB_BIN")
	fakeOutput := `#!/bin/sh
case "$1 $2" in
  "generate reality-keypair") printf 'PrivateKey: fake-private\nPublicKey: fake-public\n' ;;
  "generate tls-keypair") printf '%s\n' '-----BEGIN CERTIFICATE-----' 'Y2VydA==' '-----END CERTIFICATE-----' '-----BEGIN PRIVATE KEY-----' 'a2V5' '-----END PRIVATE KEY-----' ;;
esac
exit 0
`
	if err := os.WriteFile(fakeSB, []byte(fakeOutput), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"type":"vless","name":"reality","port":8443}`,
		`{"type":"trojan","name":"tls","port":9443}`,
	} {
		code, result := doJSON(t, ts, http.MethodPost, "/api/nodes", body)
		if code != http.StatusOK {
			t.Fatalf("create failed: %d %#v", code, result)
		}
	}
	data, err := os.ReadFile(nodesFile)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected two nodes: %s", data)
	}
	for _, key := range []string{"uuid", "private_key", "public_key", "short_id", "password", "cert", "key"} {
		if _, ok := got[0][key]; key != "password" && key != "cert" && key != "key" && !ok {
			t.Errorf("Reality node missing generated %s", key)
		}
	}
	for _, key := range []string{"password", "cert", "key"} {
		if _, ok := got[1][key]; !ok {
			t.Errorf("Trojan node missing generated %s", key)
		}
	}
	certInfo, err := os.Stat(filepath.Join(filepath.Dir(nodesFile), "certs", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if certInfo.Mode().Perm() != 0o600 {
		t.Errorf("generated private TLS key mode=%o, want 600", certInfo.Mode().Perm())
	}
	_, listing := doJSON(t, ts, http.MethodGet, "/api/nodes", "")
	serialized, _ := json.Marshal(listing)
	for _, leaked := range []string{"fake-private", "fake-public", "password", "private_key"} {
		if strings.Contains(string(serialized), leaked) {
			t.Errorf("node listing leaks credential marker %q: %s", leaked, serialized)
		}
	}
}

func TestPanelDeleteReportsCandidateGenerationCause(t *testing.T) {
	ts, nodesFile, _, _, _ := newNodeCRUDTestServer(t)
	code, created := doJSON(t, ts, http.MethodPost, "/api/nodes", `{"type":"shadowsocks","name":"candidate-error","port":8390}`)
	if code != http.StatusOK {
		t.Fatalf("create failed: %d %#v", code, created)
	}
	if err := os.WriteFile(os.Getenv("SBX_SB_CONF"), []byte(`{"inbounds":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, result := doJSON(t, ts, http.MethodDelete, "/api/nodes/1", "")
	if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(result["error"]), "inbounds 必须是数组") {
		t.Fatalf("delete should expose safe candidate validation cause, got %d %#v", code, result)
	}
	data, err := os.ReadFile(nodesFile)
	if err != nil {
		t.Fatal(err)
	}
	var nodes []any
	if err := json.Unmarshal(data, &nodes); err != nil || len(nodes) != 1 {
		t.Fatalf("failed delete must preserve the original node: %s err=%v", data, err)
	}
}

func TestPanelDeletePreservesHistoryByDefault(t *testing.T) {
	ts, nodesFile, _, _, srv := newNodeCRUDTestServer(t)
	code, _ := doJSON(t, ts, http.MethodPost, "/api/nodes", `{"type":"shadowsocks","name":"keep-history","port":8389}`)
	if code != http.StatusOK {
		t.Fatalf("create failed: %d", code)
	}
	for _, query := range []string{
		"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2026-10-05','node:1',9,4,0,0)",
		"INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('node:1',9,4,0,0)",
		"INSERT INTO samples(ts,scope,rx,tx,duration_ms,valid) VALUES(1,'node:1',9,4,2000,1)",
	} {
		if _, err := srv.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	code, result := doJSON(t, ts, http.MethodDelete, "/api/nodes/1", "")
	if code != http.StatusOK || result["history_cleared"] != false || result["cumulative_preserved"] != true {
		t.Fatalf("默认删除必须保留历史: %d %#v", code, result)
	}
	// 删掉的历史无法恢复，误留的数据随时能再清——所以默认必须是"保留"。
	for _, table := range []string{"daily", "samples", "totals"} {
		var count int
		if err := srv.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE scope='node:1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("默认删除必须保留 %s 里的历史, 实得 %d 行", table, count)
		}
	}
	// 展示名要留档：节点已从 nodes.json 移除，名字丢了就只剩"节点 #1"没人认得。
	var name string
	if err := srv.db.QueryRow("SELECT v FROM meta WHERE k='node_name:1'").Scan(&name); err != nil {
		t.Fatalf("删除时应记下展示名: %v", err)
	}
	if name != "keep-history" {
		t.Errorf("留档的名称应为 keep-history, 实得 %q", name)
	}
	// 关键回归：删除后，面板的日趋势必须仍能看到这个节点。
	// "数据留在库里但面板查不到"等于没保留——这正是用户报的那个问题。
	code, daily := doJSON(t, ts, http.MethodGet, "/api/daily?days=180", "")
	if code != http.StatusOK {
		t.Fatalf("daily 查询失败: %d", code)
	}
	days, _ := daily["days"].([]any)
	found := false
	for _, raw := range days {
		row, _ := raw.(map[string]any)
		if row["day"] == "2026-10-05" {
			found = true
			if row["rx"] != float64(9) || row["tx"] != float64(4) {
				t.Errorf("已删除节点的当日数据应保留, 实得 %#v", row)
			}
		}
	}
	if !found {
		t.Error("已删除节点的历史应仍出现在日趋势中")
	}
	// 单独查看该节点也要能取到（前端「历史节点」入口走的就是这个 scope）。
	code, one := doJSON(t, ts, http.MethodGet, "/api/daily?days=180&scope=node:1", "")
	if code != http.StatusOK || len(one["days"].([]any)) != 1 {
		t.Errorf("按已删除节点查询应返回其历史: code=%d %#v", code, one)
	}

	// 面板的节点下拉走 /api/summary：已删除节点必须出现在 history_nodes 里，
	// 否则用户虽然保住了数据，却在界面上找不到入口。
	code, sum := doJSON(t, ts, http.MethodGet, "/api/summary", "")
	if code != http.StatusOK {
		t.Fatalf("summary 查询失败: %d", code)
	}
	hlist, _ := sum["history_nodes"].([]any)
	if len(hlist) != 1 {
		t.Fatalf("history_nodes 应含 1 个已删除节点, 实得 %#v", sum["history_nodes"])
	}
	h0, _ := hlist[0].(map[string]any)
	if h0["id"] != "1" || h0["name"] != "keep-history" {
		t.Errorf("历史节点应带留档名, 实得 %#v", h0)
	}

	data, err := os.ReadFile(nodesFile)
	if err != nil {
		t.Fatal(err)
	}
	var remaining []any
	if err := json.Unmarshal(data, &remaining); err != nil || len(remaining) != 0 {
		t.Fatalf("node should be deleted: %s err=%v", data, err)
	}
}

// TestPanelBatchDeletePreservesHistory：批量删除与单个删除必须同一语义。
//
// 批量入口是"一次删好几个"，用户更容易在删完才发现历史没了；所以这里
// 逐项锁死：每日明细、采样、累计全部保留，展示名留档，且日报仍查得到。
func TestPanelBatchDeletePreservesHistory(t *testing.T) {
	ts, nodesFile, _, _, srv := newNodeCRUDTestServer(t)
	for i, name := range []string{"batch-a", "batch-b", "batch-c"} {
		code, _ := doJSON(t, ts, http.MethodPost, "/api/nodes",
			fmt.Sprintf(`{"type":"shadowsocks","name":%q,"port":%d}`, name, 8391+i))
		if code != http.StatusOK {
			t.Fatalf("create %s failed: %d", name, code)
		}
	}
	for _, id := range []string{"1", "2"} {
		for _, query := range []string{
			fmt.Sprintf("INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2026-10-06','node:%s',7,3,0,0)", id),
			fmt.Sprintf("INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('node:%s',7,3,0,0)", id),
			fmt.Sprintf("INSERT INTO samples(ts,scope,rx,tx,duration_ms,valid) VALUES(1,'node:%s',7,3,2000,1)", id),
		} {
			if _, err := srv.db.Exec(query); err != nil {
				t.Fatal(err)
			}
		}
	}

	code, result := doJSON(t, ts, http.MethodPost, "/api/nodes/delete-batch", `{"ids":["1","2"]}`)
	if code != http.StatusOK || result["history_cleared"] != false || result["cumulative_preserved"] != true {
		t.Fatalf("批量删除默认必须保留历史: %d %#v", code, result)
	}
	if deleted, _ := result["deleted"].([]any); len(deleted) != 2 {
		t.Fatalf("应删除 2 个节点: %#v", result)
	}
	for _, id := range []string{"1", "2"} {
		for _, table := range []string{"daily", "samples", "totals"} {
			var count int
			if err := srv.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE scope='node:" + id + "'").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Errorf("批量删除必须保留 node:%s 的 %s 历史, 实得 %d 行", id, table, count)
			}
		}
		var name string
		if err := srv.db.QueryRow("SELECT v FROM meta WHERE k='node_name:" + id + "'").Scan(&name); err != nil {
			t.Fatalf("批量删除应为 node:%s 留档展示名: %v", id, err)
		}
		if name == "" {
			t.Errorf("node:%s 的留档名称不应为空", id)
		}
	}
	// 未选中的节点必须原样保留。
	var saved []any
	data, err := os.ReadFile(nodesFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil || len(saved) != 1 {
		t.Fatalf("批量删除应只删掉选中的 2 个: %s err=%v", data, err)
	}
	// 日报下拉的历史节点入口同样要能看到这两个节点。
	code, sum := doJSON(t, ts, http.MethodGet, "/api/summary", "")
	if code != http.StatusOK {
		t.Fatalf("summary 查询失败: %d", code)
	}
	if hlist, _ := sum["history_nodes"].([]any); len(hlist) != 2 {
		t.Fatalf("history_nodes 应含 2 个已删除节点, 实得 %#v", sum["history_nodes"])
	}
}

// TestPanelDeleteCanExplicitlyClearHistory 覆盖显式清理这条逃生通道。
//
// 它只应被显式请求（clear_history=1）触发，用来清掉"建错了/试试看的"节点。
func TestPanelDeleteCanExplicitlyClearHistory(t *testing.T) {
	ts, _, _, _, srv := newNodeCRUDTestServer(t)
	code, _ := doJSON(t, ts, http.MethodPost, "/api/nodes", `{"type":"shadowsocks","name":"wipe-history","port":8391}`)
	if code != http.StatusOK {
		t.Fatalf("create failed: %d", code)
	}
	for _, query := range []string{
		"INSERT INTO daily(day,scope,rx,tx,rx_pkts,tx_pkts) VALUES('2026-10-05','node:1',9,4,0,0)",
		"INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('node:1',9,4,0,0)",
		"INSERT INTO samples(ts,scope,rx,tx,duration_ms,valid) VALUES(1,'node:1',9,4,2000,1)",
	} {
		if _, err := srv.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	code, result := doJSON(t, ts, http.MethodDelete, "/api/nodes/1?clear_history=1", "")
	if code != http.StatusOK || result["history_cleared"] != true || result["cumulative_preserved"] != true {
		t.Fatalf("clear_history=1 应清理每日与采样: %d %#v", code, result)
	}
	for _, table := range []string{"daily", "samples"} {
		var count int
		if err := srv.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE scope='node:1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("显式清理后 %s 应为空, 实得 %d 行", table, count)
		}
	}
	// 累计流量无论如何都保留——这是"删节点不删账"的底线。
	var totalsCount int
	if err := srv.db.QueryRow("SELECT COUNT(*) FROM totals WHERE scope='node:1'").Scan(&totalsCount); err != nil {
		t.Fatal(err)
	}
	if totalsCount != 1 {
		t.Fatalf("累计必须始终保留, count=%d", totalsCount)
	}
}

// 批量创建：一次变更建 N 个节点（一次重启）、端口自动分配互不冲突、
// 指定起始端口撞占用时整体拒绝；批量删除：一次变更删 N 个、任一 id
// 不存在整体拒绝。全成或全不动，不做部分成功。
func TestPanelNodeBatchCreateAndDelete(t *testing.T) {
	ts, nodesFile, restarts, applies, _ := newNodeCRUDTestServer(t)
	restartsBefore, appliesBefore := *restarts, *applies

	code, body := doJSON(t, ts, http.MethodPost, "/api/nodes/batch",
		`{"type":"shadowsocks","name_prefix":"员工","count":3}`)
	if code != http.StatusOK {
		t.Fatalf("batch create status=%d body=%v", code, body)
	}
	created, ok := body["created"].([]any)
	if !ok || len(created) != 3 {
		t.Fatalf("应创建 3 个节点: %#v", body)
	}
	ports := map[float64]bool{}
	for i, raw := range created {
		n := raw.(map[string]any)
		want := fmt.Sprintf("员工-%d", i+1)
		if n["name"] != want {
			t.Errorf("第 %d 个节点名应为 %s，实得 %v", i+1, want, n["name"])
		}
		p := n["port"].(float64)
		if p < 30000 || ports[p] {
			t.Errorf("端口 %v 越界或重复", p)
		}
		ports[p] = true
	}
	if *restarts != restartsBefore+1 || *applies != appliesBefore+1 {
		t.Fatalf("批量创建应只触发一次变更: restart=%d apply=%d", *restarts-restartsBefore, *applies-appliesBefore)
	}
	var saved []map[string]any
	b, _ := os.ReadFile(nodesFile)
	_ = json.Unmarshal(b, &saved)
	if len(saved) != 3 {
		t.Fatalf("nodes.json 应有 3 个节点，实得 %d", len(saved))
	}
	if saved[0]["password"] == saved[1]["password"] {
		t.Fatal("各节点密码必须互不相同")
	}

	// 指定起始端口撞占用 → 422 整体拒绝
	var usedPort int
	for p := range ports {
		usedPort = int(p)
		break
	}
	code, body = doJSON(t, ts, http.MethodPost, "/api/nodes/batch",
		fmt.Sprintf(`{"type":"shadowsocks","count":1,"port_start":%d}`, usedPort))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("端口占用应 422，实得 %d: %v", code, body)
	}

	// 批量删除：含不存在的 id → 422 整体拒绝
	code, body = doJSON(t, ts, http.MethodPost, "/api/nodes/delete-batch", `{"ids":["1","999"]}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("删除不存在节点应 422，实得 %d", code)
	}

	restartsBefore, appliesBefore = *restarts, *applies
	code, body = doJSON(t, ts, http.MethodPost, "/api/nodes/delete-batch", `{"ids":["1","3"]}`)
	if code != http.StatusOK {
		t.Fatalf("batch delete status=%d body=%v", code, body)
	}
	if deleted, _ := body["deleted"].([]any); len(deleted) != 2 {
		t.Fatalf("应删除 2 个: %#v", body)
	}
	if *restarts != restartsBefore+1 {
		t.Fatalf("批量删除应只触发一次重启")
	}
	b, _ = os.ReadFile(nodesFile)
	_ = json.Unmarshal(b, &saved)
	if len(saved) != 1 || saved[0]["id"] != float64(2) {
		t.Fatalf("应只剩节点 2: %s", b)
	}
}

// TestPanelNodeBatchCreateHonorsMethodAndVersion：批量创建必须尊重选择的
// 加密算法 / Snell 版本，而不是一律用默认值；非法值在入口处 400 拒绝。
func TestPanelNodeBatchCreateHonorsMethodAndVersion(t *testing.T) {
	ts, nodesFile, _, _, _ := newNodeCRUDTestServer(t)
	code, body := doJSON(t, ts, http.MethodPost, "/api/nodes/batch",
		`{"type":"shadowsocks","name_prefix":"ss256","count":2,"method":"2022-blake3-aes-256-gcm"}`)
	if code != http.StatusOK {
		t.Fatalf("batch create ss status=%d body=%v", code, body)
	}
	var saved []map[string]any
	raw, err := os.ReadFile(nodesFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 {
		t.Fatalf("应创建 2 个节点: %s", raw)
	}
	for _, n := range saved {
		if n["method"] != "2022-blake3-aes-256-gcm" {
			t.Errorf("加密算法应为 256-gcm, 实得 %v", n["method"])
		}
		// 256 位算法的密码必须是 32 字节密钥的 base64（44 字符）；用错算法
		// 生成会在 sing-box 启动时才失败，所以这里按长度钉死。
		if pw, _ := n["password"].(string); len(pw) != 44 {
			t.Errorf("256-gcm 密码长度应为 44, 实得 %d (%q)", len(pw), pw)
		}
	}

	// 非法算法：明确 400，且不落盘。
	code, body = doJSON(t, ts, http.MethodPost, "/api/nodes/batch",
		`{"type":"shadowsocks","count":1,"method":"aes-256-gcm"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法加密算法应 400，实得 %d: %v", code, body)
	}

	// Snell v6：nodes.json 里 version 必须是 6。
	code, body = doJSON(t, ts, http.MethodPost, "/api/nodes/batch",
		`{"type":"snell","name_prefix":"sn","count":1,"version":6}`)
	if code != http.StatusOK {
		t.Fatalf("batch create snell status=%d body=%v", code, body)
	}
	raw, _ = os.ReadFile(nodesFile)
	_ = json.Unmarshal(raw, &saved)
	last := saved[len(saved)-1]
	if last["version"] != float64(6) {
		t.Errorf("Snell 版本应为 6, 实得 %v", last["version"])
	}
	// 非法版本：400。
	code, body = doJSON(t, ts, http.MethodPost, "/api/nodes/batch",
		`{"type":"snell","count":1,"version":4}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 Snell 版本应 400，实得 %d: %v", code, body)
	}
}
