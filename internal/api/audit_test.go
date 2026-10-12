package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func captureAuditLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestAuditLogsLoginFailureAndNodeDeletionWithoutSecrets(t *testing.T) {
	logs := captureAuditLogs(t)

	loginServer, _, _ := newTestServer(t, testToken, &fakeSource{backend: "nft"}, "")
	req, err := http.NewRequest(http.MethodPost, loginServer.URL+"/login", strings.NewReader("token=not-the-token"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("失败登录应重定向回登录页，got %d", resp.StatusCode)
	}

	successBody := strings.NewReader("token=" + url.QueryEscape(testToken))
	req, err = http.NewRequest(http.MethodPost, loginServer.URL+"/login", successBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = noRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("正确凭据登录应成功，got %d", resp.StatusCode)
	}

	nodeServer, _, _, _, _ := newNodeCRUDTestServer(t)
	code, created := doJSON(t, nodeServer, http.MethodPost, "/api/nodes", `{"type":"shadowsocks","name":"audit-node","port":18388,"method":"2022-blake3-aes-128-gcm"}`)
	if code != http.StatusOK {
		t.Fatalf("test node create failed: %d %#v", code, created)
	}
	code, deleted := doJSON(t, nodeServer, http.MethodDelete, "/api/nodes/1?clear_history=0", "")
	if code != http.StatusOK {
		t.Fatalf("test node delete failed: %d %#v", code, deleted)
	}

	text := logs.String()
	for _, field := range []string{"event=login_failure", "result=failure", "event=node_delete", "result=success", "node_id=1", "request_id=", "source_ip="} {
		if !strings.Contains(text, field) {
			t.Errorf("审计日志缺少字段 %q: %s", field, text)
		}
	}
	for _, secret := range []string{testToken, "not-the-token", "secret-one", "password=", "psk="} {
		if strings.Contains(text, secret) {
			t.Errorf("审计日志泄漏凭据 %q: %s", secret, text)
		}
	}
}
