package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx/internal/policy"
)

// ---- /healthz 降级详情（v3.0.12） ------------------------------------------

// 干净状态：输出必须与旧版完全一致（既有探活脚本依赖 {"ok":true}）。
func TestHealthzCleanMatchesLegacy(t *testing.T) {
	ts, _, _ := newTestServer(t, "", &fakeSource{backend: "nft"}, "")
	resp := doReq(t, ts, http.MethodGet, "/healthz", nil)
	got := body(t, resp)
	if got != `{"ok":true}` {
		t.Fatalf("干净状态 healthz 应与旧版一致, got %s", got)
	}
}

// 降级状态：采集错误 / 采样年龄 / 策略错误如实透出，但仍 200 + ok:true。
func TestHealthzDegradedFields(t *testing.T) {
	ts, _, _ := newTestServer(t, "", &fakeSource{
		backend: "nft",
		err:     "nft 读取失败: permission denied",
		lastOK:  time.Now().Unix() - 30,
	}, "")
	resp := doReq(t, ts, http.MethodGet, "/healthz", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("降级状态仍应 200, got %d", resp.StatusCode)
	}
	got := body(t, resp)
	if !strings.Contains(got, `"ok":true`) {
		t.Fatalf("缺少 ok:true: %s", got)
	}
	if !strings.Contains(got, `"collector_error":"nft 读取失败: permission denied"`) {
		t.Fatalf("缺少 collector_error: %s", got)
	}
	var health map[string]any
	if err := json.Unmarshal([]byte(got), &health); err != nil {
		t.Fatalf("healthz JSON 无效: %v", err)
	}
	age, ok := health["sample_age_s"].(float64)
	if !ok || age < 30 || age > 32 {
		t.Fatalf("sample_age_s 应处于 30–32 秒，got %v (%s)", health["sample_age_s"], got)
	}
}

// ---- SSE 跨连接共享序列化缓存（v3.0.12） ------------------------------------

// sseTestServer 构造带 policy 服务的 Server（nft 应用为 no-op）。
func sseTestServer(t *testing.T, nodesJSON string) (*Server, *policy.Service) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "traffic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	writeTemp(t, filepath.Join(dir, "nodes.json"), nodesJSON)
	pol := policy.New(db.DB, dir, filepath.Join(dir, "policy.nft"))
	pol.SetNFTApply(func(ctx context.Context, path string) error { return nil })
	cfg := &config.Config{
		DB:        filepath.Join(dir, "traffic.db"),
		NodesFile: filepath.Join(dir, "nodes.json"),
		NftConf:   filepath.Join(dir, "nft.conf"),
		Listen:    "127.0.0.1", Port: 8080,
		Token: "", Interval: 2, TZ: "UTC",
	}
	s, _ := New(cfg, db, &fakeSource{hasConns: true}, pol)
	return s, pol
}

// 同一策略版本反复取 payload 必须只序列化一次；版本变化后重建。
func TestSSEPayloadsSharedAcrossConnections(t *testing.T) {
	s, pol := sseTestServer(t,
		`{"nodes":[{"id":1,"type":"vless","port":10001,"uuid":"u","name":"n1"}]}`)

	var calls int
	orig := marshalSnapFn
	marshalSnapFn = func(ns policy.NodeIPSnapshot) string {
		calls++
		return orig(ns)
	}
	t.Cleanup(func() { marshalSnapFn = orig })

	// 版本 V0（首次 reconcile 前）：初始快照
	s.ssePayloads()
	first := calls

	// 模拟另外 4 个连接在版本未变时取 payload —— 不应再序列化
	for i := 0; i < 4; i++ {
		s.ssePayloads()
	}
	if calls != first {
		t.Fatalf("版本未变时不应重复序列化: calls=%d first=%d", calls, first)
	}

	// 推进版本（reconcile 发布新快照）→ 必须重建
	if err := pol.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	s.ssePayloads()
	if calls <= first {
		t.Fatalf("版本变化后应重新序列化: calls=%d first=%d", calls, first)
	}
}

// 缓存内容与直接序列化一致（共享不能改变 payload 内容）。
func TestSSEPayloadsContentIdentical(t *testing.T) {
	s, pol := sseTestServer(t,
		`{"nodes":[{"id":1,"type":"vless","port":10001,"uuid":"u","name":"n1"},{"id":2,"type":"vless","port":10002,"uuid":"u","name":"n2"}]}`)
	if err := pol.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	payloads, _ := s.ssePayloads()
	snap := pol.IPStateSnapshot()
	if len(payloads) != len(snap) {
		t.Fatalf("payload 节点数=%d, 直接快照=%d", len(payloads), len(snap))
	}
	for id, ns := range snap {
		var cached, direct map[string]any
		if err := json.Unmarshal([]byte(payloads[id]), &cached); err != nil {
			t.Fatalf("payload 不是合法 JSON: %v", err)
		}
		b, _ := json.Marshal(ns)
		if err := json.Unmarshal(b, &direct); err != nil {
			t.Fatal(err)
		}
		cb, _ := json.Marshal(cached)
		dbj, _ := json.Marshal(direct)
		if string(cb) != string(dbj) {
			t.Fatalf("节点 %s payload 与直接序列化不一致:\n%s\n%s", id, cb, dbj)
		}
	}
}
