package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/uvwt/agenthub/internal/audit"
	"github.com/uvwt/agenthub/internal/core"
)

// handleListInvokes 返回当前用户自己的调用记录。
// GET /v1/invokes?limit=50&cursor=<RFC3339Nano>&tool=<前缀>&outcome=error
func (s *Server) handleListInvokes(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	query, ok := parseInvokeQuery(w, r)
	if !ok {
		return
	}
	query.UserID = session.UserID
	page, err := s.invokes.ListByUser(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "list invokes failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleAdminInvokes 返回全局调用流水（仅元数据 + 关联用户名，管理员可见）。
// GET /v1/admin/invokes?limit=&page=&user=<用户名前缀>&outcome=
func (s *Server) handleAdminInvokes(w http.ResponseWriter, r *http.Request) {
	query, ok := parseInvokeQuery(w, r)
	if !ok {
		return
	}
	if user := strings.TrimSpace(r.URL.Query().Get("user")); user != "" {
		query.Username = user
	}
	page, err := s.invokes.ListAll(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "list invokes failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleAdminStats 返回每用户近 24h 调用量统计（含用户名）。
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.invokes.StatsSince(r.Context(), time.Now().Add(-24*time.Hour))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "stats failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": stats})
}

// parseInvokeQuery 解析 page/limit/tool/outcome 查询参数；非法时写 400 并返回 false。
func parseInvokeQuery(w http.ResponseWriter, r *http.Request) (audit.InvokeQuery, bool) {
	params := r.URL.Query()
	query := audit.InvokeQuery{Limit: 50, Page: 1}
	if raw := params.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 || limit > 200 {
			writeError(w, http.StatusBadRequest, string(core.CodeValidation), "limit must be 1-200")
			return query, false
		}
		query.Limit = limit
	}
	if raw := params.Get("page"); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page <= 0 {
			writeError(w, http.StatusBadRequest, string(core.CodeValidation), "page must be >= 1")
			return query, false
		}
		query.Page = page
	}
	if tool := strings.TrimSpace(params.Get("tool")); tool != "" {
		query.ToolName = tool
	}
	if outcome := strings.TrimSpace(params.Get("outcome")); outcome != "" {
		switch outcome {
		case audit.OutcomeSuccess, audit.OutcomeError, audit.OutcomeTimeout, audit.OutcomeCancelled:
			query.Outcome = outcome
		default:
			writeError(w, http.StatusBadRequest, string(core.CodeValidation), "outcome must be success|error|timeout|cancelled")
			return query, false
		}
	}
	return query, true
}
