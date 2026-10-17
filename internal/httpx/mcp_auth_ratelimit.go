package httpx

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/uvwt/agenthub/internal/config"
)

// mcpAuthRateLimiter 是 /mcp 的认证失败按 IP 限流器（内存态）。
//
// DESIGN §3.2 要求"认证失败按 IP 限流"：网关侧每用户 60 req/min 的速率限制
// 只在认证成功之后生效，无效 Bearer token 的 401 此前完全无限流，可被无成本
// 爆破。这里补上认证失败侧的独立限流：失败超过阈值即封锁该 IP 一段时间。
//
// 内存态而非落 login_throttles：MCP 侧是机器对机器入口，进程重启清零可接受
// （与 /v1/nodes/pair 的 pairRateLimiter 同一立场）。
type mcpAuthRateLimiter struct {
	mu      sync.Mutex
	entries map[string]*mcpAuthWindow
}

type mcpAuthWindow struct {
	failures    int
	blockedTill time.Time
}

func newMCPAuthRateLimiter() *mcpAuthRateLimiter {
	return &mcpAuthRateLimiter{entries: make(map[string]*mcpAuthWindow)}
}

// allow 报告该 IP 是否仍可尝试认证；被封锁时返回剩余等待秒数。
func (l *mcpAuthRateLimiter) allow(ip string) (bool, int) {
	if ip == "" {
		ip = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	entry, exists := l.entries[ip]
	if !exists {
		l.entries[ip] = &mcpAuthWindow{}
		// 顺手清理已解除封锁且无失败累积的旧条目，防止 map 无界增长。
		for key, window := range l.entries {
			if now.After(window.blockedTill) && window.failures == 0 {
				delete(l.entries, key)
			}
		}
		return true, 0
	}
	if now.Before(entry.blockedTill) {
		return false, int(entry.blockedTill.Sub(now).Seconds()) + 1
	}
	if entry.failures >= config.MCPAuthFailureLimitPerMinute {
		entry.blockedTill = now.Add(config.MCPAuthFailureBlock)
		return false, int(config.MCPAuthFailureBlock.Seconds())
	}
	return true, 0
}

// recordFailure 累加该 IP 的认证失败计数。
func (l *mcpAuthRateLimiter) recordFailure(ip string) {
	if ip == "" {
		ip = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, exists := l.entries[ip]
	if !exists {
		entry = &mcpAuthWindow{}
		l.entries[ip] = entry
	}
	entry.failures++
}

// middleware 包装 /mcp 处理器：认证失败超限时 429 + Retry-After。
//
// 只能包在"认证失败"这一段上——成功认证的请求不应被此限流器影响，
// 那由网关自身的每用户速率限制负责。
func (l *mcpAuthRateLimiter) middleware(clientIP func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if allowed, retryAfter := l.allow(ip); !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many authentication failures")
			return
		}
		// 认证失败时累加计数（成功认证的请求不累加）。
		rec := &authFailureRecorder{ResponseWriter: w, onFailure: func() {
			l.recordFailure(ip)
		}}
		next(rec, r)
	}
}

// authFailureRecorder 观察下游是否写了 401；只关心状态码，不改写响应体。
type authFailureRecorder struct {
	http.ResponseWriter
	status    int
	onFailure func()
}

func (a *authFailureRecorder) WriteHeader(status int) {
	a.status = status
	if status == http.StatusUnauthorized {
		a.onFailure()
	}
	a.ResponseWriter.WriteHeader(status)
}

func (a *authFailureRecorder) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.ResponseWriter.Write(b)
}
