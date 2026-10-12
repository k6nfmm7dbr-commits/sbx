package api

import (
	"net/http"
	"sync"
	"time"
)

// 面板每个标签页默认每秒产生约 0.625 次轮询（live 0.5/s + summary 0.125/s），
// 50 req/s、100 burst 给多标签页与人工操作留出很大余量，同时能限制异常请求洪泛。
const (
	apiRatePerSecond = 50.0
	apiRateBurst     = 100.0
	apiRateMaxIPs    = 8192
	apiRateGCPeriod  = 30 * time.Second
	apiRateIdleTTL   = 2 * time.Minute
)

type apiRateBucket struct {
	tokens float64
	last   time.Time
}

// apiRateLimiter 是按 RemoteAddr IP 限制的有界令牌桶。状态容量固定，过期项按周期回收；
// 表满时拒绝尚未登记的新来源，不扩容失控。绝不读取 X-Forwarded-For，因为它可由
// 任意客户端伪造，采信会让攻击者绕过限速并用虚构地址撑爆状态表。
type apiRateLimiter struct {
	mu      sync.Mutex
	clients map[string]apiRateBucket
	lastGC  time.Time
}

func (l *apiRateLimiter) allowIP(ip string, now time.Time) bool {
	if ip == "" {
		ip = "<unknown>"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.clients == nil {
		l.clients = make(map[string]apiRateBucket)
	}
	if l.lastGC.IsZero() || now.Sub(l.lastGC) >= apiRateGCPeriod {
		for key, bucket := range l.clients {
			if now.Sub(bucket.last) > apiRateIdleTTL {
				delete(l.clients, key)
			}
		}
		l.lastGC = now
	}

	bucket, exists := l.clients[ip]
	if !exists {
		if len(l.clients) >= apiRateMaxIPs {
			return false
		}
		l.clients[ip] = apiRateBucket{tokens: apiRateBurst - 1, last: now}
		return true
	}
	elapsed := now.Sub(bucket.last).Seconds()
	if elapsed < 0 { // 时钟回拨不应凭空补 token。
		elapsed = 0
	}
	bucket.tokens += elapsed * apiRatePerSecond
	if bucket.tokens > apiRateBurst {
		bucket.tokens = apiRateBurst
	}
	bucket.last = now
	if bucket.tokens < 1 {
		l.clients[ip] = bucket
		return false
	}
	bucket.tokens--
	l.clients[ip] = bucket
	return true
}

func (l *apiRateLimiter) allowRequest(r *http.Request, now time.Time) bool {
	return l.allowIP(loginClientKey(r), now)
}

func (l *apiRateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.clients)
}
