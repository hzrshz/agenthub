package httpx

import (
	"bufio"
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 访问日志只覆盖 API 面（/v1/* 与 /mcp），静态资源与健康探针不记，
// 并携带来源 IP 与最终状态码。
func TestAccessLogCoversAPIOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	handler := accessLog(logger, func(*http.Request) string { return "192.0.2.1" }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/me" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		path         string
		wantContains string
		wantLog      bool
	}{
		{"/v1/auth/me", "status=401", true},
		{"/mcp", "path=/mcp", true},
		{"/assets/index.js", "", false},
		{"/healthz", "", false},
		{"/", "", false},
	}
	for _, tc := range cases {
		buf.Reset()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tc.path, nil))
		got := buf.String()
		if tc.wantLog {
			if !strings.Contains(got, tc.wantContains) {
				t.Fatalf("%s must be logged with %q, got %q", tc.path, tc.wantContains, got)
			}
			if !strings.Contains(got, "remote=192.0.2.1") {
				t.Fatalf("%s log must carry client IP, got %q", tc.path, got)
			}
			continue
		}
		if got != "" {
			t.Fatalf("%s must not be access-logged, got %q", tc.path, got)
		}
	}
}

// 包装后的 ResponseWriter 必须透传 Hijack，否则 WebSocket 升级拿不到
// http.Hijacker；升级成功的请求按 101 记录。
func TestAccessLogPreservesHijack(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	hijacked := false
	handler := accessLog(logger, func(*http.Request) string { return "192.0.2.2" }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("wrapped response writer must keep http.Hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
		hijacked = true
	}))

	w := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/nodes/connect", nil))
	if !hijacked {
		t.Fatal("handler did not run through hijack path")
	}
	if !strings.Contains(buf.String(), "status=101") {
		t.Fatalf("hijacked request must be logged as 101, got %q", buf.String())
	}
}

type hijackableRecorder struct {
	*httptest.ResponseRecorder
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	client, server := net.Pipe()
	go func() { _ = server.Close() }()
	return client, bufio.NewReadWriter(bufio.NewReader(client), bufio.NewWriter(client)), nil
}
