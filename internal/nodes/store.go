// Package nodes 承载 AgentDock 节点域：配对码、配对、节点存储与查询。
// 与 nexusdock 的关键差异是多租户：节点与配对码都绑定 owner_user_id，
// 所有用户侧操作必须带 owner 过滤，防止跨用户访问。
package nodes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/config"
	"github.com/uvwt/agenthub/internal/core"
)

var (
	ErrNodeNotFound       = errors.New("节点不存在")
	ErrNodeExists         = errors.New("设备已经配对")
	ErrNodeDisabled       = errors.New("节点已停用")
	ErrNodeLimitReached   = errors.New("节点数量已达上限")
	ErrPairingCodeInvalid = errors.New("配对码无效或已过期")
	// ErrOwnerDisabled 表示配对码归属的用户已被禁用。
	// DESIGN §4.3 要求"禁用用户 = 配对码作废"：禁用后仍能兑换出新节点，
	// 会让已禁用的账号持续消耗 MaxNodesPerUser 配额并留下无效节点行。
	ErrOwnerDisabled = errors.New("用户已禁用")
)

// Node 是节点的业务视图（不含 token 哈希）。
type Node struct {
	ID               string     `json:"id"`
	OwnerUserID      string     `json:"owner_user_id"`
	DeviceID         string     `json:"device_id"`
	Name             string     `json:"name"`
	Enabled          bool       `json:"enabled"`
	Version          string     `json:"version,omitempty"`
	ProtocolVersion  string     `json:"protocol_version,omitempty"`
	OS               string     `json:"os,omitempty"`
	Arch             string     `json:"arch,omitempty"`
	Capabilities     []string   `json:"capabilities"`
	ToolContractHash string     `json:"tool_contract_hash,omitempty"`
	Online           bool       `json:"online"`
	LastSeenAt       *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`

	// DeviceToken 只在 Pair 返回时填充一次（明文 token 随 201 响应发给节点），
	// 其余查询路径恒为空。
	DeviceToken string `json:"-"`
}

// PairingCode 是生成的配对码（明文只出现一次）。
type PairingCode struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PairInput 与 protocol.PairRequest 字段同形；HTTP 边界显式转换，
// 协议字段变化时在编译期暴露。
type PairInput struct {
	Code     string `json:"code"`
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
}

// Store 是节点域的存储与业务入口。
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// recordAudit 写一条审计事件（append-only）。
//
// 节点侧动作（配对/启停/删除）此前完全不写审计：DESIGN §5 声明 actor 含 device，
// 出了事（节点被盗用、误删）没有可追溯的记录。审计失败不阻断主流程——
// 业务动作已经发生，为记一笔日志而回滚会更糟。
func (s *Store) recordAudit(ctx context.Context, actorType, actorID, action, objectID string, metadata map[string]any) {
	metadataJSON := "{}"
	if metadata != nil {
		if raw, err := json.Marshal(metadata); err == nil {
			metadataJSON = string(raw)
		}
	}
	id, err := core.NewID("evt")
	if err != nil {
		return
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit_events(
		id, occurred_at, actor_type, actor_id, action, object_type, object_id, result, metadata_json
	) VALUES(?, ?, ?, ?, ?, 'node', ?, 'succeeded', ?)`,
		id, s.now().UTC().Format(time.RFC3339Nano), actorType, actorID, action, objectID, metadataJSON)
}

// CreatePairingCode 为指定用户生成一次性配对码；哈希落库、明文只返回一次。
func (s *Store) CreatePairingCode(ctx context.Context, ownerUserID string) (PairingCode, error) {
	if strings.TrimSpace(ownerUserID) == "" {
		return PairingCode{}, core.NewError(core.CodeValidation, "owner user id is required", nil)
	}
	raw, err := core.NewSecret(24)
	if err != nil {
		return PairingCode{}, err
	}
	code := "pair_" + raw
	id, err := core.NewID("pair")
	if err != nil {
		return PairingCode{}, err
	}
	now := s.now().UTC()
	expiresAt := now.Add(config.PairingCodeTTL)
	_, err = s.db.ExecContext(ctx, `INSERT INTO pairing_codes(id, owner_user_id, code_hash, expires_at, created_at)
		VALUES(?, ?, ?, ?, ?)`, id, ownerUserID, core.HashToken(code), expiresAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return PairingCode{}, fmt.Errorf("保存配对码: %w", err)
	}
	return PairingCode{Code: code, ExpiresAt: expiresAt}, nil
}

// Pair 用一次性配对码创建节点，归属由配对码的 owner 决定。
// 配对码消费与节点创建在同一事务内：单次语义靠 UPDATE ... used_at IS NULL 保证。
func (s *Store) Pair(ctx context.Context, input PairInput) (Node, error) {
	code := strings.TrimSpace(input.Code)
	deviceID := strings.TrimSpace(input.DeviceID)
	name := strings.TrimSpace(input.Name)
	if code == "" {
		return Node{}, core.NewError(core.CodeValidation, "配对码不能为空", nil)
	}
	if !protocol.ValidDeviceID(deviceID) {
		return Node{}, core.NewError(core.CodeValidation, "设备 ID 格式无效", nil)
	}
	if !protocol.ValidNodeName(name) {
		return Node{}, core.NewError(core.CodeValidation, "节点名称必须为 1 到 100 个字符", nil)
	}

	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Node{}, fmt.Errorf("开始配对事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var ownerUserID, pairID string
	err = tx.QueryRowContext(ctx, `SELECT p.id, p.owner_user_id FROM pairing_codes p
		JOIN users u ON u.id = p.owner_user_id
		WHERE p.code_hash = ? AND p.used_at IS NULL AND p.expires_at > ? AND u.status = 'active'`,
		core.HashToken(code), now.Format(time.RFC3339Nano)).Scan(&pairID, &ownerUserID)
	if errors.Is(err, sql.ErrNoRows) {
		// 码不存在 / 已用 / 过期 / 归属用户已被禁用——一律同码，
		// 不向公网无认证入口泄露"码存在但主人被禁了"这一信息。
		return Node{}, ErrPairingCodeInvalid
	}
	if err != nil {
		return Node{}, fmt.Errorf("查询配对码: %w", err)
	}

	// 节点数上限：兑换码前校验，防止用尽配额。
	var nodeCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE owner_user_id = ?`, ownerUserID).Scan(&nodeCount); err != nil {
		return Node{}, fmt.Errorf("统计用户节点数: %w", err)
	}
	if nodeCount >= config.MaxNodesPerUser {
		return Node{}, ErrNodeLimitReached
	}

	nodeID, err := core.NewID("node")
	if err != nil {
		return Node{}, err
	}
	deviceToken, err := core.NewSecret(32)
	if err != nil {
		return Node{}, err
	}
	// 消费配对码必须断言命中行数：单次语义靠 used_at IS NULL 的条件更新保证，
	// 但两个并发事务可能都通过了上面的 SELECT（SQLite 读不阻塞），
	// 若不检查 RowsAffected，第二个事务会静默更新 0 行后继续建节点，
	// 一个配对码就产出两个节点 + 两个 device token。
	result, err := tx.ExecContext(ctx, `UPDATE pairing_codes SET used_at = ? WHERE id = ? AND used_at IS NULL`,
		now.Format(time.RFC3339Nano), pairID)
	if err != nil {
		// 写锁竞争：别人正在消费同一枚码。对调用方而言等价于"码已被用"。
		if strings.Contains(strings.ToLower(err.Error()), "locked") {
			return Node{}, ErrPairingCodeInvalid
		}
		return Node{}, fmt.Errorf("消费配对码: %w", err)
	}
	if consumed, _ := result.RowsAffected(); consumed != 1 {
		// 已被并发请求抢走：本事务回滚，不建节点。
		return Node{}, ErrPairingCodeInvalid
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO nodes(
		id, owner_user_id, device_id, name, enabled, device_token_hash, created_at, updated_at
	) VALUES(?, ?, ?, ?, 1, ?, ?, ?)`,
		nodeID, ownerUserID, deviceID, name, core.HashToken(deviceToken),
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		if core.IsSQLiteConflict(err) || strings.Contains(strings.ToLower(err.Error()), "unique") {
			return Node{}, ErrNodeExists
		}
		// 写锁竞争（SQLITE_BUSY）本质是"别人正在消费这个码"，
		// 对调用方与"码已被用过"无差别，统一映射成 ErrPairingCodeInvalid，
		// 不把底层数据库错误泄漏到 HTTP 响应里。
		if strings.Contains(strings.ToLower(err.Error()), "locked") {
			return Node{}, ErrPairingCodeInvalid
		}
		return Node{}, fmt.Errorf("创建节点: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Node{}, fmt.Errorf("提交配对事务: %w", err)
	}
	// 审计 actor 是 device：配对由 AgentDock 用配对码发起，归属用户由码决定。
	s.recordAudit(ctx, "device", nodeID, "node.pair", nodeID, map[string]any{"owner": ownerUserID, "device_id": deviceID})
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return Node{}, err
	}
	// token 明文通过节点视图一次性带出（不落入 Node 结构，避免日志泄漏面）。
	node.DeviceToken = deviceToken
	return node, nil
}

// AuthenticateDeviceToken 用 Bearer token 解析节点身份。
// 用户被禁用时其节点一律拒连（JOIN users 过滤 status）。
func (s *Store) AuthenticateDeviceToken(ctx context.Context, rawToken string) (Node, error) {
	rawToken = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rawToken), "Bearer "))
	if rawToken == "" {
		return Node{}, core.NewError(core.CodeAuthRequired, "device token is required", nil)
	}
	var node Node
	var enabled int
	var lastSeen sql.NullString
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `SELECT n.id, n.owner_user_id, n.device_id, n.name, n.enabled,
		n.last_seen_at, n.created_at, n.updated_at FROM nodes n
		JOIN users u ON u.id = n.owner_user_id
		WHERE n.device_token_hash = ? AND u.status = 'active'`, core.HashToken(rawToken)).
		Scan(&node.ID, &node.OwnerUserID, &node.DeviceID, &node.Name, &enabled, &lastSeen, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, core.NewError(core.CodeInvalidToken, "device token is invalid", nil)
	}
	if err != nil {
		return Node{}, fmt.Errorf("验证设备 token: %w", err)
	}
	if enabled != 1 {
		return Node{}, ErrNodeDisabled
	}
	node.Enabled = true
	node.Capabilities = []string{}
	if lastSeen.Valid {
		value, err := time.Parse(time.RFC3339Nano, lastSeen.String)
		if err != nil {
			return Node{}, fmt.Errorf("解析节点在线时间: %w", err)
		}
		node.LastSeenAt = &value
	}
	if node.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Node{}, fmt.Errorf("解析节点创建时间: %w", err)
	}
	if node.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Node{}, fmt.Errorf("解析节点更新时间: %w", err)
	}
	return node, nil
}

// RevokeOwnerAccess 是禁用用户时的一次性清理：作废该用户全部未用配对码，
// 返回其全部节点 ID 供调用方踢线。
//
// DESIGN §4.3 要求"禁用用户 = 节点连接断开 + 配对码作废"。配对码在此显式作废
// （而不是只靠 Pair 侧校验用户状态）是为了不让已禁用的账号继续囤积有效配对码——
// 否则禁用解除后，一段时间内签发过的旧码仍然可用。
func (s *Store) RevokeOwnerAccess(ctx context.Context, ownerUserID string) ([]string, error) {
	nodeList, err := s.ListByOwner(ctx, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("列出待踢线节点: %w", err)
	}
	nodeIDs := make([]string, 0, len(nodeList))
	for _, node := range nodeList {
		nodeIDs = append(nodeIDs, node.ID)
	}
	// 立即占用已用标记：Pair 的兑换条件是 used_at IS NULL，
	// 置上即可让存量码立即失效（expires_at 保持原值，不影响审计追溯）。
	if _, err := s.db.ExecContext(ctx, `UPDATE pairing_codes SET used_at = ?
		WHERE owner_user_id = ? AND used_at IS NULL`,
		s.now().UTC().Format(time.RFC3339Nano), ownerUserID); err != nil {
		return nil, fmt.Errorf("作废用户配对码: %w", err)
	}
	return nodeIDs, nil
}

// ListAllOwners 分页列出拥有节点的用户 ID，供启动恢复 fleet 公开契约使用。
func (s *Store) ListAllOwners(ctx context.Context, limit, offset int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT owner_user_id FROM nodes ORDER BY owner_user_id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("列出节点归属用户: %w", err)
	}
	defer rows.Close()
	owners := make([]string, 0, limit)
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, fmt.Errorf("扫描节点归属用户: %w", err)
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

// ListByOwner 返回用户的全部节点。
//
// 排序必须全序确定：fleet 合并同名工具时以 descriptors[0] 为基准决定公开契约，
// 若同毫秒创建的节点顺序在两次查询间翻转，会让公开 descriptor 抖动、MCP 客户端
// 反复刷新工具面。created_at 是 TEXT 时间戳，单独用它排序无法区分同毫秒记录，
// 因此追加 id 作为 tie-breaker（与 nexusdock ORDER BY name, id 同一立场）。
func (s *Store) ListByOwner(ctx context.Context, ownerUserID string) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, getColumns()+` FROM nodes WHERE owner_user_id = ?
		ORDER BY created_at, id`, strings.TrimSpace(ownerUserID))
	if err != nil {
		return nil, fmt.Errorf("列出用户节点: %w", err)
	}
	defer rows.Close()
	result := make([]Node, 0)
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		node.OwnerUserID = ownerUserID
		result = append(result, node)
	}
	return result, rows.Err()
}

// GetOwned 取用户自己的节点；不存在与不属于该用户统一返回 NotFound，
// 不向调用方泄露他人资源的存在性。
func (s *Store) GetOwned(ctx context.Context, ownerUserID, nodeID string) (Node, error) {
	node, err := s.get(ctx, nodeID)
	if err != nil {
		return Node{}, err
	}
	if node.OwnerUserID != ownerUserID {
		return Node{}, ErrNodeNotFound
	}
	return node, nil
}

func (s *Store) get(ctx context.Context, nodeID string) (Node, error) {
	row := s.db.QueryRowContext(ctx, getColumns()+` FROM nodes WHERE id = ?`, strings.TrimSpace(nodeID))
	node, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNodeNotFound
	}
	return node, err
}

// Get 供 Bridge/MCP 层内部使用（已认证节点自身或路由上下文）。
func (s *Store) Get(ctx context.Context, nodeID string) (Node, error) {
	return s.get(ctx, nodeID)
}

// SetEnabled 禁用/启用节点。禁用可逆；调用方负责踢线。
func (s *Store) SetEnabled(ctx context.Context, ownerUserID, nodeID string, enabled bool) (Node, error) {
	if _, err := s.GetOwned(ctx, ownerUserID, nodeID); err != nil {
		return Node{}, err
	}
	value := 0
	if enabled {
		value = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET enabled = ?, updated_at = ? WHERE id = ? AND owner_user_id = ?`,
		value, s.now().UTC().Format(time.RFC3339Nano), nodeID, ownerUserID)
	if err != nil {
		return Node{}, fmt.Errorf("更新节点状态: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Node{}, ErrNodeNotFound
	}
	action := "node.disable"
	if enabled {
		action = "node.enable"
	}
	s.recordAudit(ctx, "user", ownerUserID, action, nodeID, map[string]any{"enabled": enabled})
	return s.GetOwned(ctx, ownerUserID, nodeID)
}

// Delete 删除节点：token 随行删除（device_token_hash 不再存在），
// 契约与快照靠外键级联清理。不可逆；调用方负责踢线。
func (s *Store) Delete(ctx context.Context, ownerUserID, nodeID string) error {
	if _, err := s.GetOwned(ctx, ownerUserID, nodeID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ? AND owner_user_id = ?`, nodeID, ownerUserID)
	if err != nil {
		return fmt.Errorf("删除节点: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrNodeNotFound
	}
	s.recordAudit(ctx, "user", ownerUserID, "node.delete", nodeID, nil)
	return nil
}

// UpdateHello 持久化握手快照：节点元数据、工具契约、Bridge 能力。
// 设备身份必须与配对记录一致，防止同 token 伪装其他设备。
func (s *Store) UpdateHello(ctx context.Context, nodeID string, hello protocol.Hello) (Node, error) {
	node, err := s.get(ctx, nodeID)
	if err != nil {
		return Node{}, err
	}
	if !node.Enabled {
		return Node{}, ErrNodeDisabled
	}
	if strings.TrimSpace(hello.DeviceID) != node.DeviceID {
		return Node{}, core.NewError(core.CodeValidation, "设备身份与配对记录不一致", nil)
	}
	capabilities, err := json.Marshal(normalizeCapabilities(hello.Capabilities))
	if err != nil {
		return Node{}, fmt.Errorf("编码节点能力: %w", err)
	}
	bridgeCapabilities, err := json.Marshal(normalizeCapabilities(hello.BridgeCapabilities))
	if err != nil {
		return Node{}, fmt.Errorf("编码 Bridge 能力: %w", err)
	}
	descriptors, err := json.Marshal(hello.Tools)
	if err != nil {
		return Node{}, fmt.Errorf("编码工具契约: %w", err)
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Node{}, fmt.Errorf("开始握手事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET version = ?, protocol_version = ?, os = ?, arch = ?,
		capabilities_json = ?, tool_contract_hash = ?, last_seen_at = ?, updated_at = ? WHERE id = ?`,
		strings.TrimSpace(hello.Version), strings.TrimSpace(hello.ProtocolVersion), strings.TrimSpace(hello.OS),
		strings.TrimSpace(hello.Arch), string(capabilities), strings.TrimSpace(hello.ToolContractHash), now, now, nodeID); err != nil {
		return Node{}, fmt.Errorf("更新节点握手信息: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tool_contracts(node_id, descriptors_json, updated_at)
		VALUES(?, ?, ?) ON CONFLICT(node_id) DO UPDATE SET descriptors_json = excluded.descriptors_json, updated_at = excluded.updated_at`,
		nodeID, string(descriptors), now); err != nil {
		return Node{}, fmt.Errorf("保存工具契约: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO bridge_capabilities(node_id, capabilities_json, updated_at)
		VALUES(?, ?, ?) ON CONFLICT(node_id) DO UPDATE SET capabilities_json = excluded.capabilities_json, updated_at = excluded.updated_at`,
		nodeID, string(bridgeCapabilities), now); err != nil {
		return Node{}, fmt.Errorf("保存 Bridge 能力: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Node{}, fmt.Errorf("提交握手事务: %w", err)
	}
	return s.get(ctx, nodeID)
}

// ToolDescriptors 读取节点 hello 时的工具契约快照。
func (s *Store) ToolDescriptors(ctx context.Context, nodeID string) ([]protocol.ToolDescriptor, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT descriptors_json FROM tool_contracts WHERE node_id = ?`, strings.TrimSpace(nodeID)).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return []protocol.ToolDescriptor{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取工具契约: %w", err)
	}
	var descriptors []protocol.ToolDescriptor
	if err := json.Unmarshal([]byte(encoded), &descriptors); err != nil {
		return nil, fmt.Errorf("解析工具契约: %w", err)
	}
	return descriptors, nil
}

// Touch 更新心跳时间。
func (s *Store) Touch(ctx context.Context, nodeID string) error {
	now := s.now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET last_seen_at = ?, updated_at = ? WHERE id = ? AND enabled = 1`, now, now, nodeID)
	if err != nil {
		return fmt.Errorf("更新节点心跳: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrNodeNotFound
	}
	return nil
}

func getColumns() string {
	return `SELECT id, owner_user_id, device_id, name, enabled, version, protocol_version,
		os, arch, capabilities_json, tool_contract_hash, last_seen_at, created_at, updated_at`
}

func scanNode(scanner interface{ Scan(...any) error }) (Node, error) {
	var node Node
	var enabled int
	var capabilitiesJSON, createdAt, updatedAt string
	var lastSeen sql.NullString
	if err := scanner.Scan(&node.ID, &node.OwnerUserID, &node.DeviceID, &node.Name, &enabled, &node.Version,
		&node.ProtocolVersion, &node.OS, &node.Arch, &capabilitiesJSON, &node.ToolContractHash,
		&lastSeen, &createdAt, &updatedAt); err != nil {
		return Node{}, err
	}
	if err := json.Unmarshal([]byte(capabilitiesJSON), &node.Capabilities); err != nil {
		return Node{}, fmt.Errorf("解析节点能力: %w", err)
	}
	var err error
	if node.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Node{}, fmt.Errorf("解析节点创建时间: %w", err)
	}
	if node.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Node{}, fmt.Errorf("解析节点更新时间: %w", err)
	}
	if lastSeen.Valid {
		value, parseErr := time.Parse(time.RFC3339Nano, lastSeen.String)
		if parseErr != nil {
			return Node{}, fmt.Errorf("解析节点在线时间: %w", parseErr)
		}
		node.LastSeenAt = &value
	}
	node.Enabled = enabled == 1
	return node, nil
}

func normalizeCapabilities(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
