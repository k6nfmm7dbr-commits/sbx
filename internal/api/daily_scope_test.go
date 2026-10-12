package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestDailyScopeValidation(t *testing.T) {
	ts, _, _ := newTestServer(t, "", &fakeSource{backend: "nft"}, "")
	for _, scope := range []string{"", "node:1", "node:123456789012345678"} {
		path := "/api/daily"
		if scope != "" {
			path += "?scope=" + url.QueryEscape(scope)
		}
		resp := doReq(t, ts, http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("合法 scope %q 应返回 200，got %d: %s", scope, resp.StatusCode, body(t, resp))
		} else {
			resp.Body.Close()
		}
	}
	for _, scope := range []string{"node:abc", "node:-1", "node:1;DROP", "node:0", "node:" + strings.Repeat("9", 19)} {
		path := "/api/daily?scope=" + url.QueryEscape(scope)
		resp := doReq(t, ts, http.MethodGet, path, nil)
		got := body(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("非法 scope %q 应返回 400，got %d: %s", scope, resp.StatusCode, got)
			continue
		}
		if !strings.Contains(got, `"error_code":"`+codeInvalidScope+`"`) || !strings.Contains(got, "node:<positive integer>") {
			t.Errorf("非法 scope 响应缺错误码或原因: %s", got)
		}
	}
}
