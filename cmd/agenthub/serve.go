package main

import (
	"context"
	"database/sql"
	"errors"
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
	handler := httpx.NewServer(authService, nodeStore, hub, gateway, invokeStore, cfg.PublicURL).Handler()

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

	srv := &http.Server{Addr: cfg.Listen, Handler: handler}
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
