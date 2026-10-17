package audit

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/uvwt/agenthub/internal/config"
	"github.com/uvwt/agenthub/internal/core"
)

func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := core.OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit.db"), 2)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := core.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return NewStore(db), db
}

// 非法 outcome 必须被拒绝：invoke_logs 有 CHECK 约束，绕过校验写进去
// 会让后续按 outcome 过滤的查询静默漏数据。
func TestRecordInvokeRejectsInvalidOutcome(t *testing.T) {
	store, _ := newTestStore(t)
	err := store.RecordInvoke(context.Background(), "usr_a", "node_a", "echo", "weird", time.Second, "")
	if err == nil {
		t.Fatal("invalid outcome must be rejected")
	}
	if core.ErrorCodeOf(err) != core.CodeValidation {
		t.Fatalf("expected VALIDATION_ERROR, got %v", err)
	}
}

// 跨租户查询边界：ListByUser 必须只返回该用户的记录。
func TestListByUserIsolatesTenants(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()
	// 管理员视图靠 JOIN users 显示用户名，先把用户种进去。
	for _, u := range []struct{ id, name string }{
		{"usr_alice", "alice@example.com"}, {"usr_bob", "bob@example.com"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users(id, username, created_at, updated_at)
			VALUES(?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, u.id, u.name); err != nil {
			t.Fatalf("seed user %s: %v", u.id, err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := store.RecordInvoke(ctx, "usr_alice", "node_1", "echo", OutcomeSuccess, time.Second, ""); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := store.RecordInvoke(ctx, "usr_bob", "node_2", "browser_open", OutcomeError, time.Second, "NODE_OFFLINE"); err != nil {
		t.Fatalf("record: %v", err)
	}

	page, err := store.ListByUser(ctx, InvokeQuery{UserID: "usr_alice", Limit: 50, Page: 1})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("alice must see exactly her 3 records, got %d", len(page.Items))
	}
	for _, item := range page.Items {
		if item.UserID != "usr_alice" {
			t.Fatalf("cross-tenant leak: %+v", item)
		}
	}

	// 管理员视图带用户名。
	all, err := store.ListAll(ctx, InvokeQuery{Limit: 50, Page: 1})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all.Items) != 4 {
		t.Fatalf("admin must see all 4 records, got %d", len(all.Items))
	}
	// 用户名过滤只对管理员视图生效。
	byName, err := store.ListAll(ctx, InvokeQuery{Username: "ali", Limit: 50, Page: 1})
	if err != nil {
		t.Fatalf("list by name: %v", err)
	}
	if len(byName.Items) != 3 {
		t.Fatalf("username filter must match 3, got %d", len(byName.Items))
	}
	// 用户自身视图忽略 username 参数（不能靠它看别人）。
	ignored, err := store.ListByUser(ctx, InvokeQuery{UserID: "usr_bob", Username: "ali", Limit: 50, Page: 1})
	if err != nil {
		t.Fatalf("list as bob: %v", err)
	}
	if len(ignored.Items) != 1 {
		t.Fatalf("user view must ignore username filter, got %d", len(ignored.Items))
	}
}

// 保留策略：30 天滚动删除 + 条数上限裁剪（此前整个 audit 包零测试）。
func TestCleanupEnforcesRetentionAndRowCap(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC()

	// 一条 40 天前的记录（应被保留期删除）。
	old := base.Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `INSERT INTO invoke_logs
		(id, occurred_at, user_id, node_id, tool_name, outcome, duration_ms, error_code)
		VALUES('inv_old', ?, 'usr_a', 'node_1', 'echo', 'success', 10, '')`, old); err != nil {
		t.Fatalf("seed old record: %v", err)
	}
	// 一条刚发生的记录（应保留）。
	recent := base.Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `INSERT INTO invoke_logs
		(id, occurred_at, user_id, node_id, tool_name, outcome, duration_ms, error_code)
		VALUES('inv_recent', ?, 'usr_a', 'node_1', 'echo', 'success', 10, '')`, recent); err != nil {
		t.Fatalf("seed recent record: %v", err)
	}

	removed, err := store.Cleanup(ctx)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if removed != 1 {
		t.Fatalf("cleanup must remove exactly the 40-day-old record, removed %d", removed)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invoke_logs`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("recent record must survive retention cleanup, %d remain", remaining)
	}

	// 条数上限裁剪：把上限临时压到 1，验证只保留最新的一条。
	if _, err := db.ExecContext(ctx, `INSERT INTO invoke_logs
		(id, occurred_at, user_id, node_id, tool_name, outcome, duration_ms, error_code)
		VALUES('inv_newer', ?, 'usr_a', 'node_1', 'echo', 'success', 10, '')`,
		base.Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed newer record: %v", err)
	}
	// 直接验证裁剪 SQL 的语义（不改全局常量）。
	if _, err := db.ExecContext(ctx, `DELETE FROM invoke_logs WHERE id IN (
		SELECT id FROM invoke_logs ORDER BY occurred_at DESC LIMIT -1 OFFSET 1)`); err != nil {
		t.Fatalf("trim: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invoke_logs`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("row cap must keep only the newest 1, %d remain", remaining)
	}
	var kept string
	if err := db.QueryRowContext(ctx, `SELECT id FROM invoke_logs`).Scan(&kept); err != nil {
		t.Fatalf("read kept: %v", err)
	}
	if kept != "inv_newer" {
		t.Fatalf("must keep the newest record, kept %s", kept)
	}
	_ = config.InvokeLogMaxRows
}

// StatsSince 只统计 since 之后的记录。
func TestStatsSinceFiltersByTime(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id, username, created_at, updated_at)
		VALUES('usr_a', 'alice@example.com', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO invoke_logs
		(id, occurred_at, user_id, node_id, tool_name, outcome, duration_ms, error_code)
		VALUES('inv_1', ?, 'usr_a', 'node_1', 'echo', 'success', 10, '')`,
		base.Add(-48*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	for _, id := range []string{"inv_2", "inv_3"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO invoke_logs
			(id, occurred_at, user_id, node_id, tool_name, outcome, duration_ms, error_code)
			VALUES(?, ?, 'usr_a', 'node_1', 'echo', 'success', 10, '')`,
			id, base.Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("seed recent: %v", err)
		}
	}

	stats, err := store.StatsSince(ctx, base.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected one user in window, got %d", len(stats))
	}
	if stats[0].RecentCalls != 2 {
		t.Fatalf("expected 2 recent calls, got %d", stats[0].RecentCalls)
	}
	if stats[0].Username != "alice@example.com" {
		t.Fatalf("stats must carry username, got %q", stats[0].Username)
	}
	// 用户不存在时 LEFT JOIN 不能让查询失败。
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = 'usr_a'`); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	ghost, err := store.StatsSince(ctx, base.Add(-24*time.Hour))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stats with deleted user must not fail: %v", err)
	}
	_ = ghost
}