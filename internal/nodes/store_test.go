package nodes

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/core"
)

func helloFor(deviceID string) protocol.Hello {
	return protocol.Hello{
		DeviceID:        deviceID,
		Version:         "1.0.0",
		ProtocolVersion: protocol.ConnectionProtocolVersion,
		OS:              "darwin", Arch: "arm64",
		Capabilities:     []string{"browser_open"},
		ToolContractHash: "hash-1",
		Tools: []protocol.ToolDescriptor{{
			Name:        "browser_open",
			Title:       "Open Browser",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}},
		}},
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	db, err := core.OpenSQLite(ctx, filepath.Join(t.TempDir(), "test.db"), 2)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := core.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	// 预置用户满足 pairing_codes/nodes 对 users 的外键约束。
	for _, u := range []struct{ id, name string }{
		{"usr_alice", "alice@example.com"}, {"usr_bob", "bob@example.com"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users(id, username, created_at, updated_at)
			VALUES(?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, u.id, u.name); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db)
}

// 完整配对流程：生成码→兑换→节点归属发起用户。
func TestPairOwnershipFollowsCode(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.CreatePairingCode(ctx, ""); err == nil {
		t.Fatal("pairing code requires owner")
	}
	code, err := store.CreatePairingCode(ctx, "usr_alice")
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}
	node, err := store.Pair(ctx, PairInput{Code: code.Code, DeviceID: "device-alice-01", Name: "alice-dev"})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if node.OwnerUserID != "usr_alice" {
		t.Fatalf("node must belong to code owner, got %s", node.OwnerUserID)
	}
	if node.DeviceToken == "" {
		t.Fatal("pair must return device token once")
	}

	// token 能换回节点身份。
	identified, err := store.AuthenticateDeviceToken(ctx, node.DeviceToken)
	if err != nil {
		t.Fatalf("authenticate device token: %v", err)
	}
	if identified.ID != node.ID || identified.OwnerUserID != "usr_alice" {
		t.Fatalf("token must identify the paired node: %+v", identified)
	}

	// 配对码单次：第二次兑换必须失败。
	if _, err := store.Pair(ctx, PairInput{Code: code.Code, DeviceID: "device-alice-02", Name: "again"}); !errors.Is(err, ErrPairingCodeInvalid) {
		t.Fatalf("reused pairing code must be invalid, got %v", err)
	}
	// 错误码。
	if _, err := store.Pair(ctx, PairInput{Code: "pair_wrong", DeviceID: "device-alice-03", Name: "bad"}); !errors.Is(err, ErrPairingCodeInvalid) {
		t.Fatalf("wrong pairing code must be invalid, got %v", err)
	}
	// 非法设备 ID。
	if _, err := store.Pair(ctx, PairInput{Code: mustCode(t, store, "usr_alice"), DeviceID: "bad id!", Name: "x"}); err == nil {
		t.Fatal("invalid device id must be rejected")
	}
}

func mustCode(t *testing.T, store *Store, owner string) string {
	t.Helper()
	code, err := store.CreatePairingCode(context.Background(), owner)
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	return code.Code
}

// 跨用户隔离：Bob 不能读取、禁用或删除 Alice 的节点；错误一律 NotFound 不泄露存在性。
func TestCrossUserIsolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	code, err := store.CreatePairingCode(ctx, "usr_alice")
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}
	node, err := store.Pair(ctx, PairInput{Code: code.Code, DeviceID: "device-alice-01", Name: "alice-dev"})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	if _, err := store.GetOwned(ctx, "usr_bob", node.ID); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("bob reading alice's node must be NotFound, got %v", err)
	}
	if _, err := store.SetEnabled(ctx, "usr_bob", node.ID, false); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("bob disabling alice's node must be NotFound, got %v", err)
	}
	if err := store.Delete(ctx, "usr_bob", node.ID); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("bob deleting alice's node must be NotFound, got %v", err)
	}
	list, err := store.ListByOwner(ctx, "usr_bob")
	if err != nil || len(list) != 0 {
		t.Fatalf("bob's node list must be empty: %v %v", list, err)
	}
	// alice 自己可见。
	if _, err := store.GetOwned(ctx, "usr_alice", node.ID); err != nil {
		t.Fatalf("alice must see own node: %v", err)
	}
}

// 禁用节点的 device token 立即拒连；重新启用后恢复。
func TestDisabledNodeRejectsToken(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	code, _ := store.CreatePairingCode(ctx, "usr_alice")
	node, err := store.Pair(ctx, PairInput{Code: code.Code, DeviceID: "device-alice-01", Name: "dev"})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	if _, err := store.SetEnabled(ctx, "usr_alice", node.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := store.AuthenticateDeviceToken(ctx, node.DeviceToken); !errors.Is(err, ErrNodeDisabled) {
		t.Fatalf("disabled node token must be rejected, got %v", err)
	}
	if _, err := store.SetEnabled(ctx, "usr_alice", node.ID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := store.AuthenticateDeviceToken(ctx, node.DeviceToken); err != nil {
		t.Fatalf("re-enabled node token must work: %v", err)
	}
}

// 删除节点后 token 永久失效（必须重新配对）。
func TestDeleteRevokesToken(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	code, _ := store.CreatePairingCode(ctx, "usr_alice")
	node, err := store.Pair(ctx, PairInput{Code: code.Code, DeviceID: "device-alice-01", Name: "dev"})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if err := store.Delete(ctx, "usr_alice", node.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.AuthenticateDeviceToken(ctx, node.DeviceToken); err == nil {
		t.Fatal("deleted node token must be invalid forever")
	}
}

// UpdateHello 校验设备身份一致性并持久化快照。
func TestUpdateHelloSnapshot(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	code, err := store.CreatePairingCode(ctx, "usr_alice")
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}
	node, err := store.Pair(ctx, PairInput{Code: code.Code, DeviceID: "device-alice-01", Name: "dev"})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	hello := helloFor(node.DeviceID)
	if _, err := store.UpdateHello(ctx, node.ID, hello); err != nil {
		t.Fatalf("update hello: %v", err)
	}
	descriptors, err := store.ToolDescriptors(ctx, node.ID)
	if err != nil || len(descriptors) != 1 || descriptors[0].Name != "browser_open" {
		t.Fatalf("tool descriptors must be persisted: %v %v", descriptors, err)
	}
	// 设备身份不一致必须拒绝。
	badHello := helloFor("device-someone-else")
	if _, err := store.UpdateHello(ctx, node.ID, badHello); err == nil {
		t.Fatal("hello with mismatched device id must be rejected")
	}
}
