package httpx

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/uvwt/agenthub/internal/audit"
)

// 调用记录查询：管理员看全局流水与统计（含用户名）；普通用户禁入管理接口；
// 用户自己的 /v1/invokes 只能查到属于自己的记录；游标分页与筛选可用。
func TestInvokeQueryBoundaries(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin, err := e.svc.InitializeAdmin(ctx, "root@example.com", strongPassword)
	if err != nil {
		t.Fatalf("init admin: %v", err)
	}
	alice, err := e.svc.CreateUser(ctx, admin.ID, "alice@example.com", "initial-pass-123456")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := e.invokes.RecordInvoke(ctx, alice.ID, "node_1", "echo", audit.OutcomeSuccess, 120*time.Millisecond, ""); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := e.invokes.RecordInvoke(ctx, alice.ID, "node_1", "browser_open", audit.OutcomeError, 30*time.Millisecond, "VALIDATION_ERROR"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := e.invokes.RecordInvoke(ctx, "usr_someone_else", "node_2", "exec", audit.OutcomeError, 30*time.Millisecond, "NODE_OFFLINE"); err != nil {
		t.Fatalf("record: %v", err)
	}

	adminToken, adminCSRF := e.loginAs(t, "root@example.com", strongPassword)
	resp, body := e.do(t, "GET", "/v1/admin/invokes", adminToken, adminCSRF, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin invokes: %d", resp.StatusCode)
	}
	items := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("admin must see all 3 invokes: %v", body)
	}
	// 用户名必须关联展示。
	first := items[0].(map[string]any)
	if first["username"] == "" {
		t.Fatalf("admin view must carry username: %v", first)
	}

	// 筛选：outcome=error 只剩 2 条。
	resp, body = e.do(t, "GET", "/v1/admin/invokes?outcome=error", adminToken, adminCSRF, nil)
	if len(body["items"].([]any)) != 2 {
		t.Fatalf("outcome filter: %v", body)
	}
	// 筛选：tool=browser 前缀匹配只剩 1 条。
	resp, body = e.do(t, "GET", "/v1/admin/invokes?tool=browser", adminToken, adminCSRF, nil)
	if len(body["items"].([]any)) != 1 {
		t.Fatalf("tool filter: %v", body)
	}

	// 页码分页：limit=2 首页 2 条 + total=3；第 2 页取到剩余 1 条。
	resp, body = e.do(t, "GET", "/v1/admin/invokes?limit=2", adminToken, adminCSRF, nil)
	firstPage := body["items"].([]any)
	if len(firstPage) != 2 {
		t.Fatalf("first page must have 2: %v", body)
	}
	if body["total"].(float64) != 3 || body["page"].(float64) != 1 {
		t.Fatalf("page metadata wrong: %v", body)
	}
	resp, body = e.do(t, "GET", "/v1/admin/invokes?limit=2&page=2", adminToken, adminCSRF, nil)
	secondPage := body["items"].([]any)
	if len(secondPage) != 1 {
		t.Fatalf("second page must have 1: %v", body)
	}
	if body["page"].(float64) != 2 {
		t.Fatalf("page echo wrong: %v", body)
	}

	// 统计接口（含用户名）。
	resp, body = e.do(t, "GET", "/v1/admin/stats", adminToken, adminCSRF, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin stats: %d", resp.StatusCode)
	}
	stats := body["stats"].([]any)
	stat0 := stats[0].(map[string]any)
	if stat0["username"] == "" {
		t.Fatalf("stats must carry username: %v", stat0)
	}

	// 用户自己的视图：只见自己的 2 条，且不泄露他人记录。
	aliceToken, aliceCSRF := e.loginAs(t, "alice@example.com", "initial-pass-123456")
	resp, body = e.do(t, "GET", "/v1/invokes", aliceToken, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user invokes: %d", resp.StatusCode)
	}
	records := body["items"].([]any)
	if len(records) != 2 {
		t.Fatalf("alice must see only her 2 records: %v", body)
	}
	record := records[0].(map[string]any)
	if record["user_id"] != alice.ID {
		t.Fatalf("record ownership mismatch: %v", record)
	}

	// 非法 outcome 400。
	resp, _ = e.do(t, "GET", "/v1/invokes?outcome=bogus", aliceToken, "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid outcome must be 400, got %d", resp.StatusCode)
	}

	// 普通用户禁入管理流水。
	resp, _ = e.do(t, "GET", "/v1/admin/invokes", aliceToken, aliceCSRF, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user hitting admin invokes must be 403, got %d", resp.StatusCode)
	}
}
