package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/uvwt/agenthub/internal/config"
)

// 安全响应头基线（等保二级）。
//
// 这类检查最容易在后续改动中被"顺手删掉"——某个 handler 自己写 header、
// 或者有人改 routes() 时把中间件摘掉，都不会有编译错误。因此逐项断言锁死。
func TestSecurityHeadersPresent(t *testing.T) {
	// 直接构造 Server 以拿到 Handler（testEnv 只暴露了 httptest.Server）。
	srv := NewServer(nil, nil, nil, nil, nil, "https://hub.example.com")

	// 覆盖不同类型的响应：API、未匹配路由的 404、静态资源。
	// 扫描器不区分状态码，404 缺头照样会被报。
	cases := []struct {
		name string
		path string
	}{
		{"api", "/v1/auth/me"},
		{"404", "/no-such-path"},
		{"static", "/index.html"},
		{"healthz", "/healthz"},
	}
	want := map[string]string{
		"Content-Security-Policy":   contentSecurityPolicy,
		"X-Frame-Options":           xFrameOptions,
		"X-Content-Type-Options":    xContentTypeOptions,
		"Referrer-Policy":           referrerPolicy,
		"Cross-Origin-Opener-Policy": crossOriginOpenerPolicy,
		"X-Permitted-Cross-Domain-Policies": "none",
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			for header, expected := range want {
				if got := rec.Header().Get(header); got != expected {
					t.Errorf("%s: %s = %q, want %q", tc.path, header, got, expected)
				}
			}
		})
	}
}

// CSP 必须真正有效，而不只是"有个 CSP 头"。
// 特别断言 script-src 不含 unsafe-inline——那是 CSP 防注入的核心价值所在。
func TestContentSecurityPolicyIsStrict(t *testing.T) {
	if strings.Contains(contentSecurityPolicy, "script-src 'self' 'unsafe-inline'") {
		t.Fatal("script-src must not allow unsafe-inline: the build output has no inline scripts, so allowing it defeats the point of CSP")
	}
	if !strings.Contains(contentSecurityPolicy, "object-src 'none'") {
		t.Error("object-src must be 'none' (blocks plugin/embed based attacks)")
	}
	if !strings.Contains(contentSecurityPolicy, "frame-ancestors 'none'") {
		t.Error("frame-ancestors must be 'none' (clickjacking defence)")
	}
	if !strings.Contains(contentSecurityPolicy, "base-uri 'none'") {
		t.Error("base-uri must be 'none' (blocks base tag hijacking)")
	}
	// 字体必须允许同源：字体已自托管在 /fonts/，CSP 若不放行会把字全变系统字体。
	if !strings.Contains(contentSecurityPolicy, "font-src 'self'") {
		t.Error("font-src must allow 'self': fonts are self-hosted under /fonts/")
	}
}

// style-src 放宽到 unsafe-inline 是必需的（React style={{...}} 会渲染成
// style 属性）。这条测试记录这个取舍，防止后来者误以为可以收紧。
func TestStyleSrcAllowsInlineForReactStyles(t *testing.T) {
	if !strings.Contains(contentSecurityPolicy, "style-src 'self' 'unsafe-inline'") {
		t.Fatal("style-src must allow unsafe-inline: React renders style={{...}} as style attributes, which CSP blocks otherwise")
	}
}

// HSTS 只在真正走 HTTPS 时下发。
//
// 关键在于不能无条件发：hub 按设计只听 HTTP（TLS 由反代终结），在明文
// 连接上发 HSTS 浏览器会忽略，却会让扫描器误判为"已配置"。
func TestHSTSOnlyOverHTTPS(t *testing.T) {
	resolver, err := config.Config{TrustedProxyCIDRs: []string{"10.0.0.0/8"}}.TrustedProxies()
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	cases := []struct {
		name       string
		remoteAddr string
		proto      string
		wantHSTS   bool
	}{
		{"plain http, no header", "10.0.0.1:1234", "", false},
		{"plain http, forged proto from untrusted", "203.0.113.9:1234", "https", false},
		{"https via trusted proxy", "10.0.0.1:1234", "https", true},
		{"http via trusted proxy", "10.0.0.1:1234", "http", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			rec := httptest.NewRecorder()
			securityHeaders(resolver, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(rec, req)

			got := rec.Header().Get("Strict-Transport-Security")
			if tc.wantHSTS && got == "" {
				t.Error("expected HSTS over HTTPS, got none")
			}
			if !tc.wantHSTS && got != "" {
				t.Errorf("HSTS must not be sent over plain HTTP, got %q", got)
			}
		})
	}
}

// 不暴露服务端实现细节。
func TestServerHeaderDoesNotLeakImplementation(t *testing.T) {
	srv := NewServer(nil, nil, nil, nil, nil, "https://hub.example.com")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := rec.Header().Get("Server"); got != "agenthub" {
		t.Errorf("Server header = %q, want %q (must not expose Go/net/http version)", got, "agenthub")
	}
}