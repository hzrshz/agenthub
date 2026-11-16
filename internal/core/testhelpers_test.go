package core

import (
	"context"
	"database/sql"
	"testing"
)

func openTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	ctx := context.Background()
	// OpenTestDB 默认 SQLite 临时文件；CI 真库 job 通过 HUB_TEST_DB_* 切换，
	// 此处无需关心后端差异。
	db, cleanup, err := OpenTestDB(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	return db, cleanup
}
