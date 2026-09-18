package firewall

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 合并读取（single-flight）：同一时刻的并发 Read 只能触发一次 exec。
func TestNftReadSingleFlight(t *testing.T) {
	var execs int32
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once

	restore := swapRunCmd(func(ctx context.Context, args ...string) (int, string, string) {
		atomic.AddInt32(&execs, 1)
		once.Do(func() { close(started) })
		<-release // 卡住首次 exec，逼其余 Read 进入等待
		return 0, `{"nftables":[{"counter":{"name":"sbx_n1_i","bytes":10,"packets":2}}]}`, ""
	})
	defer restore()

	n := NewNft("/nonexistent")
	const readers = 12
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := n.Read(context.Background())
			if err != nil {
				t.Errorf("Read 失败: %v", err)
				return
			}
			if snap["sbx_n1_i"] != [2]int64{10, 2} {
				t.Errorf("快照内容异常: %v", snap)
			}
		}()
	}
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&execs); got != 1 {
		t.Errorf("并发 Read 应只 exec 1 次, got %d", got)
	}
}

// 串行 Read 不得复用上一次结果（无陈旧风险）：每次都是新的真实读数。
func TestNftReadNoStaleCache(t *testing.T) {
	var execs int32
	restore := swapRunCmd(func(ctx context.Context, args ...string) (int, string, string) {
		n := atomic.AddInt32(&execs, 1)
		if n == 1 {
			return 0, `{"nftables":[{"counter":{"name":"sbx_n1_i","bytes":100,"packets":1}}]}`, ""
		}
		return 0, `{"nftables":[{"counter":{"name":"sbx_n1_i","bytes":999,"packets":9}}]}`, ""
	})
	defer restore()

	n := NewNft("/nonexistent")
	first, err := n.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := n.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first["sbx_n1_i"][0] != 100 {
		t.Errorf("首次读数错误: %v", first)
	}
	if second["sbx_n1_i"][0] != 999 {
		t.Errorf("第二次 Read 复用了旧结果（出现陈旧缓存）: %v", second)
	}
	if got := atomic.LoadInt32(&execs); got != 2 {
		t.Errorf("串行两次 Read 应 exec 2 次, got %d", got)
	}
}

// 合并读取不得把错误也"共享"成永久状态：失败后下一次必须重新尝试。
func TestNftReadErrorNotSticky(t *testing.T) {
	var execs int32
	restore := swapRunCmd(func(ctx context.Context, args ...string) (int, string, string) {
		if atomic.AddInt32(&execs, 1) == 1 {
			return 1, "", "Operation not permitted"
		}
		return 0, `{"nftables":[{"counter":{"name":"sbx_n1_i","bytes":5,"packets":1}}]}`, ""
	})
	defer restore()

	n := NewNft("/nonexistent")
	if _, err := n.Read(context.Background()); err == nil {
		t.Fatal("首次应失败")
	}
	snap, err := n.Read(context.Background())
	if err != nil {
		t.Fatalf("失败不应粘住, 第二次应成功: %v", err)
	}
	if snap["sbx_n1_i"][0] != 5 {
		t.Errorf("快照异常: %v", snap)
	}
}

// 失败路径与 ErrLookup 分类保持不变。
func TestNftReadErrorClassification(t *testing.T) {
	cases := []struct {
		name    string
		rc      int
		stderr  string
		wantLkp bool
	}{
		{"表不存在 → ErrLookup", 1, "Error: No such file or directory", true},
		{"无权限 → 普通错误", 1, "Operation not permitted", false},
		{"语法错误 → 普通错误", 1, "syntax error", false},
	}
	for _, c := range cases {
		restore := swapRunCmd(func(ctx context.Context, args ...string) (int, string, string) {
			return c.rc, "", c.stderr
		})
		_, err := NewNft("/nonexistent").Read(context.Background())
		restore()
		if err == nil {
			t.Errorf("%s: 应返回错误", c.name)
			continue
		}
		if got := IsLookup(err); got != c.wantLkp {
			t.Errorf("%s: IsLookup=%v want %v (err=%v)", c.name, got, c.wantLkp, err)
		}
	}
	_ = errors.New
}

// swapRunCmd 替换包级命令执行钩子（测试用）。
func swapRunCmd(fn func(context.Context, ...string) (int, string, string)) func() {
	old := runCmdFn
	runCmdFn = fn
	return func() { runCmdFn = old }
}
