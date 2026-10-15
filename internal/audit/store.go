// Package audit 承载调用记录（invoke_logs）的写入与查询。
// 只记元数据：时间/用户/节点/工具/成败/耗时/错误码——hub 是中继不是落地，
// 工具参数与结果绝不入库。保留策略：30 天滚动 + 条数上限。
package audit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/uvwt/agenthub/internal/config"
	"github.com/uvwt/agenthub/internal/core"
)

// InvokeLog 是一次 MCP 工具调用的元数据记录。
type InvokeLog struct {
	ID         string    `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	UserID     string    `json:"user_id"`
	NodeID     string    `json:"node_id"`
	ToolName   string    `json:"tool_name"`
	Outcome    string    `json:"outcome"` // success | error | timeout | cancelled
	DurationMS int64     `json:"duration_ms"`
	ErrorCode  string    `json:"error_code,omitempty"`
}

// Outcome 常量。
const (
	OutcomeSuccess   = "success"
	OutcomeError     = "error"
	OutcomeTimeout   = "timeout"
	OutcomeCancelled = "cancelled"
)

// Store 是调用记录存储。
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// RecordInvoke 写入一条调用记录。失败只返回错误给调用方自行决策（记录失败不阻断调用本身）。
func (s *Store) RecordInvoke(ctx context.Context, userID, nodeID, toolName, outcome string, duration time.Duration, errorCode string) error {
	if outcome != OutcomeSuccess && outcome != OutcomeError && outcome != OutcomeTimeout && outcome != OutcomeCancelled {
		return core.NewError(core.CodeValidation, "invalid invoke outcome", nil)
	}
	id, err := core.NewID("inv")
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO invoke_logs(
		id, occurred_at, user_id, node_id, tool_name, outcome, duration_ms, error_code
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		id, s.now().UTC().Format(time.RFC3339Nano), userID, nodeID, toolName, outcome,
		duration.Milliseconds(), strings.TrimSpace(errorCode))
	if err != nil {
		return fmt.Errorf("写入调用记录: %w", err)
	}
	return nil
}

// ListByUser 返回用户自己的调用记录（分页：limit ≤ 200，before 早于该时间）。
func (s *Store) ListByUser(ctx context.Context, userID string, limit int, before *time.Time) ([]InvokeLog, error) {
	return s.list(ctx, `user_id = ?`, userID, limit, before)
}

// ListAll 返回全局调用记录（管理员全局流水）。
func (s *Store) ListAll(ctx context.Context, limit int, before *time.Time) ([]InvokeLog, error) {
	return s.list(ctx, `1=1`, nil, limit, before)
}

func (s *Store) list(ctx context.Context, where string, userID any, limit int, before *time.Time) ([]InvokeLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id, occurred_at, user_id, node_id, tool_name, outcome, duration_ms, error_code
		FROM invoke_logs WHERE ` + where
	args := make([]any, 0, 3)
	if userID != nil {
		args = append(args, userID)
	}
	if before != nil {
		query += ` AND occurred_at < ?`
		args = append(args, before.UTC().Format(time.RFC3339Nano))
	}
	query += ` ORDER BY occurred_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询调用记录: %w", err)
	}
	defer rows.Close()
	result := make([]InvokeLog, 0)
	for rows.Next() {
		var log InvokeLog
		var occurred string
		if err := rows.Scan(&log.ID, &occurred, &log.UserID, &log.NodeID, &log.ToolName, &log.Outcome, &log.DurationMS, &log.ErrorCode); err != nil {
			return nil, fmt.Errorf("扫描调用记录: %w", err)
		}
		log.OccurredAt = parseTimeOrZero(occurred)
		result = append(result, log)
	}
	return result, rows.Err()
}

// Cleanup 执行保留策略：删除 30 天前记录并裁剪超出条数上限的旧行。
// 由 serve 的后台任务周期调用；幂等。
func (s *Store) Cleanup(ctx context.Context) (int64, error) {
	cutoff := s.now().UTC().Add(-config.InvokeLogRetention).Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `DELETE FROM invoke_logs WHERE occurred_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("清理过期调用记录: %w", err)
	}
	removed, _ := result.RowsAffected()

	// 条数上限裁剪：保留最新 InvokeLogMaxRows 条。
	if _, err := s.db.ExecContext(ctx, `DELETE FROM invoke_logs WHERE id IN (
		SELECT id FROM invoke_logs ORDER BY occurred_at DESC LIMIT -1 OFFSET ?
	)`, config.InvokeLogMaxRows); err != nil {
		return removed, fmt.Errorf("裁剪调用记录条数: %w", err)
	}
	return removed, nil
}

// UserStats 汇总单用户近期调用量（管理页统计）。
type UserStats struct {
	UserID      string `json:"user_id"`
	RecentCalls int64  `json:"recent_calls"`
}

// StatsSince 统计每个用户 since 之后的调用次数（全局概览用）。
func (s *Store) StatsSince(ctx context.Context, since time.Time) ([]UserStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, COUNT(*) FROM invoke_logs
		WHERE occurred_at >= ? GROUP BY user_id`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("统计调用量: %w", err)
	}
	defer rows.Close()
	result := make([]UserStats, 0)
	for rows.Next() {
		var stat UserStats
		if err := rows.Scan(&stat.UserID, &stat.RecentCalls); err != nil {
			return nil, fmt.Errorf("扫描调用量统计: %w", err)
		}
		result = append(result, stat)
	}
	return result, rows.Err()
}

func parseTimeOrZero(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
