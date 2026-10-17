package httpx

import (
	"net/http"
)

// handleAdminOnlyReady 在管理员账号处于 must_change_password 时拒绝执行。
//
// 管理员 CLI reset 之后 must_change_password 会被置 1。此时管理员持有的是
// 尚未验证过的口令，若不拦截，拿到该口令的人可直接管理全局用户、禁用他人。
// 用 403 + 明确错误码而不是 401（会话本身有效）。
func (s *Server) handleAdminOnlyReady(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := sessionFromContext(r.Context())
		if session.Role == "admin" && session.MustChangePassword {
			writeError(w, http.StatusForbidden, "PASSWORD_CHANGE_REQUIRED", "administrator must change password first")
			return
		}
		next(w, r)
	}
}

// requireReadyForUser 是 handleAdminOnlyReady 对普通用户业务接口的等价拦截。
//
// 强制改密不能只靠前端（web/src/App.tsx 的 must_change_password 分支）——
// 直接打 API 能绕过 UI。未改密前用户只能调 /v1/auth/me 与 /v1/auth/password。
func (s *Server) requireReadyForUser(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := sessionFromContext(r.Context())
		if session.MustChangePassword {
			writeError(w, http.StatusForbidden, "PASSWORD_CHANGE_REQUIRED", "must change password first")
			return
		}
		next(w, r)
	}
}