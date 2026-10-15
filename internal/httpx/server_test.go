package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/uvwt/agenthub/internal/auth"
	"github.com/uvwt/agenthub/internal/bridge"
	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/nodes"
)

const strongPassword = "correct-horse-battery"

type testEnv struct {
	svc *auth.Service
	ts  *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	db, err := core.OpenSQLite(ctx, filepath.Join(t.TempDir(), "test.db"), 2)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := core.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := auth.NewService(db)
	nodeStore := nodes.NewStore(db)
	hub := bridge.NewHub(nodeStore)
	server := NewServer(svc, nodeStore, hub, "https://hub.example.com")
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{svc: svc, ts: ts}
}

// do 发起 JSON 请求；sessionToken/CSRF 非空时带上凭证头。
func (e *testEnv) do(t *testing.T, method, path, sessionToken, csrf string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if sessionToken != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	}
	if csrf != "" {
		req.Header.Set(csrfHeaderName, csrf)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var result map[string]any
	if resp.Body != nil {
		_ = json.NewDecoder(resp.Body).Decode(&result)
	}
	return resp, result
}

func (e *testEnv) loginAs(t *testing.T, username, password string) (token, csrf string) {
	t.Helper()
	resp, body := e.do(t, "POST", "/v1/auth/login", "", "", map[string]string{"username": username, "password": password})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login %s: status %d body %v", username, resp.StatusCode, body)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == sessionCookieName {
			token = cookie.Value
		}
	}
	csrfRaw, _ := body["csrf_token"].(string)
	return token, csrfRaw
}

// 完整登录→whoami 流程；Cookie 即凭证。
func TestLoginAndMe(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if _, err := e.svc.InitializeAdmin(ctx, "root@example.com", strongPassword); err != nil {
		t.Fatalf("init admin: %v", err)
	}
	token, csrf := e.loginAs(t, "root@example.com", strongPassword)
	if token == "" || csrf == "" {
		t.Fatal("login must set session cookie and csrf token")
	}

	resp, body := e.do(t, "GET", "/v1/auth/me", token, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me: status %d", resp.StatusCode)
	}
	if body["username"] != "root@example.com" || body["role"] != "admin" {
		t.Fatalf("unexpected me body: %v", body)
	}

	// 未认证 401。
	resp, _ = e.do(t, "GET", "/v1/auth/me", "", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated me must be 401, got %d", resp.StatusCode)
	}
	// 错误口令 401。
	resp, _ = e.do(t, "POST", "/v1/auth/login", "", "", map[string]string{"username": "root@example.com", "password": "wrong-password-123"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password must be 401, got %d", resp.StatusCode)
	}
}

// 写操作缺 CSRF 头必须 403。
func TestWriteRequiresCSRF(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if _, err := e.svc.InitializeAdmin(ctx, "root@example.com", strongPassword); err != nil {
		t.Fatalf("init admin: %v", err)
	}
	token, _ := e.loginAs(t, "root@example.com", strongPassword)
	resp, _ := e.do(t, "POST", "/v1/admin/users", token, "", map[string]string{"username": "a@example.com", "password": "another-strong-pass1"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write without CSRF must be 403, got %d", resp.StatusCode)
	}
}

// 管理员建号返回一次性 MCP token 明文；普通用户访问管理接口必须 403。
func TestAdminUserManagementBoundary(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if _, err := e.svc.InitializeAdmin(ctx, "root@example.com", strongPassword); err != nil {
		t.Fatalf("init admin: %v", err)
	}
	adminToken, adminCSRF := e.loginAs(t, "root@example.com", strongPassword)

	resp, body := e.do(t, "POST", "/v1/admin/users", adminToken, adminCSRF, map[string]string{
		"username": "alice@example.com", "password": "initial-pass-123456",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user: status %d body %v", resp.StatusCode, body)
	}
	if body["mcp_token"] == "" || body["mcp_endpoint"] != "https://hub.example.com/mcp" {
		t.Fatalf("create user must return one-time mcp token and endpoint: %v", body)
	}
	userID, _ := body["user"].(map[string]any)["id"].(string)
	if userID == "" {
		t.Fatalf("created user must have id: %v", body)
	}
	// 新用户立即可用该 token。
	if _, err := e.svc.AuthenticateMCPToken(ctx, body["mcp_token"].(string)); err != nil {
		t.Fatalf("issued token must authenticate: %v", err)
	}

	// 用 alice 登录（首登强制改密语义由业务层保证），验证普通用户越权被拒。
	userToken, userCSRF := e.loginAs(t, "alice@example.com", "initial-pass-123456")
	resp, _ = e.do(t, "GET", "/v1/admin/users", userToken, userCSRF, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user hitting admin endpoint must be 403, got %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "POST", "/v1/admin/users/"+userID+"/disable", userToken, userCSRF, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user disabling others must be 403, got %d", resp.StatusCode)
	}
}

// MCP token 重置：旧 token 立即失效。
func TestResetMCPTokenImmediate(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin, err := e.svc.InitializeAdmin(ctx, "root@example.com", strongPassword)
	if err != nil {
		t.Fatalf("init admin: %v", err)
	}
	_, err = e.svc.CreateUser(ctx, admin.ID, "bob@example.com", "initial-pass-123456")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	oldToken, err := e.svc.IssueMCPToken(ctx, admin.ID)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	token, csrf := e.loginAs(t, "root@example.com", strongPassword)
	resp, body := e.do(t, "POST", "/v1/mcp-token/reset", token, csrf, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset token: status %d", resp.StatusCode)
	}
	newToken, _ := body["token"].(string)
	if newToken == "" || newToken == oldToken {
		t.Fatal("reset must return a fresh token")
	}
	if _, err := e.svc.AuthenticateMCPToken(ctx, oldToken); err == nil {
		t.Fatal("old token must be invalid immediately after reset")
	}
	if _, err := e.svc.AuthenticateMCPToken(ctx, newToken); err != nil {
		t.Fatalf("new token must work: %v", err)
	}
}

// 改密后所有会话失效，旧 Cookie 立即 401。
func TestChangePasswordInvalidatesSessions(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if _, err := e.svc.InitializeAdmin(ctx, "root@example.com", strongPassword); err != nil {
		t.Fatalf("init admin: %v", err)
	}
	token, csrf := e.loginAs(t, "root@example.com", strongPassword)
	resp, _ := e.do(t, "POST", "/v1/auth/password", token, csrf, map[string]string{
		"current_password": strongPassword, "new_password": "brand-new-passphrase1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("change password: status %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "GET", "/v1/auth/me", token, "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session must be revoked after password change, got %d", resp.StatusCode)
	}
}
