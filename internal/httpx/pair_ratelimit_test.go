package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uvwt/agenthub/internal/config"
)

// pair 限流：同 IP 超过 PairAttemptsPerMinute 后 429 + Retry-After；不同 IP 不受影响。
func TestPairRateLimiter(t *testing.T) {
	limiter := newPairRateLimiter()
	handler := limiter.middleware(config.RemoteIP, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	do := func(ip string) int {
		req := httptest.NewRequest("POST", "/v1/nodes/pair", nil)
		req.RemoteAddr = ip + ":12345"
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}

	for i := 0; i < 10; i++ {
		if code := do("203.0.113.1"); code != http.StatusOK {
			t.Fatalf("attempt %d from same IP should pass, got %d", i+1, code)
		}
	}
	if code := do("203.0.113.1"); code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt must be 429, got %d", code)
	}
	if code := do("203.0.113.2"); code != http.StatusOK {
		t.Fatalf("different IP must be unaffected, got %d", code)
	}
}
