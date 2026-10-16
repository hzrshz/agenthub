package config

import "testing"

// PublicURL 校验：https 任意放行；http 仅限 localhost/回环/局域网 IP
// （测试场景）；公网 http 域名拒绝；带路径/query 拒绝；空值拒绝。
func TestValidatePublicURL(t *testing.T) {
	allowed := []string{
		"https://hub.example.com",
		"https://hub.example.com:8443",
		"http://localhost",
		"http://localhost:8787",
		"http://127.0.0.1:8787",
		"http://192.168.1.10:8787", // 局域网 IP 测试
		"http://10.0.0.5",          // 内网
		"http://[::1]:8787",        // IPv6 回环
	}
	for _, value := range allowed {
		if err := ValidatePublicURL(value); err != nil {
			t.Fatalf("%q should be allowed, got %v", value, err)
		}
	}
	rejected := []string{
		"",                            // 必填
		"http://hub.example.com",      // 公网域名明文
		"https://hub.example.com/mcp", // 不允许路径
		"https://hub.example.com/?x=1",
		"ftp://hub.example.com",
		"hub.example.com", // 无 scheme
	}
	for _, value := range rejected {
		if err := ValidatePublicURL(value); err == nil {
			t.Fatalf("%q should be rejected", value)
		}
	}
}
