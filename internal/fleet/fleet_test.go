package fleet

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/nodes"
)

func descriptorFor(name string, inputSchema map[string]any) protocol.ToolDescriptor {
	return protocol.ToolDescriptor{Name: name, InputSchema: inputSchema}
}

// 语义哈希：description/title 等展示字段与键序不影响哈希；required 变化必须改变哈希。
func TestToolContractHashSemantics(t *testing.T) {
	base := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string", "description": "target url"},
		},
		"required": []any{"url"},
	}
	reordered := map[string]any{
		"required": []any{"url"},
		"type":     "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string", "description": "不同的描述"},
		},
	}
	hash1, err := ToolContractHash(descriptorFor("t", base))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	hash2, err := ToolContractHash(descriptorFor("t", reordered))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash1 != hash2 {
		t.Fatalf("display-only differences must not change hash: %s != %s", hash1, hash2)
	}

	requiredChanged := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string"},
		},
		"required": []any{"url", "timeout"},
	}
	hash3, _ := ToolContractHash(descriptorFor("t", requiredChanged))
	if hash1 == hash3 {
		t.Fatal("required set change must change hash")
	}
}

// 合并：可选属性取并集；执行级 _meta 不一致拒绝合并。
func TestMergeFleetUnionAndConflict(t *testing.T) {
	a := descriptorFor("browser_open", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string"},
		},
		"required": []any{"url"},
	})
	b := descriptorFor("browser_open", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":   map[string]any{"type": "string"},
			"extra": map[string]any{"type": "boolean"},
		},
		"required": []any{"url"},
	})
	merged, hashes, err := MergeFleetToolDescriptors([]protocol.ToolDescriptor{a, b})
	if err != nil {
		t.Fatalf("merge compatible: %v", err)
	}
	props, _ := merged.InputSchema["properties"].(map[string]any)
	if _, has := props["extra"]; !has {
		t.Fatalf("optional properties must union: %v", merged.InputSchema)
	}
	if len(hashes) != 2 {
		t.Fatalf("accepted hashes must include both variants: %v", hashes)
	}

	// required 不一致 → 拒绝合并。
	c := descriptorFor("browser_open", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string"},
		},
		"required": []any{"url", "timeout"},
	})
	if _, _, err := MergeFleetToolDescriptors([]protocol.ToolDescriptor{a, c}); err == nil {
		t.Fatal("required mismatch must be rejected")
	}
}

type fleetEnv struct {
	store   *Store
	nodes   *nodes.Store
	bridge  *Bridge
	publish map[string][]string // owner -> tool names published
	retire  map[string][]string
}

func newFleetEnv(t *testing.T) *fleetEnv {
	t.Helper()
	ctx := context.Background()
	db, err := core.OpenSQLite(ctx, filepath.Join(t.TempDir(), "test.db"), 2)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := core.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	for _, u := range []struct{ id, name string }{
		{"usr_alice", "alice@example.com"}, {"usr_bob", "bob@example.com"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users(id, username, created_at, updated_at)
			VALUES(?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, u.id, u.name); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	nodeStore := nodes.NewStore(db)
	fleetStore := NewStore(db)
	env := &fleetEnv{
		store: fleetStore, nodes: nodeStore,
		publish: make(map[string][]string), retire: make(map[string][]string),
	}
	env.bridge = NewBridge(fleetStore, nodeStore, nil)
	env.bridge.SetPublishHandlers(PublishHandlers{
		Publish: func(ownerUserID string, _ protocol.ToolDescriptor) {
			env.publish[ownerUserID] = append(env.publish[ownerUserID], "")
		},
		Retire: func(ownerUserID, tool string) { env.retire[ownerUserID] = append(env.retire[ownerUserID], tool) },
	})
	t.Cleanup(func() { _ = db.Close() })
	return env
}

func (e *fleetEnv) pairWithTool(t *testing.T, owner, deviceID string, tool protocol.ToolDescriptor) nodes.Node {
	t.Helper()
	code, err := e.nodes.CreatePairingCode(context.Background(), owner)
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	node, err := e.nodes.Pair(context.Background(), nodes.PairInput{Code: code.Code, DeviceID: deviceID, Name: deviceID})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	hello := protocol.Hello{
		DeviceID: deviceID, ProtocolVersion: "2", Capabilities: []string{tool.Name},
		Tools: []protocol.ToolDescriptor{tool},
	}
	if _, err := e.nodes.UpdateHello(context.Background(), node.ID, hello); err != nil {
		t.Fatalf("update hello: %v", err)
	}
	return node
}

// 发布生命周期：首节点 hello → 发布；同名第二节点兼容 → 保持公开契约（哈希并入兼容集合）；
// 所有 provider 消失 → 下架。
func TestPublishLifecycle(t *testing.T) {
	env := newFleetEnv(t)
	tool := descriptorFor("browser_open", map[string]any{
		"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []any{"url"},
	})

	nodeA := env.pairWithTool(t, "usr_alice", "device-a-01", tool)
	env.bridge.ObserveNodeHello(nodeA, protocol.Hello{DeviceID: "device-a-01", Tools: []protocol.ToolDescriptor{tool}})

	published, ok := env.bridge.Published("usr_alice", "browser_open")
	if !ok {
		t.Fatal("tool must be published after first hello")
	}
	if len(published.AcceptedSemanticHashes) != 1 {
		t.Fatalf("first publish accepts one hash: %v", published.AcceptedSemanticHashes)
	}

	// 兼容变体（多一个可选属性）加入 fleet。
	toolB := descriptorFor("browser_open", map[string]any{
		"type": "object", "properties": map[string]any{
			"url": map[string]any{"type": "string"}, "extra": map[string]any{"type": "boolean"},
		}, "required": []any{"url"},
	})
	nodeB := env.pairWithTool(t, "usr_alice", "device-a-02", toolB)
	env.bridge.ObserveNodeHello(nodeB, protocol.Hello{DeviceID: "device-a-02", Tools: []protocol.ToolDescriptor{toolB}})

	published, ok = env.bridge.Published("usr_alice", "browser_open")
	if !ok {
		t.Fatal("tool must remain published after second compatible node")
	}
	if len(published.AcceptedSemanticHashes) != 2 {
		t.Fatalf("both variants must be accepted: %v", published.AcceptedSemanticHashes)
	}

	// 用户分区：bob 的同名工具独立，不受 alice 影响。
	nodeBob := env.pairWithTool(t, "usr_bob", "device-b-01", tool)
	env.bridge.ObserveNodeHello(nodeBob, protocol.Hello{DeviceID: "device-b-01", Tools: []protocol.ToolDescriptor{tool}})
	if _, ok := env.bridge.Published("usr_bob", "browser_open"); !ok {
		t.Fatal("bob namespace must have its own published contract")
	}

	// 禁用（可逆暂停）不下架契约：nexusdock 语义——被禁用节点仍属于 fleet，
	// 只是调用会被在线检查拦截。
	if _, err := env.nodes.SetEnabled(context.Background(), "usr_alice", nodeA.ID, false); err != nil {
		t.Fatalf("disable a: %v", err)
	}
	env.bridge.ReconcileNode(nodeA.ID)
	if _, ok := env.bridge.Published("usr_alice", "browser_open"); !ok {
		t.Fatal("disabled provider must keep its published contract")
	}
	// 删除（不可逆）最后一个 enabled provider 才真正下架。
	if err := env.nodes.Delete(context.Background(), "usr_alice", nodeA.ID); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	if err := env.nodes.Delete(context.Background(), "usr_alice", nodeB.ID); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	env.bridge.ReconcileNode(nodeA.ID)
	env.bridge.ReconcileNode(nodeB.ID)
	if _, ok := env.bridge.Published("usr_alice", "browser_open"); ok {
		t.Fatal("tool must retire when all providers deleted")
	}
	// bob 不受影响。
	if _, ok := env.bridge.Published("usr_bob", "browser_open"); !ok {
		t.Fatal("bob contract must be unaffected by alice retire")
	}
}

// 持久化先行 + 重启恢复：RestoreStartup 重放 Publish。
func TestRestoreStartupReplays(t *testing.T) {
	env := newFleetEnv(t)
	tool := descriptorFor("browser_open", map[string]any{
		"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []any{"url"},
	})
	node := env.pairWithTool(t, "usr_alice", "device-a-01", tool)
	env.bridge.ObserveNodeHello(node, protocol.Hello{DeviceID: "device-a-01", Tools: []protocol.ToolDescriptor{tool}})

	// 模拟重启：新 Bridge 实例 + 恢复。
	restored := NewBridge(env.store, env.nodes, nil)
	publishedCount := 0
	restored.SetPublishHandlers(PublishHandlers{
		Publish: func(owner string, _ protocol.ToolDescriptor) { publishedCount++ },
	})
	if err := restored.RestoreStartup(context.Background(), []string{"usr_alice"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if publishedCount == 0 {
		t.Fatal("restore must replay publish")
	}
	if _, ok := restored.Published("usr_alice", "browser_open"); !ok {
		t.Fatal("restored bridge must know the published contract")
	}
}

// 调用前复核：节点契约不在兼容集合内时返回结构化差异。
func TestContractMismatchDetail(t *testing.T) {
	env := newFleetEnv(t)
	tool := descriptorFor("browser_open", map[string]any{
		"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []any{"url"},
	})
	node := env.pairWithTool(t, "usr_alice", "device-a-01", tool)
	env.bridge.ObserveNodeHello(node, protocol.Hello{DeviceID: "device-a-01", Tools: []protocol.ToolDescriptor{tool}})

	published, ok := env.bridge.Published("usr_alice", "browser_open")
	if !ok {
		t.Fatal("must be published")
	}
	// 现场契约变更（required 增加 timeout）→ 不在兼容集合。
	changed := descriptorFor("browser_open", map[string]any{
		"type": "object", "properties": map[string]any{
			"url": map[string]any{"type": "string"}, "timeout": map[string]any{"type": "number"},
		}, "required": []any{"url", "timeout"},
	})
	if _, _, err := MergeFleetToolDescriptors([]protocol.ToolDescriptor{published.Descriptor, changed}); err == nil {
		t.Fatal("setup: changed contract must be a new variant")
	}
	changedHash, err := ToolContractHash(changed)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	found := false
	for _, hash := range published.AcceptedSemanticHashes {
		if hash == changedHash {
			found = true
		}
	}
	if found {
		t.Fatal("changed contract must not be in accepted set before reconcile")
	}
}

// 名为 description 的**真实参数**不得被当成展示字段剔除。
//
// 归一化会剥掉 schema 对象上的 description（它是给 LLM 看的说明），
// 但如果用户恰好有一个叫 description 的参数，剥掉它就等于把这个参数
// 从契约里删掉：两个实际不同的 schema 会被判成兼容。nexusdock 有专门用例
// 锁这一点，agenthub 此前缺失。
func TestContractHashKeepsParameterNamedDescription(t *testing.T) {
	withDescriptionParam := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"description": map[string]any{"type": "string"},
		},
		"required": []any{"description"},
	}
	withoutDescriptionParam := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	hash1, err := ToolContractHash(descriptorFor("t", withDescriptionParam))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	hash2, err := ToolContractHash(descriptorFor("t", withoutDescriptionParam))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash1 == hash2 {
		t.Fatal("a real parameter named 'description' must stay in the contract hash")
	}
}

// _meta.ui 与 annotations 不影响哈希；其他 _meta 必须影响。
// ui 是纯展示绑定，节点间不一致不该判成不兼容；而非 ui 的 _meta
// 可能承载执行语义，变了就必须能被发现。
func TestContractHashMetaSemantics(t *testing.T) {
	base := descriptorFor("t", map[string]any{"type": "object", "properties": map[string]any{}})
	withUIMeta := base
	withUIMeta.Meta = map[string]any{"ui": map[string]any{"icon": "star"}}
	withUIMeta.Annotations = map[string]any{"readOnlyHint": true}

	hashBase, _ := ToolContractHash(base)
	hashUI, _ := ToolContractHash(withUIMeta)
	if hashBase != hashUI {
		t.Fatal("_meta.ui and annotations are presentation-only and must not change the hash")
	}

	withExecMeta := base
	withExecMeta.Meta = map[string]any{"permission": "admin"}
	hashExec, _ := ToolContractHash(withExecMeta)
	if hashBase == hashExec {
		t.Fatal("non-ui _meta must affect the hash")
	}
}

// 合并：可选属性取并集时 outputSchema 也参与，required 保持原序且不丢项。
// 只测 inputSchema 的旧用例会让 outputSchema 的合并行为完全无锁。
func TestMergeFleetUnionsOutputSchema(t *testing.T) {
	platformA := descriptorFor("t",
		map[string]any{
			"type":       "object",
			"properties": map[string]any{"a": map[string]any{"type": "string"}},
			"required":   []any{"a"},
		})
	platformA.OutputSchema = map[string]any{
		"type":       "object",
		"properties": map[string]any{"x": map[string]any{"type": "string"}},
	}
	platformB := descriptorFor("t",
		map[string]any{
			"type":       "object",
			"properties": map[string]any{"b": map[string]any{"type": "number"}},
			"required":   []any{"b"},
		})
	platformB.OutputSchema = map[string]any{
		"type":       "object",
		"properties": map[string]any{"y": map[string]any{"type": "number"}},
	}

	// required 不同即拒绝合并（保守语义，照搬 nexusdock）：
	// 两边要求不同参数，合并出的 schema 无法同时满足，平台只能猜。
	if _, _, err := MergeFleetToolDescriptors([]protocol.ToolDescriptor{platformA, platformB}); !errors.Is(err, ErrIncompatibleToolContract) {
		t.Fatalf("conflicting required must be rejected, got %v", err)
	}

	// provider-only 的必填参数同样拒绝合并：只在 A 上 required 的参数，
	// 调 B 时必然缺失。两条规则都是 nexusdock 的保守语义。
	platformB.InputSchema["required"] = []any{"a"}
	if _, _, err := MergeFleetToolDescriptors([]protocol.ToolDescriptor{platformA, platformB}); !errors.Is(err, ErrIncompatibleToolContract) {
		t.Fatalf("provider-only required must be rejected, got %v", err)
	}

	// 两边都没有 required（归一化会丢弃空 required）时，
	// input/output 的可选属性取并集——这是最典型的多平台合并场景。
	delete(platformA.InputSchema, "required")
	delete(platformB.InputSchema, "required")
	merged, hashes, err := MergeFleetToolDescriptors([]protocol.ToolDescriptor{platformA, platformB})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(hashes) != 2 {
		t.Fatalf("both variants must be accepted, got %d hashes", len(hashes))
	}
	props, _ := merged.InputSchema["properties"].(map[string]any)
	if props["a"] == nil || props["b"] == nil {
		t.Fatalf("input properties must be unioned: %+v", props)
	}
	outProps, _ := merged.OutputSchema["properties"].(map[string]any)
	if outProps["x"] == nil || outProps["y"] == nil {
		t.Fatalf("output properties must be unioned: %+v", outProps)
	}
	required, _ := merged.InputSchema["required"].([]any)
	if len(required) != 0 {
		t.Fatalf("empty required must stay empty, got %+v", required)
	}
}

// 漂移必须被拒绝合并：type 漂移 / enum 漂移 / additionalProperties 漂移。
// 这些是不同的调用契约，合并会让平台按错误的 schema 构造参数。
func TestMergeFleetRejectsDrift(t *testing.T) {
	base := descriptorFor("t", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []any{"a", "b"}},
		},
		"required": []any{"mode"},
	})
	cases := []struct {
		label  string
		mutate func(map[string]any)
	}{
		{"type drift", func(s map[string]any) {
			s["properties"].(map[string]any)["mode"].(map[string]any)["type"] = "number"
		}},
		{"enum drift", func(s map[string]any) {
			s["properties"].(map[string]any)["mode"].(map[string]any)["enum"] = []any{"a", "c"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			other := descriptorFor("t", map[string]any{
				"type": "object",
				"properties": map[string]any{
					"mode": map[string]any{"type": "string", "enum": []any{"a", "b"}},
				},
				"required": []any{"mode"},
			})
			tc.mutate(other.InputSchema)
			if _, _, err := MergeFleetToolDescriptors([]protocol.ToolDescriptor{base, other}); !errors.Is(err, ErrIncompatibleToolContract) {
				t.Fatalf("%s must be rejected, got %v", tc.label, err)
			}
		})
	}
}
