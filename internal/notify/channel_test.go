package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func textResp(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
}

// TestTelegramRejectsSemanticFailure：Telegram 业务失败时也返回 HTTP 200，
// 只看状态码会把失败当成功——这是最容易漏的一类误判。
func TestTelegramRejectsSemanticFailure(t *testing.T) {
	srv := textResp(`{"ok":false,"description":"chat not found"}`)
	defer srv.Close()

	ch := &telegramChannel{
		channelBase: channelBase{name: "tg"},
		endpoint:    srv.URL + "/bot", token: "123456789:x", chatID: "-1",
		client: srv.Client(),
	}
	if err := ch.Send(context.Background(), Message{Blocks: []string{"hi"}}); err == nil {
		t.Fatal("HTTP 200 但 ok=false 必须判为失败")
	}

	srv2 := textResp(`{"ok":true}`)
	defer srv2.Close()
	ch2 := &telegramChannel{
		channelBase: channelBase{name: "tg"},
		endpoint:    srv2.URL + "/bot", token: "123456789:x", chatID: "-1",
		client: srv2.Client(),
	}
	if err := ch2.Send(context.Background(), Message{Blocks: []string{"hi"}}); err != nil {
		t.Fatalf("ok=true 应判为成功: %v", err)
	}
}

func TestTelegramRetryAfterOn429(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"ok":false,"parameters":{"retry_after":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	ch := &telegramChannel{
		channelBase: channelBase{name: "tg"},
		endpoint:    srv.URL + "/bot", token: "123456789:x", chatID: "-1",
		client: srv.Client(),
	}
	if err := ch.Send(context.Background(), Message{Blocks: []string{"hi"}}); err != nil {
		t.Fatalf("429 后按 retry_after 重试应成功: %v", err)
	}
	if calls != 2 {
		t.Errorf("应恰好请求 2 次, 实得 %d", calls)
	}
}

func TestWxWorkChecksErrCode(t *testing.T) {
	srv := textResp(`{"errcode":93000,"errmsg":"invalid webhook url"}`)
	defer srv.Close()
	ch := &wxworkChannel{
		channelBase: channelBase{name: "wx"},
		webhook:     srv.URL + "/hook?key=abc", msgtype: "text", client: srv.Client(),
	}
	if err := ch.Send(context.Background(), Message{Blocks: []string{"hi"}}); err == nil {
		t.Fatal("errcode != 0 必须判为失败")
	}

	srv2 := textResp(`{"errcode":0,"errmsg":"ok"}`)
	defer srv2.Close()
	ch2 := &wxworkChannel{
		channelBase: channelBase{name: "wx"},
		webhook:     srv2.URL + "/hook?key=abc", msgtype: "text", client: srv2.Client(),
	}
	if err := ch2.Send(context.Background(), Message{Blocks: []string{"hi"}}); err != nil {
		t.Fatalf("errcode=0 应判为成功: %v", err)
	}
}

func TestWxWorkCapacityDependsOnMsgType(t *testing.T) {
	text := &wxworkChannel{msgtype: "text"}
	md := &wxworkChannel{msgtype: "markdown"}
	if text.Caps().MaxBytes != wxWorkTextMaxBytes {
		t.Errorf("text 容量应为 %d, 实得 %d", wxWorkTextMaxBytes, text.Caps().MaxBytes)
	}
	if md.Caps().MaxBytes != wxWorkMarkdownMaxBytes {
		t.Errorf("markdown 容量应为 %d, 实得 %d", wxWorkMarkdownMaxBytes, md.Caps().MaxBytes)
	}
}

func TestServerChanAndPushPlusCodeChecks(t *testing.T) {
	// Server酱：成功是 code == 0
	scFail := textResp(`{"code":40001,"message":"bad key"}`)
	defer scFail.Close()
	sc := &serverChanChannel{
		channelBase: channelBase{name: "sc"},
		endpoint:    scFail.URL + "/", sendKey: "SCTabc", client: scFail.Client(),
	}
	if err := sc.Send(context.Background(), Message{Title: "t", Blocks: []string{"b"}}); err == nil {
		t.Error("Server酱 code != 0 必须失败")
	}

	// PushPlus：成功是 code == 200，不是 0
	ppWrong := textResp(`{"code":0,"msg":"unexpected"}`)
	defer ppWrong.Close()
	pp := &pushPlusChannel{
		channelBase: channelBase{name: "pp"},
		endpoint:    ppWrong.URL, token: "abcdef1234567890", client: ppWrong.Client(),
	}
	if err := pp.Send(context.Background(), Message{Title: "t", Blocks: []string{"b"}}); err == nil {
		t.Error("PushPlus 的 code=0 不是成功码（应为 200），必须判失败")
	}

	ppOK := textResp(`{"code":200,"msg":"请求成功"}`)
	defer ppOK.Close()
	pp2 := &pushPlusChannel{
		channelBase: channelBase{name: "pp"},
		endpoint:    ppOK.URL, token: "abcdef1234567890", client: ppOK.Client(),
	}
	if err := pp2.Send(context.Background(), Message{Title: "t", Blocks: []string{"b"}}); err != nil {
		t.Errorf("PushPlus code=200 应成功: %v", err)
	}
}

func TestWebhookSignsAndDeliversStructuredData(t *testing.T) {
	var (
		gotSig  string
		gotBody []byte
		gotEvt  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-SBX-Signature")
		gotEvt = r.Header.Get("X-SBX-Event")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const secret = "s3cret-key"
	ch := &webhookChannel{
		channelBase: channelBase{name: "wh"},
		url:         srv.URL, secret: secret, maxBytes: 8192, client: srv.Client(),
	}
	snap := mkSnapshot(KindDaily, 3)
	blocks, _ := Render(snap, ch.Caps())
	if err := ch.Send(context.Background(), Message{
		Event: "daily", Day: snap.Day, Blocks: blocks, Snap: snap,
	}); err != nil {
		t.Fatalf("webhook 发送失败: %v", err)
	}

	if gotEvt != "daily" {
		t.Errorf("事件头不正确: %q", gotEvt)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(gotSig), []byte(want)) {
		t.Errorf("HMAC 签名不匹配:\n  got  %s\n  want %s", gotSig, want)
	}

	var payload struct {
		Event  string `json:"event"`
		Day    string `json:"day"`
		Nodes  []any  `json:"nodes"`
		Replay []any  `json:"replay"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v", err)
	}
	if payload.Event != "daily" || payload.Day != snap.Day {
		t.Errorf("payload 头部不符: %+v", payload)
	}
	if len(payload.Nodes) != 3 {
		t.Errorf("应携带 3 个节点, 实得 %d", len(payload.Nodes))
	}
	if len(payload.Replay) == 0 {
		t.Error("应携带可回放行")
	}
}

func TestWebhookTruncatesNodesButKeepsReplay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := &webhookChannel{
		channelBase: channelBase{name: "wh"},
		url:         srv.URL, maxBytes: 900, // 故意很小，迫使裁剪
		client: srv.Client(),
	}
	const nodes = 40
	snap := mkSnapshot(KindDaily, nodes)
	blocks, _ := Render(snap, ch.Caps())
	payload := ch.buildPayload(Message{Event: "daily", Day: snap.Day, Blocks: blocks, Snap: snap})

	if !payload.Truncated {
		t.Error("超限时应标记 truncated")
	}
	if len(payload.Nodes) >= nodes {
		t.Errorf("节点明细应被裁剪, 实得 %d", len(payload.Nodes))
	}
	// 关键：裁剪的代价只能落在 nodes/text 上，可回放数据必须完整。
	if len(payload.Replay) == 0 {
		t.Fatal("裁剪时绝不能丢掉可回放数据")
	}
	recs, skipped := ExtractReplays(strings.Join(payload.Replay, "\n"))
	if skipped != 0 {
		t.Errorf("可回放数据出现 %d 个损坏片段", skipped)
	}
	merged, errs := MergeRecords(recs)
	if len(errs) != 0 {
		t.Errorf("可回放数据重组报错: %v", errs)
	}
	if len(merged) != 1 || len(merged[0].R) != nodes {
		t.Fatalf("裁剪后可回放数据不完整: %d 条 / %d 行", len(merged), len(merged[0].R))
	}
}

func TestPostJSONLimitsResponseBody(t *testing.T) {
	// 服务端返回超大响应：必须被限长读取，不能把内存打满。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("x", 1<<20)), 1<<20)
	}))
	defer srv.Close()

	srv2 := textResp(`{"ok":true}`)
	defer srv2.Close()

	data, err := postJSON(context.Background(), srv.Client(), srv.URL, map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("非 2xx 之外的读取失败不应报错: %v", err)
	}
	if len(data) > maxRespBytes {
		t.Errorf("响应体未被限长: %d > %d", len(data), maxRespBytes)
	}
}

func TestHTTPStatusErrorIsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad key SCT99887766 for bot `+testBotToken+`"}`)
	}))
	defer srv.Close()

	_, err := postJSON(context.Background(), srv.Client(), srv.URL, map[string]string{})
	if err == nil {
		t.Fatal("非 2xx 应返回错误")
	}
	msg := SafeErr(err)
	for _, s := range []string{testBotToken, "SCT99887766"} {
		if strings.Contains(msg, s) {
			t.Fatalf("HTTP 错误信息泄露凭据 %q: %s", s, msg)
		}
	}
}
