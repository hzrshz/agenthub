// Package core 的 schema 部分：控制库全部表结构与版本化迁移。
// 与 nexusdock 同构：SQLite 用 PRAGMA user_version 作为唯一 Schema 版本来源，
// 空库直接建当前结构，已存在的库逐版本升级，两边收敛由测试保证。
//
// 多后端（SQLite 默认 / MySQL 8.0 协议兼容 / PostgreSQL）：
//   - SQLite 路径保持历史行为不动（PRAGMA user_version + 原语句原样）；
//   - MySQL/PG 用 schema_migrations 表记录版本（服务器库没有 user_version）；
//   - 每个方言一份 DDL（见 currentSchema / currentSchemaMySQL），语句级可读，
//     不为"一句多态"引入生成器——差异本来就只有建表/迁移这十几条。
package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// CurrentSchemaVersion 是当前程序对应的控制库 Schema 版本。
const CurrentSchemaVersion = 3

type schemaMigration struct {
	version    int
	name       string
	statements []string // SQLite 方言（历史语义）
	mysql      []string // MySQL 方言；为空 = 与 SQLite 相同
	postgres   []string // PostgreSQL 方言；为空 = 与 SQLite 相同
}

// forDialect 返回该迁移在指定方言下的语句列表。
func (m schemaMigration) forDialect(d Dialect) []string {
	switch d {
	case DialectMySQL:
		if len(m.mysql) > 0 {
			return m.mysql
		}
	case DialectPostgres:
		if len(m.postgres) > 0 {
			return m.postgres
		}
	}
	return m.statements
}

// currentSchema 是全新空库初始化用的当前结构（SQLite 方言）。
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
	// source_ip 记录来源 IP——等保二级审计要求能追溯"谁从哪台机器操作"。
	`CREATE TABLE IF NOT EXISTS audit_events (
    id TEXT PRIMARY KEY,
    occurred_at TEXT NOT NULL,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('user', 'device', 'admin', 'system')),
    actor_id TEXT NOT NULL,
    action TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id TEXT NOT NULL,
    result TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    source_ip TEXT NOT NULL DEFAULT ''
)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_events_time ON audit_events(occurred_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_events_object ON audit_events(object_type, object_id, occurred_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_events_source_ip ON audit_events(source_ip, occurred_at DESC)`,
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
    client_ip TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT ''
)`,
	`CREATE INDEX IF NOT EXISTS idx_invoke_logs_user_time
    ON invoke_logs(user_id, occurred_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_invoke_logs_time ON invoke_logs(occurred_at)`,
}

// currentSchemaMySQL 与 currentSchema 同结构同约束，仅翻译成 MySQL 8.0 语法。
// 与 SQLite 版的差异及原因：
//   - 主键/唯一列定长 VARCHAR/CHAR：InnoDB 不允许 TEXT 参与主键，且乱码边界
//     由代码保证（id ≤ 38 字符、SHA-256 哈希恰 64 字符、RFC3339Nano ≤ 35 字符）；
//   - 表级 COLLATE utf8mb4_bin：保住与 SQLite BINARY 完全一致的大小写敏感
//     比较/唯一语义——token 哈希与 username 唯一性都依赖它，漏掉这条会静默
//     改变安全行为（等保审查时重点核对）；
//   - 索引内联进 CREATE TABLE：MySQL 8.0 没有 CREATE INDEX IF NOT EXISTS，
//     内联声明天然幂等（迁移里的索引语句由 execSchemaStatement 兜底容错）；
//   - JSON/契约列 MEDIUMTEXT：工具契约可能远超 TEXT 的 64KB 上限；
//   - 外键全部表级声明：MySQL 列级 REFERENCES 只是语法糖不建约束；
//   - MEDIUMTEXT 默认值带括号 DEFAULT ('[]')：MySQL 8.0.13+ 要求 TEXT 族
//     默认值写成表达式。
var currentSchemaMySQL = []string{
	`CREATE TABLE IF NOT EXISTS users (
    id VARCHAR(64) PRIMARY KEY,
    username VARCHAR(255) NOT NULL UNIQUE,
    display_name VARCHAR(255) NOT NULL DEFAULT '',
    role VARCHAR(16) NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user')),
    status VARCHAR(16) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    must_change_password TINYINT NOT NULL DEFAULT 0 CHECK (must_change_password IN (0, 1)),
    created_at VARCHAR(40) NOT NULL,
    updated_at VARCHAR(40) NOT NULL
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS user_credentials (
    user_id VARCHAR(64) PRIMARY KEY,
    password_hash VARCHAR(255) NOT NULL,
    updated_at VARCHAR(40) NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS user_sessions (
    id VARCHAR(64) PRIMARY KEY,
    user_id VARCHAR(64) NOT NULL,
    token_hash CHAR(64) NOT NULL UNIQUE,
    created_at VARCHAR(40) NOT NULL,
    absolute_expires_at VARCHAR(40) NOT NULL,
    revoked_at VARCHAR(40),
    user_agent_summary VARCHAR(512) NOT NULL DEFAULT '',
    INDEX idx_user_sessions_user_active (user_id, revoked_at, absolute_expires_at),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	// 每用户一个 MCP Token（/mcp 的 Bearer 身份）。存哈希；重置即旧行删除。
	`CREATE TABLE IF NOT EXISTS user_mcp_tokens (
    id VARCHAR(64) PRIMARY KEY,
    user_id VARCHAR(64) NOT NULL UNIQUE,
    token_hash CHAR(64) NOT NULL UNIQUE,
    created_at VARCHAR(40) NOT NULL,
    updated_at VARCHAR(40) NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS login_throttles (
    key_type VARCHAR(16) NOT NULL CHECK (key_type IN ('account', 'ip')),
    key_value CHAR(64) NOT NULL,
    failures INT NOT NULL DEFAULT 0 CHECK (failures >= 0),
    blocked_until VARCHAR(40),
    last_failed_at VARCHAR(40) NOT NULL,
    PRIMARY KEY (key_type, key_value)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	// 节点归属：owner_user_id 是多租户路由的根。device_token_hash 吊销即删除行。
	`CREATE TABLE IF NOT EXISTS nodes (
    id VARCHAR(64) PRIMARY KEY,
    owner_user_id VARCHAR(64) NOT NULL,
    device_id VARCHAR(128) NOT NULL UNIQUE,
    name VARCHAR(255) NOT NULL,
    enabled TINYINT NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    device_token_hash CHAR(64) NOT NULL UNIQUE,
    version VARCHAR(64) NOT NULL DEFAULT '',
    protocol_version VARCHAR(64) NOT NULL DEFAULT '',
    os VARCHAR(64) NOT NULL DEFAULT '',
    arch VARCHAR(64) NOT NULL DEFAULT '',
    capabilities_json MEDIUMTEXT NOT NULL DEFAULT ('[]'),
    tool_contract_hash VARCHAR(64) NOT NULL DEFAULT '',
    last_seen_at VARCHAR(40),
    created_at VARCHAR(40) NOT NULL,
    updated_at VARCHAR(40) NOT NULL,
    INDEX idx_nodes_owner (owner_user_id),
    FOREIGN KEY (owner_user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	// 配对码绑定用户：兑换成功后节点归发起配对的用户所有。
	`CREATE TABLE IF NOT EXISTS pairing_codes (
    id VARCHAR(64) PRIMARY KEY,
    owner_user_id VARCHAR(64) NOT NULL,
    code_hash CHAR(64) NOT NULL UNIQUE,
    expires_at VARCHAR(40) NOT NULL,
    used_at VARCHAR(40),
    created_at VARCHAR(40) NOT NULL,
    INDEX idx_pairing_codes_active (code_hash, used_at, expires_at),
    FOREIGN KEY (owner_user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	// 节点级 hello 快照。
	`CREATE TABLE IF NOT EXISTS tool_contracts (
    node_id VARCHAR(64) PRIMARY KEY,
    descriptors_json MEDIUMTEXT NOT NULL DEFAULT ('[]'),
    updated_at VARCHAR(40) NOT NULL,
    FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS bridge_capabilities (
    node_id VARCHAR(64) PRIMARY KEY,
    capabilities_json MEDIUMTEXT NOT NULL DEFAULT ('[]'),
    updated_at VARCHAR(40) NOT NULL,
    FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	// Fleet 公开契约：tool_name 在 owner_user_id 内唯一（复合主键），
	// 冲突合并、variants 历史均照搬 nexusdock 语义，只是作用域从全局换成用户。
	`CREATE TABLE IF NOT EXISTS published_tool_contracts (
    owner_user_id VARCHAR(64) NOT NULL,
    tool_name VARCHAR(255) NOT NULL,
    descriptor_json MEDIUMTEXT NOT NULL,
    source_node_id VARCHAR(64) NOT NULL DEFAULT '',
    source_version VARCHAR(64) NOT NULL DEFAULT '',
    updated_at VARCHAR(40) NOT NULL,
    PRIMARY KEY (owner_user_id, tool_name),
    FOREIGN KEY (owner_user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS published_tool_variants (
    owner_user_id VARCHAR(64) NOT NULL,
    tool_name VARCHAR(255) NOT NULL,
    semantic_hash CHAR(64) NOT NULL,
    PRIMARY KEY (owner_user_id, tool_name, semantic_hash),
    FOREIGN KEY (owner_user_id, tool_name)
        REFERENCES published_tool_contracts(owner_user_id, tool_name) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	// 审计：append-only，trigger 防改删（MySQL 用 SIGNAL 中止，见 execSchemaStatement
	// 的幂等说明——CREATE TRIGGER 无 IF NOT EXISTS，重复时按 1359 忽略）。
	// source_ip 记录来源 IP——等保二级审计要求能追溯"谁从哪台机器操作"。
	`CREATE TABLE IF NOT EXISTS audit_events (
    id VARCHAR(64) PRIMARY KEY,
    occurred_at VARCHAR(40) NOT NULL,
    actor_type VARCHAR(16) NOT NULL CHECK (actor_type IN ('user', 'device', 'admin', 'system')),
    actor_id VARCHAR(255) NOT NULL,
    action VARCHAR(64) NOT NULL,
    object_type VARCHAR(64) NOT NULL,
    object_id VARCHAR(255) NOT NULL,
    result VARCHAR(16) NOT NULL,
    metadata_json MEDIUMTEXT NOT NULL DEFAULT ('{}'),
    source_ip VARCHAR(64) NOT NULL DEFAULT '',
    INDEX idx_audit_events_time (occurred_at DESC),
    INDEX idx_audit_events_object (object_type, object_id, occurred_at DESC),
    INDEX idx_audit_events_source_ip (source_ip, occurred_at DESC)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TRIGGER audit_events_no_update
BEFORE UPDATE ON audit_events FOR EACH ROW
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_events are append-only'`,
	`CREATE TRIGGER audit_events_no_delete
BEFORE DELETE ON audit_events FOR EACH ROW
SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_events are append-only'`,
	// 调用记录：只存元数据，绝不存 arguments/result（hub 是中继不是落地）。
	`CREATE TABLE IF NOT EXISTS invoke_logs (
    id VARCHAR(64) PRIMARY KEY,
    occurred_at VARCHAR(40) NOT NULL,
    user_id VARCHAR(64) NOT NULL,
    node_id VARCHAR(64) NOT NULL,
    tool_name VARCHAR(255) NOT NULL,
    outcome VARCHAR(16) NOT NULL CHECK (outcome IN ('success', 'error', 'timeout', 'cancelled')),
    duration_ms BIGINT NOT NULL,
    error_code VARCHAR(64) NOT NULL DEFAULT '',
    client_ip VARCHAR(64) NOT NULL DEFAULT '',
    request_id VARCHAR(64) NOT NULL DEFAULT '',
    INDEX idx_invoke_logs_user_time (user_id, occurred_at DESC),
    INDEX idx_invoke_logs_time (occurred_at)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`,
}

// v1 即初始版本；v2 为审计表补充来源 IP；v3 为调用记录补充来源 IP。
// MySQL 的迁移语句与 SQLite 的差异仅在列类型（VARCHAR vs TEXT）与
// CREATE INDEX 不接受 IF NOT EXISTS（由 execSchemaStatement 兜底）。
var schemaMigrations = []schemaMigration{
	{
		version: 2,
		name:    "audit_source_ip",
		statements: []string{
			// 等保二级审计要求"记录访问者的源 IP"。此前 audit_events 没有任何
			// 来源字段，登录成功/失败都无法追溯是哪台机器发起的——而 Login()
			// 其实早就拿到了 ip（用于限流键），只是没写进审计。
			//
			// 新库不需要这条：currentSchema 里已包含该列。这里只处理已存在的库。
			// 走 ALTER TABLE 而不是改 currentSchema，是因为已有库不会重建。
			`ALTER TABLE audit_events ADD COLUMN source_ip TEXT NOT NULL DEFAULT ''`,
			// 按来源 IP 检索审计记录（安全事件溯源时按 IP 过滤）。
			`CREATE INDEX IF NOT EXISTS idx_audit_events_source_ip
    ON audit_events(source_ip, occurred_at DESC)`,
		},
		mysql: []string{
			`ALTER TABLE audit_events ADD COLUMN source_ip VARCHAR(64) NOT NULL DEFAULT ''`,
			`CREATE INDEX idx_audit_events_source_ip ON audit_events(source_ip, occurred_at DESC)`,
		},
	},
	{
		version: 3,
		name:    "invoke_logs_client_ip",
		statements: []string{
			// 等保二级审计要求调用记录可追溯到来源地址。/mcp 是 Bearer 入口，
			// 此前 invoke_logs 只有 user/node/tool，无法回答"这个调用来自哪个 IP"。
			// 新库不需要这条：currentSchema 里已包含该列。这里只处理已存在的库。
			`ALTER TABLE invoke_logs ADD COLUMN client_ip TEXT NOT NULL DEFAULT ''`,
		},
		mysql: []string{
			`ALTER TABLE invoke_logs ADD COLUMN client_ip VARCHAR(64) NOT NULL DEFAULT ''`,
		},
	},
}

// EnsureSchema 把控制库升级到 CurrentSchemaVersion，失败时启动流程必须中止。
// 方言（SQLite/MySQL）由连接自身推断，调用方无需关心后端差异。
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	return migrateSchema(ctx, db)
}

func migrateSchema(ctx context.Context, db *sql.DB) error {
	d := DialectOf(db)
	// 服务器方言先把版本表建出来（SQLite 用 user_version，不需要）。
	if err := ensureMigrationTable(ctx, db, d); err != nil {
		return err
	}
	version, err := readSchemaVersion(ctx, db, d)
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
		return initializeSchema(ctx, db, d)
	}
	if version > CurrentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, CurrentSchemaVersion)
	}
	for _, migration := range schemaMigrations {
		if migration.version <= version {
			continue
		}
		if err := applyMigration(ctx, db, d, migration); err != nil {
			return err
		}
	}
	return nil
}

// ensureMigrationTable 在服务器方言下确保 schema_migrations 表存在。
// SQLite 返回 nil 不动库——user_version 机制是历史契约，不迁移。
// 无占位符，MySQL 与 PG 的建表语句各自给出。
func ensureMigrationTable(ctx context.Context, db *sql.DB, d Dialect) error {
	if d == DialectSQLite {
		return nil
	}
	var ddl string
	switch d {
	case DialectPostgres:
		ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
	    version INTEGER PRIMARY KEY,
	    name TEXT NOT NULL DEFAULT '',
	    applied_at TEXT NOT NULL
	)`
	default: // DialectMySQL
		ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
	    version INT PRIMARY KEY,
	    name VARCHAR(64) NOT NULL DEFAULT '',
	    applied_at VARCHAR(40) NOT NULL
	) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_bin`
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}
	return nil
}

func initializeSchema(ctx context.Context, db *sql.DB, d Dialect) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range currentSchemaFor(d) {
		if err := execSchemaStatement(ctx, tx, d, statement); err != nil {
			return fmt.Errorf("initialize schema: %w", err)
		}
	}
	if err := writeSchemaVersion(ctx, tx, d, CurrentSchemaVersion, "initial"); err != nil {
		return err
	}
	// 注意：MySQL 的 DDL 会隐式提交，"全部成功才落版本号"在服务器方言下
	// 不是原子保证。但这不破坏正确性：所有语句幂等（IF NOT EXISTS /
	// 索引触发器重复容错），版本号是最后一步，中途失败下次启动重跑即可。
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema initialization: %w", err)
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, d Dialect, migration schemaMigration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration v%d (%s): %w", migration.version, migration.name, err)
	}
	defer func() { _ = tx.Rollback() }()
	for i, statement := range migration.forDialect(d) {
		if err := execSchemaStatement(ctx, tx, d, statement); err != nil {
			return fmt.Errorf("migration v%d (%s) statement %d: %w", migration.version, migration.name, i+1, err)
		}
	}
	if err := writeSchemaVersion(ctx, tx, d, migration.version, migration.name); err != nil {
		return fmt.Errorf("migration v%d (%s): %w", migration.version, migration.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration v%d (%s): %w", migration.version, migration.name, err)
	}
	return nil
}

// currentSchemaPostgres 与 currentSchema 同结构同约束（PostgreSQL 14+）。
// 与 SQLite 版几乎逐句相同——PG 的 TEXT 主键/外键/CHECK/IF NOT EXISTS 索引
// 都与 SQLite 写法一致，唯一实质差异是 append-only 触发器：PG 没有
// RAISE(ABORT)，需要 plpgsql 函数 + EXECUTE FUNCTION 触发器；
// 且 CREATE TRIGGER 无 IF NOT EXISTS，重复由 execSchemaStatement 按
// duplicate_object（42710）容错。
var currentSchemaPostgres = []string{
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
	// Fleet 公开契约：tool_name 在 owner_user_id 内唯一（复合主键）。
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
	// 审计：append-only，trigger 防改删（plpgsql RAISE EXCEPTION）。
	// source_ip 记录来源 IP——等保二级审计要求能追溯"谁从哪台机器操作"。
	`CREATE TABLE IF NOT EXISTS audit_events (
    id TEXT PRIMARY KEY,
    occurred_at TEXT NOT NULL,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('user', 'device', 'admin', 'system')),
    actor_id TEXT NOT NULL,
    action TEXT NOT NULL,
    object_type TEXT NOT NULL,
    object_id TEXT NOT NULL,
    result TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    source_ip TEXT NOT NULL DEFAULT ''
)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_events_time ON audit_events(occurred_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_events_object ON audit_events(object_type, object_id, occurred_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_events_source_ip ON audit_events(source_ip, occurred_at DESC)`,
	`CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_events are append-only';
END;
$$ LANGUAGE plpgsql`,
	`CREATE TRIGGER audit_events_no_update
BEFORE UPDATE ON audit_events FOR EACH ROW
EXECUTE FUNCTION audit_events_append_only()`,
	`CREATE TRIGGER audit_events_no_delete
BEFORE DELETE ON audit_events FOR EACH ROW
EXECUTE FUNCTION audit_events_append_only()`,
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
    client_ip TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT ''
)`,
	`CREATE INDEX IF NOT EXISTS idx_invoke_logs_user_time
    ON invoke_logs(user_id, occurred_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_invoke_logs_time ON invoke_logs(occurred_at)`,
}

func currentSchemaFor(d Dialect) []string {
	switch d {
	case DialectMySQL:
		return currentSchemaMySQL
	case DialectPostgres:
		return currentSchemaPostgres
	}
	return currentSchema
}

// execSchemaStatement 执行一条 schema 语句；服务器方言的幂等 DDL 错误
// 按成功处理——MySQL 无 IF NOT EXISTS 的 CREATE INDEX/CREATE TRIGGER，
// PG 的 CREATE TRIGGER 同样没有（PG 索引可以用 IF NOT EXISTS），
// 重复执行是"已存在"而非失败。
func execSchemaStatement(ctx context.Context, execer schemaExecer, d Dialect, statement string) error {
	if _, err := execer.ExecContext(ctx, statement); err != nil {
		switch d {
		case DialectMySQL:
			if isIdempotentMySQLDDLError(err) {
				return nil
			}
		case DialectPostgres:
			if isIdempotentPostgresDDLError(err) {
				return nil
			}
		}
		return err
	}
	return nil
}

type schemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func isIdempotentMySQLDDLError(err error) bool {
	var myErr *mysql.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	switch myErr.Number {
	case 1050, 1060, 1061, 1359: // 表已存在 / 列已存在 / 索引名已存在 / 触发器已存在
		return true
	}
	return false
}

func isIdempotentPostgresDDLError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "42710": // duplicate_object：触发器/函数重名
		return true
	}
	return false
}

// writeSchemaVersion 与建表语句同事务执行。
// SQLite 走 PRAGMA（不支持参数绑定，版本号是程序内部 int 常量，直接拼接安全）；
// MySQL/PG 记录到 schema_migrations（同版本重复写入 = 幂等重跑覆盖）。
// 服务器方言的 SQL 直接按各自占位符写法给出（这里不走 core.DB 的 ? 重写，
// 因为 schema 层持有的是裸 *sql.DB / *sql.Tx）。
func writeSchemaVersion(ctx context.Context, tx *sql.Tx, d Dialect, version int, name string) error {
	if d == DialectSQLite {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
			return fmt.Errorf("write schema version: %w", err)
		}
		return nil
	}
	var stmt string
	switch d {
	case DialectPostgres:
		stmt = `INSERT INTO schema_migrations(version, name, applied_at)
			VALUES($1, $2, $3)
			ON CONFLICT(version) DO UPDATE SET name = EXCLUDED.name`
	default: // DialectMySQL
		stmt = `INSERT INTO schema_migrations(version, name, applied_at)
			VALUES(?, ?, ?) AS new
			ON DUPLICATE KEY UPDATE name = new.name`
	}
	if _, err := tx.ExecContext(ctx, stmt, version, name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("write schema migration: %w", err)
	}
	return nil
}

func readSchemaVersion(ctx context.Context, db *sql.DB, d Dialect) (int, error) {
	if d == DialectSQLite {
		var version int
		if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			return 0, fmt.Errorf("read schema version: %w", err)
		}
		return version, nil
	}
	// 无占位符，MySQL 与 PG 通用。
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var query string
	switch DialectOf(db) {
	case DialectMySQL:
		query = `SELECT table_name FROM information_schema.tables
			WHERE table_schema = DATABASE() AND table_name = ?`
	case DialectPostgres:
		query = `SELECT tablename FROM pg_catalog.pg_tables
			WHERE schemaname = current_schema() AND tablename = $1`
	default:
		query = `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`
	}
	var found string
	err := db.QueryRowContext(ctx, query, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check table %s: %w", name, err)
	}
	return true, nil
}
