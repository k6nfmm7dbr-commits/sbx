package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestEndToEndDailyCardDelivery 走完整链路：
// 调度触发 → 查询历史 → 渲染卡片 → 经飞书渠道发出 → 解析收到的报文 → 断言内容。
//
// 单测各自覆盖了自己的片段，这条把"接起来之后还对不对"也钉住——
// 例如卡片被包进了错误的 msg_type，或渠道把 card 字段写错，都只有端到端才发现。
func TestEndToEndDailyCardDelivery(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-11", 100, 50)
	insertDaily(t, db, "2026-10-12", 200, 100)
	insertDaily(t, db, "2026-10-13", 400, 200)
	insertDaily(t, db, "2026-10-14", 9, 9) // 今天刚开始，不该出现在日报里

	m.SetSender(&feishuChannel{
		name: "feishu", webhook: srv.URL, client: srv.Client(), now: time.Now,
	})
	m.now = func() time.Time { return time.Date(2026, 10, 14, 0, 5, 0, 0, time.UTC) }
	m.tick(context.Background())

	mu.Lock()
	payload := got
	mu.Unlock()
	if payload == nil {
		t.Fatal("未收到任何请求")
	}
	if payload["msg_type"] != "interactive" {
		t.Fatalf("msg_type 应为 interactive, 实得 %v", payload["msg_type"])
	}
	card, ok := payload["card"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 card 对象: %v", payload)
	}
	header, _ := card["header"].(map[string]any)
	title, _ := header["title"].(map[string]any)
	titleText, _ := title["content"].(string)
	if !strings.Contains(titleText, "10月13日") {
		t.Errorf("标题应指向 10-13, 实得 %q", titleText)
	}

	elements, _ := card["elements"].([]any)
	if len(elements) < 3 {
		t.Fatalf("卡片元素过少: %d", len(elements))
	}
	// 第一块是三栏概览
	cols, _ := elements[0].(map[string]any)["columns"].([]any)
	if len(cols) != 3 {
		t.Fatalf("概览应为三栏, 实得 %d", len(cols))
	}
	// 最后一块是历史记录
	last, _ := elements[len(elements)-1].(map[string]any)
	text, _ := last["text"].(map[string]any)
	content, _ := text["content"].(string)
	for _, want := range []string{"历史记录", "10-13", "10-12", "10-11", "3 天"} {
		if !strings.Contains(content, want) {
			t.Errorf("历史正文应含 %q, 实得:\n%s", want, content)
		}
	}
	if strings.Contains(content, "10-14") {
		t.Errorf("日报不应包含刚开始的今天, 实得:\n%s", content)
	}
	// 三栏数值应为 10-13 的量：总量 600（400+200）
	if b, err := json.Marshal(elements[0]); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(b), "600") {
		t.Errorf("概览总量应为 600, 实得 %s", string(b))
	}
}

// TestEndToEndRecoverCardDelivery 覆盖异常补推的端到端形态。
func TestEndToEndRecoverCardDelivery(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = body
		mu.Unlock()
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	m, db := newTestManager(t)
	insertDaily(t, db, "2026-10-13", 100, 50)
	insertDaily(t, db, "2026-10-14", 700, 300)
	m.SetSender(&feishuChannel{
		name: "feishu", webhook: srv.URL, client: srv.Client(), now: time.Now,
	})
	m.now = func() time.Time { return time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC) }

	m.recoverPush(context.Background())

	mu.Lock()
	payload := got
	mu.Unlock()
	if payload == nil {
		t.Fatal("未收到任何请求")
	}
	card, _ := payload["card"].(map[string]any)
	header, _ := card["header"].(map[string]any)
	title, _ := header["title"].(map[string]any)
	titleText, _ := title["content"].(string)
	for _, want := range []string{"补发", "10月14日", "12:30"} {
		if !strings.Contains(titleText, want) {
			t.Errorf("补推标题应含 %q, 实得 %q", want, titleText)
		}
	}
	if tpl, _ := header["template"].(string); tpl != "orange" {
		t.Errorf("补推标题栏应为橙色警示, 实得 %q", tpl)
	}
	elements, _ := card["elements"].([]any)
	last, _ := elements[len(elements)-1].(map[string]any)
	text, _ := last["text"].(map[string]any)
	content, _ := text["content"].(string)
	if !strings.Contains(content, "（进行中）") {
		t.Errorf("补推历史应标注进行中的当天, 实得:\n%s", content)
	}
}
