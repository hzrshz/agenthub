// Package core 的 schema 部分：控制库全部表结构与版本化迁移。
// 与 nexusdock 同构：PRAGMA user_version 是唯一 Schema 版本来源，
// 空库直接建当前结构，已存在的库逐版本升级，两边收敛由测试保证。
package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CurrentSchemaVersion 是当前程序对应的控制库 Schema 版本。
const CurrentSchemaVersion = 1

type schemaMigration struct {
	version    int
	name       string
	statements []string
}

// currentSchema 是全新空库初始化用的当前结构。
// 与 nexusdock 的关键差异（多租户）：
//   - nodes/pairing_codes 等节点侧资源全部带 owner_user_id，归属即路由依据；
//   - published_tool_contracts 按用户分区，tool_name 只在用户内唯一；
//   - 节点/工具契约相关表不复用 nexusdock 的全局主键。
var currentSchema = []string{
	`CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user')),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    must_change_password INTEGER NOT NULL DEFAULT 0 CHECK (must_change_password IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS user_credentials (
    user_id TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
)`,
	`CREATE TABLE IF NOT EXISTS user_sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    absolute_expires_at TEXT NOT NULL,
    revoked_at TEXT,
    user_agent_summary TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
)`,
	`CREATE INDEX IF NOT EXISTS idx_user_sessions_user_active
    ON user_sessions(user_id, revoked_at, absolute_expires_at)`,
	// 每用户一个 MCP Token（/mcp 的 Bearer 身份）。存哈希；重置即旧行删除。
	`CREATE TABLE IF NOT EXISTS user_mcp_tokens (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL UNIQUE,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
)`,
	`CREATE TABLE IF NOT EXISTS login_throttles (
    key_type TEXT NOT NULL CHECK (key_type IN ('account', 'ip')),
    key_value TEXT NOT NULL,
    failures INTEGER NOT NULL DEFAULT 0 CHECK (failures >= 0),
    blocked_until TEXT,
    last_failed_at TEXT NOT NULL,
    PRIMARY KEY (key_type, key_value)
)`,
	// 节点归属：owner_user_id 是多租户路由的根。device_token_hash 吊销即删除行。
	`CREATE TABLE IF NOT EXISTS nodes (
    id TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL,
    device_id TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    device_token_hash TEXT NOT NULL UNIQUE,
    version TEXT NOT NULL DEFAULT '',
    protocol_version TEXT NOT NULL DEFAULT '',
    os TEXT NOT NULL DEFAULT '',
    arch TEXT NOT NULL DEFAULT '',
    capabilities_json TEXT NOT NULL DEFAULT '[]',
    tool_contract_hash TEXT NOT NULL DEFAULT '',
    last_seen_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (owner_user_id) REFERENCES users(id) ON DELETE CASCADE
)`,
	`CREATE INDEX IF NOT EXISTS idx_nodes_owner ON nodes(owner_user_id)`,
	// 配对码绑定用户：兑换成功后节点归发起配对的用户所有。
	`CREATE TABLE IF NOT EXISTS pairing_codes (
    id TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL,
    code_hash TEXT NOT NULL UNIQUE,
    expires_at TEXT NOT NULL,
    used_at TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY (owner_user_id) REFERENCES users(id) ON DELETE CASCADE
)`,
	`CREATE INDEX IF NOT EXISTS idx_pairing_codes_active
    ON pairing_codes(code_hash, used_at, expires_at)`,
	// 节点级 hello 快照。
	`CREATE TABLE IF NOT EXISTS tool_contracts (
    node_id TEXT PRIMARY KEY,
    descriptors_json TEXT NOT NULL DEFAULT '[]',
    updated_at TEXT NOT NULL,
    FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE
)`,
	`CREATE TABLE IF NOT EXISTS bridge_capabilities (
    node_id TEXT PRIMARY KEY,
    capabilities_json TEXT NOT NULL DEFAULT '[]',
    updated_at TEXT NOT NULL,
    FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE
)`,
	// Fleet 公开契约：tool_name 在 owner_user_id 内唯一（复合主键），
	// 冲突合并、variants 历史均照搬 nexusdock 语义，只是作用域从全局换成用户。
	`CREATE TABLE IF NOT EXISTS published_tool_contracts (
    owner_user_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    descriptor_json TEXT NOT NULL,
    source_node_id TEXT NOT NULL DEFAULT '',
    source_version TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (owner_user_id, tool_name),
    FOREIGN KEY (owner_user_id) REFERENCES users(id) ON DELETE CASCADE
)`,
	`CREATE TABLE IF NOT EXISTS published_tool_variants (
    owner_user_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    semantic_hash TEXT NOT NULL,
    PRIMARY KEY (owner_user_id, tool_name, semantic_hash),
    FOREIGN KEY (owner_user_id, tool_name)
        REFERENCES published_tool_contracts(owner_user_id, tool_name) ON DELETE CASCADE
)`,
	// 审计：append-only，trigger 防改删。actor 用 type+id，与 nexusdock 一致。
	`CREATE TABLE IF NOT EXISTS audit_events (
    id TEXT PRIMARY KEY,
    occurred_at TEXT NOT NULL,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('user', 'device', 'admin', 'system')),
    actor_id TEXT NOT NULL,
    action TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id TEXT NOT NULL,
    result TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}'
)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_events_time ON audit_events(occurred_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_events_object ON audit_events(object_type, object_id, occurred_at DESC)`,
	`CREATE TRIGGER IF NOT EXISTS audit_events_no_update
BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events are append-only');
END`,
	`CREATE TRIGGER IF NOT EXISTS audit_events_no_delete
BEFORE DELETE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events are append-only');
END`,
	// 调用记录：只存元数据，绝不存 arguments/result（hub 是中继不是落地）。
	`CREATE TABLE IF NOT EXISTS invoke_logs (
    id TEXT PRIMARY KEY,
    occurred_at TEXT NOT NULL,
    user_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('success', 'error', 'timeout', 'cancelled')),
    duration_ms INTEGER NOT NULL,
    error_code TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT ''
)`,
	`CREATE INDEX IF NOT EXISTS idx_invoke_logs_user_time
    ON invoke_logs(user_id, occurred_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_invoke_logs_time ON invoke_logs(occurred_at)`,
}

// v1 即初始版本；此表保持结构，未来变更追加新的 schemaMigration。
var schemaMigrations = []schemaMigration{}

// EnsureSchema 把控制库升级到 CurrentSchemaVersion，失败时启动流程必须中止。
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	return migrateSchema(ctx, db)
}

func migrateSchema(ctx context.Context, db *sql.DB) error {
	version, err := readSchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	// user_version=0 同时对应"全新空库"和"未版本化旧库"。hub 没有历史库，
	// 用 users 表是否存在区分：真实 hub 库一定有 users。
	hasCoreTables, err := tableExists(ctx, db, "users")
	if err != nil {
		return err
	}
	if version == 0 && !hasCoreTables {
		return initializeSchema(ctx, db)
	}
	if version > CurrentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, CurrentSchemaVersion)
	}
	for _, migration := range schemaMigrations {
		if migration.version <= version {
			continue
		}
		if err := applyMigration(ctx, db, migration); err != nil {
			return err
		}
	}
	return nil
}

func initializeSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range currentSchema {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize schema: %w", err)
		}
	}
	if err := writeSchemaVersion(ctx, tx, CurrentSchemaVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema initialization: %w", err)
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, migration schemaMigration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration v%d (%s): %w", migration.version, migration.name, err)
	}
	defer func() { _ = tx.Rollback() }()
	for i, statement := range migration.statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migration v%d (%s) statement %d: %w", migration.version, migration.name, i+1, err)
		}
	}
	if err := writeSchemaVersion(ctx, tx, migration.version); err != nil {
		return fmt.Errorf("migration v%d (%s): %w", migration.version, migration.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration v%d (%s): %w", migration.version, migration.name, err)
	}
	return nil
}

// writeSchemaVersion 与建表语句同事务执行；PRAGMA 不支持参数绑定，
// 版本号是程序内部的 int 常量，直接拼接是安全的。
func writeSchemaVersion(ctx context.Context, tx *sql.Tx, version int) error {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("write schema version %d: %w", version, err)
	}
	return nil
}

func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var found string
	err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check table %s: %w", name, err)
	}
	return true, nil
}
