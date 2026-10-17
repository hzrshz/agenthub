package httpx

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/uvwt/agenthub/internal/config"
)

// clientIP 回归：默认不采信 X-Forwarded-For（客户端可伪造，会绕过所有按 IP 限流）。
//
// 审查实测过的绕过：同一真实 IP 每次伪造不同 XFF，15 次非法配对请求全部
// 返回 400、零个 429。这条测试锁死修复后的行为。
func TestClientIPIgnoresUntrustedForwardedFor(t *testing.T) {
	newServer := func(trusted *config.Resolver) *Server {
		s := newServer(nil, nil, nil, nil, nil, "", trusted)
		return s
	}

	t.Run("no trusted proxies", func(t *testing.T) {
		s := newServer(nil)
		for _, xff := range []string{"10.0.0.1", "10.0.0.2", "203.0.113.9"} {
			req := httptest.NewRequest("POST", "/v1/nodes/pair", nil)
			req.RemoteAddr = "198.51.100.7:5555"
			req.Header.Set("X-Forwarded-For", xff)
			if got := s.clientIP(req); got != "198.51.100.7" {
				t.Fatalf("XFF must be ignored without trusted proxies: got %q (xff=%s)", got, xff)
			}
		}
	})

	t.Run("proxy in trusted range", func(t *testing.T) {
		resolver, err := config.Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}.TrustedProxies()
		if err != nil {
			t.Fatalf("resolver: %v", err)
		}
		s := newServer(resolver)
		req := httptest.NewRequest("POST", "/v1/nodes/pair", nil)
		req.RemoteAddr = "10.0.0.1:5555" // 反代在可信网段内
		req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
		if got := s.clientIP(req); got != "203.0.113.9" {
			t.Fatalf("trusted proxy must be able to supply the real client IP, got %q", got)
		}
	})

	t.Run("attacker outside trusted range", func(t *testing.T) {
		resolver, err := config.Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}.TrustedProxies()
		if err != nil {
			t.Fatalf("resolver: %v", err)
		}
		s := newServer(resolver)
		req := httptest.NewRequest("POST", "/v1/nodes/pair", nil)
		// 攻击者直连（不在可信网段），伪造 XFF：必须被无视。
		req.RemoteAddr = "203.0.113.66:5555"
		req.Header.Set("X-Forwarded-For", "10.0.0.1")
		if got := s.clientIP(req); got != "203.0.113.66" {
			t.Fatalf("XFF from untrusted source must be ignored, got %q", got)
		}
	})
}

// 可信反代网段解析：非法配置必须在启动时报错，而不是静默降级。
func TestTrustedProxyCIDRValidation(t *testing.T) {
	if _, err := (config.Config{TrustedProxyCIDRs: []string{"not-a-cidr"}}).TrustedProxies(); err == nil {
		t.Fatal("invalid CIDR must fail at startup")
	}
	if _, err := (config.Config{TrustedProxyCIDRs: []string{"127.0.0.1/32"}}).TrustedProxies(); err != nil {
		t.Fatalf("valid CIDR must parse: %v", err)
	}
}

// /v1/nodes/pair 按 IP 限流的端到端 429：走真实路由，不只测裸中间件。
func TestPairEndpointRateLimitEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	limited := 0
	for i := 0; i < config.PairAttemptsPerMinute+3; i++ {
		req, _ := http.NewRequest("POST", e.ts.URL+"/v1/nodes/pair",
			strings.NewReader(`{"code":"pair_wrong","device_id":"device-x-01","name":"n"}`))
		req.Header.Set("Content-Type", "application/json")
		// 不带 XFF：同一真实连接 IP，必须触发限流。
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("pair request: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited++
			if resp.Header.Get("Retry-After") == "" {
				t.Fatal("429 must carry Retry-After")
			}
		}
	}
	if limited == 0 {
		t.Fatalf("pair endpoint must rate-limit by IP, got no 429 in %d attempts", config.PairAttemptsPerMinute+3)
	}
}

// 伪造 XFF 不得绕过 /v1/nodes/pair 的按 IP 限流。
func TestPairEndpointRateLimitSurvivesForgedXFF(t *testing.T) {
	e := newTestEnv(t)
	limited := 0
	for i := 0; i < config.PairAttemptsPerMinute+3; i++ {
		req, _ := http.NewRequest("POST", e.ts.URL+"/v1/nodes/pair",
			strings.NewReader(`{"code":"pair_wrong","device_id":"device-x-02","name":"n"}`))
		req.Header.Set("Content-Type", "application/json")
		// 每次伪造一个不同的"来源 IP"——修复前这会让限流完全失效。
		req.Header.Set("X-Forwarded-For", "10."+strconv.Itoa(i/256)+"."+strconv.Itoa(i%256)+".1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("pair request: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatalf("forged X-Forwarded-For must not bypass the pair rate limit (got 0 x 429)")
	}
}

// /mcp 认证失败按 IP 限流（DESIGN §3.2）：无效 token 的 401 不能无限流。
func TestMCPAuthFailureRateLimit(t *testing.T) {
	limiter := newMCPAuthRateLimiter()
	handler := limiter.middleware(config.RemoteIP, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	do := func(ip string) int {
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.RemoteAddr = ip + ":12345"
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}
	blocked := 0
	for i := 0; i < config.MCPAuthFailureLimitPerMinute+3; i++ {
		if do("203.0.113.50") == http.StatusTooManyRequests {
			blocked++
		}
	}
	if blocked == 0 {
		t.Fatalf("repeated MCP auth failures must eventually be rate limited")
	}
}