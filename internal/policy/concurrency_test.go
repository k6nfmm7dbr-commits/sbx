package policy

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/connection"
)

// TestSnapshotReadersNeverTouchMutableState 是并发安全回归测试。
//
// 历史 bug：reconcile 只持 runMu 就直接改 s.ipStates 及其内部
// Slots/Observed/Rejected map，而 IPStateSnapshot / NodeIPSnapshot / ActiveIPs
// 只持 s.mu.RLock 就遍历同一批 map——两把锁互不相干，1s 一次的 reconcile
// 与每秒一次的 SSE 快照在多核上必然 `fatal error: concurrent map iteration
// and map write` 并崩掉整个面板进程。
//
// 该测试必须在 GOMAXPROCS>1 下跑（CI 默认满足）才有意义；配合 -race 更佳。
func TestSnapshotReadersNeverTouchMutableState(t *testing.T) {
	s := newTestService(t)
	for i := 1; i <= 4; i++ {
		seedNode(t, s, int64(i), "vless", int64(4430+i))
	}
	ctx := context.Background()
	s.SetRemoteIPs(procResult(nil, nil, false))

	// 每轮返回不同客户端 IP，逼迫 slot/observed map 反复插入与删除。
	var round int64
	s.SetConntrack(func(string) connection.ConntrackResult {
		cur := atomic.AddInt64(&round, 1)
		flows := make([]connection.ConntrackFlow, 0, 8)
		for i := 1; i <= 4; i++ {
			flows = append(flows, connection.ConntrackFlow{
				Proto: "tcp", State: "ESTABLISHED",
				SrcIP:   "203.0.113." + strconv.FormatInt(cur%40+1, 10),
				SrcPort: int(cur%1000) + 2000,
				DstPort: 4430 + i,
				Bytes:   cur * 100,
			})
		}
		return connection.ConntrackResult{Available: true, Flows: flows}
	})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // 模拟 policy.Run
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.reconcile(ctx)
		}
	}()

	// 模拟两个 SSE 客户端 + ip-state / active-ips 端点并发读。
	for k := 0; k < 2; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.IPStateSnapshot()
				_ = s.NodeIPSnapshot("1")
				_ = s.ActiveIPs("2")
				_, _ = s.Snapshot()
			}
		}()
	}

	time.Sleep(1500 * time.Millisecond)
	close(stop)
	wg.Wait()
	// 能跑到这里就说明没有并发 map 读写导致的 fatal error。
}

// 发布的快照必须是不可变的：调用方修改返回值不得影响服务内部状态，
// 也不得影响后续快照（防止读侧写坏共享结构）。
func TestPublishedSnapshotIsIndependent(t *testing.T) {
	s := newTestService(t)
	seedNode(t, s, 1, "vless", 443)
	ctx := context.Background()
	s.SetRemoteIPs(procResult(nil, nil, false))
	s.SetConntrack(func(string) connection.ConntrackResult {
		return connection.ConntrackResult{Available: true, Flows: []connection.ConntrackFlow{
			{Proto: "tcp", State: "ESTABLISHED", SrcIP: "203.0.113.9", SrcPort: 5000, DstPort: 443, Bytes: 100},
		}}
	})
	if err := s.reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	ips := s.ActiveIPs("1")
	if len(ips) != 1 {
		t.Fatalf("应有 1 个在线 IP, got %v", ips)
	}
	ips[0] = "tampered"
	if got := s.ActiveIPs("1"); got[0] != "203.0.113.9" {
		t.Errorf("ActiveIPs 返回了内部切片引用, 被调用方改坏: %v", got)
	}

	m := s.IPStateSnapshot()
	delete(m, "1")
	if _, ok := s.IPStateSnapshot()["1"]; !ok {
		t.Error("IPStateSnapshot 返回了内部 map 引用, 被调用方删掉了节点")
	}
}
