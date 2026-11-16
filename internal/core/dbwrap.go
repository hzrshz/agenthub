// 方言感知的 database/sql 薄包装：只做"PG 占位符重写"这一件事，
// 其余原样转发。目的：业务 SQL 统一写 ?（与 SQLite/MySQL 一致），
// PostgreSQL（pgx）要求 $N，由这里在调用侧一次性转换，避免 60 余条
// 查询语句各维护一份 PG 变体。
//
// 这是实现多后端可移植的必要机制，不是形式化分层：转换是确定性纯函数
// （RebindPlaceholders 有独立测试），数据流可见性不变。
package core

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// DB 包装 *sql.DB，让 store 层无需关心占位符方言。
type DB struct {
	db *sql.DB
	d  Dialect
}

// NewDB 包装一个已打开的数据库；方言由驱动实例推断。
func NewDB(db *sql.DB) *DB {
	return &DB{db: db, d: DialectOf(db)}
}

// Dialect 返回被包装连接的方言（store 侧选 upsert 变体时用）。
func (db *DB) Dialect() Dialect { return db.d }

func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.db.ExecContext(ctx, db.rebind(query), args...)
}

func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.db.QueryContext(ctx, db.rebind(query), args...)
}

func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.db.QueryRowContext(ctx, db.rebind(query), args...)
}

// BeginTx 返回方言感知的事务（事务内的查询同样需要占位符重写）。
func (db *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := db.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, d: db.d}, nil
}

func (db *DB) rebind(query string) string {
	if db.d == DialectPostgres {
		return RebindPlaceholders(query)
	}
	return query
}

// Tx 包装 *sql.Tx，与 DB 同样做占位符重写。
type Tx struct {
	tx *sql.Tx
	d  Dialect
}

func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.tx.ExecContext(ctx, tx.rebind(query), args...)
}

func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.tx.QueryContext(ctx, tx.rebind(query), args...)
}

func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.tx.QueryRowContext(ctx, tx.rebind(query), args...)
}

func (tx *Tx) Commit() error   { return tx.tx.Commit() }
func (tx *Tx) Rollback() error { return tx.tx.Rollback() }

func (tx *Tx) rebind(query string) string {
	if tx.d == DialectPostgres {
		return RebindPlaceholders(query)
	}
	return query
}

// RebindPlaceholders 把 PostgreSQL 的 ? 占位符按出现顺序重写为 $1..$N。
//
// 依赖一个前提：本仓库所有 SQL 语句里，"?"只做参数占位符出现——
// 语句中没有字符串字面量或注释包含问号（建表/迁移 DDL 与 trigger 函数体
// 均无占位符，不经过这里）。若未来引入含问号的字面量，需要改为扫描式重写。
func RebindPlaceholders(query string) string {
	if !strings.ContainsRune(query, '?') {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}
