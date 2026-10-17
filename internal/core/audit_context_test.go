package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// openBlankDB 开一个**未建表**的空库——迁移测试需要自己构造 v1 结构，
// 不能用 testhelpers_test.go 里的 openTestDB（它已经 EnsureSchema 过）。
func openBlankDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "core.db"), 2)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// 迁移 v2：已有 v1 库升级后必须拿到 source_ip 列。
//
// 这条路径是真实部署场景——现网库里 audit_events 是 v1 结构（没有该列），
// 且表上有 append-only trigger，不能靠重建表来加列。若迁移没生效，
// 所有审计 INSERT 都会因列数不匹配而失败（而审计失败是被吞掉的，
// 表面看不出问题，只是审计记录静默缺失）。
func TestMigrationV2AddsSourceIPToExistingAuditTable(t *testing.T) {
	ctx := context.Background()
	db := openBlankDB(t)

	// 构造一个 v1 库：先建 v1 结构，再把 user_version 设回 1。
	if _, err := db.ExecContext(ctx, `CREATE TABLE audit_events (
		id TEXT PRIMARY KEY,
		occurred_at TEXT NOT NULL,
		actor_type TEXT NOT NULL,
		actor_id TEXT NOT NULL,
		action TEXT NOT NULL,
		object_type TEXT NOT NULL,
		object_id TEXT NOT NULL,
		result TEXT NOT NULL,
		metadata_json TEXT NOT NULL DEFAULT '{}'
	)`); err != nil {
		t.Fatalf("create v1 audit table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO audit_events
		(id, occurred_at, actor_type, actor_id, action, object_type, object_id, result)
		VALUES ('evt_old', '2026-01-01T00:00:00Z', 'user', 'usr_a', 'auth.login', 'account', 'x', 'succeeded')`); err != nil {
		t.Fatalf("seed v1 row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set version: %v", err)
	}

	// 升级到当前版本。
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, CurrentSchemaVersion)
	}

	// 新列存在且历史行被补上默认空值（不能因为加列把旧数据弄丢）。
	var sourceIP string
	if err := db.QueryRowContext(ctx, `SELECT source_ip FROM audit_events WHERE id = 'evt_old'`).Scan(&sourceIP); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if sourceIP != "" {
		t.Errorf("migrated legacy row source_ip = %q, want empty", sourceIP)
	}

	// 迁移后能写入含 source_ip 的新行，且旧行仍在。
	if _, err := db.ExecContext(ctx, `INSERT INTO audit_events
		(id, occurred_at, actor_type, actor_id, action, object_type, object_id, result, source_ip)
		VALUES ('evt_new', '2026-01-02T00:00:00Z', 'user', 'usr_a', 'auth.login', 'account', 'y', 'succeeded', '203.0.113.9')`); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 2 {
		t.Fatalf("migration must preserve existing rows, got %d rows", total)
	}
}

// 全新库直接建出带 source_ip 的结构（不走 ALTER）。
func TestFreshSchemaHasSourceIPColumn(t *testing.T) {
	ctx := context.Background()
	db := openBlankDB(t)
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO audit_events
		(id, occurred_at, actor_type, actor_id, action, object_type, object_id, result, source_ip)
		VALUES ('evt_1', '2026-01-01T00:00:00Z', 'user', 'usr_a', 'auth.login', 'account', 'x', 'succeeded', '198.51.100.1')`); err != nil {
		t.Fatalf("fresh schema must accept source_ip: %v", err)
	}
	var got string
	if err := db.QueryRowContext(ctx, `SELECT source_ip FROM audit_events WHERE id='evt_1'`).Scan(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "198.51.100.1" {
		t.Fatalf("source_ip round-trip = %q", got)
	}
}

// 审计来源 IP 的 context 传递。
func TestAuditIPContext(t *testing.T) {
	t.Run("no IP leaves context untouched", func(t *testing.T) {
		ctx := context.Background()
		if got := AuditSourceIP(ctx); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
		// 空 IP 不应写入 context（本地 CLI 路径）。
		if WithAuditIP(ctx, "   ") != ctx {
			t.Error("blank IP must not be stored in context")
		}
	})
	t.Run("IP is trimmed and readable", func(t *testing.T) {
		ctx := WithAuditIP(context.Background(), "  203.0.113.9  ")
		if got := AuditSourceIP(ctx); got != "203.0.113.9" {
			t.Errorf("AuditSourceIP = %q, want %q", got, "203.0.113.9")
		}
	})
}
