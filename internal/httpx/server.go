// Package httpx 承载 AgentHub 的 HTTP 边界：会话 Cookie 认证、
// 管理员用户管理、用户 MCP Token 与节点管理 API、MCP 与 Bridge 入口挂载。
// 动态 JSON 只在此层解码，进入业务前转成明确参数。
package httpx

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/audit"
	"github.com/uvwt/agenthub/internal/auth"
	"github.com/uvwt/agenthub/internal/bridge"
	"github.com/uvwt/agenthub/internal/config"
	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/nodes"
)

const (
	sessionCookieName = "agenthub_session"
	// csrfHeaderName Web 写操作的 CSRF 头。CSRF token 由前端从登录响应获取。
	csrfHeaderName = "X-AgentHub-CSRF"
)

// FleetReconciler 是节点/用户状态变更后触发 fleet 契约重算的最小接口。
// 由 fleet.Bridge 实现；测试可注入 nil 跳过。
type FleetReconciler interface {
	// ReconcileNode 节点启停/删除后重算该节点涉及的工具。
	ReconcileNode(nodeID string)
	// ReconcileOwner 用户级变更（如禁用）后重算该用户的全部工具。
	ReconcileOwner(ownerUserID string)
}

// Server 是 HTTP 层的依赖容器。
type Server struct {
	auth        *auth.Service
	nodes       *nodes.Store
	hub         *bridge.Hub
	gateway     MCPGateway
	invokes     InvokeLister
	fleet       FleetReconciler
	pairLimiter *pairRateLimiter
	public      string // PublicURL，用于给用户展示 MCP endpoint
	// trustedProxies 决定是否采信 X-Forwarded-For；nil 或空 = 一律不采信。
	trustedProxies *config.Resolver
	mcpAuthLimiter *mcpAuthRateLimiter
	log            *slog.Logger
	mux            *http.ServeMux
}

// logger 返回 Server 的日志器，未注入时回退到 slog.Default（测试常用）。
func (s *Server) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// MCPGateway 是 /mcp 挂载所需的最小网关接口（避免 httpx 反向依赖 mcpgw）。
type MCPGateway interface {
	Handler() http.Handler
}

// InvokeLister 是调用记录查询的最小接口。
type InvokeLister interface {
	ListByUser(ctx context.Context, query audit.InvokeQuery) (audit.InvokePage, error)
	ListAll(ctx context.Context, query audit.InvokeQuery) (audit.InvokePage, error)
	StatsSince(ctx context.Context, since time.Time) ([]audit.UserStats, error)
}

func NewServer(authService *auth.Service, nodeStore *nodes.Store, hub *bridge.Hub, gateway MCPGateway, invokes InvokeLister, publicURL string) *Server {
	return NewServerWithProxies(authService, nodeStore, hub, gateway, invokes, publicURL, nil)
}

// NewServerWithProxies 与 NewServer 相同，但显式指定可信反代网段解析器。
// trustedProxies 决定是否采信 X-Forwarded-For（限流键的来源）。
func NewServerWithProxies(authService *auth.Service, nodeStore *nodes.Store, hub *bridge.Hub, gateway MCPGateway, invokes InvokeLister, publicURL string, trustedProxies *config.Resolver) *Server {
	s := newServer(authService, nodeStore, hub, gateway, invokes, publicURL, trustedProxies)
	s.routes()
	return s
}

// NewServerWithFleet 是生产装配入口：额外注入 fleet 契约重算器。
func NewServerWithFleet(authService *auth.Service, nodeStore *nodes.Store, hub *bridge.Hub, gateway MCPGateway, invokes InvokeLister, publicURL string, trustedProxies *config.Resolver, fleetBridge FleetReconciler) *Server {
	s := newServer(authService, nodeStore, hub, gateway, invokes, publicURL, trustedProxies)
	s.fleet = fleetBridge
	s.routes()
	return s
}

func newServer(authService *auth.Service, nodeStore *nodes.Store, hub *bridge.Hub, gateway MCPGateway, invokes InvokeLister, publicURL string, trustedProxies *config.Resolver) *Server {
	return &Server{auth: authService, nodes: nodeStore, hub: hub, gateway: gateway, invokes: invokes, pairLimiter: newPairRateLimiter(), public: publicURL, trustedProxies: trustedProxies, mcpAuthLimiter: newMCPAuthRateLimiter(), log: slog.Default(), mux: http.NewServeMux()}
}

// SetLogger 注入结构化日志器（DESIGN §7 要求认证失败与节点拒绝都有日志）。
func (s *Server) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.log = logger
	}
}

// Handler 返回最终处理器。安全响应头在最外层包，这样连"未匹配到任何路由"
// 的 404 响应也带全套头——扫描器不会区分你返回的是 200 还是 404。
// 来源 IP 也在这里注入上下文：后续所有审计记录（登录、改密、管理操作）
// 都能自动带上来源 IP，无需逐个业务函数传参。
func (s *Server) Handler() http.Handler {
	return securityHeaders(s.trustedProxies, auditIPInjector(s.clientIP, s.mux))
}

// auditIPInjector 把请求来源 IP 放进上下文供审计使用。
func auditIPInjector(clientIP func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.WithAuditIP(r.Context(), clientIP(r))))
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// 认证
	s.mux.HandleFunc("POST /v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /v1/auth/logout", s.requireSession(s.handleLogout))
	s.mux.HandleFunc("POST /v1/auth/password", s.requireSession(s.handleChangePassword))
	s.mux.HandleFunc("GET /v1/auth/me", s.requireSession(s.handleMe))

	// 管理员：用户管理
	s.mux.HandleFunc("GET /v1/admin/users", s.requireAdmin(s.handleAdminOnlyReady(s.handleListUsers)))
	s.mux.HandleFunc("POST /v1/admin/users", s.requireAdmin(s.handleAdminOnlyReady(s.handleCreateUser)))
	s.mux.HandleFunc("POST /v1/admin/users/{id}/disable", s.requireAdmin(s.handleAdminOnlyReady(s.handleDisableUser)))
	s.mux.HandleFunc("POST /v1/admin/users/{id}/enable", s.requireAdmin(s.handleAdminOnlyReady(s.handleEnableUser)))
	s.mux.HandleFunc("POST /v1/admin/users/{id}/reset-password", s.requireAdmin(s.handleAdminOnlyReady(s.handleResetPassword)))
	s.mux.HandleFunc("GET /v1/admin/invokes", s.requireAdmin(s.handleAdminOnlyReady(s.handleAdminInvokes)))
	s.mux.HandleFunc("GET /v1/admin/stats", s.requireAdmin(s.handleAdminOnlyReady(s.handleAdminStats)))

	// 用户：MCP Token
	s.mux.HandleFunc("GET /v1/mcp-token", s.requireSession(s.requireReadyForUser(s.handleGetMCPToken)))
	s.mux.HandleFunc("POST /v1/mcp-token/reset", s.requireSession(s.requireReadyForUser(s.handleResetMCPToken)))

	// 用户：调用记录
	s.mux.HandleFunc("GET /v1/invokes", s.requireSession(s.requireReadyForUser(s.handleListInvokes)))

	// 用户：节点管理
	s.mux.HandleFunc("GET /v1/nodes", s.requireSession(s.requireReadyForUser(s.handleListNodes)))
	s.mux.HandleFunc("POST /v1/nodes/pairing-codes", s.requireSession(s.requireReadyForUser(s.handleCreatePairingCode)))
	s.mux.HandleFunc("POST /v1/nodes/{id}/disable", s.requireSession(s.requireReadyForUser(s.handleDisableNode)))
	s.mux.HandleFunc("POST /v1/nodes/{id}/enable", s.requireSession(s.requireReadyForUser(s.handleEnableNode)))
	s.mux.HandleFunc("DELETE /v1/nodes/{id}", s.requireSession(s.requireReadyForUser(s.handleDeleteNode)))

	// 节点侧（AgentDock 出站，无会话，配对码/device token 认证）
	s.mux.HandleFunc("POST "+protocol.PairPath, s.pairLimiter.middleware(s.clientIP, s.handleNodePair))
	s.mux.HandleFunc("GET "+protocol.ConnectPath, s.handleNodeConnect)

	// 平台侧 MCP（Bearer MCP Token 即身份，网关内部完成认证与限流）
	if s.gateway != nil {
		// 外层包一层认证失败限流：网关内的每用户速率限制只在认证成功后生效，
		// 无效 token 的 401 必须另有按 IP 的上限，否则可无成本爆破 token。
		s.mux.Handle("/mcp", s.mcpAuthLimiter.middleware(s.clientIP, s.gateway.Handler().ServeHTTP))
	}

	// Web 界面（构建产物嵌入；API 路由优先）。
	s.mux.Handle("/", webHandler())
}

// ---- 认证辅助 ----

type sessionKey struct{}

// sessionFromContext 取出 requireSession 放入的会话。
func sessionFromContext(ctx context.Context) (auth.Session, bool) {
	session, ok := ctx.Value(sessionKey{}).(auth.Session)
	return session, ok
}

func (s *Server) sessionCookie(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// requireSession 包裹需要登录的处理器；未认证统一 401。
func (s *Server) requireSession(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := s.auth.AuthenticateSession(r.Context(), s.sessionCookie(r))
		if err != nil {
			// 认证失败留痕：会话 cookie 被盗用/爆破在这里露头。
			s.logger().Warn("会话认证失败", "path", r.URL.Path, "remote", r.RemoteAddr, "error", err)
			writeAuthError(w, err)
			return
		}
		// 写操作要求 CSRF 头与会话绑定校验：Cookie 是 SameSite=Lax，
		// 双保险防跨站写。
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.csrfValid(r, session.ID) {
				s.logger().Warn("CSRF 校验失败", "path", r.URL.Path, "method", r.Method, "user", session.UserID)
				writeError(w, http.StatusForbidden, "CSRF_REQUIRED", "csrf header is required")
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, session)))
	}
}

func (s *Server) csrfValid(r *http.Request, sessionID string) bool {
	token := r.Header.Get(csrfHeaderName)
	if token == "" {
		return false
	}
	// CSRF token 直接派生自会话 token 哈希 + 会话 ID；此处对比头值与
	// 客户端无法从 Cookie 推导的派生值（由登录响应下发）。
	expected := deriveCSRFToken(sessionID)
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func deriveCSRFToken(sessionID string) string {
	// 登录时会话 ID 参与派生即可满足"客户端不能自行伪造"；
	// token 本体不出现在 Cookie 之外的任何位置。
	return auth.CSRFTokenFor(sessionID)
}

// requireAdmin 包裹管理员专用处理器。
func (s *Server) requireAdmin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		session, _ := sessionFromContext(r.Context())
		if session.Role != "admin" {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "administrator role is required")
			return
		}
		next(w, r)
	})
}

// clientIP 返回限流与审计使用的客户端 IP，语义见 config.ClientIP：
// 只有可信反代来源才采信 X-Forwarded-For，默认不采信，避免客户端伪造绕过限流。
func (s *Server) clientIP(r *http.Request) string {
	return config.ClientIP(r, s.trustedProxies)
}

// ---- 错误输出 ----

func statusForCode(code core.ErrorCode) int {
	switch code {
	case core.CodeAuthRequired, core.CodeInvalidToken, core.CodeTokenRevoked:
		return http.StatusUnauthorized
	case core.CodeForbidden:
		return http.StatusForbidden
	case core.CodeNotFound:
		return http.StatusNotFound
	case core.CodeValidation:
		return http.StatusBadRequest
	case core.CodeDBConflict, core.CodeVersionConflict:
		return http.StatusConflict
	case core.CodeRateLimited, core.CodeTooManyRequests:
		return http.StatusTooManyRequests
	case core.CodeNodeOffline:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

// writeAuthError 统一认证错误；401 携带 WWW-Authenticate（MCP 规范要求）。
func writeAuthError(w http.ResponseWriter, err error) {
	code := core.ErrorCodeOf(err)
	status := statusForCode(code)
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agenthub"`)
	}
	writeError(w, status, string(code), safeMessage(err))
}

// safeMessage 只透出业务错误消息，内部包装错误不外泄细节。
func safeMessage(err error) string {
	var coded *core.CodedError
	if errors.As(err, &coded) {
		return coded.Message
	}
	return "internal error"
}

// ---- JSON 输入输出 ----

func decodeJSON[T any](r *http.Request) (T, error) {
	var value T
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(&value); err != nil {
		return value, core.NewError(core.CodeValidation, "invalid JSON body", err)
	}
	return value, nil
}

func writeJSON[T any](w http.ResponseWriter, status int, value T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// ---- 处理器：认证 ----

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	SessionID          string `json:"session_id"`
	UserID             string `json:"user_id"`
	Username           string `json:"username"`
	Role               string `json:"role"`
	MustChangePassword bool   `json:"must_change_password"`
	CSRFToken          string `json:"csrf_token"`
	ExpiresAt          string `json:"expires_at"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	req, err := decodeJSON[loginRequest](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid login body")
		return
	}
	ip := s.clientIP(r)
	issued, err := s.auth.Login(r.Context(), req.Username, req.Password, ip)
	if err != nil {
		var rate *auth.RateLimitError
		if errors.As(err, &rate) {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(rate.RetryAfter.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many login attempts")
			return
		}
		writeAuthError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    issued.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// TLS 由反代终结；X-Forwarded-Proto 存在时浏览器侧必为 https。
		Secure:  true,
		Expires: issued.Session.AbsoluteExpiresAt,
	})
	writeJSON(w, http.StatusOK, loginResponse{
		SessionID:          issued.Session.ID,
		UserID:             issued.Session.UserID,
		Username:           issued.Session.Username,
		Role:               issued.Session.Role,
		MustChangePassword: issued.Session.MustChangePassword,
		CSRFToken:          deriveCSRFToken(issued.Session.ID),
		ExpiresAt:          issued.Session.AbsoluteExpiresAt.Format(time.RFC3339),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	if err := s.auth.Logout(r.Context(), session.UserID, session.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "logout failed")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	req, err := decodeJSON[changePasswordRequest](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid body")
		return
	}
	if err := s.auth.ChangePassword(r.Context(), session.UserID, req.CurrentPassword, req.NewPassword); err != nil {
		writeAuthError(w, err)
		return
	}
	// 改密后全部会话已吊销，前端应回到登录页。
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type meResponse struct {
	UserID             string `json:"user_id"`
	Username           string `json:"username"`
	Role               string `json:"role"`
	MustChangePassword bool   `json:"must_change_password"`
	PublicURL          string `json:"public_url,omitempty"`
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	writeJSON(w, http.StatusOK, meResponse{
		UserID: session.UserID, Username: session.Username, Role: session.Role,
		MustChangePassword: session.MustChangePassword, PublicURL: s.public,
	})
}

// ---- 处理器：管理员用户管理 ----

type createUserRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.auth.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "list users failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	req, err := decodeJSON[createUserRequest](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid body")
		return
	}
	user, err := s.auth.CreateUser(r.Context(), session.UserID, req.Username, req.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	// 建号即签发 MCP token，保证新用户立即可用；明文只在此响应出现一次。
	token, err := s.auth.IssueMCPToken(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "issue mcp token failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"user": user, "mcp_token": token, "mcp_endpoint": s.public + "/mcp",
	})
}

func (s *Server) handleDisableUser(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	targetUserID := r.PathValue("id")
	if err := s.auth.DisableUser(r.Context(), session.UserID, targetUserID); err != nil {
		writeAuthError(w, err)
		return
	}
	// 禁用即时性（DESIGN §4.3）：users.status 只是让"下一次认证被拒"，
	// 已建立的 WS 连接不会自己断开，必须显式踢线，否则被禁用用户的节点
	// 仍然在线并继续被路由调用。配对码同时作废，禁用解除后旧码也不再生效。
	nodeIDs, err := s.nodes.RevokeOwnerAccess(r.Context(), targetUserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "revoke user access failed")
		return
	}
	for _, nodeID := range nodeIDs {
		s.hub.Disconnect(nodeID)
	}
	s.reconcileOwner(targetUserID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// reconcileOwner 在用户级变更后重算该用户命名空间的 fleet 契约（下架失效工具）。
func (s *Server) reconcileOwner(ownerUserID string) {
	if s.fleet != nil {
		s.fleet.ReconcileOwner(ownerUserID)
	}
}

func (s *Server) handleEnableUser(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	if err := s.auth.EnableUser(r.Context(), session.UserID, r.PathValue("id")); err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type resetPasswordRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	req, err := decodeJSON[resetPasswordRequest](r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid body")
		return
	}
	if err := s.auth.ResetUserPassword(r.Context(), session.UserID, r.PathValue("id"), req.Password); err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- 处理器：MCP Token ----

func (s *Server) handleGetMCPToken(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	info, err := s.auth.EnsureMCPToken(r.Context(), session.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "read mcp token failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"has_token":    !info.UpdatedAt.IsZero(),
		"created_at":   info.CreatedAt,
		"updated_at":   info.UpdatedAt,
		"mcp_endpoint": s.public + "/mcp",
	})
}

func (s *Server) handleResetMCPToken(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	token, err := s.auth.IssueMCPToken(r.Context(), session.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "reset mcp token failed")
		return
	}
	// 明文只在重置响应中出现一次；旧 token 已被覆盖失效。
	writeJSON(w, http.StatusOK, map[string]any{
		"token":        token,
		"mcp_endpoint": s.public + "/mcp",
	})
}
