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
	"net"
	"net/http"
	"strings"
	"time"

	protocol "github.com/uvwt/agentdock-protocol"
	"github.com/uvwt/agenthub/internal/auth"
	"github.com/uvwt/agenthub/internal/bridge"
	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/nodes"
)

const (
	sessionCookieName = "agenthub_session"
	// csrfHeaderName Web 写操作的 CSRF 头。CSRF token 由前端从登录响应获取。
	csrfHeaderName = "X-AgentHub-CSRF"
)

// Server 是 HTTP 层的依赖容器。
type Server struct {
	auth   *auth.Service
	nodes  *nodes.Store
	hub    *bridge.Hub
	public string // PublicURL，用于给用户展示 MCP endpoint
	mux    *http.ServeMux
}

func NewServer(authService *auth.Service, nodeStore *nodes.Store, hub *bridge.Hub, publicURL string) *Server {
	s := &Server{auth: authService, nodes: nodeStore, hub: hub, public: publicURL, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// 认证
	s.mux.HandleFunc("POST /v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /v1/auth/logout", s.requireSession(s.handleLogout))
	s.mux.HandleFunc("POST /v1/auth/password", s.requireSession(s.handleChangePassword))
	s.mux.HandleFunc("GET /v1/auth/me", s.requireSession(s.handleMe))

	// 管理员：用户管理
	s.mux.HandleFunc("GET /v1/admin/users", s.requireAdmin(s.handleListUsers))
	s.mux.HandleFunc("POST /v1/admin/users", s.requireAdmin(s.handleCreateUser))
	s.mux.HandleFunc("POST /v1/admin/users/{id}/disable", s.requireAdmin(s.handleDisableUser))
	s.mux.HandleFunc("POST /v1/admin/users/{id}/enable", s.requireAdmin(s.handleEnableUser))
	s.mux.HandleFunc("POST /v1/admin/users/{id}/reset-password", s.requireAdmin(s.handleResetPassword))

	// 用户：MCP Token
	s.mux.HandleFunc("GET /v1/mcp-token", s.requireSession(s.handleGetMCPToken))
	s.mux.HandleFunc("POST /v1/mcp-token/reset", s.requireSession(s.handleResetMCPToken))

	// 用户：节点管理
	s.mux.HandleFunc("GET /v1/nodes", s.requireSession(s.handleListNodes))
	s.mux.HandleFunc("POST /v1/nodes/pairing-codes", s.requireSession(s.handleCreatePairingCode))
	s.mux.HandleFunc("POST /v1/nodes/{id}/disable", s.requireSession(s.handleDisableNode))
	s.mux.HandleFunc("POST /v1/nodes/{id}/enable", s.requireSession(s.handleEnableNode))
	s.mux.HandleFunc("DELETE /v1/nodes/{id}", s.requireSession(s.handleDeleteNode))

	// 节点侧（AgentDock 出站，无会话，配对码/device token 认证）
	s.mux.HandleFunc("POST "+protocol.PairPath, s.handleNodePair)
	s.mux.HandleFunc("GET "+protocol.ConnectPath, s.handleNodeConnect)
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
			writeAuthError(w, err)
			return
		}
		// 写操作要求 CSRF 头与会话绑定校验：Cookie 是 SameSite=Lax，
		// 双保险防跨站写。
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.csrfValid(r, session.ID) {
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

func clientIP(r *http.Request) string {
	// 反代终结 TLS 的部署形态：信任 X-Forwarded-For 的第一跳。
	// hub 只在反代之后监听，不直接暴露公网。
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
	ip := clientIP(r)
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
	if err := s.auth.DisableUser(r.Context(), session.UserID, r.PathValue("id")); err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
