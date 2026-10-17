// 跨租户安全自测：越权访问、禁用即时性、配对码复用、token 伪造。
// 每条对应设计规格 docs/DESIGN.md §6 的一条安全约束。
package e2e

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/uvwt/agenthub/internal/auth"
	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/nodes"
)

type hub struct {
	db    *sql.DB
	auth  *auth.Service
	nodes *nodes.Store
}

func newHub(t *testing.T) *hub {
	t.Helper()
	ctx := context.Background()
	db, err := core.OpenSQLite(ctx, filepath.Join(t.TempDir(), "hub.db"), 2)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := core.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// 预置用户（绕过 auth 层以聚焦 nodes 域的隔离语义）。
	for _, u := range []string{"usr_alice", "usr_bob"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users(id, username, created_at, updated_at)
			VALUES(?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, u, u+"@example.com"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return &hub{db: db, auth: auth.NewService(db), nodes: nodes.NewStore(db)}
}

// 越权：Bob 不能用任何节点 API 触达 Alice 的节点。
func TestCrossTenantNodeIsolation(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	code, _ := h.nodes.CreatePairingCode(ctx, "usr_alice")
	aliceNode, err := h.nodes.Pair(ctx, nodes.PairInput{Code: code.Code, DeviceID: "device-alice-x", Name: "a"})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	// GetOwned / SetEnabled / Delete 全部 NotFound。
	if _, err := h.nodes.GetOwned(ctx, "usr_bob", aliceNode.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant get must be NotFound, got %v", err)
	}
	if _, err := h.nodes.SetEnabled(ctx, "usr_bob", aliceNode.ID, false); !isNotFound(err) {
		t.Fatalf("cross-tenant disable must be NotFound, got %v", err)
	}
	if err := h.nodes.Delete(ctx, "usr_bob", aliceNode.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant delete must be NotFound, got %v", err)
	}
}

// 禁用即时性：禁用用户后其 MCP token 与节点 token 全部立即失效。
func TestDisableUserImmediate(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	if _, err := h.auth.InitializeAdmin(ctx, "root@example.com", "Root7strongKp3z"); err != nil {
		t.Fatalf("init: %v", err)
	}
	alice, err := h.auth.CreateUser(ctx, "root", "alice@example.com", "Alice7xKp2mQ9z")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	token, _ := h.auth.IssueMCPToken(ctx, alice.ID)
	code, _ := h.nodes.CreatePairingCode(ctx, alice.ID)
	node, err := h.nodes.Pair(ctx, nodes.PairInput{Code: code.Code, DeviceID: "device-alice-d", Name: "a"})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	if err := h.auth.DisableUser(ctx, "root", alice.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// MCP token 立即失效。
	if _, err := h.auth.AuthenticateMCPToken(ctx, token); err == nil {
		t.Fatal("disabled user's MCP token must be rejected immediately")
	}
	// device token 立即失效（JOIN users 过滤 status）。
	if _, err := h.nodes.AuthenticateDeviceToken(ctx, node.DeviceToken); err == nil {
		t.Fatal("disabled user's device token must be rejected immediately")
	}
	// 配对码立即作废（DESIGN §4.3）。
	// 这里断言的是"拿真实签发的码去兑换"，而不是"伪造 token 认证失败"——
	// 后者无论实现对错都会通过，等于没测。
	//
	// 需要两枚码：一枚在禁用后签发（Pair 侧的用户状态校验拦下），
	// 一枚在禁用前签发（RevokeOwnerAccess 显式作废拦下）。两条路径都要覆盖。
	lateCode, err := h.nodes.CreatePairingCode(ctx, alice.ID)
	if err != nil {
		t.Fatalf("create late pairing code: %v", err)
	}
	if _, err := h.nodes.Pair(ctx, nodes.PairInput{Code: lateCode.Code, DeviceID: "device-late-01", Name: "late"}); !errors.Is(err, nodes.ErrPairingCodeInvalid) {
		t.Fatalf("code issued after disable must be rejected as invalid, got %v", err)
	}
	// 禁用前签发的存量码也必须已被作废（RevokeOwnerAccess 是禁用路径的清理动作，
	// 它还必须枚举出用户全部节点供调用方踢线）。
	nodeIDs, err := h.nodes.RevokeOwnerAccess(ctx, alice.ID)
	if err != nil {
		t.Fatalf("revoke owner access: %v", err)
	}
	if len(nodeIDs) != 1 || nodeIDs[0] != node.ID {
		t.Fatalf("revoke must enumerate the user's nodes for kick-out, got %v", nodeIDs)
	}
	var stillActive int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pairing_codes
		WHERE owner_user_id = ? AND used_at IS NULL`, alice.ID).Scan(&stillActive); err != nil {
		t.Fatalf("count active pairing codes: %v", err)
	}
	if stillActive != 0 {
		t.Fatalf("disable must void all unused pairing codes, %d still active", stillActive)
	}
	// 作废是持久的：恢复用户后旧码不能复活。
	if err := h.auth.EnableUser(ctx, "root", alice.ID); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := h.nodes.Pair(ctx, nodes.PairInput{Code: code.Code, DeviceID: "device-revive-01", Name: "revive"}); err == nil {
		t.Fatal("voided pairing code must not become usable after re-enabling the user")
	}
}

// 配对码复用与伪造。
func TestPairingCodeReuseAndForgery(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	code, _ := h.nodes.CreatePairingCode(ctx, "usr_alice")
	if _, err := h.nodes.Pair(ctx, nodes.PairInput{Code: code.Code, DeviceID: "device-a", Name: "a"}); err != nil {
		t.Fatalf("first pair: %v", err)
	}
	// 复用拒绝。
	if _, err := h.nodes.Pair(ctx, nodes.PairInput{Code: code.Code, DeviceID: "device-b", Name: "b"}); err == nil {
		t.Fatal("pairing code reuse must fail")
	}
	// 同 device_id 重复配对拒绝（配对码正确也不行）。
	code2, _ := h.nodes.CreatePairingCode(ctx, "usr_alice")
	if _, err := h.nodes.Pair(ctx, nodes.PairInput{Code: code2.Code, DeviceID: "device-a", Name: "a2"}); err == nil {
		t.Fatal("duplicate device_id must fail")
	}
	// 伪造 token 拒绝。
	if _, err := h.nodes.AuthenticateDeviceToken(ctx, "Bearer fake-token"); err == nil {
		t.Fatal("forged device token must fail")
	}
}

// 配对码并发双兑换：单次语义必须在并发下成立。
//
// 这条锁的是 UPDATE ... used_at IS NULL 的 RowsAffected 断言：两个事务可能都
// 通过前面的 SELECT（SQLite 读不阻塞），若不检查命中行数，第二个会静默更新
// 0 行后继续建节点，一个码产出两个节点 + 两个 device token。
func TestPairingCodeConcurrentSingleUse(t *testing.T) {
	h := newHub(t)
	ctx := context.Background()
	code, err := h.nodes.CreatePairingCode(ctx, "usr_alice")
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}

	const racers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, racers)
	deviceIDs := []string{"device-race-01", "device-race-02", "device-race-03", "device-race-04"}
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = h.nodes.Pair(ctx, nodes.PairInput{Code: code.Code, DeviceID: deviceIDs[i], Name: "racer"})
		}(i)
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for i, err := range results {
		if err == nil {
			succeeded++
			continue
		}
		// 并发失败方必须拿到业务错误，而不是泄漏的原始 SQLite 错误。
		if !errors.Is(err, nodes.ErrPairingCodeInvalid) {
			t.Fatalf("racer %d failed with unexpected error (want ErrPairingCodeInvalid): %v", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("exactly one racer must succeed, got %d", succeeded)
	}
	var nodeCount int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE owner_user_id = 'usr_alice'`).Scan(&nodeCount); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodeCount != 1 {
		t.Fatalf("a single pairing code must produce exactly one node, got %d", nodeCount)
	}
}

func isNotFound(err error) bool {
	return core.ErrorCodeOf(err) == core.CodeNotFound || err != nil && err.Error() == nodes.ErrNodeNotFound.Error()
}
