// Package core 提供 AgentHub 的底层公共件：SQLite 打开、错误分类与 ID 生成。
package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

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
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
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
	return db, nil
}

// IsSQLiteConflict 判断错误是否为可重试的约束/锁冲突。
func IsSQLiteConflict(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "constraint failed") ||
		strings.Contains(text, "database is locked") ||
		errors.Is(err, sql.ErrTxDone)
}
