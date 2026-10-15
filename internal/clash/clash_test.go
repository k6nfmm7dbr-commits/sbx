package clash

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConnectionsParsesAndSendsSecret(t *testing.T) {
	const body = `{"connections":[
	  {"id":"c1","upload":11,"download":22,"rule":"final",
	   "metadata":{"network":"tcp","type":"shadowsocks/sbx-n2","host":"a.com","destinationIP":"1.2.3.4"}},
	  {"id":"c2","upload":33,"download":44,
	   "metadata":{"network":"udp","type":"trojan/sbx-n5","host":""}}
	],"uploadTotal":44,"downloadTotal":66}`

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connections" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(strings.TrimPrefix(srv.URL, "http://"), "sekrit")
	conns, err := c.Connections(context.Background())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if gotAuth != "Bearer sekrit" {
		t.Errorf("应带上密钥鉴权，实得 %q", gotAuth)
	}
	if len(conns) != 2 {
		t.Fatalf("应解析出 2 条连接，实得 %d", len(conns))
	}
	if conns[0].Metadata.Host != "a.com" || conns[0].Upload != 11 || conns[0].Download != 22 {
		t.Errorf("首条连接解析不对: %+v", conns[0])
	}
	if tag := conns[0].Metadata.InboundTag(); tag != "sbx-n2" {
		t.Errorf("应取出去 / 之后的标签，实得 %q", tag)
	}
	if tag := conns[1].Metadata.InboundTag(); tag != "sbx-n5" {
		t.Errorf("实得 %q", tag)
	}
}

// 未授权必须被判为错误，不能当成「没有连接」——
// 否则配置写错时会静默地一直采不到数据。
func TestConnectionsRejectsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := New(strings.TrimPrefix(srv.URL, "http://"), "wrong")
	if _, err := c.Connections(context.Background()); err == nil {
		t.Fatal("401 应返回错误")
	}
}

func TestConnectionsRejectsBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()
	c := New(strings.TrimPrefix(srv.URL, "http://"), "")
	if _, err := c.Connections(context.Background()); err == nil {
		t.Fatal("非 JSON 响应应返回错误")
	}
}

// 没有 "/" 的 type 字段原样返回（防御异常输入）。
func TestInboundTagWithoutSlash(t *testing.T) {
	if got := (Metadata{Type: "weird"}).InboundTag(); got != "weird" {
		t.Errorf("实得 %q", got)
	}
}
