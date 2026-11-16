package core

import (
	"database/sql"
	"testing"
)

// RebindPlaceholders 必须把所有 ? 按序重写为 $1..$N；无占位符时原样返回。
func TestRebindPlaceholders(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`SELECT * FROM users WHERE id = ?`, `SELECT * FROM users WHERE id = $1`},
		{`INSERT INTO t(a, b) VALUES(?, ?)`, `INSERT INTO t(a, b) VALUES($1, $2)`},
		{`UPDATE t SET a = ? WHERE b = ? AND c = ?`, `UPDATE t SET a = $1 WHERE b = $2 AND c = $3`},
		{`SELECT COUNT(*) FROM t`, `SELECT COUNT(*) FROM t`}, // 无占位符必须原样
		{`DELETE FROM invoke_logs WHERE id IN (
			SELECT id FROM invoke_logs ORDER BY occurred_at DESC LIMIT -1 OFFSET ?
		)`, `DELETE FROM invoke_logs WHERE id IN (
			SELECT id FROM invoke_logs ORDER BY occurred_at DESC LIMIT -1 OFFSET $1
		)`},
		// 同一参数样式混排（审计查询的 JOIN + LIKE 前匹配）。
		{`SELECT l.id FROM invoke_logs l LEFT JOIN users u ON u.id = l.user_id
			WHERE u.username LIKE ? AND l.tool_name LIKE ? ORDER BY l.id DESC LIMIT ? OFFSET ?`,
			`SELECT l.id FROM invoke_logs l LEFT JOIN users u ON u.id = l.user_id
			WHERE u.username LIKE $1 AND l.tool_name LIKE $2 ORDER BY l.id DESC LIMIT $3 OFFSET $4`},
	}
	for _, tc := range cases {
		if got := RebindPlaceholders(tc.in); got != tc.want {
			t.Errorf("RebindPlaceholders(%q) =\n%q\nwant\n%q", tc.in, got, tc.want)
		}
	}
}

// DialectOf 必须从驱动实例推断出正确方言。mysql/pg 用惰性 sql.Open 即可
// 拿到注册的驱动实例（不真正连服务器）。
func TestDialectOf(t *testing.T) {
	db, cleanup := openTestDB(t)
	if got := DialectOf(db); got != DialectSQLite {
		t.Fatalf("DialectOf(sqlite) = %q, want %q", got, DialectSQLite)
	}
	cleanup()

	my, err := sql.Open("mysql", "u@tcp(127.0.0.1:1)/")
	if err != nil {
		t.Fatalf("open mysql driver: %v", err)
	}
	defer my.Close()
	if got := DialectOf(my); got != DialectMySQL {
		t.Fatalf("DialectOf(mysql) = %q, want %q", got, DialectMySQL)
	}

	pg, err := sql.Open("pgx", "host=127.0.0.1 user=x dbname=x")
	if err != nil {
		t.Fatalf("open pgx driver: %v", err)
	}
	defer pg.Close()
	if got := DialectOf(pg); got != DialectPostgres {
		t.Fatalf("DialectOf(postgres) = %q, want %q", got, DialectPostgres)
	}
}

// core.DB 包装在 PG 方言下必须重写占位符——这是 store 层统一写 ? 的前提。
func TestDBRebindForPostgres(t *testing.T) {
	pg, err := sql.Open("pgx", "host=127.0.0.1 user=x dbname=x")
	if err != nil {
		t.Fatalf("open pgx driver: %v", err)
	}
	defer pg.Close()
	wrapped := NewDB(pg)
	if wrapped.rebind(`SELECT * FROM t WHERE a = ? AND b = ?`) != `SELECT * FROM t WHERE a = $1 AND b = $2` {
		t.Fatal("postgres rebind not applied")
	}
	// 非 PG 方言原样透传。
	sqliteDB, cleanup := openTestDB(t)
	defer cleanup()
	if wrapped := NewDB(sqliteDB); wrapped.rebind(`SELECT * FROM t WHERE a = ?`) != `SELECT * FROM t WHERE a = ?` {
		t.Fatal("non-postgres dialect must not rewrite placeholders")
	}
}