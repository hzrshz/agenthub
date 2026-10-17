package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 全新库必须建到当前版本，且重复 Ensure 幂等。
func TestEnsureSchemaFreshDB(t *testing.T) {
	db, closeDB := openTestDB(t)
	defer closeDB()
	ctx := context.Background()

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, CurrentSchemaVersion)
	}
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("second ensure should be idempotent: %v", err)
	}
}

// 审计表 append-only：UPDATE 与 DELETE 都必须被 trigger 拒绝。
func TestAuditEventsAppendOnly(t *testing.T) {
	db, closeDB := openTestDB(t)
	defer closeDB()
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `INSERT INTO audit_events(
		id, occurred_at, actor_type, actor_id, action, object_type, object_id, result
	) VALUES ('e1', '2026-01-01T00:00:00Z', 'user', 'u1', 'login', 'session', 's1', 'success')`)
	if err != nil {
		t.Fatalf("insert audit event: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE audit_events SET result = 'failure' WHERE id = 'e1'`); err == nil {
		t.Fatal("audit_events UPDATE must be aborted")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM audit_events WHERE id = 'e1'`); err == nil {
		t.Fatal("audit_events DELETE must be aborted")
	}
}

// 外键级联：删除用户必须连带清掉其节点、配对码、MCP token、会话与凭证——
// 禁用/删除用户的"立即断干净"依赖这一层兜底。
func TestUserCascadeDelete(t *testing.T) {
	db, closeDB := openTestDB(t)
	defer closeDB()
	ctx := context.Background()

	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("exec %s: %v", query, err)
		}
	}
	mustExec(`INSERT INTO users(id, username, created_at, updated_at)
		VALUES ('u1', 'user@example.com', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO nodes(id, owner_user_id, device_id, name, device_token_hash, created_at, updated_at)
		VALUES ('n1', 'u1', 'device-aaaaaaaa', 'dev', 'hash1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO tool_contracts(node_id, descriptors_json, updated_at)
		VALUES ('n1', '[]', '2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO pairing_codes(id, owner_user_id, code_hash, expires_at, created_at)
		VALUES ('p1', 'u1', 'codehash', '2026-01-01T01:00:00Z', '2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO user_mcp_tokens(id, user_id, token_hash, created_at, updated_at)
		VALUES ('m1', 'u1', 'mcphash', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO user_sessions(id, user_id, token_hash, created_at, absolute_expires_at)
		VALUES ('s1', 'u1', 'sesshash', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')`)
	mustExec(`INSERT INTO published_tool_contracts(owner_user_id, tool_name, descriptor_json, updated_at)
		VALUES ('u1', 'browser_open', '{}', '2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO published_tool_variants(owner_user_id, tool_name, semantic_hash)
		VALUES ('u1', 'browser_open', 'abc123')`)

	mustExec(`DELETE FROM users WHERE id = 'u1'`)

	for _, table := range []string{"nodes", "tool_contracts", "pairing_codes", "user_mcp_tokens", "user_sessions", "published_tool_contracts", "published_tool_variants"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("table %s has %d rows after user cascade delete, want 0", table, count)
		}
	}
}

// 用户内 tool_name 唯一、跨用户可重名：多租户 fleet 分区的核心约束。
func TestPublishedContractUniquePerUser(t *testing.T) {
	db, closeDB := openTestDB(t)
	defer closeDB()
	ctx := context.Background()

	insertUser := func(id string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO users(id, username, created_at, updated_at)
			VALUES (?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, id, id+"@example.com"); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	insertContract := func(owner, name string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO published_tool_contracts(owner_user_id, tool_name, descriptor_json, updated_at)
			VALUES (?, ?, '{}', '2026-01-01T00:00:00Z')`, owner, name)
		return err
	}
	insertUser("u1")
	insertUser("u2")

	if err := insertContract("u1", "browser_open"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insertContract("u1", "browser_open"); err == nil {
		t.Fatal("duplicate tool_name within one user must conflict")
	}
	if err := insertContract("u2", "browser_open"); err != nil {
		t.Fatalf("same tool_name under another user must be allowed: %v", err)
	}
}

// 未来版本号高于当前二进制时必须拒绝启动。
func TestEnsureSchemaRejectsNewerVersion(t *testing.T) {
	db, closeDB := openTestDB(t)
	defer closeDB()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 999`); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := EnsureSchema(ctx, db); err == nil {
		t.Fatal("newer schema version must be rejected")
	}
}

// 控制库文件与数据目录权限必须收紧到 0600 / 0700。
//
// 基线检查项：控制库存着全部口令哈希、session/token 哈希与审计记录。
// 默认 umask(022) 下新建文件是 0644、预建目录是 0755，同机其他用户可读。
// 目录这条尤其容易被漏——MkdirAll 只对**新建**目录应用 mode，运维预先建好的
// 数据目录不会被收紧。
func TestControlDBFilePermissionsAreRestrictive(t *testing.T) {
	ctx := context.Background()
	// 模拟运维预先创建的 0755 数据目录（常见的部署姿势）。
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatalf("chmod to simulate pre-created dir: %v", err)
	}

	dbPath := filepath.Join(dataDir, "control.db")
	db, err := OpenSQLite(ctx, dbPath, 2)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	fileInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("control.db permissions = %04o, want 0600", got)
	}
	dirInfo, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("data directory permissions = %04o, want 0700", got)
	}
}

// 已存在的宽松权限文件也会被收紧（处理历史遗留）。
func TestControlDBPermissionsTightenExistingFile(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "control.db")
	db, err := OpenSQLite(ctx, dbPath, 2)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	// 人为放宽，模拟历史遗留。
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_ = db.Close()

	reopened, err := OpenSQLite(ctx, dbPath, 2)
	if err != nil {
		t.Fatalf("reopen sqlite: %v", err)
	}
	defer reopened.Close()
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("permissions after reopen = %04o, want 0600 (legacy loose modes must be tightened)", got)
	}
}
