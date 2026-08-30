package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetLoginFails() {
	loginFailMu.Lock()
	loginFails = map[string]*loginFailState{}
	loginFailMu.Unlock()
}

func TestLoginThrottleAfterRepeatedFailures(t *testing.T) {
	resetLoginFails()
	t.Cleanup(resetLoginFails)

	now := time.Now()
	key := "198.51.100.5"

	// 前 loginFailBurst 次失败不加延迟（正常用户手滑不受惩罚）
	for i := 0; i < loginFailBurst; i++ {
		loginRecordFail(key, now)
		if d := loginThrottle(key, now); i+1 < loginFailBurst && d != 0 {
			t.Fatalf("第 %d 次失败后不应节流, got %v", i+1, d)
		}
	}
	if d := loginThrottle(key, now); d != loginFailDelay {
		t.Fatalf("达到阈值后应节流 %v, got %v", loginFailDelay, d)
	}

	// 成功登录立即清零
	loginRecordSuccess(key)
	if d := loginThrottle(key, now); d != 0 {
		t.Fatalf("成功后应清零计数, got %v", d)
	}
}

func TestLoginThrottleWindowExpires(t *testing.T) {
	resetLoginFails()
	t.Cleanup(resetLoginFails)

	base := time.Now()
	key := "198.51.100.6"
	for i := 0; i < loginFailBurst+2; i++ {
		loginRecordFail(key, base)
	}
	if d := loginThrottle(key, base); d == 0 {
		t.Fatal("应处于节流状态")
	}
	// 超过窗口后自动恢复
	later := base.Add(loginFailWindow + time.Second)
	if d := loginThrottle(key, later); d != 0 {
		t.Fatalf("超过窗口应恢复, got %v", d)
	}
}

// 成功登录必须零延迟：节流只惩罚失败，不能让手滑几次的用户输对也等 2s。
func TestSuccessfulLoginNotDelayed(t *testing.T) {
	resetLoginFails()
	t.Cleanup(resetLoginFails)

	dir := t.TempDir()
	nodesFile := filepath.Join(dir, "nodes.json")
	if err := os.WriteFile(nodesFile, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	const token = "0123456789abcdef0123456789abcdef"
	ts, _ := newPolicyTestServer(t, token, nodesFile)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	post := func(tok string) (int, time.Duration) {
		start := time.Now()
		resp, err := client.Post(ts.URL+"/login", "application/x-www-form-urlencoded",
			strings.NewReader("token="+tok))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode, time.Since(start)
	}

	// 连续失败到超过阈值
	for i := 0; i < loginFailBurst+1; i++ {
		post("wrong")
	}
	// 正确 token 必须立刻成功
	code, dur := post(token)
	if code != http.StatusFound {
		t.Fatalf("正确 token 应 302, got %d", code)
	}
	if dur >= loginFailDelay {
		t.Errorf("成功登录被节流延迟了 %v（不应超过 %v）", dur, loginFailDelay)
	}
}

// 节流键只能取 RemoteAddr 的 IP，不得信任可伪造的 X-Forwarded-For
// （否则攻击者每次换头即可绕过，并把追踪表撑大）。
func TestLoginClientKeyIgnoresForwardedFor(t *testing.T) {
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "203.0.113.9:44321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := loginClientKey(r); got != "203.0.113.9" {
		t.Fatalf("应使用 RemoteAddr 的 IP, got %q", got)
	}
}

func TestLoginFailTableBounded(t *testing.T) {
	resetLoginFails()
	t.Cleanup(resetLoginFails)

	now := time.Now()
	for i := 0; i < loginFailMaxLen+500; i++ {
		loginRecordFail("10.0."+itoa(i/256)+"."+itoa(i%256), now)
	}
	loginFailMu.Lock()
	n := len(loginFails)
	loginFailMu.Unlock()
	if n > loginFailMaxLen {
		t.Fatalf("追踪表未设上限: %d > %d", n, loginFailMaxLen)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
