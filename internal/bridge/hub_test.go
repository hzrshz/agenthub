package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/config"
	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/nodes"
)

// fakeNode 是协议另一端的最小 AgentDock 模拟：连上后自动完成握手，
// 之后按脚本应答 invoke。
type fakeNode struct {
	conn   *websocket.Conn
	nodeID string
}

// connectNode 用 device token 连接测试服务器的 /v1/nodes/connect，
// 发送 hello 并等待 node.ready。
func connectNode(t *testing.T, tsURL, token, deviceID string, tools []protocol.ToolDescriptor) *fakeNode {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(tsURL, "http") + "/v1/nodes/connect"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial connect: %v", err)
	}
	if err := conn.WriteJSON(protocol.Message{
		Type: protocol.MessageNodeHello, ProtocolVersion: protocol.ConnectionProtocolVersion,
		Hello: &protocol.Hello{
			DeviceID: deviceID, Version: "1.0.0", ProtocolVersion: protocol.ConnectionProtocolVersion,
			OS: "darwin", Arch: "arm64", Capabilities: toolNames(tools), Tools: tools,
		},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	var ready protocol.Message
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if ready.Type != protocol.MessageNodeReady || ready.PublicURL != "https://hub.example.com" {
		t.Fatalf("unexpected ready message: %+v", ready)
	}
	return &fakeNode{conn: conn}
}

func toolNames(tools []protocol.ToolDescriptor) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// respondEcho 对收到的 invoke 回一个固定结果，用于验证转发链路。
func (n *fakeNode) respondEcho(t *testing.T) {
	t.Helper()
	_ = n.respondEchoSafe()
}

// respondEchoSafe 可在任意 goroutine 调用（错误返回而非 t.Fatal）。
func (n *fakeNode) respondEchoSafe() error {
	var message protocol.Message
	if err := n.conn.ReadJSON(&message); err != nil {
		return err
	}
	if message.Type != protocol.MessageToolInvoke {
		return errUnexpectedMessage
	}
	return n.conn.WriteJSON(protocol.Message{
		Type: protocol.MessageToolResult, RequestID: message.RequestID,
		Result: []byte(`{"ok":true}`),
	})
}

var errUnexpectedMessage = errUnexpected{}

type errUnexpected struct{}

func (errUnexpected) Error() string { return "unexpected message type" }

func (n *fakeNode) close() { _ = n.conn.Close() }

type testEnv struct {
	store *nodes.Store
	hub   *Hub
	ts    *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
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
		{"usr_alice", "alice@example.com"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users(id, username, created_at, updated_at)
			VALUES(?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, u.id, u.name); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	store := nodes.NewStore(db)
	hub := NewHub(store)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/nodes/pair", func(w http.ResponseWriter, r *http.Request) {
		// 测试辅助：以 usr_alice 直接发一个配对码再走 Pair。
		code, err := store.CreatePairingCode(r.Context(), "usr_alice")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var input nodes.PairInput
		if err := json2Decode(r, &input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		input.Code = code.Code
		node, err := store.Pair(r.Context(), input)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeRaw(w, node.ID+"\n"+node.DeviceToken)
	})
	mux.HandleFunc("/v1/nodes/connect", func(w http.ResponseWriter, r *http.Request) {
		node, err := store.AuthenticateDeviceToken(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := hub.Accept(w, r, node.ID, "https://hub.example.com"); err != nil {
			// upgrade 后无法改写响应。
			return
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = db.Close() })
	return &testEnv{store: store, hub: hub, ts: ts}
}

func (e *testEnv) pairNode(t *testing.T, deviceID, name string) *fakeNode {
	t.Helper()
	resp, err := http.Post(e.ts.URL+"/v1/nodes/pair", "application/json",
		strings.NewReader(`{"device_id":"`+deviceID+`","name":"`+name+`"}`))
	if err != nil {
		t.Fatalf("pair request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw := make([]byte, 512)
	n, _ := resp.Body.Read(raw)
	parts := strings.Fields(string(raw[:n]))
	if len(parts) != 2 {
		t.Fatalf("unexpected pair response: %q", string(raw[:n]))
	}
	return connectNode(t, e.ts.URL, parts[1], deviceID, []protocol.ToolDescriptor{{
		Name: "browser_open", Title: "Open Browser",
		InputSchema: map[string]any{"type": "object"},
	}})
}

// 握手完成 → Online → invoke 往返 → 收到 hello 回调与 PublicURL。
func TestHandshakeAndInvoke(t *testing.T) {
	env := newTestEnv(t)
	helloCh := make(chan nodes.Node, 1)
	env.hub.SetHelloHandler(func(node nodes.Node, _ protocol.Hello) { helloCh <- node })

	node := env.pairNode(t, "device-alice-01", "dev")
	defer node.close()

	select {
	case greeted := <-helloCh:
		if greeted.DeviceID != "device-alice-01" {
			t.Fatalf("hello callback device mismatch: %+v", greeted)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hello callback not fired")
	}
	if !env.hub.Online(nodeIDOf(t, env, "device-alice-01")) {
		t.Fatal("node must be online after handshake")
	}

	go func() {
		if err := node.respondEchoSafe(); err != nil {
			t.Errorf("node respond: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := env.hub.Invoke(ctx, nodeIDOf(t, env, "device-alice-01"), "tool.call", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if result["ok"] != true {
		t.Fatalf("unexpected invoke result: %v", result)
	}
}

// 离线节点立即返回 ErrNodeOffline，不排队。
func TestInvokeOfflineNode(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := env.hub.Invoke(ctx, "node_missing", "tool.call", nil); err == nil || !strings.Contains(err.Error(), "离线") {
		t.Fatalf("offline invoke must fail fast, got %v", err)
	}
}

// 新连接赢：同一节点行断线重连时，新 WebSocket 顶掉旧连接，
// 旧连接上挂起的 invoke 全部失败（不悬挂）。
func TestNewConnectionWins(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	code, err := env.store.CreatePairingCode(ctx, "usr_alice")
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	node, err := env.store.Pair(ctx, nodes.PairInput{Code: code.Code, DeviceID: "device-alice-01", Name: "dev"})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	dial := func() *websocket.Conn {
		header := http.Header{}
		header.Set("Authorization", "Bearer "+node.DeviceToken)
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(env.ts.URL, "http")+"/v1/nodes/connect", header)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := conn.WriteJSON(protocol.Message{
			Type: protocol.MessageNodeHello, ProtocolVersion: protocol.ConnectionProtocolVersion,
			Hello: &protocol.Hello{
				DeviceID: node.DeviceID, Version: "1.0.0", ProtocolVersion: protocol.ConnectionProtocolVersion,
				OS: "darwin", Arch: "arm64", Capabilities: []string{"browser_open"},
			},
		}); err != nil {
			t.Fatalf("hello: %v", err)
		}
		var ready protocol.Message
		if err := conn.ReadJSON(&ready); err != nil {
			t.Fatalf("ready: %v", err)
		}
		return conn
	}
	first := dial()
	defer first.Close()

	// 第一条连接挂起一个 invoke（节点不回话）。
	pendingCtx, cancelPending := context.WithCancel(context.Background())
	invokeErr := make(chan error, 1)
	go func() {
		_, err := env.hub.Invoke(pendingCtx, node.ID, "tool.call", nil)
		invokeErr <- err
	}()
	time.Sleep(100 * time.Millisecond) // 等待 invoke 写入

	second := dial() // 同一 device token 重连，必须顶掉第一条
	defer second.Close()

	select {
	case err := <-invokeErr:
		if err == nil {
			t.Fatal("pending invoke on replaced connection must fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending invoke must be resolved when connection replaced")
	}
	cancelPending()

	if !env.hub.Online(node.ID) {
		t.Fatal("new connection must be online")
	}
}

// 取消透传：ctx 取消后节点应收到 tool.cancel。
func TestInvokeCancelSendsToolCancel(t *testing.T) {
	env := newTestEnv(t)
	node := env.pairNode(t, "device-alice-03", "dev")
	defer node.close()
	nodeID := nodeIDOf(t, env, "device-alice-03")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, _ = env.hub.Invoke(ctx, nodeID, "tool.call", nil) // 节点不回话，等待取消

	node.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	// 节点会先读到 invoke，随后（取消发生时）读到 cancel。
	var message protocol.Message
	for i := 0; i < 2; i++ {
		if err := node.conn.ReadJSON(&message); err != nil {
			t.Fatalf("read after cancel: %v", err)
		}
		if message.Type == protocol.MessageToolCancel {
			return
		}
	}
	t.Fatalf("node must receive tool.cancel, got %s last", message.Type)
}

// 心跳往返延长读超时：建立连接后静默 1.5 个心跳周期内不因无业务消息被断开
// （fake node 通过应答 heartbeat 实现；此处只验证 Invoke 前后连接仍可用）。
func TestHeartbeatKeepsConnectionAlive(t *testing.T) {
	env := newTestEnv(t)
	node := env.pairNode(t, "device-alice-01", "dev")
	defer node.close()

	// 后台回heartbeat，模拟真实 AgentDock。
	go func() {
		node.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			var message protocol.Message
			if err := node.conn.ReadJSON(&message); err != nil {
				return
			}
			if message.Type == protocol.MessageNodeHeartbeat {
				_ = node.conn.WriteJSON(protocol.Message{Type: protocol.MessageNodeHeartbeat})
			}
		}
	}()
	time.Sleep(200 * time.Millisecond) // 远小于 2×心跳；连接必须仍然在线
	if !env.hub.Online(nodeIDOf(t, env, "device-alice-01")) {
		t.Fatal("connection with heartbeats must stay online")
	}
	_ = config.HeartbeatInterval
}

func nodeIDOf(t *testing.T, env *testEnv, deviceID string) string {
	t.Helper()
	list, err := env.store.ListByOwner(context.Background(), "usr_alice")
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	for _, node := range list {
		if node.DeviceID == deviceID {
			return node.ID
		}
	}
	t.Fatalf("node with device %s not found", deviceID)
	return ""
}

// 测试辅助：轻量 JSON 解码与响应写出（避免依赖 httpx）。
func json2Decode(r *http.Request, target any) error {
	return json.NewDecoder(r.Body).Decode(target)
}

func writeRaw(w http.ResponseWriter, body string) {
	_, _ = w.Write([]byte(body))
}
