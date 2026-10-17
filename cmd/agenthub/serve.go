package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/audit"
	"github.com/uvwt/agenthub/internal/auth"
	"github.com/uvwt/agenthub/internal/bridge"
	"github.com/uvwt/agenthub/internal/config"
	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/fleet"
	"github.com/uvwt/agenthub/internal/httpx"
	"github.com/uvwt/agenthub/internal/mcpgw"
	"github.com/uvwt/agenthub/internal/nodes"
)

// serve 启动 HTTP 服务：Web 界面 + 管理 API + 节点 Bridge + MCP 网关。
func serve(ctx context.Context, cfg config.Config) error {
	logger := slog.Default()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	authService := auth.NewService(db)
	nodeStore := nodes.NewStore(db)
	hub := bridge.NewHub(nodeStore)
	fleetStore := fleet.NewStore(db)
	fleetBridge := fleet.NewBridge(fleetStore, nodeStore, logger)
	invokeStore := audit.NewStore(db)
	gateway := mcpgw.NewGateway(authService, nodeStore, hub, fleetBridge, invokeStore, logger)
	// fleet 契约变化 → MCP 工具面注册/下架；握手与节点变更 → fleet 重算。
	fleetBridge.SetPublishHandlers(fleet.PublishHandlers{
		Publish: func(ownerUserID string, descriptor protocol.ToolDescriptor) {
			logger.Info("fleet 工具公开", "owner", ownerUserID, "tool", descriptor.Name)
		},
		Retire: func(ownerUserID, toolName string) {
			logger.Info("fleet 工具下架", "owner", ownerUserID, "tool", toolName)
		},
	})
	hub.SetHelloHandler(fleetBridge.ObserveNodeHello)
	trustedProxies, err := cfg.TrustedProxies()
	if err != nil {
		return err
	}
	handler := httpx.NewServerWithFleet(authService, nodeStore, hub, gateway, invokeStore, cfg.PublicURL, trustedProxies, fleetBridge).Handler()

	// 启动恢复：把持久化的公开契约装回内存态并重放 Publish。
	// 不做这一步，进程重启后 fleet 内存态为空——工具面要等节点全部重连发 hello
	// 才恢复，期间用户看到空工具面，且删节点等事件无处收敛。
	if err := fleetBridge.RestoreStartup(ctx, fleetOwnerUserIDs(ctx, nodeStore)); err != nil {
		return fmt.Errorf("恢复 fleet 公开契约: %w", err)
	}

	// 调用记录滚动清理：每小时一次，失败只记日志。
	cleanupStop := make(chan struct{})
	defer close(cleanupStop)
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-cleanupStop:
				return
			case <-ticker.C:
				removed, err := invokeStore.Cleanup(ctx)
				if err != nil {
					logger.Warn("调用记录清理失败", "error", err)
				} else if removed > 0 {
					logger.Info("调用记录清理完成", "removed", removed)
				}
			}
		}
	}()

	// 超时配置（Slowloris 防护）：裸 http.Server 没有任何超时，
	// 攻击者可以用慢速请求占住连接不放，直到耗尽文件描述符或 goroutine。
	//
	// WebSocket 必须豁免 WriteTimeout——节点连接是长生命周期的，
	// 套上写超时会在 120 秒后静默掐断所有节点连接。
	// Go 的正确做法是只设 ReadHeaderTimeout（限制建连与请求头阶段），
	// 由连接自己的读/写 deadline 管长连接（bridge 里已对 socket 设了
	// 读超时与心跳超时）。
	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
		// 限制读取请求头的时长：Slowloris 的核心手法就是慢速发请求头。
		ReadHeaderTimeout: 15 * time.Second,
		// 不设 ReadTimeout：它同样会打断长生命周期的 WebSocket 读。
		// 节点连接的读超时由 bridge 自己的 socket deadline 控制。
		// 不设 WriteTimeout：同理，会掐断 WebSocket 与长时间的 MCP 调用。
		// 慢响应体由请求上下文的超时与调用链自身控制。
		IdleTimeout: 120 * time.Second, // keep-alive 空闲连接回收
	}
	logger.Info("agenthub listening", "version", version, "listen", cfg.Listen, "public_url", cfg.PublicURL)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		return srv.Shutdown(context.Background())
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func openDB(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	db, err := core.OpenSQLite(ctx, cfg.ControlDBPath(), 2)
	if err != nil {
		return nil, err
	}
	if err := core.EnsureSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// fleetOwnerUserIDs 列出所有拥有节点的用户，作为启动恢复 fleet 的作用域。
// 从 nodes 表取而不是 users 表：没有节点的用户本来就没有公开契约可恢复。
func fleetOwnerUserIDs(ctx context.Context, store *nodes.Store) []string {
	seen := make(map[string]struct{})
	var owners []string
	for {
		page, err := store.ListAllOwners(ctx, 200, len(owners))
		if err != nil || len(page) == 0 {
			return owners
		}
		for _, owner := range page {
			if _, exists := seen[owner]; exists {
				continue
			}
			seen[owner] = struct{}{}
			owners = append(owners, owner)
		}
		if len(page) < 200 {
			return owners
		}
	}
}
