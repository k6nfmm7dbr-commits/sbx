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

func TestPanelDeleteClearsDailyAndSamplesButKeepsTotalsByDefault(t *testing.T) {
	ts, nodesFile, _, _, srv := newNodeCRUDTestServer(t)
	code, _ := doJSON(t, ts, http.MethodPost, "/api/nodes", `{"type":"shadowsocks","name":"clear-history","port":8389}`)
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
	if code != http.StatusOK || result["history_cleared"] != true || result["cumulative_preserved"] != true {
		t.Fatalf("delete default should clear daily/samples but preserve cumulative: %d %#v", code, result)
	}
	for _, table := range []string{"daily", "samples"} {
		var count int
		if err := srv.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE scope='node:1'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("default delete must clear %s history, count=%d", table, count)
		}
	}
	var totalsCount int
	if err := srv.db.QueryRow("SELECT COUNT(*) FROM totals WHERE scope='node:1'").Scan(&totalsCount); err != nil {
		t.Fatal(err)
	}
	if totalsCount != 1 {
		t.Fatalf("default delete must preserve cumulative totals, count=%d", totalsCount)
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

func TestPanelDeleteCanExplicitlyKeepHistory(t *testing.T) {
	ts, _, _, _, srv := newNodeCRUDTestServer(t)
	code, _ := doJSON(t, ts, http.MethodPost, "/api/nodes", `{"type":"shadowsocks","name":"keep-history","port":8391}`)
	if code != http.StatusOK {
		t.Fatalf("create failed: %d", code)
	}
	if _, err := srv.db.Exec("INSERT INTO totals(scope,rx,tx,rx_pkts,tx_pkts) VALUES('node:1',9,4,0,0)"); err != nil {
		t.Fatal(err)
	}
	code, result := doJSON(t, ts, http.MethodDelete, "/api/nodes/1?clear_history=0", "")
	if code != http.StatusOK || result["history_cleared"] != false || result["cumulative_preserved"] != true {
		t.Fatalf("explicit clear_history=0 should preserve history: %d %#v", code, result)
	}
	var count int
	if err := srv.db.QueryRow("SELECT COUNT(*) FROM totals WHERE scope='node:1'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("explicit opt-out must retain history, count=%d", count)
	}
}
