// Package bridge 承载节点 WebSocket 连接生命周期（Hub）：
// 握手、心跳踢线、同设备竞态（新连接赢）、invoke 转发与取消。
// 连接层语义照搬 nexusdock internal/agentdock Hub，按 agenthub 需要裁剪：
// 去掉 UI resource 校验与 trace 注入的强依赖，保留 traceparent 透传。
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/config"
	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/nodes"
)

var (
	ErrNodeOffline      = errors.New("节点当前离线")
	ErrNodeDisconnected = errors.New("节点连接已断开")
	// ErrProtocolMismatch 表示节点的协议版本低于 hub 的最低要求。
	// 与内部原因区分开，是为了让 HTTP 层能把它映射成可读的 400 + 升级指引，
	// 而不是一句无法定位的"连接被关闭"。
	ErrProtocolMismatch = errors.New("节点协议版本过低（请升级 AgentDock 后重新配对）")
)

// checkProtocolVersion 校验节点声明的协议版本。
//
// 语义（DESIGN §3.1）：低于 config.MinProtocolVersion 拒连；
// 高于 hub 已知版本按 hub 已知的最新版本处理——hub 只认自己这代协议，
// 对方报更高版本说明节点更新，不应因此拒绝接入。
func checkProtocolVersion(version string) error {
	trimmed := strings.TrimSpace(version)
	if trimmed == "" {
		// 老节点不带版本字段：视为低于门槛，给出升级指引而不是含糊的"无效握手"。
		return fmt.Errorf("%w：节点未声明协议版本", ErrProtocolMismatch)
	}
	current, err := strconv.Atoi(trimmed)
	if err != nil {
		return fmt.Errorf("%w：无法识别的协议版本 %q", ErrProtocolMismatch, trimmed)
	}
	minimum, err := strconv.Atoi(config.MinProtocolVersion)
	if err != nil {
		// 常量非法属于编程错误，不能表现为对节点的拒连理由。
		return fmt.Errorf("配置错误：MinProtocolVersion %q 非法: %w", config.MinProtocolVersion, err)
	}
	if current < minimum {
		return fmt.Errorf("%w：节点协议版本 %d，hub 最低要求 %d", ErrProtocolMismatch, current, minimum)
	}
	return nil
}

type pendingResult struct {
	result json.RawMessage
	err    error
}

type nodeConnection struct {
	nodeID    string
	socket    *websocket.Conn
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[string]chan pendingResult
	closed    bool
	ready     chan struct{}
	readyOnce sync.Once
}

// Hub 管理全部在线节点连接。OnHello 回调在握手完成后触发（fleet 合并挂在这里）。
type Hub struct {
	store       *nodes.Store
	mu          sync.RWMutex
	connections map[string]*nodeConnection
	onHello     func(nodes.Node, protocol.Hello)
	logger      *slog.Logger
}

func NewHub(store *nodes.Store) *Hub {
	return NewHubWithLogger(store, slog.Default())
}

// NewHubWithLogger 允许注入日志器；DESIGN §7 要求连接/断开必须有结构化日志，
// 生产装配走 serve.go 传入的实例化 logger。
func NewHubWithLogger(store *nodes.Store, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{store: store, connections: make(map[string]*nodeConnection), logger: logger}
}

// rejectHandshake 在握手拒绝前尽力把原因写回节点再关闭连接。
//
// Upgrade 之后已无法写 HTTP 状态码，若直接关闭，AgentDock 只能看到一个凭空
// 断开的连接，拿不到"请升级 AgentDock"这类可操作指引（DESIGN §3.1 要求拒连时
// 给出升级指引）。这里复用协议已有的 MessageToolError 作为错误通道把原因递过去
// （不新增协议消息类型，保证 AgentDock 端无需改动即可解析）；写失败也照常关闭。
func rejectHandshake(connection *nodeConnection, reason string) {
	_ = connection.write(protocol.Message{
		Type:            protocol.MessageToolError,
		ProtocolVersion: protocol.ConnectionProtocolVersion,
		Error:           &protocol.RemoteError{Code: "PROTOCOL_VERSION_UNSUPPORTED", Message: reason, Retryable: false},
	})
	connection.close(errors.New(reason))
}

// SetHelloHandler 注册握手回调；回调在持有读锁快照后异步调用方语义由实现方保证。
func (h *Hub) SetHelloHandler(handler func(nodes.Node, protocol.Hello)) {
	h.mu.Lock()
	h.onHello = handler
	h.mu.Unlock()
}

func (h *Hub) Online(nodeID string) bool {
	h.mu.RLock()
	connection := h.connections[nodeID]
	h.mu.RUnlock()
	return connection != nil && connection.isReady()
}

// Disconnect 踢掉节点当前连接（禁用/删除节点时调用）。
func (h *Hub) Disconnect(nodeID string) {
	h.mu.Lock()
	connection := h.connections[nodeID]
	delete(h.connections, nodeID)
	h.mu.Unlock()
	if connection != nil {
		connection.close(ErrNodeDisconnected)
	}
}

// Accept 完成节点 WebSocket 握手：认证已由 HTTP 层完成（nodeID 已确认）。
// 握手失败时连接会被关闭并返回错误；Upgrade 成功后不再写 HTTP 响应。
func (h *Hub) Accept(w http.ResponseWriter, r *http.Request, nodeID, publicURL string) error {
	node, err := h.store.Get(r.Context(), nodeID)
	if err != nil {
		return err
	}
	if !node.Enabled {
		return nodes.ErrNodeDisabled
	}

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		CheckOrigin: func(request *http.Request) bool {
			// 节点连接不是浏览器会话：身份完全由 Device Token 绑定，
			// Origin 无鉴别意义（真实 AgentDock 客户端可能带任意 Origin）。
			return true
		},
	}
	socket, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	socket.SetReadLimit(config.MaxMessageBytes)
	_ = socket.SetReadDeadline(time.Now().Add(15 * time.Second))
	connection := &nodeConnection{
		nodeID: nodeID, socket: socket,
		pending: make(map[string]chan pendingResult), ready: make(chan struct{}),
	}

	var first protocol.Message
	if err := socket.ReadJSON(&first); err != nil {
		connection.close(err)
		return fmt.Errorf("读取节点握手: %w", err)
	}
	// 协议版本门槛（DESIGN §3.1）：低于 hub 最低要求 → 拒连并给升级指引；
	// 高于 hub 知道的版本 → 按 hub 已知的最新版本处理，不拒连（向前兼容）。
	if first.Type != protocol.MessageNodeHello || first.Hello == nil {
		rejectHandshake(connection, "握手消息格式无效：首帧必须是 node.hello")
		return ErrProtocolMismatch
	}
	if err := checkProtocolVersion(first.ProtocolVersion); err != nil {
		rejectHandshake(connection, err.Error())
		return err
	}
	if err := checkProtocolVersion(first.Hello.ProtocolVersion); err != nil {
		rejectHandshake(connection, err.Error())
		return err
	}
	updated, err := h.store.UpdateHello(r.Context(), nodeID, *first.Hello)
	if err != nil {
		connection.close(err)
		return err
	}
	// 先登记连接再发 node.ready：Invoke 会等待 ready，
	// 保证客户端收到 ready 后立即发起请求不会撞上登记竞态。
	h.mu.Lock()
	previous := h.connections[nodeID]
	h.connections[nodeID] = connection
	onHello := h.onHello
	h.mu.Unlock()
	if err := connection.write(protocol.Message{
		Type: protocol.MessageNodeReady, ProtocolVersion: protocol.ConnectionProtocolVersion,
		HeartbeatMS: int(config.HeartbeatInterval / time.Millisecond),
		PublicURL:   strings.TrimRight(strings.TrimSpace(publicURL), "/"),
	}); err != nil {
		h.mu.Lock()
		if h.connections[nodeID] == connection {
			if previous != nil {
				h.connections[nodeID] = previous
			} else {
				delete(h.connections, nodeID)
			}
		}
		h.mu.Unlock()
		connection.close(err)
		return fmt.Errorf("确认节点握手: %w", err)
	}
	connection.markReady()
	_ = socket.SetReadDeadline(time.Now().Add(2 * config.HeartbeatInterval))
	h.logger.Info("节点已连接", "node", nodeID, "user", node.OwnerUserID,
		"device", node.DeviceID, "version", updated.Version, "protocol", first.Hello.ProtocolVersion,
		"replaced_previous", previous != nil)

	// 新连接赢：踢掉同节点旧连接。
	if previous != nil {
		previous.close(errors.New("节点建立了新连接"))
	}
	if onHello != nil {
		onHello(updated, *first.Hello)
	}

	go h.readLoop(connection)
	return nil
}

// Invoke 把一次 tool.invoke 转发到指定节点并等待结果。
// ctx 取消/超时会透传为 tool.cancel 发给节点。
func (h *Hub) Invoke(ctx context.Context, nodeID, operation string, arguments any) (map[string]any, error) {
	h.mu.RLock()
	connection := h.connections[nodeID]
	h.mu.RUnlock()
	if connection == nil {
		return nil, ErrNodeOffline
	}
	if operation == "" {
		return nil, errors.New("节点操作不能为空")
	}
	if err := connection.waitReady(ctx); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("编码节点调用参数: %w", err)
	}
	requestID, err := core.NewID("req")
	if err != nil {
		return nil, err
	}
	resultChannel := make(chan pendingResult, 1)
	if err := connection.addPending(requestID, resultChannel); err != nil {
		return nil, err
	}
	defer connection.removePending(requestID)

	message := protocol.Message{
		Type: protocol.MessageToolInvoke, RequestID: requestID,
		Operation: operation, Arguments: encoded,
		Traceparent: traceparentFrom(ctx),
	}
	if err := connection.write(message); err != nil {
		connection.close(err)
		return nil, ErrNodeDisconnected
	}
	select {
	case <-ctx.Done():
		_ = connection.write(protocol.Message{Type: protocol.MessageToolCancel, RequestID: requestID})
		return nil, ctx.Err()
	case response := <-resultChannel:
		if response.err != nil {
			return nil, response.err
		}
		var result map[string]any
		if err := json.Unmarshal(response.result, &result); err != nil {
			return nil, fmt.Errorf("解析节点结果: %w", err)
		}
		return result, nil
	}
}

// traceparentFrom 提取上下文中的 traceparent（MCP 层注入），纯透传不做解析。
func traceparentFrom(ctx context.Context) string {
	value, _ := ctx.Value(traceparentKey{}).(string)
	return value
}

type traceparentKey struct{}

// WithTraceparent 把入站 traceparent 放入上下文以便转发。
func WithTraceparent(ctx context.Context, value string) context.Context {
	if strings.TrimSpace(value) == "" {
		return ctx
	}
	return context.WithValue(ctx, traceparentKey{}, value)
}

func (h *Hub) readLoop(connection *nodeConnection) {
	defer func() {
		h.mu.Lock()
		if h.connections[connection.nodeID] == connection {
			delete(h.connections, connection.nodeID)
		}
		h.mu.Unlock()
		connection.close(ErrNodeDisconnected)
		h.logger.Info("节点连接断开", "node", connection.nodeID)
	}()
	for {
		var message protocol.Message
		if err := connection.socket.ReadJSON(&message); err != nil {
			return
		}
		_ = connection.socket.SetReadDeadline(time.Now().Add(2 * config.HeartbeatInterval))
		switch message.Type {
		case protocol.MessageToolResult:
			connection.resolve(message.RequestID, pendingResult{result: message.Result})
		case protocol.MessageToolError:
			if message.Error == nil {
				message.Error = &protocol.RemoteError{Code: "NODE_BAD_RESPONSE", Message: "节点返回了空错误"}
			}
			connection.resolve(message.RequestID, pendingResult{err: message.Error})
		case protocol.MessageNodeHeartbeat:
			_ = h.store.Touch(context.Background(), connection.nodeID)
			_ = connection.write(protocol.Message{Type: protocol.MessageNodeHeartbeat})
		}
	}
}

func (c *nodeConnection) markReady() {
	c.readyOnce.Do(func() { close(c.ready) })
}

func (c *nodeConnection) isReady() bool {
	select {
	case <-c.ready:
		return true
	default:
		return false
	}
}

func (c *nodeConnection) waitReady(ctx context.Context) error {
	select {
	case <-c.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *nodeConnection) write(message protocol.Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrNodeDisconnected
	}
	_ = c.socket.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return c.socket.WriteJSON(message)
}

func (c *nodeConnection) addPending(requestID string, result chan pendingResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrNodeDisconnected
	}
	c.pending[requestID] = result
	return nil
}

func (c *nodeConnection) removePending(requestID string) {
	c.mu.Lock()
	delete(c.pending, requestID)
	c.mu.Unlock()
}

func (c *nodeConnection) resolve(requestID string, result pendingResult) {
	c.mu.Lock()
	channel := c.pending[requestID]
	delete(c.pending, requestID)
	c.mu.Unlock()
	if channel != nil {
		channel <- result
	}
}

func (c *nodeConnection) close(reason error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = make(map[string]chan pendingResult)
	c.mu.Unlock()
	c.markReady()
	_ = c.socket.Close()
	for _, channel := range pending {
		channel <- pendingResult{err: reason}
	}
}
