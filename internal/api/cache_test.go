package api

import (
	"errors"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 审计项「HTTP API 缓存/节流」要求的测试：命中、过期、并发访问。

func TestCacheHitAvoidsRebuild(t *testing.T) {
	c := newTTLCache(time.Minute)
	var calls int32
	build := func() (any, error) {
		atomic.AddInt32(&calls, 1)
		return "value", nil
	}
	for i := 0; i < 5; i++ {
		v, err := c.load("k", build)
		if err != nil || v != "value" {
			t.Fatalf("第 %d 次: v=%v err=%v", i, v, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("命中缓存时构建次数应为 1, got %d", got)
	}
	if c.size() != 1 {
		t.Errorf("缓存条目数应为 1, got %d", c.size())
	}
}

func TestCacheExpiry(t *testing.T) {
	c := newTTLCache(30 * time.Millisecond)
	var calls int32
	build := func() (any, error) {
		n := atomic.AddInt32(&calls, 1)
		return n, nil
	}
	if _, err := c.load("k", build); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if v, _ := c.load("k", build); v != int32(2) {
		t.Errorf("过期后应重新构建, got %v", v)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("构建次数应为 2, got %d", got)
	}
}

func TestCacheInvalidate(t *testing.T) {
	c := newTTLCache(time.Minute)
	var calls int32
	build := func() (any, error) { return atomic.AddInt32(&calls, 1), nil }
	if _, err := c.load("k", build); err != nil {
		t.Fatal(err)
	}
	c.invalidate()
	if c.size() != 0 {
		t.Errorf("失效后条目数应为 0, got %d", c.size())
	}
	if v, _ := c.load("k", build); v != int32(2) {
		t.Errorf("失效后应重新构建, got %v", v)
	}
}

// 并发单飞：同一 key 的 N 个并发请求只能触发一次加载（防缓存击穿）。
func TestCacheSingleFlight(t *testing.T) {
	c := newTTLCache(time.Minute)
	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.load("k", func() (any, error) {
				atomic.AddInt32(&calls, 1)
				once.Do(func() { close(started) })
				<-release // 卡住首个加载，逼其余请求进入等待
				return "v", nil
			})
			if err != nil || v != "v" {
				t.Errorf("并发 load 失败: v=%v err=%v", v, err)
			}
		}()
	}
	<-started
	time.Sleep(50 * time.Millisecond) // 让其余 goroutine 进入 inflight 等待
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("并发同 key 只应加载 1 次, got %d", got)
	}
}

// 并发访问不同 key 不得互相阻塞或串数据。
func TestCacheConcurrentDistinctKeys(t *testing.T) {
	c := newTTLCache(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "k" + string(rune('a'+i%26))
			want := key + "-val"
			for j := 0; j < 20; j++ {
				v, err := c.load(key, func() (any, error) { return want, nil })
				if err != nil || v != want {
					t.Errorf("key=%s v=%v err=%v", key, v, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// 加载失败不得被缓存（否则一次瞬时故障会被放大成持续故障）。
func TestCacheDoesNotCacheErrors(t *testing.T) {
	c := newTTLCache(time.Minute)
	var calls int32
	build := func() (any, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, http.ErrHandlerTimeout
		}
		return "ok", nil
	}
	if _, err := c.load("k", build); err == nil {
		t.Fatal("首次应返回错误")
	}
	v, err := c.load("k", build)
	if err != nil || v != "ok" {
		t.Errorf("失败不应被缓存, 第二次应成功: v=%v err=%v", v, err)
	}
}

// 端到端：数据版本变化必须让缓存 key 变化（否则会返回过期数据）。
func TestCacheKeyTracksDataVersion(t *testing.T) {
	ts, _, _ := newTestServer(t, "", &fakeSource{backend: "nft", lastOK: 100}, "")
	defer ts.Close()

	s := &Server{src: &fakeSource{backend: "nft", lastOK: 100}}
	k1 := s.cacheKey("summary")
	s.src = &fakeSource{backend: "nft", lastOK: 200}
	k2 := s.cacheKey("summary")
	if k1 == k2 {
		t.Fatal("采集器采样时间变化后缓存 key 必须变化")
	}
	if got := s.cacheKey("daily", "7", "node:1"); got == s.cacheKey("daily", "30", "node:1") {
		t.Error("days 不同必须产生不同 key")
	}
	if got := s.cacheKey("daily", "7", "node:1"); got == s.cacheKey("daily", "7", "node:2") {
		t.Error("scope 不同必须产生不同 key")
	}
}

// 端到端：同一请求重复两次必须返回完全一致的响应体（走缓存路径）。
func TestSummaryEndpointServedFromCache(t *testing.T) {
	ts, _, _ := newTestServer(t, "", &fakeSource{backend: "nft", lastOK: 12345}, "")
	_, _, b1 := getBody(t, ts.URL+"/api/summary")
	_, _, b2 := getBody(t, ts.URL+"/api/summary")
	if b1 != b2 {
		t.Errorf("两次请求响应不一致:\n%s\n%s", b1, b2)
	}
	if !strings.Contains(b1, `"healthy":true`) {
		t.Errorf("summary 响应异常: %s", b1)
	}
	_, _, d1 := getBody(t, ts.URL+"/api/daily?days=7")
	_, _, d2 := getBody(t, ts.URL+"/api/daily?days=7")
	if d1 != d2 {
		t.Errorf("daily 两次响应不一致")
	}
}

// loader panic 也必须释放 singleflight 等待者并清除 inflight，避免单个异常
// 请求导致同 key 后续请求永久阻塞。
func TestCacheLoaderPanicReleasesWaiters(t *testing.T) {
	c := newTTLCache(time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		defer func() { _ = recover() }()
		_, _ = c.load("panic", func() (any, error) {
			close(started)
			<-release
			panic("test loader panic")
		})
	}()
	<-started
	waiterDone := make(chan error, 1)
	go func() {
		_, err := c.load("panic", func() (any, error) {
			return nil, errors.New("waiter incorrectly ran loader")
		})
		waiterDone <- err
	}()
	// 确认 follower 已在 inflight call 上注册，避免依赖固定 sleep 时长（Alpine CI
	// 慢调度下可能让 waiter 尚未运行就释放 leader）。
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		call := c.inflight["panic"]
		registered := call != nil && call.waiters > 0
		c.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter did not register with inflight loader")
		}
		runtime.Gosched()
	}
	close(release)
	select {
	case <-leaderDone:
	case <-time.After(time.Second):
		t.Fatal("leader panic did not return")
	}
	select {
	case err := <-waiterDone:
		if err == nil || !strings.Contains(err.Error(), "cache loader panic") {
			t.Fatalf("waiter should receive loader panic error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("singleflight waiter blocked forever after loader panic")
	}
	// inflight 已清理；后续同 key 请求可以重新执行 loader。
	if got, err := c.load("panic", func() (any, error) { return "recovered", nil }); err != nil || got != "recovered" {
		t.Fatalf("retry after panic failed: got=%v err=%v", got, err)
	}
}

var benchmarkVersionSink string

func BenchmarkDataVersionCached(b *testing.B) {
	s := &Server{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkVersionSink = s.dataVersion()
	}
}

func BenchmarkDataVersionFormat(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lastOK, polVer := int64(i), uint64(i)
		benchmarkVersionSink = strconv.FormatInt(lastOK, 10) + "." + strconv.FormatUint(polVer, 10)
	}
}

func BenchmarkCacheKeyBuilder(b *testing.B) {
	s := &Server{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkVersionSink = s.cacheKey("daily", "365", "node:123456")
	}
}

func BenchmarkCacheKeyConcat(b *testing.B) {
	s := &Server{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key := "daily|" + s.dataVersion()
		key += "|365"
		key += "|node:123456"
		benchmarkVersionSink = key
	}
}

func TestCacheExpiredKeysAreReclaimedOnScheduledSweep(t *testing.T) {
	c := newTTLCache(25 * time.Millisecond)
	if _, err := c.load("old", func() (any, error) { return "old", nil }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := c.load("new", func() (any, error) { return "new", nil }); err != nil {
		t.Fatal(err)
	}
	if got := c.size(); got != 1 {
		t.Fatalf("scheduled sweep should reclaim expired key, size=%d", got)
	}
}

func TestConcurrentCacheInitializationAndInvalidation(t *testing.T) {
	s := &Server{}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if i%2 == 0 {
					s.cacheFor()
				} else {
					s.invalidateCache()
				}
			}
		}(i)
	}
	wg.Wait()
	if s.cacheFor() == nil {
		t.Fatal("cache was not initialized")
	}
}
