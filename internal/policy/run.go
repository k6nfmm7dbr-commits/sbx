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

	// IP/连接采集 ~1s 一次，让新 IP 上线 / 掉线后约 1 个周期内在页面反映出来。
	interval := time.Second
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
		}
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
