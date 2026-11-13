package httpx

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// accessLog 记录 API 与 MCP 请求的访问日志（方法/路径/状态/耗时/来源 IP/UA）。
//
// 为什么需要：hub 此前只有"异常与生命周期"日志，成功请求与 /mcp 的 401
// 都不留痕——MCP 是机器对机器入口，客户端报"连接中/超时"时无法判断请求
// 是否到达过 hub、返回了什么状态码（排查 Cherry Studio 连接问题即因此只能
// 靠 curl 复现）。只在 /v1/* 与 /mcp 记录，静态资源与 /healthz 不记，
// 避免探活与页面加载刷屏。
//
// 只记录传输层元数据：绝不记录 Authorization/Cookie 等请求头与请求体，
// 与"工具 arguments/result 绝不落库"同一安全立场。
func accessLog(logger *slog.Logger, clientIP func(*http.Request) string, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !accessLoggablePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		// /mcp 单独记一行开始：MCP 存在长连接类请求（如 modern 的
		// subscriptions/listen），若服务端永远不结束响应，只有完成日志
		// 就永远看不到这条请求——排查"客户端一直连接中"时正需要它。
		if r.URL.Path == "/mcp" {
			logger.Info("http 请求开始",
				"method", r.Method,
				"path", r.URL.Path,
				"mcp_method", r.Header.Get("mcp-method"),
				"remote", clientIP(r),
				"user_agent", r.UserAgent(),
			)
		}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		// WebSocket 升级后 next 直到连接关闭才返回，duration 即连接存活时长。
		// mcp-method 是现代 MCP 客户端的请求方法头（initialize/tools/call/
		// subscriptions/listen…），排查"客户端连不上"时能直接看出卡在哪个方法。
		logger.Info("http 请求",
			"method", r.Method,
			"path", r.URL.Path,
			"mcp_method", r.Header.Get("mcp-method"),
			"status", rec.statusCode(),
			"duration", time.Since(start),
			"remote", clientIP(r),
			"user_agent", r.UserAgent(),
		)
	})
}

// accessLoggablePath 只覆盖 API 面：Web 静态资源与健康探针不记。
func accessLoggablePath(path string) bool {
	return path == "/mcp" || strings.HasPrefix(path, "/v1/")
}

// statusRecorder 捕获响应状态供访问日志使用，同时透传 Flusher/Hijacker。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// Flush 透传流式响应能力（SSE 等场景）。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 透传连接抢占能力，否则包装后 /v1/nodes/connect 的 WebSocket
// 升级会因 gorilla 拿不到 http.Hijacker 而失败。升级响应由 upgrader
// 直接写 hijacked 连接，这里把状态记为 101。
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	r.status = http.StatusSwitchingProtocols
	return hijacker.Hijack()
}
