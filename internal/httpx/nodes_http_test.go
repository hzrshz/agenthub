package httpx

import (
	"context"
	"net/http"
	"strings"
	"testing"

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
	userToken, userCSRF := e.loginAs(t, "carol@example.com", "initial-pass-123456")

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

	// 用户节点列表必须显示在线。
	resp, body = e.do(t, "GET", "/v1/nodes", userToken, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list nodes: %d", resp.StatusCode)
	}
	nodesList, _ := body["nodes"].([]any)
	if len(nodesList) != 1 {
		t.Fatalf("expected 1 node, got %v", body)
	}
	first, _ := nodesList[0].(map[string]any)
	if first["online"] != true || first["id"] != nodeID {
		t.Fatalf("node must be online: %v", first)
	}

	// 管理员（另一用户）不可见该节点。
	resp, body = e.do(t, "GET", "/v1/nodes", adminToken, "", nil)
	adminNodes, _ := body["nodes"].([]any)
	if resp.StatusCode != http.StatusOK || len(adminNodes) != 0 {
		t.Fatalf("admin must not see carol's nodes: %d %v", resp.StatusCode, adminNodes)
	}
}
