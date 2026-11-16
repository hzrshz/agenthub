// Package core 的方言抽象：SQLite / MySQL / PostgreSQL 三个后端在
// 占位符、Schema 版本化、DDL 幂等与错误分类上的差异都收敛在这里，
// 业务 SQL 尽量保持单份，只有真正不通用的语句才按方言出变体。
package core

import (
	"database/sql"

	"github.com/go-sql-driver/mysql"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
	sqlite "modernc.org/sqlite"
)

// Dialect 标识控制库后端方言。
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectMySQL    Dialect = "mysql"
	DialectPostgres Dialect = "postgres"
)

// IsMySQL / IsPostgres 是业务层选 SQL 变体时的便捷判断。
func (d Dialect) IsMySQL() bool    { return d == DialectMySQL }
func (d Dialect) IsPostgres() bool { return d == DialectPostgres }

// DialectOf 从 *sql.DB 推断后端方言。
//
// database/sql 不暴露连接用的驱动注册名，但三个驱动在 init 时都是
// 注册包级单例（*sqlite.Driver / *mysql.MySQLDriver / *stdlib.Driver），
// 按实例类型区分即可，无需在开关链路里额外传方言。未知驱动保守回退
// SQLite，避免新后端没登记时业务层误走别的 SQL 分支。
func DialectOf(db *sql.DB) Dialect {
	switch db.Driver().(type) {
	case *mysql.MySQLDriver:
		return DialectMySQL
	case *sqlite.Driver:
		return DialectSQLite
	case *pgxstdlib.Driver:
		return DialectPostgres
	}
	return DialectSQLite
}
