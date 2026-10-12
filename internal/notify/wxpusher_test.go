package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
)

// TestWxPusherSuccessCodeIs1000：官方成功码是 1000，既不是 0 也不是 200。
// 本包已有 Server酱(code==0)、PushPlus(code==200) 的判定，这里是第三种口径——
// 只看 HTTP 200 会把业务失败当成功。
func TestWxPusherSuccessCodeIs1000(t *testing.T) {
	srv := textResp(`{"code":1001,"msg":"消息内容超长"}`)
	defer srv.Close()
	ch := &wxPusherChannel{
		channelBase: channelBase{name: "wx"},
		baseURL:     srv.URL, simple: true, spt: "SPT_test123", client: srv.Client(),
	}
	if err := ch.Send(context.Background(), Message{Title: "t", Blocks: []string{"b"}}); err == nil {
		t.Fatal("HTTP 200 但 code=1001 必须判为失败")
	}

	srv2 := textResp(`{"code":1000,"msg":"处理成功","success":true}`)
	defer srv2.Close()
	ch2 := &wxPusherChannel{
		channelBase: channelBase{name: "wx"},
		baseURL:     srv2.URL, simple: true, spt: "SPT_test123", client: srv2.Client(),
	}
	if err := ch2.Send(context.Background(), Message{Title: "t", Blocks: []string{"b"}}); err != nil {
		t.Fatalf("code=1000 应判为成功: %v", err)
	}
}

// TestWxPusherPayloadModes：极简与标准走不同端点、带不同凭据字段，互不混淆。
func TestWxPusherPayloadModes(t *testing.T) {
	var (
		gotPath string
		gotBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"code":1000,"msg":"ok"}`)
	}))
	defer srv.Close()

	// ---- 极简（SPT）----
	simple := &wxPusherChannel{
		channelBase: channelBase{name: "wx"},
		baseURL:     srv.URL, simple: true, spt: "SPT_test123", client: srv.Client(),
	}
	if err := simple.Send(context.Background(), Message{Title: "T", Blocks: []string{"B"}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/send/message/simple-push" {
		t.Errorf("极简模式应走 simple-push 端点, 实得 %s", gotPath)
	}
	assertWxPusherBody(t, gotBody, map[string]any{"spt": "SPT_test123", "contentType": float64(1)})
	if strings.Contains(string(gotBody), "appToken") {
		t.Error("极简模式不应带 appToken")
	}

	// ---- 标准（appToken + uids）----
	std := &wxPusherChannel{
		channelBase: channelBase{name: "wx"},
		baseURL:     srv.URL, appToken: "AT_test123", uids: []string{"UID_abc"}, client: srv.Client(),
	}
	if err := std.Send(context.Background(), Message{Title: "T", Blocks: []string{"B"}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/send/message" {
		t.Errorf("标准模式应走 /api/send/message, 实得 %s", gotPath)
	}
	assertWxPusherBody(t, gotBody, map[string]any{"appToken": "AT_test123", "contentType": float64(1)})
	if strings.Contains(string(gotBody), `"spt"`) {
		t.Error("标准模式不应带 spt")
	}
	var payload struct {
		UIDs []string `json:"uids"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.UIDs) != 1 || payload.UIDs[0] != "UID_abc" {
		t.Errorf("uids 应为数组且含 UID_abc, 实得 %v", payload.UIDs)
	}
}

// TestWxPusherUsesPlainTextContentType：contentType 必须是 1（纯文本）。
//
// html(2) 与 markdown(3) 会解析 < > * 等字符，可能破坏内嵌的 SBX1 数据行——
// 这条断言守的是"推送内容仍然可回放"。
func TestWxPusherUsesPlainTextContentType(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"code":1000,"msg":"ok"}`)
	}))
	defer srv.Close()

	ch := &wxPusherChannel{
		channelBase: channelBase{name: "wx"},
		baseURL:     srv.URL, simple: true, spt: "SPT_test123", client: srv.Client(),
	}
	if err := ch.Send(context.Background(), Message{Title: "T", Blocks: []string{"B"}}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatal(err)
	}
	if ct := got["contentType"]; ct != float64(wxPusherContentTypeText) || wxPusherContentTypeText != 1 {
		t.Errorf("contentType 必须是 1(纯文本), 实得 %v", ct)
	}
}

// TestWxPusherRoundTripsReplayLine：走完整渲染 → 发送 → 从收到的 content 恢复，
// 端到端验证纯文本 contentType 没有破坏可回放数据。
func TestWxPusherRoundTripsReplayLine(t *testing.T) {
	var gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(body, &p)
		gotContent = p.Content
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"code":1000,"msg":"ok"}`)
	}))
	defer srv.Close()

	const nodes = 12
	ch := &wxPusherChannel{
		channelBase: channelBase{name: "wx"},
		baseURL:     srv.URL, simple: true, spt: "SPT_test123", client: srv.Client(),
	}
	snap := mkSnapshot(KindDaily, nodes)
	blocks, dropped := Render(snap, ch.Caps())
	if len(dropped) != 0 {
		t.Fatalf("不应丢弃数据行: %d", len(dropped))
	}
	if err := ch.Send(context.Background(), Message{
		Event: config.EventDaily, Title: "SBX 日报", Day: snap.Day,
		Blocks: blocks, Snap: snap,
	}); err != nil {
		t.Fatal(err)
	}

	recs, skipped := ExtractReplays(gotContent)
	if skipped != 0 {
		t.Fatalf("收到的内容里出现 %d 个不可解析片段", skipped)
	}
	merged, errs := MergeRecords(recs)
	if len(errs) != 0 {
		t.Fatalf("重组报错: %v", errs)
	}
	if len(merged) != 1 {
		t.Fatalf("应重组成 1 条记录, 实得 %d", len(merged))
	}
	if len(merged[0].R) != nodes {
		t.Errorf("可回放数据不完整: %d 行 (期望 %d)", len(merged[0].R), nodes)
	}
}

func assertWxPusherBody(t *testing.T, body []byte, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v", err)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("字段 %s: got=%v want=%v", k, got[k], v)
		}
	}
}
