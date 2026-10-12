package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/policy"
)

// getBody 发起 GET 并返回 (状态码, 响应头, 响应体)。
func getBody(t *testing.T, url string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// 安全回归：内部错误不得把 err.Error()（含文件绝对路径、SQL、nft 报错）透传给客户端；
// 必须返回稳定的 error_code + request_id，且 request_id 与响应头一致，便于对齐日志。
func TestInternalErrorsDoNotLeakDetails(t *testing.T) {
	dir := t.TempDir()
	nodesFile := filepath.Join(dir, "nodes.json")
	writeTemp(t, nodesFile, `[{"id":1,"name":"n1","type":"vless","port":443}]`)

	ts, db, _ := newTestServer(t, "", &fakeSource{backend: "nft"}, nodesFile)
	// 关掉数据库制造内部错误（模拟磁盘故障/文件被删）。
	db.Close()

	code, hdr, body := getBody(t, ts.URL+"/api/daily?days=7")
	if code != http.StatusInternalServerError {
		t.Fatalf("应 500, got %d (%s)", code, body)
	}
	// 1) 不得泄漏内部细节
	for _, leak := range []string{dir, "traffic.db", "sql:", "database is closed", "SELECT"} {
		if strings.Contains(body, leak) {
			t.Errorf("响应体泄漏内部细节 %q: %s", leak, body)
		}
	}
	// 2) 必须带稳定错误码与关联 ID
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("响应不是 JSON: %s", body)
	}
	if got["error"] != errInternalText {
		t.Errorf("error 应为通用文案 %q, got %v", errInternalText, got["error"])
	}
	if got["error_code"] != codeDailyFailed {
		t.Errorf("error_code 应为 %q, got %v", codeDailyFailed, got["error_code"])
	}
	rid, _ := got["request_id"].(string)
	if rid == "" {
		t.Fatal("响应缺少 request_id")
	}
	// 3) 关联 ID 与响应头一致（用户报障时两者都能用来定位日志）
	if h := hdr.Get("X-Request-Id"); h != rid {
		t.Errorf("X-Request-Id(%q) 应与 body.request_id(%q) 一致", h, rid)
	}
}

// 策略端点在 nodes.json 损坏时仍须说明「策略维持上一轮」，但不得回显文件路径与解析细节。
func TestPolicyUnavailableHidesInternalDetails(t *testing.T) {
	dir := t.TempDir()
	nodesFile := filepath.Join(dir, "nodes.json")
	writeTemp(t, nodesFile, `[{"id":1,"name":"n1","type":"vless","port":443}]`)
	ts, _ := newPolicyTestServer(t, "", nodesFile)

	if err := os.WriteFile(nodesFile, []byte("{ broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, body := getBody(t, ts.URL+"/api/nodes/1/policy")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("应 503, got %d (%s)", code, body)
	}
	if !strings.Contains(body, "维持上一轮") {
		t.Errorf("应说明策略维持上一轮, got %s", body)
	}
	if strings.Contains(body, dir) || strings.Contains(body, "invalid character") {
		t.Errorf("503 响应泄漏了内部细节: %s", body)
	}
	if !strings.Contains(body, codeNodesFileUnavailable) {
		t.Errorf("503 应带 error_code=%s, got %s", codeNodesFileUnavailable, body)
	}
}

// healthz 免鉴权故障探针只暴露布尔状态，不得回显 nodes.json 绝对路径、文件权限或 nft 错误。
// 原始错误仍应在结构化服务端日志中带 request_id 与稳定 error_code，供维护者排障。
func TestAuditHealthzHidesInternalErrors(t *testing.T) {
	dir := t.TempDir()
	nodesFile := filepath.Join(dir, "nodes.json")
	writeTemp(t, nodesFile, `{broken`)
	db, err := database.Open(filepath.Join(dir, "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pol := policy.New(db.DB, dir, filepath.Join(dir, "policy.nft"))
	pol.SetNodesFile(nodesFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pol.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(2 * time.Second)
	for pol.LastError() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if pol.LastError() == "" {
		t.Fatal("测试策略服务未进入故障状态")
	}

	cfg := &config.Config{
		DB: db.Path(), NodesFile: nodesFile, NftConf: filepath.Join(dir, "nft.conf"),
		Listen: "127.0.0.1", Port: 8080, Token: testToken, Interval: 2, TZ: "UTC",
	}
	secret := "nodes.json 读取失败: open /etc/sbx/nodes.json: permission denied"
	srv, _ := New(cfg, db, &fakeSource{backend: "nft", err: secret, lastOK: time.Now().Unix() - 30}, pol)

	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(oldLogger)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 未提供 Bearer/Cookie：探活端点仍免鉴权且返回 200。
	code, hdr, body := getBody(t, ts.URL+"/healthz")
	if code != http.StatusOK {
		t.Fatalf("/healthz 故障时仍须 200，got %d: %s", code, body)
	}
	for _, want := range []string{`"ok":true`, `"collector_ok":false`, `"policy_ok":false`, `"sample_age_s"`} {
		if !strings.Contains(body, want) {
			t.Errorf("healthz 缺少 %s: %s", want, body)
		}
	}
	for _, leak := range []string{"/etc/", "nodes.json", "permission", "nft 读取失败"} {
		if strings.Contains(body, leak) {
			t.Errorf("healthz 响应泄漏内部细节 %q: %s", leak, body)
		}
	}
	rid := hdr.Get("X-Request-Id")
	if rid == "" {
		t.Fatal("healthz 响应缺少 request id")
	}
	logText := logs.String()
	for _, want := range []string{rid, codeCollectorDegraded, codePolicyDegraded, "/etc/sbx/nodes.json", "permission denied"} {
		if !strings.Contains(logText, want) {
			t.Errorf("服务端降级日志缺少 %q: %s", want, logText)
		}
	}
}

// panic 恢复：响应尚未写出时应补 500（而不是让连接悬空）；日志/响应都带 request_id。
func TestRecoverMiddlewarePanicWithoutPartialWrite(t *testing.T) {
	s := &Server{}
	h := s.withRequestID(s.recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom: /etc/sbx/secret-path")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应返回 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), errInternalText) {
		t.Errorf("panic 响应体应为通用文案, got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-path") {
		t.Errorf("panic 响应泄漏了内部信息: %s", rec.Body.String())
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("panic 响应应带 X-Request-Id")
	}
}

// panic 发生在响应已开始写出之后（SSE 场景）：不得重复写头，且不 panic 出栈。
func TestRecoverMiddlewarePanicAfterPartialWrite(t *testing.T) {
	s := &Server{}
	h := s.withRequestID(s.recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("late boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("已开始写出的响应不应被改写, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "partial" {
		t.Errorf("响应体不应被追加错误文本, got %q", got)
	}
}

// trackingWriter 必须保留 http.Flusher，否则 SSE 会静默失去推送能力。
func TestTrackingWriterPreservesFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := &trackingWriter{ResponseWriter: rec}
	f, ok := any(tw).(http.Flusher)
	if !ok {
		t.Fatal("trackingWriter 必须实现 http.Flusher")
	}
	f.Flush()
	if !rec.Flushed {
		t.Error("Flush 未透传到底层 ResponseWriter")
	}
	if tw.Unwrap() == nil {
		t.Error("Unwrap 必须返回底层 writer（ResponseController 依赖它清 SSE 写超时）")
	}
}
