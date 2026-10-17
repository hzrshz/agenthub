// Package config 承载 AgentHub 的启动配置与安全配额常量。
// 配额全部是编译期常量，不做管理界面可调——这些是防滥用的底线值，
// 调整应当伴随代码评审与发版，而不是运行时随手改。
package config

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 配额与安全常量。设计规格 docs/DESIGN.md §6 固定的默认值。
const (
	// MaxNodesPerUser 每用户可配对的 AgentDock 节点数上限。
	MaxNodesPerUser = 10
	// MCPRequestsPerMinute 每用户 /mcp 调用速率限制，超限 429。
	MCPRequestsPerMinute = 60
	// MaxInflightPerNode 每节点并发在途 tool.invoke 上限，超限立即 429，
	// 防止单个慢节点拖垮该用户的整条 WS 连接。
	MaxInflightPerNode = 16
	// MaxInflightPerUser 每用户跨全部节点的并发在途上限（DESIGN §6
	// "每用户/每节点在途请求 16"是两个独立维度）。单节点上限挡不住
	// 用户把压力分散到自己的多个节点上。
	MaxInflightPerUser = 16
	// PairingCodeTTL 配对码有效期；单次使用。
	PairingCodeTTL = 10 * time.Minute
	// PairAttemptsPerMinute /v1/nodes/pair 是公网无认证入口，按 IP 限流防配对码爆破。
	PairAttemptsPerMinute = 10
	// LoginThrottleMaxFailures 登录失败多少次后进入最长封锁（15 分钟）。
	// 阶梯在 auth.loginDelay 内按此上界分段。
	LoginThrottleMaxFailures = 10
	// InvokeTimeout 单次 tool.invoke 从转发到节点到放弃的等待上限。
	InvokeTimeout = 120 * time.Second
	// MaxMessageBytes 单条 WS/MCP 消息体上限；超限报错而非缓冲，
	// 防止工具返回超大结果打爆 hub 内存与反代 buffer。
	MaxMessageBytes = 10 << 20
	// HeartbeatInterval 节点心跳间隔；连续两个周期无心跳即判定断线。
	HeartbeatInterval = 30 * time.Second
	// WebSessionAbsoluteTTL Web 会话绝对过期时长，不做滑动续期。
	WebSessionAbsoluteTTL = 24 * time.Hour
	// InvokeLogRetention invoke_logs 保留天数，滚动清理。
	InvokeLogRetention = 30 * 24 * time.Hour
	// InvokeLogMaxRows invoke_logs 条数硬上限，清理时一并裁剪。
	InvokeLogMaxRows = 100000
	// MinProtocolVersion 节点 hello 允许的最低协议版本；低于此值拒连并给升级指引。
	MinProtocolVersion = "2"
	// MCPAuthFailureLimitPerMinute 单 IP 每分钟允许的 /mcp 认证失败次数，
	// 超过即 429。DESIGN §3.2 要求"认证失败按 IP 限流"——无效 token 的 401
	// 是可无成本爆破的入口，必须在认证失败侧也有速率上限。
	MCPAuthFailureLimitPerMinute = 30
	// MCPAuthFailureBlock 返回 429 前对该 IP 的封锁时长。
	MCPAuthFailureBlock = 5 * time.Minute
)

// Config 是进程启动所需的全部外部配置。除 PublicURL 外都有可用默认值。
type Config struct {
	// DataDir SQLite 与秘密文件的根目录。
	DataDir string
	// Listen HTTP 监听地址（hub 本体只听 HTTP，TLS 由反代终结）。
	Listen string
	// PublicURL hub 的公网 HTTPS origin（无路径）。节点据此推导回调地址，
	// 是唯一必填配置。
	PublicURL string
	// TrustedProxyCIDRs 允许提供 X-Forwarded-For 的反代来源网段。
	// 安全边界：clientIP 只在这些网段的来源才采信 XFF，否则一律用 RemoteAddr。
	// 空列表 = 不采信任何 XFF（默认，配合"hub 只监听回环"的部署形态最安全）。
	TrustedProxyCIDRs []string
}

// Resolver 是把可信反代网段预解析成可快速比对的集合。
type Resolver struct {
	nets []*net.IPNet
}

// Load 从环境变量读取配置并校验。
func Load() (Config, error) {
	cfg := Config{
		DataDir:   envDefault("HUB_DATA_DIR", ""),
		Listen:    envDefault("HUB_LISTEN", "127.0.0.1:8787"),
		PublicURL: strings.TrimRight(strings.TrimSpace(os.Getenv("HUB_PUBLIC_URL")), "/"),
	}
	if cfg.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return cfg, fmt.Errorf("resolve home dir: %w", err)
		}
		cfg.DataDir = filepath.Join(home, ".agenthub")
	}
	for _, entry := range strings.Split(envDefault("HUB_TRUSTED_PROXY_CIDRS", ""), ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			cfg.TrustedProxyCIDRs = append(cfg.TrustedProxyCIDRs, trimmed)
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate 校验配置组合的完整性。
func (c Config) Validate() error {
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("data dir is empty")
	}
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen address is empty")
	}
	if _, err := c.TrustedProxies(); err != nil {
		return err
	}
	return ValidatePublicURL(c.PublicURL)
}

// TrustedProxies 解析可信反代网段；非法网段在启动时直接失败，
// 避免部署后才发现 XFF 根本没被采信（或反被伪造）。
func (c Config) TrustedProxies() (*Resolver, error) {
	resolver := &Resolver{}
	for _, entry := range c.TrustedProxyCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf("invalid HUB_TRUSTED_PROXY_CIDRS entry %q: %w", entry, err)
		}
		resolver.nets = append(resolver.nets, network)
	}
	return resolver, nil
}

// Trusts 报告该远端地址是否属于可信反代。
func (r *Resolver) Trusts(remoteAddr string) bool {
	if r == nil || len(r.nets) == 0 {
		return false
	}
	host := remoteAddr
	if parsed, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = parsed
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return false
	}
	for _, network := range r.nets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP 返回限流与审计使用的客户端 IP。
//
// 安全边界：X-Forwarded-For 由客户端可任意伪造，若无条件采信，攻击者每次换一个
// 伪造 IP 即可完全绕过所有按 IP 的限流（登录爆破、配对码爆破、MCP token 爆破）。
// 因此只有当直接对端（RemoteAddr）落在配置的可信反代网段时才采信 XFF，否则一律
// 用 RemoteAddr。默认无可信网段 = 完全不采信 XFF。
//
// 放在 config 包是因为 httpx 与 mcpgw 两处都要用它做限流键，必须是同一套语义。
func ClientIP(r *http.Request, trusted *Resolver) string {
	remote := RemoteIP(r)
	if trusted != nil && trusted.Trusts(remote) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// 取最左侧一跳：可信反代按"客户端, 代理1, 代理2"顺序追加，
			// 最左侧才是最初的客户端。
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	return remote
}

// RemoteIP 从 RemoteAddr 取出 IP（不采信任何请求头）。
func RemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// ValidatePublicURL 校验 HUB_PUBLIC_URL 形态。
// 生产环境必须是 https origin（TLS 由反代终结）；本机/局域网测试允许
// http://localhost 或 http://<局域网IP>——与 AgentDock 节点侧
// normalizeEndpoint 的规则对齐（节点侧本就允许 http+IP/loopback 配对），
// 否则"先用 IP 测试、再迁 HTTPS"的路径走不通。
func ValidatePublicURL(publicURL string) error {
	if publicURL == "" {
		return errors.New("HUB_PUBLIC_URL is required (e.g. https://hub.example.com or http://192.168.1.10:8787 for LAN testing)")
	}
	parsed, err := url.Parse(publicURL)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("HUB_PUBLIC_URL must be a bare origin (scheme://host[:port], no path/query)")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		host := parsed.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
			// 局域网 IP：允许，用于反代就位前的测试。
			return nil
		}
		return errors.New("HUB_PUBLIC_URL with http:// is only allowed for localhost or an IP address; public hostnames must use https://")
	default:
		return errors.New("HUB_PUBLIC_URL scheme must be https:// (or http:// with localhost/IP for testing)")
	}
}

// ControlDBPath 返回控制库文件路径（DataDir 下单文件）。
func (c Config) ControlDBPath() string {
	return filepath.Join(c.DataDir, "control.db")
}

// SecretsDir 返回秘密文件目录（预留；当前秘密全部在控制库内）。
func (c Config) SecretsDir() string {
	return filepath.Join(c.DataDir, "secrets")
}

func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
