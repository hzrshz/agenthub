// Package e2e 提供跨包的全链路验收测试：从管理员建号到 MCP 调用真实节点。
// 安全自测集中在 security_test.go；这里验证正向旅程。
package e2e

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/uvwt/agenthub/internal/auth"
	"github.com/uvwt/agenthub/internal/core"
)

// JourneyAdminCreatesUser 验证管理员自举→建号→用户凭证可用的最小旅程。
// （完整 MCP 调用旅程在 internal/mcpgw/gateway_test.go 的 SDK 级测试覆盖；
// 此测试锁定跨包装配的正确性：同一个库上多个 Service 协同工作。）
func TestJourneyAdminCreatesUser(t *testing.T) {
	ctx := context.Background()
	db, err := core.OpenSQLite(ctx, filepath.Join(t.TempDir(), "hub.db"), 2)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := core.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	svc := auth.NewService(db)

	initialized, err := svc.AdminStatus(ctx)
	if err != nil || initialized {
		t.Fatalf("fresh hub must be uninitialized: %v", err)
	}
	if _, err := svc.InitializeAdmin(ctx, "root@example.com", "root-strong-passphrase"); err != nil {
		t.Fatalf("init admin: %v", err)
	}
	alice, err := svc.CreateUser(ctx, "root", "alice@example.com", "alice-passphrase-1")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if !alice.MustChangePassword {
		t.Fatal("created user must change password on first login")
	}
	if _, err := svc.Login(ctx, "alice@example.com", "alice-passphrase-1", "203.0.113.5"); err != nil {
		t.Fatalf("alice login: %v", err)
	}
	token, err := svc.EnsureMCPToken(ctx, alice.ID)
	if err != nil || token.Token == "" {
		t.Fatalf("ensure mcp token: %v", err)
	}
	identified, err := svc.AuthenticateMCPToken(ctx, token.Token)
	if err != nil || identified.ID != alice.ID {
		t.Fatalf("mcp token must identify alice: %v %v", identified, err)
	}
}
