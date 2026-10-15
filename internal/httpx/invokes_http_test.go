package httpx

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/uvwt/agenthub/internal/audit"
)

// 调用记录查询：管理员看全局流水与统计；普通用户禁入管理接口；
// 用户自己的 /v1/invokes 只能查到属于自己的记录。
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
	if err := e.invokes.RecordInvoke(ctx, "usr_someone_else", "node_2", "exec", audit.OutcomeError, 30*time.Millisecond, "VALIDATION_ERROR"); err != nil {
		t.Fatalf("record: %v", err)
	}

	adminToken, adminCSRF := e.loginAs(t, "root@example.com", strongPassword)
	resp, body := e.do(t, "GET", "/v1/admin/invokes", adminToken, adminCSRF, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin invokes: %d", resp.StatusCode)
	}
	if len(body["invokes"].([]any)) != 2 {
		t.Fatalf("admin must see all invokes: %v", body)
	}

	resp, body = e.do(t, "GET", "/v1/admin/stats", adminToken, adminCSRF, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin stats: %d", resp.StatusCode)
	}

	aliceToken, aliceCSRF := e.loginAs(t, "alice@example.com", "initial-pass-123456")
	resp, body = e.do(t, "GET", "/v1/invokes", aliceToken, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user invokes: %d", resp.StatusCode)
	}
	records, _ := body["invokes"].([]any)
	if len(records) != 1 {
		t.Fatalf("alice must only see her own record: %v", body)
	}
	record, _ := records[0].(map[string]any)
	if record["user_id"] != alice.ID || record["tool_name"] != "echo" {
		t.Fatalf("record ownership mismatch: %v", record)
	}

	resp, _ = e.do(t, "GET", "/v1/admin/invokes", aliceToken, aliceCSRF, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user hitting admin invokes must be 403, got %d", resp.StatusCode)
	}
}
