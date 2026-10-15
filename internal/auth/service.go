// Package auth 承载 AgentHub 的账号体系：管理员自举、登录限流、Web 会话、
// 用户管理与每用户 MCP Token。
//
// 安全模型（docs/DESIGN.md §6）：
//   - 口令用 argon2id（与 nexusdock 同参数），登录失败做账号+IP 双维度递增限流；
//   - 三类秘密（Web session、MCP token、device token）全部只存 SHA-256 哈希；
//   - 会话有绝对过期，不做永久会话；
//   - 登录失败时对未知用户也执行等价的 argon2 计算，抹平时序差异。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/uvwt/agenthub/internal/core"
)

const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonSaltBytes   = 16
	argonKeyBytes    = 32
)

var commonWeakPasswords = map[string]struct{}{
	"123456789012": {}, "password1234": {}, "qwerty123456": {},
	"admin12345678": {}, "letmein123456": {}, "agenthub12345": {},
}

// User 是账号的公开形态（不含任何秘密）。
type User struct {
	ID                 string    `json:"id"`
	Username           string    `json:"username"`
	DisplayName        string    `json:"display_name"`
	Role               string    `json:"role"`
	Status             string    `json:"status"`
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          time.Time `json:"created_at"`
}

// Session 是已认证的 Web 会话视图。
type Session struct {
	ID                 string
	UserID             string
	Username           string
	Role               string
	MustChangePassword bool
	AbsoluteExpiresAt  time.Time
}

// IssuedSession 是登录成功后一次性返回的明文 token。
type IssuedSession struct {
	Session Session
	Token   string
}

// RateLimitError 表示触发限流；调用方应映射为 429 + Retry-After。
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return "too many attempts, retry later" }

// Service 是账号体系的业务入口。
type Service struct {
	db  *sql.DB
	now func() time.Time
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// ---- 口令 ----

// HashPassword 生成 argon2id 编码口令。
func HashPassword(secret string) (string, error) {
	if secret == "" {
		return "", errors.New("password is required")
	}
	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(secret), salt, argonIterations, argonMemory, argonParallelism, argonKeyBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 常数时间校验 argon2id 口令，参数范围越界直接判负，
// 防止伪造的库记录把内存/迭代参数拉爆。
func VerifyPassword(secret, encoded string) bool {
	return strings.HasPrefix(encoded, "$argon2id$") && verifyArgon2(secret, encoded)
}

func verifyArgon2(secret, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	var memCost uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memCost, &iterations, &parallelism); err != nil {
		return false
	}
	if memCost < 8*1024 || memCost > 1024*1024 || iterations < 1 || iterations > 20 || parallelism < 1 || parallelism > 32 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, iterations, memCost, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ValidatePassword 口令策略：至少 12 字符、不等于用户名、非常见弱口令。
func ValidatePassword(username, secret string) error {
	if len([]rune(secret)) < 12 {
		return core.NewError(core.CodeValidation, "password must be at least 12 characters", nil)
	}
	if len(secret) > 1024 {
		return core.NewError(core.CodeValidation, "password is too long", nil)
	}
	normalized := strings.ToLower(strings.TrimSpace(secret))
	if normalized == strings.ToLower(strings.TrimSpace(username)) {
		return core.NewError(core.CodeValidation, "password must not match the username", nil)
	}
	if _, weak := commonWeakPasswords[normalized]; weak {
		return core.NewError(core.CodeValidation, "password is too common", nil)
	}
	return nil
}

// ValidateUsername 用户名采用邮箱形态：唯一、可展示。
func ValidateUsername(username string) error {
	username = strings.TrimSpace(username)
	at := strings.Index(username, "@")
	if at <= 0 || at == len(username)-1 || len(username) > 254 || strings.ContainsAny(username, " \t\r\n") {
		return core.NewError(core.CodeValidation, "username must be an email-like identifier", nil)
	}
	return nil
}

// ---- 管理员自举 ----

// AdminStatus 报告管理员是否已初始化。初始化判据是存在任何带凭证的用户。
func (s *Service) AdminStatus(ctx context.Context) (initialized bool, err error) {
	var count int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_credentials`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("read admin status: %w", err)
	}
	return count > 0, nil
}

// InitializeAdmin 创建第一个管理员；库中已存在任何凭证时拒绝。
// 后续管理员（如果需要）应走管理员用户管理接口，不走这条自举路径。
func (s *Service) InitializeAdmin(ctx context.Context, username, password string) (User, error) {
	initialized, err := s.AdminStatus(ctx)
	if err != nil {
		return User{}, err
	}
	if initialized {
		return User{}, core.NewError(core.CodeDBConflict, "administrator is already initialized", nil)
	}
	return s.createUser(ctx, createParams{
		username: username, password: password, role: "admin", mustChange: false,
	}, true)
}

type createParams struct {
	username string
	password string
	role     string
	// passwordHash 允许调用方直接传入已编码口令（重置密码场景复用校验逻辑时）。
	passwordHash string
	mustChange   bool
}

// createUser 在单事务内建用户+凭证；checkAdminCount 为 true 时先校验自举唯一性。
func (s *Service) createUser(ctx context.Context, params createParams, checkAdminCount bool) (User, error) {
	username := strings.TrimSpace(params.username)
	if err := ValidateUsername(username); err != nil {
		return User{}, err
	}
	encoded := params.passwordHash
	if encoded == "" {
		if err := ValidatePassword(username, params.password); err != nil {
			return User{}, err
		}
		hash, err := HashPassword(params.password)
		if err != nil {
			return User{}, err
		}
		encoded = hash
	}
	if params.role != "admin" && params.role != "user" {
		return User{}, core.NewError(core.CodeValidation, "invalid role", nil)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin create user: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if checkAdminCount {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_credentials`).Scan(&count); err != nil {
			return User{}, fmt.Errorf("check admin credentials: %w", err)
		}
		if count > 0 {
			return User{}, core.NewError(core.CodeDBConflict, "administrator is already initialized", nil)
		}
	}
	userID, err := core.NewID("usr")
	if err != nil {
		return User{}, err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id, username, display_name, role, status, must_change_password, created_at, updated_at)
		VALUES(?, ?, ?, ?, 'active', ?, ?, ?)`,
		userID, username, username, params.role, boolInt(params.mustChange), now, now); err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_credentials(user_id, password_hash, updated_at)
		VALUES(?, ?, ?)`, userID, encoded, now); err != nil {
		return User{}, fmt.Errorf("create user credentials: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit create user: %w", err)
	}
	s.recordAudit(ctx, "admin", "system", "auth.user.create", userID, "succeeded", map[string]any{"role": params.role})
	return User{
		ID: userID, Username: username, DisplayName: username, Role: params.role,
		Status: "active", MustChangePassword: params.mustChange,
	}, nil
}

// ---- 登录 ----

// Login 校验用户名口令并签发 Web 会话。
// 用户不存在时执行 dummy argon2 计算，使"用户不存在"与"口令错误"耗时一致。
func (s *Service) Login(ctx context.Context, username, password, ip string) (IssuedSession, error) {
	username = strings.TrimSpace(username)
	accountKey := core.HashToken(strings.ToLower(username))
	ipKey := core.HashToken(ip)
	if retry := s.throttleRemaining(ctx, accountKey, ipKey); retry > 0 {
		return IssuedSession{}, &RateLimitError{RetryAfter: retry}
	}
	var userID, storedUsername, displayName, role, status, encoded string
	var mustChange int
	err := s.db.QueryRowContext(ctx, `SELECT u.id, u.username, u.display_name, u.role, u.status,
		c.password_hash, u.must_change_password FROM users u
		JOIN user_credentials c ON c.user_id = u.id
		WHERE lower(u.username) = lower(?) LIMIT 1`, username).
		Scan(&userID, &storedUsername, &displayName, &role, &status, &encoded, &mustChange)
	valid := false
	if err == nil && status == "active" {
		valid = VerifyPassword(password, encoded)
	} else {
		_ = verifyArgon2(password, dummyArgonHash())
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return IssuedSession{}, fmt.Errorf("read user credentials: %w", err)
	}
	if !valid {
		retry := s.recordLoginFailure(ctx, accountKey, ipKey)
		s.recordAudit(ctx, "admin", "system", "auth.login", accountKey[:16], "failed", nil)
		if retry >= time.Minute {
			return IssuedSession{}, &RateLimitError{RetryAfter: retry}
		}
		// 口令正确但用户被禁用等场景同样走这里；返回统一错误避免泄露原因。
		return IssuedSession{}, core.NewError(core.CodeInvalidToken, "invalid username or password", nil)
	}
	s.clearLoginFailures(ctx, accountKey, ipKey)
	issued, err := s.createSession(ctx, userID, storedUsername, role, mustChange != 0)
	if err != nil {
		return IssuedSession{}, err
	}
	s.recordAudit(ctx, "user", userID, "auth.login", issued.Session.ID, "succeeded", nil)
	return issued, nil
}

func (s *Service) createSession(ctx context.Context, userID, username, role string, mustChange bool) (IssuedSession, error) {
	raw, err := core.NewSecret(32)
	if err != nil {
		return IssuedSession{}, err
	}
	id, err := core.NewID("ses")
	if err != nil {
		return IssuedSession{}, err
	}
	now := s.now().UTC()
	expiresAt := now.Add(24 * time.Hour)
	session := Session{
		ID: id, UserID: userID, Username: username, Role: role,
		MustChangePassword: mustChange, AbsoluteExpiresAt: expiresAt,
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO user_sessions(
		id, user_id, token_hash, created_at, absolute_expires_at
	) VALUES(?, ?, ?, ?, ?)`,
		session.ID, userID, core.HashToken(raw),
		now.Format(time.RFC3339Nano), expiresAt.Format(time.RFC3339Nano)); err != nil {
		return IssuedSession{}, fmt.Errorf("create web session: %w", err)
	}
	return IssuedSession{Session: session, Token: raw}, nil
}

// AuthenticateSession 用明文 token 还原会话；过期即吊销并拒绝。
func (s *Service) AuthenticateSession(ctx context.Context, raw string) (Session, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Session{}, core.NewError(core.CodeAuthRequired, "session is required", nil)
	}
	var session Session
	var expiresAt string
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT s.id, s.user_id, u.username, u.role,
		u.must_change_password, s.absolute_expires_at, s.revoked_at
		FROM user_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND u.status = 'active'`, core.HashToken(raw)).
		Scan(&session.ID, &session.UserID, &session.Username, &session.Role, &session.MustChangePassword, &expiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, core.NewError(core.CodeInvalidToken, "session is invalid", nil)
	}
	if err != nil {
		return Session{}, fmt.Errorf("lookup web session: %w", err)
	}
	if revoked.Valid {
		return Session{}, core.NewError(core.CodeTokenRevoked, "session has been revoked", nil)
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return Session{}, fmt.Errorf("parse session expiry: %w", err)
	}
	session.AbsoluteExpiresAt = expires
	if !s.now().UTC().Before(expires) {
		_, _ = s.db.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
			s.now().UTC().Format(time.RFC3339Nano), session.ID)
		return Session{}, core.NewError(core.CodeInvalidToken, "session has expired", nil)
	}
	return session, nil
}

// Logout 吊销当前会话。
func (s *Service) Logout(ctx context.Context, userID, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = ?
		WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		s.now().UTC().Format(time.RFC3339Nano), sessionID, userID)
	if err != nil {
		return fmt.Errorf("revoke web session: %w", err)
	}
	return nil
}

// ChangePassword 校验旧口令后改密；成功后吊销该用户全部会话，
// 并清除强制改密标记。返回新登录所需信息由调用方重新走 Login。
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT c.password_hash FROM user_credentials c WHERE c.user_id = ?`, userID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return core.NewError(core.CodeNotFound, "user not found", nil)
	}
	if err != nil {
		return fmt.Errorf("read user credentials: %w", err)
	}
	if !VerifyPassword(currentPassword, encoded) {
		return core.NewError(core.CodeInvalidToken, "current password is incorrect", nil)
	}
	var username string
	if err := s.db.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, userID).Scan(&username); err != nil {
		return fmt.Errorf("read user: %w", err)
	}
	if err := ValidatePassword(username, newPassword); err != nil {
		return err
	}
	newEncoded, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin change password: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE user_credentials SET password_hash = ?, updated_at = ? WHERE user_id = ?`,
		newEncoded, now, userID); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET must_change_password = 0, updated_at = ? WHERE id = ?`, now, userID); err != nil {
		return fmt.Errorf("clear must-change flag: %w", err)
	}
	// 改密后全部会话失效：被盗会话不能借改密操作幸存。
	if _, err := tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, now, userID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit change password: %w", err)
	}
	s.recordAudit(ctx, "user", userID, "auth.password.change", userID, "succeeded", nil)
	return nil
}

// ---- 用户管理（管理员） ----

// CreateUser 管理员创建普通用户；初始口令由管理员设置，首登强制改密。
func (s *Service) CreateUser(ctx context.Context, adminID, username, password string) (User, error) {
	return s.createUser(ctx, createParams{
		username: username, password: password, role: "user", mustChange: true,
	}, false)
}

// DisableUser 禁用用户。行级状态变化即生效：所有认证查询都带
// status='active' 条件，禁用后 session/MCP token 立即 401，
// 节点连接在下一帧被 Bridge 拒绝（connect 查询同样校验用户状态）。
func (s *Service) DisableUser(ctx context.Context, adminID, userID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE users SET status = 'disabled', updated_at = ?
		WHERE id = ? AND status = 'active'`, s.now().UTC().Format(time.RFC3339Nano), userID)
	if err != nil {
		return fmt.Errorf("disable user: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return core.NewError(core.CodeNotFound, "active user not found", nil)
	}
	s.recordAudit(ctx, "admin", adminID, "auth.user.disable", userID, "succeeded", nil)
	return nil
}

// EnableUser 恢复用户。
func (s *Service) EnableUser(ctx context.Context, adminID, userID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE users SET status = 'active', updated_at = ?
		WHERE id = ? AND status = 'disabled'`, s.now().UTC().Format(time.RFC3339Nano), userID)
	if err != nil {
		return fmt.Errorf("enable user: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return core.NewError(core.CodeNotFound, "disabled user not found", nil)
	}
	s.recordAudit(ctx, "admin", adminID, "auth.user.enable", userID, "succeeded", nil)
	return nil
}

// ResetUserPassword 管理员重置用户口令，首登强制改密并吊销全部会话。
func (s *Service) ResetUserPassword(ctx context.Context, adminID, userID, newPassword string) error {
	var username string
	if err := s.db.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, userID).Scan(&username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.NewError(core.CodeNotFound, "user not found", nil)
		}
		return fmt.Errorf("read user: %w", err)
	}
	if err := ValidatePassword(username, newPassword); err != nil {
		return err
	}
	encoded, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reset password: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE user_credentials SET password_hash = ?, updated_at = ? WHERE user_id = ?`,
		encoded, now, userID); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET must_change_password = 1, updated_at = ? WHERE id = ?`, now, userID); err != nil {
		return fmt.Errorf("set must-change flag: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, now, userID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reset password: %w", err)
	}
	s.recordAudit(ctx, "admin", adminID, "auth.user.reset_password", userID, "succeeded", nil)
	return nil
}

// ListUsers 返回用户列表（管理页）。
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, display_name, role, status,
		must_change_password, created_at FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var result []User
	for rows.Next() {
		var user User
		var mustChange int
		var created string
		if err := rows.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &user.Status, &mustChange, &created); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		user.MustChangePassword = mustChange != 0
		user.CreatedAt = parseTimeOrZero(created)
		result = append(result, user)
	}
	return result, rows.Err()
}

// ---- MCP Token ----

// MCPTokenInfo 是 MCP token 的非秘密视图。
type MCPTokenInfo struct {
	Token     string    `json:"token,omitempty"` // 仅 Issued 返回
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EnsureMCPToken 幂等保证用户拥有 MCP token：不存在则签发并返回明文，
// 已存在则返回空 Token（明文只在签发时可见一次）。
func (s *Service) EnsureMCPToken(ctx context.Context, userID string) (MCPTokenInfo, error) {
	existing, err := s.lookupMCPToken(ctx, userID)
	if err != nil {
		return MCPTokenInfo{}, err
	}
	if !existing.UpdatedAt.IsZero() {
		return existing, nil
	}
	raw, err := s.IssueMCPToken(ctx, userID)
	if err != nil {
		return MCPTokenInfo{}, err
	}
	return MCPTokenInfo{Token: raw, CreatedAt: s.now().UTC(), UpdatedAt: s.now().UTC()}, nil
}

// lookupMCPToken 查询；不存在时返回零值 UpdatedAt 表示需要生成。
func (s *Service) lookupMCPToken(ctx context.Context, userID string) (MCPTokenInfo, error) {
	var created, updated string
	var info MCPTokenInfo
	err := s.db.QueryRowContext(ctx, `SELECT created_at, updated_at FROM user_mcp_tokens WHERE user_id = ?`, userID).
		Scan(&created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return MCPTokenInfo{}, nil
	}
	if err != nil {
		return MCPTokenInfo{}, fmt.Errorf("read mcp token: %w", err)
	}
	info.CreatedAt = parseTimeOrZero(created)
	info.UpdatedAt = parseTimeOrZero(updated)
	return info, nil
}

// IssueMCPToken 生成新的 MCP token 明文（同时落哈希）。用于首次签发与重置。
// 重置语义：旧 token 行被替换，旧 token 立即失效。
func (s *Service) IssueMCPToken(ctx context.Context, userID string) (string, error) {
	raw, err := core.NewSecret(32)
	if err != nil {
		return "", err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO user_mcp_tokens(id, user_id, token_hash, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET token_hash = excluded.token_hash, updated_at = excluded.updated_at`,
		"mct_"+core.HashToken(raw)[:16], userID, core.HashToken(raw), now, now); err != nil {
		return "", fmt.Errorf("issue mcp token: %w", err)
	}
	s.recordAudit(ctx, "user", userID, "auth.mcp_token.issue", userID, "succeeded", nil)
	return raw, nil
}

// AuthenticateMCPToken 把 /mcp 的 Bearer token 解析成用户。
// 禁用用户的 token 一律无效（JOIN users 过滤 status）。
func (s *Service) AuthenticateMCPToken(ctx context.Context, raw string) (User, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return User{}, core.NewError(core.CodeAuthRequired, "mcp token is required", nil)
	}
	var user User
	err := s.db.QueryRowContext(ctx, `SELECT u.id, u.username, u.display_name, u.role
		FROM user_mcp_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ? AND u.status = 'active'`, core.HashToken(raw)).
		Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, core.NewError(core.CodeInvalidToken, "mcp token is invalid", nil)
	}
	if err != nil {
		return User{}, fmt.Errorf("authenticate mcp token: %w", err)
	}
	return user, nil
}

// ---- 限流 ----

// throttleRemaining 返回账号/IP 两个节流键里剩余封锁时间最长者。
func (s *Service) throttleRemaining(ctx context.Context, accountKey, ipKey string) time.Duration {
	var longest time.Duration
	for _, key := range [][2]string{{"account", accountKey}, {"ip", ipKey}} {
		var blocked sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT blocked_until FROM login_throttles WHERE key_type = ? AND key_value = ?`, key[0], key[1]).Scan(&blocked)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			continue
		}
		if blocked.Valid {
			until, err := time.Parse(time.RFC3339Nano, blocked.String)
			if err != nil {
				continue
			}
			if remaining := until.Sub(s.now().UTC()); remaining > longest {
				longest = remaining
			}
		}
	}
	return longest
}

// recordLoginFailure 累加账号+IP 失败计数并按阶梯延长封锁，返回最长封锁时长。
func (s *Service) recordLoginFailure(ctx context.Context, accountKey, ipKey string) time.Duration {
	var longest time.Duration
	for _, key := range [][2]string{{"account", accountKey}, {"ip", ipKey}} {
		var failures int
		err := s.db.QueryRowContext(ctx, `SELECT failures FROM login_throttles WHERE key_type = ? AND key_value = ?`, key[0], key[1]).Scan(&failures)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			continue
		}
		failures++
		delay := loginDelay(failures)
		if delay > longest {
			longest = delay
		}
		now := s.now().UTC()
		_, err = s.db.ExecContext(ctx, `INSERT INTO login_throttles(key_type, key_value, failures, blocked_until, last_failed_at)
			VALUES(?, ?, ?, ?, ?)
			ON CONFLICT(key_type, key_value) DO UPDATE SET failures = excluded.failures,
			blocked_until = excluded.blocked_until, last_failed_at = excluded.last_failed_at`,
			key[0], key[1], failures, now.Add(delay).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		if err != nil {
			continue
		}
	}
	return longest
}

// loginDelay 失败次数递增的封锁时长，10 次后稳定 15 分钟。
func loginDelay(failures int) time.Duration {
	switch {
	case failures >= 10:
		return 15 * time.Minute
	case failures >= 7:
		return 5 * time.Minute
	case failures >= 6:
		return 2 * time.Minute
	case failures >= 5:
		return time.Minute
	default:
		shift := failures - 1
		if shift < 0 {
			shift = 0
		}
		return time.Duration(1<<uint(shift)) * 250 * time.Millisecond
	}
}

func (s *Service) clearLoginFailures(ctx context.Context, accountKey, ipKey string) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM login_throttles WHERE (key_type = 'account' AND key_value = ?) OR (key_type = 'ip' AND key_value = ?)`, accountKey, ipKey)
}

// ---- 审计 ----

func (s *Service) recordAudit(ctx context.Context, actorType, actorID, action, objectID, result string, metadata map[string]any) {
	metadataJSON := "{}"
	if metadata != nil {
		if raw, err := json.Marshal(metadata); err == nil {
			metadataJSON = string(raw)
		}
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit_events(
		id, occurred_at, actor_type, actor_id, action, object_type, object_id, result, metadata_json
	) VALUES(?, ?, ?, ?, ?, 'account', ?, ?, ?)`,
		mustID(), s.now().UTC().Format(time.RFC3339Nano), actorType, actorID, action, objectID, result, metadataJSON)
}

func mustID() string {
	id, err := core.NewID("evt")
	if err != nil {
		// 随机数失败极罕见；审计失败不应拖垮主流程，退化为时间戳 ID。
		return "evt_" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return id
}

// CSRFTokenFor 从会话 ID 派生 CSRF token。会话 Cookie 是 HttpOnly 的，
// 派生值只经登录响应下发一次，跨站脚本之外的伪造成本等于重算会话 ID。
func CSRFTokenFor(sessionID string) string {
	sum := sha256.Sum256([]byte("csrf:" + sessionID))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---- 小工具 ----

func parseTimeOrZero(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func dummyArgonHash() string {
	salt := []byte("agenthub-dummy-s")
	key := argon2.IDKey([]byte("not-the-password"), salt, argonIterations, argonMemory, argonParallelism, argonKeyBytes)
	return "$argon2id$v=19$m=" + strconv.Itoa(argonMemory) + ",t=" + strconv.Itoa(argonIterations) + ",p=" + strconv.Itoa(argonParallelism) + "$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key)
}
