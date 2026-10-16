package httpx

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/uvwt/agenthub/internal/config"
)

// pairRateLimiter 是 /v1/nodes/pair 的按 IP 限流器（固定窗口，内存态）。
// 配对码爆破防护：每个 IP 每分钟最多 PairAttemptsPerMinute 次尝试，
// 超限 429 + Retry-After。进程重启即清零——对爆破防护足够，
// 攻击者重启我们的进程不获利（配对码本身短时单次）。
type pairRateLimiter struct {
	mu      sync.Mutex
	windows map[string]*pairWindow
}

type pairWindow struct {
	start time.Time
	count int
}

func newPairRateLimiter() *pairRateLimiter {
	return &pairRateLimiter{windows: make(map[string]*pairWindow)}
}

// allow 报告该 IP 本分钟是否还能尝试；超限时返回重试等待秒数。
func (l *pairRateLimiter) allow(ip string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	window, exists := l.windows[ip]
	if !exists || now.Sub(window.start) >= time.Minute {
		l.windows[ip] = &pairWindow{start: now, count: 1}
		// 顺手清理上一分钟之前的旧窗口，防止 map 无界增长。
		for key, w := range l.windows {
			if now.Sub(w.start) >= 2*time.Minute {
				delete(l.windows, key)
			}
		}
		return true, 0
	}
	if window.count >= config.PairAttemptsPerMinute {
		retry := int(time.Minute.Seconds()) - int(now.Sub(window.start).Seconds()) + 1
		return false, retry
	}
	window.count++
	return true, 0
}

// middleware 包装 HTTP 处理器执行按 IP 限流。
func (l *pairRateLimiter) middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if ip == "" {
			ip = "unknown"
		}
		allowed, retryAfter := l.allow(ip)
		if !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many pairing attempts")
			return
		}
		next(w, r)
	}
}
