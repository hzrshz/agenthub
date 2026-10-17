package mcpgw

import (
	"errors"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/auth"
	"github.com/uvwt/agenthub/internal/bridge"
	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/fleet"
	"github.com/uvwt/agenthub/internal/nodes"
)

type capturedInvoke struct {
	userID, nodeID, toolName, outcome string
}

type captureRecorder struct {
	mu      sync.Mutex
	records []capturedInvoke
}

func (c *captureRecorder) RecordInvoke(_ context.Context, userID, nodeID, toolName, outcome string, _ time.Duration, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, capturedInvoke{userID, nodeID, toolName, outcome})
	return nil
}

type gwEnv struct {
	svc       *auth.Service
	nodes     *nodes.Store
	hub       *bridge.Hub
	gateway   *Gateway
	invokes   *captureRecorder
	ts        *httptest.Server
	userToken string // 用户的 MCP token（明文）
}

func newGatewayEnv(t *testing.T) *gwEnv {
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
	authSvc := auth.NewService(db)
	nodeStore := nodes.NewStore(db)
	hub := bridge.NewHub(nodeStore)
	fleetStore := fleet.NewStore(db)
	fleetBridge := fleet.NewBridge(fleetStore, nodeStore, nil)
	hub.SetHelloHandler(fleetBridge.ObserveNodeHello)
	invokes := &captureRecorder{records: make([]capturedInvoke, 0)}
	gateway := NewGateway(authSvc, nodeStore, hub, fleetBridge, invokes, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", gateway.Handler())
	mux.HandleFunc("/v1/nodes/connect", func(w http.ResponseWriter, r *http.Request) {
		node, err := nodeStore.AuthenticateDeviceToken(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = hub.Accept(w, r, node.ID, "https://hub.example.com")
	})
	ts := httptest.NewServer(mux)

	// 造两个用户的 MCP token。
	tokenAlice, err := authSvc.IssueMCPToken(ctx, "usr_alice")
	if err != nil {
		t.Fatalf("issue token alice: %v", err)
	}
	_, err = authSvc.IssueMCPToken(ctx, "usr_bob")
	if err != nil {
		t.Fatalf("issue token bob: %v", err)
	}
	t.Cleanup(func() {
		ts.Close()
		_ = db.Close()
	})
	return &gwEnv{svc: authSvc, nodes: nodeStore, hub: hub, gateway: gateway, ts: ts, userToken: tokenAlice, invokes: invokes}
}

// connectToolNode 模拟 AgentDock：配对、连接、声明一个 echo 工具并应答。
func (e *gwEnv) connectToolNode(t *testing.T, owner, deviceID string, incompatible bool) *websocket.Conn {
	t.Helper()
	code, err := e.nodes.CreatePairingCode(context.Background(), owner)
	if err != nil {
		t.Fatalf("pairing code: %v", err)
	}
	node, err := e.nodes.Pair(context.Background(), nodes.PairInput{Code: code.Code, DeviceID: deviceID, Name: deviceID})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	inputSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{"type": "string"},
		},
		"required": []any{"message"},
	}
	if incompatible {
		inputSchema = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string"},
				"extra":   map[string]any{"type": "number"},
			},
			"required": []any{"message", "extra"},
		}
	}
	descriptor := protocol.ToolDescriptor{
		Name: "echo", Title: "Echo",
		InputSchema: inputSchema,
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+node.DeviceToken)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.ts.URL, "http")+"/v1/nodes/connect", header)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := conn.WriteJSON(protocol.Message{
		Type: protocol.MessageNodeHello, ProtocolVersion: protocol.ConnectionProtocolVersion,
		Hello: &protocol.Hello{
			DeviceID: deviceID, Version: "1.0.0", ProtocolVersion: protocol.ConnectionProtocolVersion,
			Capabilities: []string{"echo"}, Tools: []protocol.ToolDescriptor{descriptor},
		},
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	var ready protocol.Message
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("ready: %v", err)
	}
	// 后台应答 echo 调用。
	go func() {
		for {
			var message protocol.Message
			if err := conn.ReadJSON(&message); err != nil {
				return
			}
			if message.Type == protocol.MessageToolInvoke && message.RequestID != "" {
				_ = conn.WriteJSON(protocol.Message{
					Type: protocol.MessageToolResult, RequestID: message.RequestID,
					Result: []byte(`{"content":[{"type":"text","text":"echo-ok"}],"isError":false}`),
				})
			}
		}
	}()
	return conn
}

// connectToolNodeWithSchema 让节点声明一个给定 inputSchema 的工具。
// 传 nil / 非 object 的 schema 用来验证畸形契约不会打穿 /mcp 工具面装配。
func (e *gwEnv) connectToolNodeWithSchema(t *testing.T, owner, deviceID string, schema map[string]any) *websocket.Conn {
	t.Helper()
	code, err := e.nodes.CreatePairingCode(context.Background(), owner)
	if err != nil {
		t.Fatalf("pairing code: %v", err)
	}
	node, err := e.nodes.Pair(context.Background(), nodes.PairInput{Code: code.Code, DeviceID: deviceID, Name: deviceID})
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	descriptor := protocol.ToolDescriptor{Name: "weird_tool", Title: "Weird", InputSchema: schema}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+node.DeviceToken)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.ts.URL, "http")+"/v1/nodes/connect", header)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := conn.WriteJSON(protocol.Message{
		Type: protocol.MessageNodeHello, ProtocolVersion: protocol.ConnectionProtocolVersion,
		Hello: &protocol.Hello{
			DeviceID: deviceID, Version: "1.0.0", ProtocolVersion: protocol.ConnectionProtocolVersion,
			Capabilities: []string{"weird_tool"}, Tools: []protocol.ToolDescriptor{descriptor},
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

// bearerClient 返回给所有请求注入 Authorization 头的 HTTP 客户端（SDK 无原生 header 字段）。
func bearerClient(token string) *http.Client {
	return &http.Client{Transport: &headerTransport{base: http.DefaultTransport, token: token}}
}

type headerTransport struct {
	base  http.RoundTripper
	token string
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+h.token)
	return h.base.RoundTrip(req)
}

// connectSession 建立 MCP 会话（Streamable HTTP + Bearer）。
func connectSession(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-platform", Version: "1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: bearerClient(token), DisableStandaloneSSE: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// MCP 端到端：list tools → nodes_context → 带 node_id 调用 echo。
func TestMCPListAndInvoke(t *testing.T) {
	env := newGatewayEnv(t)
	nodeConn := env.connectToolNode(t, "usr_alice", "device-alice-01", false)
	defer nodeConn.Close()
	// hello 回调异步触发 fleet 合并；等待公开契约就绪后再连 MCP。
	waitForPublish(t, env, "usr_alice", "echo")

	session := connectSession(t, env.ts.URL+"/mcp", env.userToken)
	ctx := context.Background()

	// list tools：必须有 echo（带 node_id）与 nodes_context。
	toolsResp, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make(map[string]bool)
	var echoTool *mcp.Tool
	for _, tool := range toolsResp.Tools {
		names[tool.Name] = true
		if tool.Name == "echo" {
			echoTool = tool
		}
	}
	if !names["echo"] || !names[NodesContextToolName] {
		t.Fatalf("tool surface must contain echo and nodes_context: %v", names)
	}
	// echo 的 schema 必须注入 node_id。
	hasNodeID := false
	if raw, err := json.Marshal(echoTool.InputSchema); err == nil {
		var schema struct {
			Required []string `json:"required"`
		}
		if json.Unmarshal(raw, &schema) == nil {
			for _, name := range schema.Required {
				if name == "node_id" {
					hasNodeID = true
				}
			}
		}
	}
	if !hasNodeID {
		t.Fatalf("echo schema must require node_id: %+v", echoTool.InputSchema)
	}

	// nodes_context 返回节点清单。
	navResult, err := session.CallTool(ctx, &mcp.CallToolParams{Name: NodesContextToolName, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("nodes_context: %v", err)
	}
	if navResult.IsError {
		t.Fatalf("nodes_context returned error: %v", navResult)
	}

	// 节点列表里拿 node_id（从存储直接查，绕过 JSON 解析）。
	nodeList, err := env.nodes.ListByOwner(ctx, "usr_alice")
	if err != nil || len(nodeList) != 1 {
		t.Fatalf("list nodes: %v %v", nodeList, err)
	}
	// 带 node_id 调用 echo。
	callResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "echo", Arguments: map[string]any{"node_id": nodeList[0].ID, "message": "hi"},
	})
	if err != nil {
		t.Fatalf("echo call: %v", err)
	}
	if callResult.IsError {
		t.Fatalf("echo returned error: %+v", callResult)
	}
}

// 错误 token 拒绝且 401 带 WWW-Authenticate。
func TestMCPAuthRejected(t *testing.T) {
	env := newGatewayEnv(t)
	resp, err := http.Post(env.ts.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("post mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mcp must be 401, got %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("401 must carry WWW-Authenticate")
	}
}

// 跨用户隔离：bob 的 token 无法触达 alice 的节点。
func TestMCPCrossUserIsolation(t *testing.T) {
	env := newGatewayEnv(t)
	nodeConn := env.connectToolNode(t, "usr_alice", "device-alice-01", false)
	defer nodeConn.Close()

	ctx := context.Background()
	tokenBob, err := env.svc.IssueMCPToken(ctx, "usr_bob")
	if err != nil {
		t.Fatalf("bob token: %v", err)
	}

	session := connectSession(t, env.ts.URL+"/mcp", tokenBob)

	// bob 的工具面里没有 echo（alice 的节点）。
	toolsResp, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range toolsResp.Tools {
		if tool.Name == "echo" {
			t.Fatal("bob must not see alice's tools")
		}
	}

	// 关键：光"看不见"不够——bob 必须也不能用 alice 的 node_id 发起调用。
	// 这条攻击路径的守卫是 callNodeTool 里 GetOwned 的归属校验，
	// 与工具列表是否可见是两道独立的防线，必须单独锁。
	nodeList, err := env.nodes.ListByOwner(ctx, "usr_alice")
	if err != nil || len(nodeList) != 1 {
		t.Fatalf("list alice nodes: %v %v", nodeList, err)
	}
	aliceNodeID := nodeList[0].ID
	if _, err := env.gateway.callNodeTool(ctx, "usr_bob", "echo", map[string]any{
		"node_id": aliceNodeID, "message": "steal",
	}); err == nil {
		t.Fatal("bob must not be able to invoke a tool on alice's node")
	}

	// bob 的 nodes_context 里不得出现 alice 的节点。
	navResult, err := session.CallTool(ctx, &mcp.CallToolParams{Name: NodesContextToolName, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("bob nodes_context: %v", err)
	}
	if strings.Contains(string(mustJSON(t, navResult)), aliceNodeID) {
		t.Fatalf("bob's nodes_context leaked alice's node id: %+v", navResult)
	}
}

// 调用前契约复核：节点现场契约漂移时必须被 TOOL_CONTRACT_MISMATCH 拒绝。
// connectToolNode 的 incompatible 形参此前从未被传 true，这条路径整条无覆盖。
func TestMCPContractMismatchRejected(t *testing.T) {
	env := newGatewayEnv(t)
	stable := env.connectToolNode(t, "usr_alice", "device-alice-01", false)
	defer stable.Close()
	waitForPublish(t, env, "usr_alice", "echo")

	ctx := context.Background()
	nodeList, err := env.nodes.ListByOwner(ctx, "usr_alice")
	if err != nil || len(nodeList) != 1 {
		t.Fatalf("list nodes: %v %v", nodeList, err)
	}
	compatibleID := nodeList[0].ID

	// 同名工具的第二个节点带不兼容契约上线。
	drifted := env.connectToolNode(t, "usr_alice", "device-alice-02", true)
	defer drifted.Close()

	// 对漂移节点调用：必须被契约复核拦下，并带结构化 mismatch。
	nodeList, err = env.nodes.ListByOwner(ctx, "usr_alice")
	if err != nil || len(nodeList) != 2 {
		t.Fatalf("list nodes: %v %v", nodeList, err)
	}
	var driftedID string
	for _, n := range nodeList {
		if n.ID != compatibleID {
			driftedID = n.ID
		}
	}
	// 契约复核发生在"节点在线"判定之后，因此两个节点都必须先 ready。
	waitForOnline(t, env, compatibleID)
	waitForOnline(t, env, driftedID)

	// 兼容节点仍可调用（漂移不得污染已公开契约）。
	if _, err := env.gateway.callNodeTool(ctx, "usr_alice", "echo", map[string]any{
		"node_id": compatibleID, "message": "hi",
	}); err != nil {
		t.Fatalf("compatible node must still be callable: %v", err)
	}

	_, err = env.gateway.callNodeTool(ctx, "usr_alice", "echo", map[string]any{
		"node_id": driftedID, "message": "hi",
	})
	if err == nil {
		t.Fatal("drifted contract must be rejected at call time")
	}
	if !strings.Contains(err.Error(), "TOOL_CONTRACT_MISMATCH") {
		t.Fatalf("error must carry TOOL_CONTRACT_MISMATCH: %v", err)
	}
}

// 畸形工具契约不能让 /mcp 崩掉：节点声明 nil / 非 object 的 inputSchema 时，
// 该工具被跳过、其余工具照常（此前在 AddTool 直接 panic，而工具面每请求装配，
// 等于一个畸形节点就能让该用户所有调用持续失败）。
func TestMCPMalformedSchemaDoesNotBreakSurface(t *testing.T) {
	env := newGatewayEnv(t)
	conn := env.connectToolNodeWithSchema(t, "usr_alice", "device-malformed-01", nil)
	defer conn.Close()

	ctx := context.Background()
	session := connectSession(t, env.ts.URL+"/mcp", env.userToken)

	toolsResp, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools must not fail on malformed node schema: %v", err)
	}
	found := false
	for _, tool := range toolsResp.Tools {
		if tool.Name == NodesContextToolName {
			found = true
		}
	}
	if !found {
		t.Fatalf("nodes_context must still be available: %+v", toolsResp.Tools)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// waitForPublish 等待 fleet 公开契约就绪（hello 回调是异步的）。
// waitForOnline 等待节点在 Hub 中登记为 ready。
//
// 不能省略：Hub.Online 以 ready 为准，而 ready 与 connectToolNode 返回之间
// 存在毫秒级窗口。CI runner 负载高时这个窗口会被放大——直接调用会拿到
// NODE_OFFLINE 而不是本用例真正要断言的契约复核结果（flaky 的来源）。
func waitForOnline(t *testing.T, env *gwEnv, nodeID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if env.hub.Online(nodeID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("node %s did not come online within 10s", nodeID)
}

func waitForPublish(t *testing.T, env *gwEnv, owner, tool string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := env.gateway.fleet.Published(owner, tool); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fleet contract %s/%s not published within 5s", owner, tool)
}

// 调用记录：成功的 echo 调用必须留一条元数据记录（不含参数）。
func TestInvokeRecorded(t *testing.T) {
	env := newGatewayEnv(t)
	nodeConn := env.connectToolNode(t, "usr_alice", "device-alice-01", false)
	defer nodeConn.Close()
	waitForPublish(t, env, "usr_alice", "echo")

	session := connectSession(t, env.ts.URL+"/mcp", env.userToken)
	ctx := context.Background()
	nodeList, _ := env.nodes.ListByOwner(ctx, "usr_alice")
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "echo", Arguments: map[string]any{"node_id": nodeList[0].ID, "message": "secret-value"},
	})
	if err != nil || result.IsError {
		t.Fatalf("echo call: %v isError=%v", err, result.IsError)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		env.invokes.mu.Lock()
		count := len(env.invokes.records)
		env.invokes.mu.Unlock()
		if count > 0 {
			env.invokes.mu.Lock()
			record := env.invokes.records[0]
			env.invokes.mu.Unlock()
			if record.userID != "usr_alice" || record.toolName != "echo" || record.outcome != OutcomeSuccess {
				t.Fatalf("unexpected record: %+v", record)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("invoke record not written within 3s")
}

// 离线节点的调用必须返回 NODE_OFFLINE 且 retryable=true（DESIGN §3.3）。
// 此前全库没有 retryable 字段，平台只能靠匹配错误字符串判断能否重试。
func TestMCPNodeOfflineIsRetryable(t *testing.T) {
	env := newGatewayEnv(t)
	nodeConn := env.connectToolNode(t, "usr_alice", "device-alice-01", false)
	defer nodeConn.Close()
	waitForPublish(t, env, "usr_alice", "echo")

	ctx := context.Background()
	nodeList, err := env.nodes.ListByOwner(ctx, "usr_alice")
	if err != nil || len(nodeList) != 1 {
		t.Fatalf("list nodes: %v %v", nodeList, err)
	}
	// 断开连接 → 节点离线。
	nodeConn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for env.hub.Online(nodeList[0].ID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	result, err := env.gateway.callNodeTool(ctx, "usr_alice", "echo", map[string]any{
		"node_id": nodeList[0].ID, "message": "hi",
	})
	if err == nil {
		t.Fatalf("offline node call must fail, got result %+v", result)
	}
	var coded *core.CodedError
	if !errors.As(err, &coded) {
		t.Fatalf("expected CodedError, got %T: %v", err, err)
	}
	if coded.Code != core.CodeNodeOffline {
		t.Fatalf("expected NODE_OFFLINE, got %s", coded.Code)
	}
	if !coded.Retryable {
		t.Fatal("NODE_OFFLINE must be retryable so the platform can back off and retry")
	}

	// 经 gatewayToolResult 包装后，结构化 code/retryable 必须出现在响应里。
	wrapped, err := gatewayToolResult("echo", nil, err)
	if err != nil {
		t.Fatalf("wrap result: %v", err)
	}
	encoded := string(mustJSON(t, wrapped))
	if !strings.Contains(encoded, "NODE_OFFLINE") {
		t.Fatalf("MCP response must carry the NODE_OFFLINE code: %s", encoded)
	}
	if !strings.Contains(encoded, `"retryable":true`) {
		t.Fatalf("MCP response must carry retryable=true: %s", encoded)
	}
}
