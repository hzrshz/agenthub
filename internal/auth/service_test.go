package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uvwt/agenthub/internal/core"
)

func newTestService(t *testing.T) *Service {
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
	return NewService(db)
}

const strongPassword = "correct-horse-battery"

// 管理员自举：首次成功，二次必须拒绝——防止公网入口被用来再建管理员。
func TestInitializeAdminOnceOnly(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	initialized, err := svc.AdminStatus(ctx)
	if err != nil || initialized {
		t.Fatalf("fresh service must be uninitialized: %v", err)
	}
	if _, err := svc.InitializeAdmin(ctx, "root@example.com", strongPassword); err != nil {
		t.Fatalf("initialize admin: %v", err)
	}
	initialized, err = svc.AdminStatus(ctx)
	if err != nil || !initialized {
		t.Fatalf("admin must be initialized: %v", err)
	}
	if _, err := svc.InitializeAdmin(ctx, "evil@example.com", strongPassword); err == nil {
		t.Fatal("second initialization must fail")
	}
}

// 登录成功签发可用会话；错误口令拒绝并累积限流。
func TestLoginAndSession(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.InitializeAdmin(ctx, "root@example.com", strongPassword); err != nil {
		t.Fatalf("initialize admin: %v", err)
	}

	issued, err := svc.Login(ctx, "root@example.com", strongPassword, "203.0.113.1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	session, err := svc.AuthenticateSession(ctx, issued.Token)
	if err != nil {
		t.Fatalf("authenticate session: %v", err)
	}
	if session.Username != "root@example.com" || session.Role != "admin" {
		t.Fatalf("unexpected session identity: %+v", session)
	}
	if session.MustChangePassword {
		t.Fatal("admin must not be forced to change password")
	}

	if _, err := svc.Login(ctx, "root@example.com", "wrong-password-123", "203.0.113.1"); err == nil {
		t.Fatal("wrong password must fail")
	}
}

// 限流：验证失败的封锁窗口内，正确口令同样被拒——防爆破的语义核心。
// 阶梯递增（250ms→15min）单独验证：连发尝试会被短窗口拦在入口，
// 计数只在窗口过期后的验证失败时累加，因此阶梯走 recordLoginFailure 直接驱动。
func TestLoginThrottleBlocksEvenCorrectPassword(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.InitializeAdmin(ctx, "root@example.com", strongPassword); err != nil {
		t.Fatalf("initialize admin: %v", err)
	}
	if _, err := svc.Login(ctx, "root@example.com", "wrong-password-123", "203.0.113.9"); err == nil {
		t.Fatal("wrong password must fail")
	}
	// 阶梯递增：连续失败最终到达长封锁档位。
	// 直接驱动 recordLoginFailure 验证阶梯；不走 Login+真实 argon2，
	// 因为 250ms 级短窗口在慢 CI（QEMU/共享 runner）上不可依赖真实时钟。
	var longest time.Duration
	for i := 2; i <= 7; i++ {
		if got := svc.recordLoginFailure(ctx, core.HashToken(strings.ToLower("root@example.com")), core.HashToken("203.0.113.9")); got > longest {
			longest = got
		}
	}
	if longest < 5*time.Minute {
		t.Fatalf("7 failures must reach 5-minute tier, got %v", longest)
	}
	// 封锁窗口内正确口令也必须被拒（此时 blocked_until 在 5 分钟后，无时钟竞态）。
	_, err := svc.Login(ctx, "root@example.com", strongPassword, "203.0.113.9")
	var rate *RateLimitError
	if !errors.As(err, &rate) {
		t.Fatalf("correct password must be blocked inside throttle window, got %v", err)
	}
}

// 不存在的用户登录耗时与存在用户一致（dummy argon2），此处只验证行为等价拒绝。
func TestLoginUnknownUser(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	_, err := svc.Login(ctx, "ghost@example.com", strongPassword, "203.0.113.2")
	if err == nil || core.ErrorCodeOf(err) != core.CodeInvalidToken {
		t.Fatalf("unknown user must return invalid token error, got %v", err)
	}
}

// 改密后旧口令失效、全部旧会话吊销、强制改密标记清除。
func TestChangePasswordRevokesSessions(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.InitializeAdmin(ctx, "root@example.com", strongPassword); err != nil {
		t.Fatalf("initialize admin: %v", err)
	}
	issued, err := svc.Login(ctx, "root@example.com", strongPassword, "203.0.113.1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := svc.ChangePassword(ctx, issued.Session.UserID, strongPassword, "newer-stronger-passphrase"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, err := svc.AuthenticateSession(ctx, issued.Token); err == nil {
		t.Fatal("session must be revoked after password change")
	}
	reissued, err := svc.Login(ctx, "root@example.com", "newer-stronger-passphrase", "203.0.113.1")
	if err != nil {
		t.Fatalf("login with new password: %v", err)
	}
	if reissued.Session.MustChangePassword {
		t.Fatal("must-change flag must be cleared after self change")
	}
	if _, err := svc.Login(ctx, "root@example.com", strongPassword, "203.0.113.1"); err == nil {
		t.Fatal("old password must stop working")
	}
}

// 管理员建普通用户：首登强制改密；禁用后 MCP token 与会话立即失效。
func TestCreateDisableUserAndMCPToken(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	admin, err := svc.InitializeAdmin(ctx, "root@example.com", strongPassword)
	if err != nil {
		t.Fatalf("initialize admin: %v", err)
	}

	user, err := svc.CreateUser(ctx, admin.ID, "alice@example.com", "initial-pass-123456")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if !user.MustChangePassword || user.Role != "user" {
		t.Fatalf("new user must be role=user with must-change: %+v", user)
	}

	token, err := svc.IssueMCPToken(ctx, user.ID)
	if err != nil {
		t.Fatalf("issue mcp token: %v", err)
	}
	identified, err := svc.AuthenticateMCPToken(ctx, token)
	if err != nil {
		t.Fatalf("authenticate mcp token: %v", err)
	}
	if identified.ID != user.ID {
		t.Fatalf("token must identify its owner, got %+v", identified)
	}

	// 重置后旧 token 立即失效。
	newToken, err := svc.IssueMCPToken(ctx, user.ID)
	if err != nil {
		t.Fatalf("reissue mcp token: %v", err)
	}
	if _, err := svc.AuthenticateMCPToken(ctx, token); err == nil {
		t.Fatal("old mcp token must be invalid after reissue")
	}
	if _, err := svc.AuthenticateMCPToken(ctx, newToken); err != nil {
		t.Fatalf("new mcp token must work: %v", err)
	}

	// 禁用后所有凭证路径立即拒绝。
	if err := svc.DisableUser(ctx, admin.ID, user.ID); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := svc.AuthenticateMCPToken(ctx, newToken); err == nil {
		t.Fatal("disabled user's mcp token must be rejected")
	}
	if _, err := svc.Login(ctx, "alice@example.com", "initial-pass-123456", "203.0.113.3"); err == nil {
		t.Fatal("disabled user must not log in")
	}
	if err := svc.EnableUser(ctx, admin.ID, user.ID); err != nil {
		t.Fatalf("enable user: %v", err)
	}
	if _, err := svc.AuthenticateMCPToken(ctx, newToken); err != nil {
		t.Fatalf("re-enabled user's token must work again: %v", err)
	}
}

// EnsureMCPToken 幂等：第二次调用不得轮换 token（否则平台侧配置会静默失效）。
func TestEnsureMCPTokenIdempotent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	admin, err := svc.InitializeAdmin(ctx, "root@example.com", strongPassword)
	if err != nil {
		t.Fatalf("initialize admin: %v", err)
	}
	first, err := svc.EnsureMCPToken(ctx, admin.ID)
	if err != nil || first.Token == "" {
		t.Fatalf("first ensure must issue token: %v", err)
	}
	second, err := svc.EnsureMCPToken(ctx, admin.ID)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if second.Token != "" {
		t.Fatal("existing token must not be re-issued in plaintext")
	}
	if _, err := svc.AuthenticateMCPToken(ctx, first.Token); err != nil {
		t.Fatalf("original token must remain valid: %v", err)
	}
}

// 口令策略：弱口令、过短、等于用户名全部拒绝。
func TestPasswordPolicy(t *testing.T) {
	cases := []struct{ name, username, password string }{
		{"too short", "u@example.com", "short-pass"},
		{"equals username", "u@example.com", "u@example.com"},
		{"common", "u@example.com", "password1234"},
	}
	for _, tc := range cases {
		if err := ValidatePassword(tc.username, tc.password); err == nil {
			t.Fatalf("%s must be rejected", tc.name)
		}
	}
	if err := ValidatePassword("u@example.com", "a-long-enough-passphrase"); err != nil {
		t.Fatalf("valid password rejected: %v", err)
	}
}

// 用户名形态校验。
func TestUsernameValidation(t *testing.T) {
	if err := ValidateUsername("alice@example.com"); err != nil {
		t.Fatalf("valid username rejected: %v", err)
	}
	for _, bad := range []string{"", "no-at-sign", "@example.com", "a b@example.com", strings.Repeat("x", 300) + "@e.com"} {
		if err := ValidateUsername(bad); err == nil {
			t.Fatalf("username %q must be rejected", bad)
		}
	}
}
