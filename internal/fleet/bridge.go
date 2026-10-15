package fleet

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"

	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/nodes"
)

// PublishHandlers 是 Bridge 对外发布的出口（MCP 网关注册/下架工具）。
type PublishHandlers struct {
	// Publish 以该用户名义公开工具（MCP AddTool）。
	Publish func(ownerUserID string, descriptor protocol.ToolDescriptor)
	// Retire 从该用户的 MCP 工具面下架工具名。
	Retire func(ownerUserID, toolName string)
}

// Bridge 维护全部用户的 fleet 公开契约状态：
//   - 节点 hello 到达时观察其工具（ObserveNodeHello）；
//   - 节点启停/删除后重算受影响工具（ReconcileNode）；
//   - 同名工具按用户分区合并，冲突（不可合并）保持现有公开契约不变；
//   - 没有任何 enabled provider 时下架公开工具。
//
// 并发模型与 nexusdock 相同：reconcileMu 串行化"读 fleet → 持久化 → 发布"，
// mu 保护内存状态。
type Bridge struct {
	store    *Store
	nodes    *nodes.Store
	logger   *slog.Logger
	handlers PublishHandlers

	reconcileMu sync.Mutex
	mu          sync.RWMutex
	// published[(ownerUserID, toolName)] = 公开契约
	published map[publishedKey]PublishedTool
}

type publishedKey struct {
	ownerUserID string
	toolName    string
}

func NewBridge(store *Store, nodeStore *nodes.Store, logger *slog.Logger) *Bridge {
	return &Bridge{
		store: store, nodes: nodeStore, logger: logger,
		published: make(map[publishedKey]PublishedTool),
	}
}

// SetPublishHandlers 注册 MCP 发布出口；启动恢复时也会调用 Publish。
func (b *Bridge) SetPublishHandlers(handlers PublishHandlers) {
	b.mu.Lock()
	b.handlers = handlers
	b.mu.Unlock()
}

// RestoreStartup 在服务启动时恢复公开契约状态并重放 Publish。
// MCP 网关先于本调用注册空工具面，恢复后工具重新可见。
func (b *Bridge) RestoreStartup(ctx context.Context, ownerUserIDs []string) error {
	for _, ownerUserID := range ownerUserIDs {
		tools, err := b.store.List(ctx, ownerUserID)
		if err != nil {
			return err
		}
		for _, tool := range tools {
			b.mu.Lock()
			b.published[publishedKey{ownerUserID, tool.Descriptor.Name}] = tool
			handlers := b.handlers
			b.mu.Unlock()
			if handlers.Publish != nil {
				handlers.Publish(ownerUserID, tool.Descriptor)
			}
		}
		// 清理 fleet 已不再提供的 stale 契约。
		if err := b.reconcileUser(ctx, ownerUserID); err != nil {
			return err
		}
	}
	return nil
}

// ObserveNodeHello 在节点握手完成后调用：观察其声明的工具并触发逐名重算。
func (b *Bridge) ObserveNodeHello(node nodes.Node, hello protocol.Hello) {
	names := make([]string, 0, len(hello.Tools))
	for _, descriptor := range hello.Tools {
		name := strings.TrimSpace(descriptor.Name)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	b.reconcileNames(node.OwnerUserID, names)
}

// ReconcileNode 在节点禁用/启用/删除后重算其全部相关工具。
func (b *Bridge) ReconcileNode(nodeID string) {
	node, err := b.nodes.Get(context.Background(), nodeID)
	if err != nil {
		// 节点已删除：用持久化快照中的 owner 触发该用户全量重算。
		b.reconcileOwnerOfDeleted(nodeID)
		return
	}
	// 禁用/启用后节点能力与快照不变，但 fleet 的 enabled provider 集变了，
	// 因此对该用户全量重算（覆盖其节点提供的所有工具名）。
	if err := b.reconcileUser(context.Background(), node.OwnerUserID); err != nil && b.logger != nil {
		b.logger.Warn("用户 fleet 重算失败", "owner", node.OwnerUserID, "error", err)
	}
}

func (b *Bridge) reconcileOwnerOfDeleted(nodeID string) {
	// 删除后 nodes 行已不在；对每个已知 owner 重算其全部已公开工具名
	// （重算以 published 目录为准，空 fleet 时走到下架分支）。
	for _, owner := range b.publishedOwners() {
		names := b.PublishedToolNames(owner)
		b.reconcileNames(owner, names)
	}
}

func (b *Bridge) publishedOwners() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	set := make(map[string]struct{}, len(b.published))
	for key := range b.published {
		set[key.ownerUserID] = struct{}{}
	}
	owners := make([]string, 0, len(set))
	for owner := range set {
		owners = append(owners, owner)
	}
	return owners
}

// PublishedToolNames 返回用户命名空间内当前公开的工具名（升序）。
func (b *Bridge) PublishedToolNames(ownerUserID string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	names := make([]string, 0)
	for key := range b.published {
		if key.ownerUserID == ownerUserID {
			names = append(names, key.toolName)
		}
	}
	sort.Strings(names)
	return names
}

// Published 返回用户命名空间内单个工具的公开契约。
func (b *Bridge) Published(ownerUserID, toolName string) (PublishedTool, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	tool, ok := b.published[publishedKey{ownerUserID, toolName}]
	return tool, ok
}

func (b *Bridge) reconcileNames(ownerUserID string, names []string) {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if err := b.reconcileOne(ownerUserID, name); err != nil && b.logger != nil {
			b.logger.Warn("工具契约收敛失败", "owner", ownerUserID, "tool", name, "error", err)
		}
	}
}

// reconcileUser 对用户命名空间内全部已知工具名（已公开 ∪ 各节点提供）重算。
func (b *Bridge) reconcileUser(ctx context.Context, ownerUserID string) error {
	names := make(map[string]struct{})
	published, err := b.store.Names(ctx, ownerUserID)
	if err != nil {
		return err
	}
	for _, name := range published {
		names[name] = struct{}{}
	}
	nodeList, err := b.nodes.ListByOwner(ctx, ownerUserID)
	if err != nil {
		return err
	}
	for _, node := range nodeList {
		descriptors, err := b.nodes.ToolDescriptors(ctx, node.ID)
		if err != nil {
			continue
		}
		for _, name := range descriptorNames(descriptors) {
			names[name] = struct{}{}
		}
	}
	flat := make([]string, 0, len(names))
	for name := range names {
		flat = append(flat, name)
	}
	b.reconcileNames(ownerUserID, flat)
	return nil
}

// reconcileOne 重算某用户命名空间内一个工具名的公开契约。
func (b *Bridge) reconcileOne(ownerUserID, toolName string) error {
	b.reconcileMu.Lock()
	defer b.reconcileMu.Unlock()

	ctx := context.Background()
	nodeList, err := b.nodes.ListByOwner(ctx, ownerUserID)
	if err != nil {
		return err
	}
	descriptors := make([]protocol.ToolDescriptor, 0)
	hasKnownProvider := false
	for _, node := range nodeList {
		if !containsCapability(node.Capabilities, toolName) {
			continue
		}
		hasKnownProvider = true
		if !node.Enabled {
			continue
		}
		nodeDescriptors, err := b.nodes.ToolDescriptors(ctx, node.ID)
		if err != nil {
			return err
		}
		descriptor, ok := findToolDescriptor(nodeDescriptors, toolName)
		if !ok {
			return errMissingDescriptor{nodeID: node.ID, tool: toolName}
		}
		descriptors = append(descriptors, descriptor)
	}
	if len(descriptors) == 0 {
		// 被禁用的节点仍属于 fleet；没有任何 enabled provider 时才下架。
		if hasKnownProvider {
			return nil
		}
		b.mu.Lock()
		key := publishedKey{ownerUserID, toolName}
		_, exists := b.published[key]
		if exists {
			if err := b.store.Delete(ctx, ownerUserID, toolName); err != nil {
				b.mu.Unlock()
				return err
			}
			delete(b.published, key)
			handlers := b.handlers
			b.mu.Unlock()
			if handlers.Retire != nil {
				handlers.Retire(ownerUserID, toolName)
			}
			return nil
		}
		b.mu.Unlock()
		return nil
	}

	merged, acceptedHashes, err := MergeFleetToolDescriptors(descriptors)
	if err != nil {
		// 不可合并：保持现有公开契约不变（保守语义，与 nexusdock 一致）。
		if errors.Is(err, ErrIncompatibleToolContract) {
			return nil
		}
		return err
	}
	contractHash, err := ToolContractHash(merged)
	if err != nil {
		return err
	}
	candidate := PublishedTool{
		Descriptor: merged, ContractHash: contractHash, AcceptedSemanticHashes: acceptedHashes,
		SourceNodeID: sourceNodeOf(descriptors), SourceVersion: sourceVersionOf(descriptors),
	}

	b.mu.Lock()
	key := publishedKey{ownerUserID, toolName}
	published, exists := b.published[key]
	descriptorChanged := !exists || !reflect.DeepEqual(published.Descriptor, candidate.Descriptor)
	if exists && published.ContractHash == candidate.ContractHash &&
		reflect.DeepEqual(published.AcceptedSemanticHashes, candidate.AcceptedSemanticHashes) && !descriptorChanged {
		b.mu.Unlock()
		return nil
	}
	if err := b.store.Save(ctx, ownerUserID, candidate); err != nil {
		b.mu.Unlock()
		return err
	}
	b.published[key] = candidate
	handlers := b.handlers
	b.mu.Unlock()
	if handlers.Publish != nil && descriptorChanged {
		handlers.Publish(ownerUserID, merged)
	}
	return nil
}

func sourceNodeOf(descriptors []protocol.ToolDescriptor) string    { return "" }
func sourceVersionOf(descriptors []protocol.ToolDescriptor) string { return "" }

type errMissingDescriptor struct {
	nodeID string
	tool   string
}

func (e errMissingDescriptor) Error() string {
	return "node " + e.nodeID + " does not provide tool descriptor " + e.tool
}

func descriptorNames(descriptors []protocol.ToolDescriptor) []string {
	names := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if name := strings.TrimSpace(descriptor.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}
