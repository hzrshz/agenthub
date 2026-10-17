package httpx

import (
	"net/http"
	"strings"

	"github.com/uvwt/agenthub/internal/config"
)

// 安全响应头（等保二级基线要求）。
//
// 这些头是应用层能自己补的部分；Strict-Transport-Security 例外——
// HSTS 只在 HTTPS 连接上才有意义，而 hub 按设计只听 HTTP（TLS 由反代终结），
// 所以 HSTS 必须在反代上配，应用层补了也不会被浏览器采纳。
const (
	// contentSecurityPolicy 锁定资源来源。
	//
	// script-src 不含 'unsafe-inline'：前端是构建后的静态产物，没有内联脚本，
	// 因此可以严格禁止任何注入的内联 JS（这是 CSP 最有价值的部分）。
	//
	// style-src 需要 'unsafe-inline'：React 的 style={{...}} 会渲染成 style
	// 属性（web/src/App.tsx、pages/*.tsx、components/Pagination.tsx 均用到），
	// 而 CSP 的 style-src 不含 unsafe-inline 时会把这些属性样式全部拦掉。
	// 这是"防 CSS 注入"与"允许组件内联样式"之间的取舍——内联样式不构成
	// 脚本执行风险，因此这里放宽是合理的。
	//
	// font-src 允许同源：字体已自托管在 /fonts/（不再外链 Google Fonts）。
	contentSecurityPolicy = "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"font-src 'self'; " +
		"img-src 'self' data:; " +
		"connect-src 'self'; " +
		"object-src 'none'; " +
		"base-uri 'none'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'"

	// xFrameOptions 是 CSP frame-ancestors 的旧版等价物。
	// 两者都给：老浏览器只认 X-Frame-Options，扫描器也普遍查它。
	xFrameOptions = "DENY"

	// nosniff 禁止浏览器猜测 MIME 类型，防止把 .json 当 HTML 执行。
	xContentTypeOptions = "nosniff"

	// referrerPolicy 不向外站泄露内部路径（内网 URL 常含节点 ID、用户 ID）。
	referrerPolicy = "no-referrer"

	// crossOriginOpenerPolicy 隔离浏览上下文，降低侧信道风险。
	crossOriginOpenerPolicy = "same-origin"
)

// securityHeaders 给所有响应补齐安全头。
//
// 用中间件而不是逐个处理器加：漏一个就是漏洞，而路由表还会持续增长。
// 静态资源（web_dist）与 API 共用同一套策略——内网部署不应为了静态资源
// 而放宽全局。
//
// trustedProxies 决定是否采信 X-Forwarded-Proto（只用于判断能否下发
// HSTS），语义与限流键取 IP 时完全一致。
func securityHeaders(trustedProxies *config.Resolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Frame-Options", xFrameOptions)
		header.Set("X-Content-Type-Options", xContentTypeOptions)
		header.Set("Referrer-Policy", referrerPolicy)
		header.Set("Cross-Origin-Opener-Policy", crossOriginOpenerPolicy)
		// 禁用不需要的浏览器能力，减少攻击面。
		header.Set("X-Permitted-Cross-Domain-Policies", "none")
		// 不暴露服务端实现细节（扫描器会查，泄露也会帮攻击者定位版本）。
		header.Set("Server", "agenthub")
		// HSTS 只在 HTTPS 连接上才有意义，而 hub 按设计只听 HTTP
		// （TLS 由反代终结）。因此只有确认本次请求走的是 HTTPS 时才下发：
		// 在明文连接上发 HSTS 不会让浏览器记住策略（浏览器会忽略），
		// 却会让扫描器/审计误以为"已配置"。
		if requestIsHTTPS(r, trustedProxies) {
			header.Set("Strict-Transport-Security", strictTransportSecurity)
		}
		next.ServeHTTP(w, r)
	})
}

// strictTransportSecurity 一年有效期。
// 不下发 includeSubDomains / preload：政务侧的反代未必覆盖全部子域，
// 预加载指令一旦提交到浏览器预加载列表就难以撤销，误配的代价远大于收益。
const strictTransportSecurity = "max-age=31536000"

// requestIsHTTPS 判断本次请求是否经 HTTPS 抵达。
//
// X-Forwarded-Proto 只在请求来自可信反代时可信，因此复用 config.ClientIP
// 的那套可信网段判定：反代可信才采信该头，否则退回 r.TLS != nil
// （直连 HTTPS 的情形）。
func requestIsHTTPS(r *http.Request, trustedProxies *config.Resolver) bool {
	if r.TLS != nil {
		return true
	}
	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		return false
	}
	if trustedProxies != nil && trustedProxies.Trusts(config.RemoteIP(r)) {
		return strings.EqualFold(proto, "https")
	}
	return false
}
