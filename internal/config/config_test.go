package config

import "testing"

// PublicURL 校验：https 任意放行；反代子路径部署允许带一段 base path
// （单段、无 ..、无 //）；http 仅限 localhost/回环/局域网 IP（测试场景）；
// query/fragment/URL 凭据拒绝；空值拒绝。
func TestValidatePublicURL(t *testing.T) {
	allowed := []string{
		"https://hub.example.com",
		"https://hub.example.com:8443",
		"https://hub.example.com/app",  // 反代子路径部署：/mcp 与回调都在该前缀下
		"https://hub.example.com/app/", // 尾部斜杠容忍（env 加载时会被 trim）
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
		"",                               // 必填
		"http://hub.example.com",         // 公网域名明文
		"https://hub.example.com/a/b",    // 多段子路径：回调地址拼接会产生歧义
		"https://hub.example.com/../app", // .. 路径穿越观感，拒绝
		"https://hub.example.com//app",   // 空段
		"https://hub.example.com/app?x=1",
		"https://hub.example.com/app#frag",
		"https://user:pw@hub.example.com/app", // URL 内嵌凭据
		"ftp://hub.example.com",
		"hub.example.com", // 无 scheme
	}
	for _, value := range rejected {
		if err := ValidatePublicURL(value); err == nil {
			t.Fatalf("%q should be rejected", value)
		}
	}
}
