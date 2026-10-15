package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	protocol "github.com/uvwt/agentdock-protocol"
)

// PublishedTool 是某用户命名空间内一个工具名的公开契约状态。
type PublishedTool struct {
	Descriptor             protocol.ToolDescriptor
	ContractHash           string
	AcceptedSemanticHashes []string
	// SourceNodeID/SourceVersion 记录当前公开契约来源（审计与排查用）。
	SourceNodeID  string
	SourceVersion string
}

// Store 负责 fleet 公开契约的持久化（按 owner_user_id 分区）。
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// List 返回用户命名空间内的全部公开契约。
func (s *Store) List(ctx context.Context, ownerUserID string) ([]PublishedTool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tool_name, descriptor_json, source_node_id, source_version
		FROM published_tool_contracts WHERE owner_user_id = ? ORDER BY tool_name`, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("列出公开契约: %w", err)
	}
	defer rows.Close()
	result := make([]PublishedTool, 0)
	for rows.Next() {
		var tool PublishedTool
		var encoded string
		if err := rows.Scan(&tool.Descriptor.Name, &encoded, &tool.SourceNodeID, &tool.SourceVersion); err != nil {
			return nil, fmt.Errorf("扫描公开契约: %w", err)
		}
		if err := json.Unmarshal([]byte(encoded), &tool.Descriptor); err != nil {
			return nil, fmt.Errorf("解析公开契约 %s: %w", tool.Descriptor.Name, err)
		}
		hashes, err := s.loadVariants(ctx, ownerUserID, tool.Descriptor.Name)
		if err != nil {
			return nil, err
		}
		tool.AcceptedSemanticHashes = hashes
		contractHash, err := ToolContractHash(tool.Descriptor)
		if err != nil {
			return nil, err
		}
		tool.ContractHash = contractHash
		result = append(result, tool)
	}
	return result, rows.Err()
}

// Load 读单个工具的公开契约（含兼容哈希集合）。
func (s *Store) Load(ctx context.Context, ownerUserID, toolName string) (PublishedTool, bool, error) {
	var encoded string
	var tool PublishedTool
	err := s.db.QueryRowContext(ctx, `SELECT descriptor_json, source_node_id, source_version FROM published_tool_contracts
		WHERE owner_user_id = ? AND tool_name = ?`, ownerUserID, toolName).Scan(&encoded, &tool.SourceNodeID, &tool.SourceVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishedTool{}, false, nil
	}
	if err != nil {
		return PublishedTool{}, false, fmt.Errorf("读取公开契约 %s: %w", toolName, err)
	}
	if err := json.Unmarshal([]byte(encoded), &tool.Descriptor); err != nil {
		return PublishedTool{}, false, fmt.Errorf("解析公开契约 %s: %w", toolName, err)
	}
	hashes, err := s.loadVariants(ctx, ownerUserID, toolName)
	if err != nil {
		return PublishedTool{}, false, err
	}
	tool.AcceptedSemanticHashes = hashes
	contractHash, err := ToolContractHash(tool.Descriptor)
	if err != nil {
		return PublishedTool{}, false, err
	}
	tool.ContractHash = contractHash
	return tool, true, nil
}

func (s *Store) loadVariants(ctx context.Context, ownerUserID, toolName string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT semantic_hash FROM published_tool_variants
		WHERE owner_user_id = ? AND tool_name = ? ORDER BY semantic_hash`, ownerUserID, toolName)
	if err != nil {
		return nil, fmt.Errorf("读取公开契约变体 %s: %w", toolName, err)
	}
	defer rows.Close()
	hashes := make([]string, 0)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("扫描公开契约变体 %s: %w", toolName, err)
		}
		hashes = append(hashes, hash)
	}
	return hashes, rows.Err()
}

// Save 把合并结果落库：契约 + 全量替换变体集合，单事务。
// 首次发布先持久化再对外公开（publish 回调由调用方在 Save 成功后触发）。
func (s *Store) Save(ctx context.Context, ownerUserID string, tool PublishedTool) error {
	descriptorJSON, err := json.Marshal(tool.Descriptor)
	if err != nil {
		return fmt.Errorf("编码公开契约 %s: %w", tool.Descriptor.Name, err)
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始公开契约事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO published_tool_contracts(
		owner_user_id, tool_name, descriptor_json, source_node_id, source_version, updated_at
	) VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(owner_user_id, tool_name) DO UPDATE SET
		descriptor_json = excluded.descriptor_json,
		source_node_id = excluded.source_node_id,
		source_version = excluded.source_version,
		updated_at = excluded.updated_at`,
		ownerUserID, tool.Descriptor.Name, string(descriptorJSON), tool.SourceNodeID, tool.SourceVersion, now); err != nil {
		return fmt.Errorf("保存公开契约 %s: %w", tool.Descriptor.Name, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM published_tool_variants WHERE owner_user_id = ? AND tool_name = ?`,
		ownerUserID, tool.Descriptor.Name); err != nil {
		return fmt.Errorf("清理公开契约变体 %s: %w", tool.Descriptor.Name, err)
	}
	for _, hash := range tool.AcceptedSemanticHashes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO published_tool_variants(owner_user_id, tool_name, semantic_hash)
			VALUES(?, ?, ?)`, ownerUserID, tool.Descriptor.Name, hash); err != nil {
			return fmt.Errorf("保存公开契约变体 %s: %w", tool.Descriptor.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交公开契约 %s: %w", tool.Descriptor.Name, err)
	}
	return nil
}

// Delete 下架公开契约（级联清掉变体）。
func (s *Store) Delete(ctx context.Context, ownerUserID, toolName string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM published_tool_contracts WHERE owner_user_id = ? AND tool_name = ?`,
		ownerUserID, strings.TrimSpace(toolName))
	if err != nil {
		return fmt.Errorf("删除公开契约 %s: %w", toolName, err)
	}
	return nil
}

// Names 返回用户命名空间内全部已公开工具名。
func (s *Store) Names(ctx context.Context, ownerUserID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tool_name FROM published_tool_contracts
		WHERE owner_user_id = ? ORDER BY tool_name`, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("列出公开契约名: %w", err)
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("扫描公开契约名: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
