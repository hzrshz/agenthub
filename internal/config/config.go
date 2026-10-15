// Package config 承载 AgentHub 的启动配置与安全配额常量。
// 配额全部是编译期常量，不做管理界面可调——这些是防滥用的底线值，
// 调整应当伴随代码评审与发版，而不是运行时随手改。
package config

import (
	"errors"
	"fmt"
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
	// PairingCodeTTL 配对码有效期；单次使用。
	PairingCodeTTL = 10 * time.Minute
	// PairAttemptsPerMinute /v1/nodes/pair 是公网无认证入口，按 IP 限流防配对码爆破。
	PairAttemptsPerMinute = 10
	// LoginAttemptsPerMinute 登录失败限流基数（账号与 IP 两个维度各算一份）。
	LoginAttemptsPerMinute = 10
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
	if !strings.HasPrefix(c.PublicURL, "https://") {
		// 节点侧 AgentDock 会基于 PublicURL 推导 wss 回调地址，
		// 非 https 公网地址会造成明文出站或无法回连，启动期直接拒绝。
		return errors.New("HUB_PUBLIC_URL must be an https:// origin (TLS is terminated by the reverse proxy)")
	}
	return nil
}

func envDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
