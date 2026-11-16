// Package core 附带的测试库打开器：默认 SQLite 临时文件，
// CI 的真库 job（MySQL/PG）通过环境变量切到对应后端。
package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
)

// OpenTestDB 打开测试控制库，行为与生产 OpenDB 一致：
//   - 默认：dataDir 下独立 SQLite 文件（等价历史测试路径，本地开发零配置）；
//   - HUB_TEST_DB_DRIVER=mysql/postgres：连真库（CI 的 mysql/pg job 与
//     本地 docker 冒烟用）。每个测试一个随机命名的独立数据库，用完即删——
//     与 SQLite per-test 临时文件保持同样的隔离语义，并行包互不踩表。
//
// 返回的 cleanup 负责删库并关闭连接；sqlite 路径下只 close。
func OpenTestDB(ctx context.Context, dataDir string) (*sql.DB, func(), error) {
	driver := strings.ToLower(strings.TrimSpace(os.Getenv("HUB_TEST_DB_DRIVER")))
	switch driver {
	case "", "sqlite":
		db, err := OpenSQLite(ctx, filepath.Join(dataDir, "test.db"), 2)
		if err != nil {
			return nil, nil, err
		}
		if err := EnsureSchema(ctx, db); err != nil {
			_ = db.Close()
			return nil, nil, err
		}
		return db, func() { _ = db.Close() }, nil
	case "mysql":
		return openMySQLTestDB(ctx)
	case "postgres":
		return openPostgresTestDB(ctx)
	default:
		return nil, nil, fmt.Errorf("invalid HUB_TEST_DB_DRIVER %q", driver)
	}
}

// openMySQLTestDB 为单个测试建独立 MySQL 库并做好 schema。
// DSN 口令规则与生产一致：必须是带口令文件的非内嵌口令 DSN（见 config）。
func openMySQLTestDB(ctx context.Context) (*sql.DB, func(), error) {
	dsn := strings.TrimSpace(os.Getenv("HUB_TEST_DB_DSN"))
	if dsn == "" {
		return nil, nil, errors.New("HUB_TEST_DB_DSN is required when HUB_TEST_DB_DRIVER=mysql")
	}
	password, err := readPasswordFile(os.Getenv("HUB_TEST_DB_PASSWORD_FILE"))
	if err != nil {
		return nil, nil, err
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parse mysql test dsn: %w", err)
	}
	if parsed.Passwd != "" {
		return nil, nil, errors.New("mysql test dsn must not embed a password, use HUB_TEST_DB_PASSWORD_FILE")
	}
	parsed.Passwd = password
	parsed.Collation = "utf8mb4_bin"

	// 管理连接：不带默认库，负责 CREATE/DROP DATABASE（需要相应权限，
	// CI 用 root，本地冒烟用授权了 *.* 的测试用户）。
	// 注意不能 defer 关闭：管理连接要活到 cleanup() 删库之后。
	admin := *parsed
	admin.DBName = ""
	adminConn, err := sql.Open("mysql", admin.FormatDSN())
	if err != nil {
		return nil, nil, fmt.Errorf("open mysql admin connection: %w", err)
	}
	adminConn.SetMaxOpenConns(1)
	dbName := "agenthub_test_" + randSuffix(8)
	if _, err := adminConn.ExecContext(ctx, "CREATE DATABASE "+dbName+" CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		_ = adminConn.Close()
		return nil, nil, fmt.Errorf("create mysql test database: %w", err)
	}
	dbConn := *parsed
	dbConn.DBName = dbName
	db, err := sql.Open("mysql", dbConn.FormatDSN())
	if err != nil {
		_, _ = adminConn.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
		_ = adminConn.Close()
		return nil, nil, fmt.Errorf("open mysql test database: %w", err)
	}
	db.SetMaxOpenConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		_, _ = adminConn.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
		_ = adminConn.Close()
		return nil, nil, fmt.Errorf("ping mysql test database: %w", err)
	}
	if err := EnsureSchema(ctx, db); err != nil {
		_ = db.Close()
		_, _ = adminConn.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
		_ = adminConn.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = db.Close()
		// 删库在连接关闭后进行：MySQL 会等所有连接断开才真正 drop，
		// 顺序反过来可能拿到"database is being dropped"噪音错误。
		_, _ = adminConn.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
		_ = adminConn.Close()
	}
	return db, cleanup, nil
}

// openPostgresTestDB 为单个测试建独立 PostgreSQL 库并做好 schema，
// 语义与 openMySQLTestDB 完全一致（独立库隔离、用完即删）。
func openPostgresTestDB(ctx context.Context) (*sql.DB, func(), error) {
	dsn := strings.TrimSpace(os.Getenv("HUB_TEST_DB_DSN"))
	if dsn == "" {
		return nil, nil, errors.New("HUB_TEST_DB_DSN is required when HUB_TEST_DB_DRIVER=postgres")
	}
	password, err := readPasswordFile(os.Getenv("HUB_TEST_DB_PASSWORD_FILE"))
	if err != nil {
		return nil, nil, err
	}
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parse postgres test dsn: %w", err)
	}
	if connConfig.Password != "" {
		return nil, nil, errors.New("postgres test dsn must not embed a password, use HUB_TEST_DB_PASSWORD_FILE")
	}
	connConfig.Password = password

	// 管理连接：用 DSN 的默认库（如 postgres）执行建库/删库。PG 的
	// CREATE DATABASE 不能在事务里执行，独立 Exec 即可。
	admin := pgxstdlib.OpenDB(*connConfig)
	admin.SetMaxOpenConns(1)
	dbName := "agenthub_test_" + randSuffix(8)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		_ = admin.Close()
		return nil, nil, fmt.Errorf("create postgres test database: %w", err)
	}
	dbCfg := *connConfig
	dbCfg.Database = dbName
	db := pgxstdlib.OpenDB(dbCfg)
	db.SetMaxOpenConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		_ = admin.Close()
		return nil, nil, fmt.Errorf("ping postgres test database: %w", err)
	}
	if err := EnsureSchema(ctx, db); err != nil {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		_ = admin.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = db.Close()
		// WITH (FORCE)（PG 13+）会断开残留连接，避免 drop 失败残留测试库。
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		_ = admin.Close()
	}
	return db, cleanup, nil
}

// randSuffix 生成随机小写十六进制后缀；随机数失败退化为时间戳（测试场景足够）。
func randSuffix(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:n]
	}
	return hex.EncodeToString(buf)[:n]
}
