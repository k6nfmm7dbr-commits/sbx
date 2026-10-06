package policy

import (
	"context"
	"log/slog"
	"time"
)

// Run 周期执行 reconcile，直到 ctx 取消。
func (s *Service) Run(ctx context.Context) {
	// 启动立即同步一次，确保策略在 API 就绪前已加载。
	if err := s.reconcile(ctx); err != nil {
		s.setErr(err)
		slog.Warn("策略初始同步失败", "err", err)
	} else {
		slog.Info("策略系统就绪")
	}

	// 自适应轮询间隔：根据活跃 IP 数量动态调整。
	// - 0 活跃 IP：5s（低流量节点，节省 CPU）
	// - 1-5 活跃 IP：2s
	// - >5 活跃 IP：1s（高流量场景需要快速响应）
	interval := s.adaptiveInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.reconcile(ctx); err != nil {
				s.setErr(err)
				slog.Warn("策略同步失败", "err", err)
			} else {
				s.setErr(nil)
			}
			// 每轮结束后重新计算间隔，适应流量变化。
			interval = s.adaptiveInterval()
			ticker.Reset(interval)
		}
	}
}

// adaptiveInterval 根据当前活跃 IP 数量返回合适的轮询间隔。
func (s *Service) adaptiveInterval() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	activeIPs := 0
	for _, st := range s.ipStates {
		activeIPs += st.activeGrantedCount()
	}
	switch {
	case activeIPs == 0:
		return 5 * time.Second
	case activeIPs <= 5:
		return 2 * time.Second
	default:
		return time.Second
	}
}

func (s *Service) setErr(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.mu.Lock()
	s.lastErr = msg
	s.mu.Unlock()
}
