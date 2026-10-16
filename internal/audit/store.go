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
	// Username 关联 users 表展示（JOIN 填充；用户被删除时为空）。
	Username   string    `json:"username,omitempty"`
	NodeID     string    `json:"node_id"`
	ToolName   string    `json:"tool_name"`
	Outcome    string    `json:"outcome"` // success | error | timeout | cancelled
	DurationMS int64     `json:"duration_ms"`
	ErrorCode  string    `json:"error_code,omitempty"`
}

// InvokeQuery 是一次查询的全部条件。
// Page 从 1 开始；Limit 每页条数（≤200）。
type InvokeQuery struct {
	UserID   string // 空 = 全部（管理员）；非空 = 限定用户
	ToolName string // 前缀匹配
	Outcome  string // success/error/timeout/cancelled，空 = 全部
	Page     int
	Limit    int
}

// InvokePage 是一页结果与分页元数据。
type InvokePage struct {
	Items   []InvokeLog `json:"items"`
	Page    int         `json:"page"`
	PerPage int         `json:"per_page"`
	Total   int64       `json:"total"` // 满足筛选条件的总行数（前端算总页数/跳转）
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

// ListByUser 返回用户自己的调用记录（页码分页）。
func (s *Store) ListByUser(ctx context.Context, query InvokeQuery) (InvokePage, error) {
	return s.query(ctx, query, false)
}

// ListAll 返回全局调用记录（管理员全局流水），JOIN users 显示用户名。
func (s *Store) ListAll(ctx context.Context, query InvokeQuery) (InvokePage, error) {
	return s.query(ctx, query, true)
}

// query 统一的页码分页查询。表有 30 天滚动 + 条数上限（InvokeLogMaxRows），
// OFFSET 深翻页上界可控；occurred_at 索引保证排序成本。
func (s *Store) query(ctx context.Context, query InvokeQuery, joinUsers bool) (InvokePage, error) {
	if query.Limit <= 0 || query.Limit > 200 {
		query.Limit = 50
	}
	if query.Page <= 0 {
		query.Page = 1
	}
	conditions := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if query.UserID != "" {
		conditions = append(conditions, `user_id = ?`)
		args = append(args, query.UserID)
	}
	if tool := strings.TrimSpace(query.ToolName); tool != "" {
		conditions = append(conditions, `tool_name LIKE ?`)
		args = append(args, tool+"%")
	}
	if query.Outcome != "" {
		conditions = append(conditions, `outcome = ?`)
		args = append(args, query.Outcome)
	}
	where := `1=1`
	if len(conditions) > 0 {
		where = strings.Join(conditions, ` AND `)
	}
	join := ""
	if joinUsers {
		join = ` FROM invoke_logs l LEFT JOIN users u ON u.id = l.user_id`
	} else {
		join = ` FROM invoke_logs l`
	}

	// 总数（满足筛选条件）。
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invoke_logs l WHERE `+where, args...).Scan(&total); err != nil {
		return InvokePage{}, fmt.Errorf("统计调用记录: %w", err)
	}

	selectCols := `l.id, l.occurred_at, l.user_id, l.node_id, l.tool_name, l.outcome, l.duration_ms, l.error_code`
	if joinUsers {
		selectCols += `, u.username`
	}
	offset := (query.Page - 1) * query.Limit
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectCols+join+`
		WHERE `+where+` ORDER BY l.occurred_at DESC, l.id DESC LIMIT ? OFFSET ?`,
		append(args, query.Limit, offset)...)
	if err != nil {
		return InvokePage{}, fmt.Errorf("查询调用记录: %w", err)
	}
	defer rows.Close()
	items := make([]InvokeLog, 0, query.Limit)
	for rows.Next() {
		var log InvokeLog
		var username sql.NullString
		var occurred string
		targets := []any{&log.ID, &occurred, &log.UserID, &log.NodeID, &log.ToolName, &log.Outcome, &log.DurationMS, &log.ErrorCode, &username}
		if !joinUsers {
			targets = targets[:8]
		}
		if err := rows.Scan(targets...); err != nil {
			return InvokePage{}, fmt.Errorf("扫描调用记录: %w", err)
		}
		log.OccurredAt = parseTimeOrZero(occurred)
		log.Username = username.String
		items = append(items, log)
	}
	if err := rows.Err(); err != nil {
		return InvokePage{}, fmt.Errorf("遍历调用记录: %w", err)
	}
	return InvokePage{Items: items, Page: query.Page, PerPage: query.Limit, Total: total}, nil
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

// UserStats 汇总单用户近期调用量（管理页统计），JOIN users 显示用户名。
type UserStats struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	RecentCalls int64  `json:"recent_calls"`
}

// StatsSince 统计每个用户 since 之后的调用次数（全局概览用）。
func (s *Store) StatsSince(ctx context.Context, since time.Time) ([]UserStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.user_id, COALESCE(u.username, ''), COUNT(*)
		FROM invoke_logs l LEFT JOIN users u ON u.id = l.user_id
		WHERE l.occurred_at >= ? GROUP BY l.user_id, u.username
		ORDER BY COUNT(*) DESC`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("统计调用量: %w", err)
	}
	defer rows.Close()
	result := make([]UserStats, 0)
	for rows.Next() {
		var stat UserStats
		if err := rows.Scan(&stat.UserID, &stat.Username, &stat.RecentCalls); err != nil {
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
