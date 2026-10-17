package httpx

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 节点注册全链路：用户生成配对码 → AgentDock 兑换 → WebSocket 连接 →
// 用户列表显示在线 → 禁用立即断线 → 删除后 token 永久失效。
func TestNodePairingLifecycle(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if _, err := e.svc.InitializeAdmin(ctx, "root@example.com", strongPassword); err != nil {
		t.Fatalf("init admin: %v", err)
	}
	adminToken, adminCSRF := e.loginAs(t, "root@example.com", strongPassword)

	// 管理员建普通用户。
	resp, body := e.do(t, "POST", "/v1/admin/users", adminToken, adminCSRF, map[string]string{
		"username": "carol@example.com", "password": "initial-pass-123456",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user: %d %v", resp.StatusCode, body)
	}
	userToken, userCSRF := e.loginAndChangePassword(t, "carol@example.com", "initial-pass-123456", "carol-changed-123456")

	// 用户生成配对码。
	resp, body = e.do(t, "POST", "/v1/nodes/pairing-codes", userToken, userCSRF, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("pairing code: %d %v", resp.StatusCode, body)
	}
	code, _ := body["code"].(string)
	if code == "" {
		t.Fatalf("pairing code missing: %v", body)
	}

	// AgentDock 兑换（无会话，公网入口）。
	pairResp, pairBody := e.do(t, "POST", "/v1/nodes/pair", "", "", map[string]string{
		"code": code, "device_id": "device-carol-01", "name": "carol-dev",
	})
	if pairResp.StatusCode != http.StatusCreated {
		t.Fatalf("pair: %d %v", pairResp.StatusCode, pairBody)
	}
	nodeRaw, _ := pairBody["node"].(map[string]any)
	nodeID, _ := nodeRaw["id"].(string)
	deviceToken, _ := pairBody["device_token"].(string)
	if nodeID == "" || deviceToken == "" {
		t.Fatalf("pair response incomplete: %v", pairBody)
	}

	// WebSocket 连接并完成握手。
	wsURL := "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/v1/nodes/connect"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+deviceToken)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{
		"type": "node.hello", "protocol_version": "2",
		"hello": map[string]any{
			"device_id": "device-carol-01", "version": "1.0.0", "protocol_version": "2",
			"os": "darwin", "arch": "arm64", "capabilities": []string{"browser_open"},
		},
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	// 等待 node.ready 确认握手完成（Online 判定以 ready 为准）。
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var readyMsg map[string]any
	if err := conn.ReadJSON(&readyMsg); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if readyMsg["type"] != "node.ready" {
		t.Fatalf("expected node.ready, got %v", readyMsg)
	}

	// 用户节点列表必须显示在线。ready 已确认，但 hub 登记与查询接口
	// 存在毫秒级窗口；慢架构（QEMU riscv64）上需要轮询等待。
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, body = e.do(t, "GET", "/v1/nodes", userToken, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list nodes: %d", resp.StatusCode)
		}
		nodesList, _ := body["nodes"].([]any)
		if len(nodesList) != 1 {
			t.Fatalf("expected 1 node, got %v", body)
		}
		first, _ := nodesList[0].(map[string]any)
		if first["online"] == true && first["id"] == nodeID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node never became online: %v", first)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 管理员（另一用户）不可见该节点。
	resp, body = e.do(t, "GET", "/v1/nodes", adminToken, "", nil)
	adminNodes, _ := body["nodes"].([]any)
	if resp.StatusCode != http.StatusOK || len(adminNodes) != 0 {
		t.Fatalf("admin must not see carol's nodes: %d %v", resp.StatusCode, adminNodes)
	}

	// 跨租户"够不着"：管理员同样不能对 carol 的节点执行启停/删除
	// （他看得见列表是空的，但直接按 id 敲 URL 也必须一律 NotFound）。
	resp, _ = e.do(t, "POST", "/v1/nodes/"+nodeID+"/disable", adminToken, adminCSRF, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant disable must be 404, got %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "DELETE", "/v1/nodes/"+nodeID, adminToken, adminCSRF, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant delete must be 404, got %d", resp.StatusCode)
	}

	// 禁用：连接必须立即断开（DESIGN §3.1/§4.3）。
	resp, body = e.do(t, "POST", "/v1/nodes/"+nodeID+"/disable", userToken, userCSRF, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable node: %d %v", resp.StatusCode, body)
	}
	// 踢线表现为服务端关闭连接：读应当立刻失败。
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
		if time.Now().After(time.Now().Add(9 * time.Second)) {
			t.Fatal("connection must be closed after node is disabled")
		}
	}
	// 禁用后离线。
	resp, body = e.do(t, "GET", "/v1/nodes", userToken, "", nil)
	nodesList, _ := body["nodes"].([]any)
	first, _ := nodesList[0].(map[string]any)
	if first["online"] != false {
		t.Fatalf("disabled node must be offline: %v", first)
	}
	// 禁用后可恢复（可逆语义，区别于删除）。
	resp, _ = e.do(t, "POST", "/v1/nodes/"+nodeID+"/enable", userToken, userCSRF, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable node: %d", resp.StatusCode)
	}

	// 删除：device token 随行删除，永久失效（不可逆）。
	resp, _ = e.do(t, "DELETE", "/v1/nodes/"+nodeID, userToken, userCSRF, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete node: %d", resp.StatusCode)
	}
	dialHeader := http.Header{}
	dialHeader.Set("Authorization", "Bearer "+deviceToken)
	if _, _, err := websocket.DefaultDialer.Dial(wsURL, dialHeader); err == nil {
		t.Fatal("device token must be permanently invalid after node deletion")
	}
	// 重复删除返回 404（记录确已不存在）。
	resp, _ = e.do(t, "DELETE", "/v1/nodes/"+nodeID, userToken, userCSRF, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleting an already-deleted node must be 404, got %d", resp.StatusCode)
	}
}

// 同一 device_id 重复配对返回 409，而不是 500。
// ErrNodeExists 是裸 errors.New，映射漏掉时会落 default 分支变成 500，
// 误导 AgentDock 无意义重试。
func TestDuplicatePairingReturnsConflict(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if _, err := e.svc.InitializeAdmin(ctx, "root@example.com", strongPassword); err != nil {
		t.Fatalf("init admin: %v", err)
	}
	adminToken, adminCSRF := e.loginAs(t, "root@example.com", strongPassword)
	resp, body := e.do(t, "POST", "/v1/admin/users", adminToken, adminCSRF, map[string]string{
		"username": "dave@example.com", "password": "initial-pass-123456",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user: %d %v", resp.StatusCode, body)
	}
	userToken, userCSRF := e.loginAndChangePassword(t, "dave@example.com", "initial-pass-123456", "dave-changed-123456")

	pair := func(deviceID string) int {
		resp, body := e.do(t, "POST", "/v1/nodes/pairing-codes", userToken, userCSRF, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("pairing code: %d %v", resp.StatusCode, body)
		}
		code, _ := body["code"].(string)
		pairResp, _ := e.do(t, "POST", "/v1/nodes/pair", "", "", map[string]string{
			"code": code, "device_id": deviceID, "name": "dave-dev",
		})
		return pairResp.StatusCode
	}
	if status := pair("device-dave-01"); status != http.StatusCreated {
		t.Fatalf("first pair must succeed, got %d", status)
	}
	if status := pair("device-dave-01"); status != http.StatusConflict {
		t.Fatalf("duplicate device_id must return 409, got %d", status)
	}
}
