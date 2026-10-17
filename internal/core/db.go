// Package core 提供 AgentHub 的底层公共件：SQLite 打开、错误分类与 ID 生成。
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
		dataDir := filepath.Dir(absolute)
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
		// 目录权限也收紧。MkdirAll 只对**新建**目录应用 mode，对已存在的目录
		// 保持原权限——而数据目录可能是运维预先建好的（默认 0755），
		// 此时同机其他用户仍可列出目录内容、读到 control.db。
		// chmod 对新建与已存在都生效，因此这里补一次。
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
