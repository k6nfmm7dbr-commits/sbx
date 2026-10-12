package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/config"
)

func TestAPIRateLimiterBurstAndNormalRate(t *testing.T) {
	limiter := apiRateLimiter{}
	now := time.Unix(1000, 0)
	for i := 0; i < int(apiRateBurst); i++ {
		if !limiter.allowIP("198.51.100.7", now) {
			t.Fatalf("burst 内第 %d 个请求不应被拒绝", i+1)
		}
	}
	if limiter.allowIP("198.51.100.7", now) {
		t.Fatal("超过 burst 且没有时间补充 token 时必须限速")
	}

	normal := apiRateLimiter{}
	for i := 0; i < 200; i++ { // 40 req/s，低于 50 req/s 的配置上限。
		at := now.Add(time.Duration(i) * 25 * time.Millisecond)
		if !normal.allowIP("198.51.100.8", at) {
			t.Fatalf("正常面板频率的第 %d 个请求被误限速", i+1)
		}
	}
}

func TestAPIRateLimiterStateIsBoundedAndGarbageCollected(t *testing.T) {
	limiter := apiRateLimiter{}
	now := time.Unix(2000, 0)
	for i := 0; i < apiRateMaxIPs+100; i++ {
		limiter.allowIP("198.51."+itoa(i/256)+"."+itoa(i%256), now)
	}
	if size := limiter.size(); size != apiRateMaxIPs {
		t.Fatalf("限速状态表必须封顶于 %d，实际 %d", apiRateMaxIPs, size)
	}
	if !limiter.allowIP("203.0.113.9", now.Add(apiRateIdleTTL+apiRateGCPeriod)) {
		t.Fatal("过期来源回收后应能接纳新来源")
	}
	if size := limiter.size(); size != 1 {
		t.Fatalf("GC 后只应留下新来源，实际 %d 条", size)
	}
}

func TestAPIRateLimitResponds429(t *testing.T) {
	srv := &Server{cfg: &config.Config{Token: ""}}
	key := "203.0.113.10"
	srv.apiLimiter.clients = map[string]apiRateBucket{key: {tokens: 0, last: time.Now()}}
	req := httptest.NewRequest(http.MethodGet, "/api/not-found", nil)
	req.RemoteAddr = key + ":12345"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("令牌耗尽应返回 429，got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["error_code"] != codeRateLimited || got["request_id"] == "" {
		t.Fatalf("限速响应缺稳定错误码/request_id: %#v", got)
	}
}

func TestAPIRateLimiterUsesRemoteAddrNotForwardedHeader(t *testing.T) {
	limiter := apiRateLimiter{}
	req := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	req.RemoteAddr = "192.0.2.12:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	if !limiter.allowRequest(req, time.Unix(3000, 0)) {
		t.Fatal("首次请求应通过")
	}
	if limiter.size() != 1 {
		t.Fatalf("限速 key 应只记录 RemoteAddr，size=%d", limiter.size())
	}
}
