// Package core 提供 AgentHub 的底层公共件：控制库打开、错误分类与 ID 生成。
package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/uvwt/agenthub/internal/config"
)

// OpenDB 按配置打开控制库后端。未配置 HUB_DB_* 时等价于 OpenSQLite
// （默认后端，行为与历史版本完全一致）。
func OpenDB(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	switch cfg.DBDriver {
	case "", "sqlite":
		return OpenSQLite(ctx, cfg.ControlDBPath(), 2)
	case "mysql":
		return openMySQL(ctx, cfg)
	case "postgres":
		return openPostgres(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported db driver %q", cfg.DBDriver)
	}
}

// openMySQL 打开 MySQL 8.0 协议兼容后端（含 OceanBase 等兼容库）。
func openMySQL(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	password, err := readPasswordFile(cfg.DBPasswordFile)
	if err != nil {
		return nil, err
	}
	parsed, err := mysql.ParseDSN(cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("parse mysql dsn: %w", err)
	}
	// 配置层已拒绝内嵌口令；这里再兜底一道，防绕开 Validate 的调用路径。
	if parsed.Passwd != "" {
		return nil, errors.New("mysql dsn must not embed a password, use the password file")
	}
	parsed.Passwd = password
	if parsed.Timeout == 0 {
		parsed.Timeout = 5 * time.Second
	}
	if parsed.ReadTimeout == 0 {
		parsed.ReadTimeout = 30 * time.Second
	}
	if parsed.WriteTimeout == 0 {
		parsed.WriteTimeout = 30 * time.Second
	}
	// 连接字符集必须与建表一致（utf8mb4_bin，见 schema.go）：哈希列与
	// 用户名的等值比较/唯一索引依赖大小写敏感的二进制语义，连接层若用
	// 默认 collation 会出现服务端与客户端判定不一致。
	if parsed.Collation == "" {
		parsed.Collation = "utf8mb4_bin"
	}
	db, err := sql.Open("mysql", parsed.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	maxOpen := cfg.DBMaxOpenConns
	if maxOpen < 1 {
		maxOpen = 10
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}

// openPostgres 打开 PostgreSQL 14+ 协议兼容后端（含 GaussDB/金仓等兼容库）。
func openPostgres(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	password, err := readPasswordFile(cfg.DBPasswordFile)
	if err != nil {
		return nil, err
	}
	connConfig, err := pgx.ParseConfig(cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	// 配置层已拒绝内嵌口令；这里再兜底一道。
	if connConfig.Password != "" {
		return nil, errors.New("postgres dsn must not embed a password, use the password file")
	}
	connConfig.Password = password
	if connConfig.ConnectTimeout == 0 {
		connConfig.ConnectTimeout = 5 * time.Second
	}
	db := pgxstdlib.OpenDB(*connConfig)
	maxOpen := cfg.DBMaxOpenConns
	if maxOpen < 1 {
		maxOpen = 10
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return db, nil
}

// readPasswordFile 读取数据库口令文件。返回 trim 掉行尾换行的口令；
// 空文件视为错误（口令为空宁可启动失败也不要连上一个空口令库）。
func readPasswordFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("db password file path is empty")
	}
	// #nosec G304 G703 -- 路径来自运维配置 HUB_DB_PASSWORD_FILE（部署侧受控
	// 文件），不是外部输入，"file inclusion by variable"不适用。配置文件读取
	// 本来就该由进程自己完成，换成 os.Root 反而把路径语义搞复杂。
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read db password file %s: %w", path, err)
	}
	secret := strings.TrimRight(string(raw), "\r\n")
	if secret == "" {
		return "", fmt.Errorf("db password file %s is empty", path)
	}
	return secret, nil
}

// OpenSQLite 打开控制库。hub 是中小规模多用户网关，写入压力低，
// 与 nexusdock 同样以可靠性优先：rollback journal + FULL 同步，
// 避免容器共享文件系统上 WAL/SHM 的一致性坑。
func OpenSQLite(ctx context.Context, path string, maxOpenConns int) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}
	dsn := path
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve database path: %w", err)
		}
		dataDir := filepath.Dir(absolute)
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
		// 目录权限也收紧。MkdirAll 只对**新建**目录应用 mode，对已存在的目录
		// 保持原权限——而数据目录可能是运维预先建好的（默认 0755），
		// 此时同机其他用户仍可列出目录内容、读到 control.db。
		// chmod 对新建与已存在都生效，因此这里补一次。
		// #nosec G302 -- 目录必须保留属主可执行位（否则无法进入），
		// 0700 是目录而非文件的权限收紧形态，等保基线要求即此值。
		if err := os.Chmod(dataDir, 0o700); err != nil {
			slog.Warn("收紧数据目录权限失败，请检查属主与挂载选项",
				"dir", dataDir, "error", err)
		}
		u := &url.URL{Scheme: "file", Path: absolute}
		query := u.Query()
		query.Add("_pragma", "foreign_keys(1)")
		query.Add("_pragma", "busy_timeout(5000)")
		query.Add("_pragma", "journal_mode(DELETE)")
		query.Add("_pragma", "synchronous(FULL)")
		u.RawQuery = query.Encode()
		dsn = u.String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if maxOpenConns < 1 {
		maxOpenConns = 1
	}
	if path == ":memory:" {
		maxOpenConns = 1
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	// 数据库文件权限收紧到 0600（放在打开成功之后——此时文件必然已创建）。
	//
	// 目录是 0700，但控制库存着全部口令哈希、session/token 哈希与审计记录。
	// 默认 umask(022) 下新建文件是 0644，同机其他用户可读；基线检查会报
	// "数据库文件权限过大"。已实测确认过该问题。
	//
	// 对已存在的文件同样生效（修正历史遗留的宽松权限）。失败不阻断启动：
	// 权限收紧失败不应让服务起不来，改由部署侧与审计兜底。
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if absolute, absErr := filepath.Abs(path); absErr == nil {
			if err := os.Chmod(absolute, 0o600); err != nil {
				slog.Warn("收紧控制库文件权限失败，请检查属主与挂载选项",
					"path", absolute, "error", err)
			}
		}
	}
	return db, nil
}

// IsConflict 报告错误是否为唯一约束冲突（排除锁竞争）。
//
// 语义用途：幂等 upsert / 配对去重的场景里，"行已存在"不是故障，调用方
// 据此映射成"已存在"类业务错误。SQLite 靠错误文本（constraint failed /
// unique），MySQL 用驱动错误码 1062（ER_DUP_ENTRY）。
func IsConflict(err error) bool {
	if err == nil {
		return false
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1062
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "constraint failed") ||
		strings.Contains(text, "unique constraint") ||
		strings.Contains(text, "unique")
}

// IsLocked 报告错误是否为数据库写锁竞争（SQLite busy / MySQL 锁等待与死锁）。
//
// 语义用途：配对码单次消费等"并发抢同一行"场景，锁竞争对调用方等价于
// "别人先完成了"，映射成同一业务错误即可，不需要重试。
func IsLocked(err error) bool {
	if err == nil {
		return false
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1205 || myErr.Number == 1213 // ER_LOCK_WAIT_TIMEOUT / ER_LOCK_DEADLOCK
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database is locked") ||
		strings.Contains(text, "busy") ||
		strings.Contains(text, "locked") ||
		errors.Is(err, sql.ErrTxDone)
}
