// Command agenthub 是 AgentHub 的单二进制入口：
//   - agenthub serve            启动 HTTP 服务（MCP + 节点 Bridge + Web 界面）
//   - agenthub admin init       本地初始化管理员（仅服务未初始化时可用）
//   - agenthub admin reset      本地重置管理员口令（要求服务已停止，避免写竞争）
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/uvwt/agenthub/internal/config"
)

// version 由发布流水线通过 -ldflags "-X main.version=..." 注入；
// 本地构建显示为 dev。
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agenthub: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agenthub <serve | admin init|reset>")
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:])
	case "admin":
		return cmdAdmin(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdServe(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg)
}

func cmdAdmin(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agenthub admin <init | reset>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return adminCommand(context.Background(), cfg, args[0], args[1:])
}
