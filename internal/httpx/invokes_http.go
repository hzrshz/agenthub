package httpx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/uvwt/agenthub/internal/core"
)

// handleListInvokes 返回当前用户自己的调用记录（GET /v1/invokes?limit=&before=）。
func (s *Server) handleListInvokes(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	before, err := parseBeforeParam(w, r)
	if err != nil {
		return // parseBeforeParam 已写响应
	}
	logs, err := s.invokes.ListByUser(r.Context(), session.UserID, parseLimitParam(r), before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "list invokes failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invokes": logs})
}

// handleAdminInvokes 返回全局调用流水（仅元数据，管理员可见）。
func (s *Server) handleAdminInvokes(w http.ResponseWriter, r *http.Request) {
	before, err := parseBeforeParam(w, r)
	if err != nil {
		return
	}
	logs, err := s.invokes.ListAll(r.Context(), parseLimitParam(r), before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "list invokes failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invokes": logs})
}

// handleAdminStats 返回每用户近 24h 调用量统计。
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.invokes.StatsSince(r.Context(), time.Now().Add(-24*time.Hour))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "stats failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": stats})
}

func parseLimitParam(r *http.Request) int {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func parseBeforeParam(w http.ResponseWriter, r *http.Request) (*time.Time, error) {
	raw := r.URL.Query().Get("before")
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, string(core.CodeValidation), "before must be RFC3339")
		return nil, err
	}
	return &parsed, nil
}
