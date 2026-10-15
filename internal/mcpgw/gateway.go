// Package mcpgw 承载 AgentHub 的 MCP 网关：
// 单一 /mcp 路径，Bearer token 即用户身份；每个请求按其用户动态装配
// 工具面（该用户在线节点的裸名工具 + nodes_context 中心工具），
// 透传工具的 inputSchema 强制注入必填 node_id。
package mcpgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/auth"
	"github.com/uvwt/agenthub/internal/bridge"
	"github.com/uvwt/agenthub/internal/config"
	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/fleet"
	"github.com/uvwt/agenthub/internal/nodes"
)

// NodesContextToolName 是中心工具名：返回用户节点清单与在线状态。
const NodesContextToolName = "nodes_context"

// Gateway 是 MCP 网关。
type Gateway struct {
	auth    *auth.Service
	nodeSvc *nodes.Store
	hub     *bridge.Hub
	fleet   *fleet.Bridge
	invokes InvokeRecorder
	logger  *slog.Logger

	// rateMu 保护每用户 MCP 调用速率窗口。
	rateMu      sync.Mutex
	rateWindows map[string]*rateWindow

	// inflight 统计每节点在途请求数。
	inflightMu sync.Mutex
	inflight   map[string]int
}

type rateWindow struct {
	windowStart time.Time
	count       int
}

// InvokeRecorder 是调用记录写入的最小接口（audit.Store 实现；测试可注入空实现）。
type InvokeRecorder interface {
	RecordInvoke(ctx context.Context, userID, nodeID, toolName, outcome string, duration time.Duration, errorCode string) error
}

// Outcome 常量与 audit 包对齐。
const (
	OutcomeSuccess   = "success"
	OutcomeError     = "error"
	OutcomeTimeout   = "timeout"
	OutcomeCancelled = "cancelled"
)

func NewGateway(authService *auth.Service, nodeStore *nodes.Store, hub *bridge.Hub, fleetBridge *fleet.Bridge, invokes InvokeRecorder, logger *slog.Logger) *Gateway {
	g := &Gateway{
		auth: authService, nodeSvc: nodeStore, hub: hub, fleet: fleetBridge, invokes: invokes, logger: logger,
		rateWindows: make(map[string]*rateWindow), inflight: make(map[string]int),
	}
	return g
}

// Handler 返回带认证与限流的 /mcp HTTP 处理器。
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := g.auth.AuthenticateMCPToken(r.Context(), bearerToken(r.Header.Get("Authorization")))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agenthub", resource_metadata="/.well-known/mcp"`)
			writeMCPError(w, http.StatusUnauthorized, string(core.ErrorCodeOf(err)), "mcp token invalid")
			return
		}
		if !g.allowRequest(user.ID) {
			writeMCPError(w, http.StatusTooManyRequests, "RATE_LIMITED", "mcp request rate limit exceeded")
			return
		}
		// 每请求临时 server：按用户动态注册工具面（Stateless 模式下 handler
		// 只在本次请求内使用该 server，注册成本低且天然用户隔离）。
		server := mcp.NewServer(&mcp.Implementation{Name: "agenthub", Version: "1"}, &mcp.ServerOptions{})
		if err := g.registerUserTools(r.Context(), user.ID, server); err != nil {
			writeMCPError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "assemble tool surface failed")
			return
		}
		g.handlerWithServer(server).ServeHTTP(w, r.WithContext(mcpContextWithUser(r.Context(), user.ID)))
	})
}

// handlerWithServer 为单次请求生成一个绑定该 server 的 handler。
// StreamableHTTPHandler 的 getServer 由构造时的闭包决定，这里为每请求
// 新建一个轻量 handler（Stateless 模式无会话存储，开销可忽略）。
func (g *Gateway) handlerWithServer(server *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			MaxRequestBodyBytes:          config.MaxMessageBytes,
			PropagateRequestCancellation: true,
		},
	)
}

// allowRequest 每用户滑动速率限制（固定窗口实现，够用且无依赖）。
func (g *Gateway) allowRequest(userID string) bool {
	g.rateMu.Lock()
	defer g.rateMu.Unlock()
	now := time.Now()
	window, exists := g.rateWindows[userID]
	if !exists || now.Sub(window.windowStart) >= time.Minute {
		g.rateWindows[userID] = &rateWindow{windowStart: now, count: 1}
		return true
	}
	if window.count >= config.MCPRequestsPerMinute {
		return false
	}
	window.count++
	return true
}

// registerUserTools 按用户装配 MCP 工具面。
func (g *Gateway) registerUserTools(ctx context.Context, userID string, server *mcp.Server) error {
	nodeList, err := g.nodeSvc.ListByOwner(ctx, userID)
	if err != nil {
		return err
	}
	// 在线节点的工具按裸名注册（fleet 合并已在 hello 阶段保证同名工具契约一致）；
	// 同名工具已由 fleet 的公开契约决定 schema，这里以 fleet 状态为准注册。
	for _, node := range nodeList {
		if !node.Enabled || !g.hub.Online(node.ID) {
			continue
		}
		descriptors, err := g.nodeSvc.ToolDescriptors(ctx, node.ID)
		if err != nil {
			continue
		}
		for _, descriptor := range descriptors {
			name := strings.TrimSpace(descriptor.Name)
			if name == "" || name == NodesContextToolName {
				continue
			}
			published, ok := g.fleet.Published(userID, name)
			if !ok {
				continue
			}
			server.AddTool(nodeMCPTool(published.Descriptor), g.nodeToolHandler(userID, name))
		}
	}
	server.AddTool(nodesContextTool(), g.nodesContextHandler(userID))
	return nil
}

// nodeMCPTool 把节点工具描述符转成 MCP Tool 并注入必填 node_id。
func nodeMCPTool(descriptor protocol.ToolDescriptor) *mcp.Tool {
	inputSchema := injectNodeIDSchema(descriptor.InputSchema)
	tool := &mcp.Tool{
		Name: descriptor.Name, Title: descriptor.Title, Description: descriptor.Description,
		InputSchema: inputSchema, OutputSchema: descriptor.OutputSchema,
	}
	if len(descriptor.Annotations) > 0 {
		encoded, _ := json.Marshal(descriptor.Annotations)
		var annotations mcp.ToolAnnotations
		if json.Unmarshal(encoded, &annotations) == nil {
			tool.Annotations = &annotations
		}
	}
	if len(descriptor.Meta) > 0 {
		meta := make(mcp.Meta, len(descriptor.Meta))
		for key, value := range descriptor.Meta {
			meta[key] = value
		}
		if len(meta) > 0 {
			tool.Meta = meta
		}
	}
	return tool
}

// injectNodeIDSchema 复制 inputSchema 并注入必填 node_id（照搬 nexusdock nodeInputSchema）。
func injectNodeIDSchema(schema map[string]any) map[string]any {
	cloned := make(map[string]any, len(schema)+2)
	for key, value := range schema {
		cloned[key] = value
	}
	properties, _ := cloned["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
	} else {
		propertiesCopy := make(map[string]any, len(properties)+1)
		for key, value := range properties {
			propertiesCopy[key] = value
		}
		properties = propertiesCopy
	}
	properties["node_id"] = map[string]any{
		"type":        "string",
		"description": "目标 AgentDock 节点 ID，来自 nodes_context 返回的节点列表。",
	}
	cloned["properties"] = properties
	required := make([]string, 0)
	if existing, ok := cloned["required"].([]any); ok {
		for _, item := range existing {
			if name, ok := item.(string); ok && name != "node_id" {
				required = append(required, name)
			}
		}
	}
	required = append(required, "node_id")
	cloned["required"] = required
	return cloned
}

func nodesContextTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        NodesContextToolName,
		Title:       "节点上下文",
		Description: "返回当前用户全部 AgentDock 节点的列表（node_id、名称、版本、能力、在线状态）。调用其他工具前先用它选择目标 node_id。",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// nodeToolHandler 生成工具调用处理器：校验 node_id → 契约复核 → 定向转发。
func (g *Gateway) nodeToolHandler(userID, toolName string) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arguments := map[string]any{}
		if request != nil && request.Params != nil && len(request.Params.Arguments) > 0 && string(request.Params.Arguments) != "null" {
			if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
				return nil, errors.New("tool arguments must be a JSON object")
			}
		}
		result, err := g.callNodeTool(ctx, userID, toolName, arguments)
		return gatewayToolResult(toolName, result, err)
	}
}

// callNodeTool 核心调用链路：node_id 校验（含跨用户隔离）→ 能力检查 →
// 契约复核 → 限流/并发 → invoke 转发。
func (g *Gateway) callNodeTool(ctx context.Context, userID, toolName string, arguments map[string]any) (map[string]any, error) {
	nodeID, _ := arguments["node_id"].(string)
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil, errors.New("node_id is required（先调用 nodes_context 选择目标节点）")
	}
	node, err := g.nodeSvc.GetOwned(ctx, userID, nodeID)
	if err != nil {
		// 不存在或不属于该用户一律 NotFound，不泄露他人节点存在性。
		return nil, fmt.Errorf("node %s not found", nodeID)
	}
	if !node.Enabled {
		return nil, fmt.Errorf("node %s is disabled", nodeID)
	}
	if !g.hub.Online(nodeID) {
		return nil, &core.CodedError{Code: core.CodeNodeOffline, Message: "节点当前离线"}
	}
	if !containsString(node.Capabilities, toolName) {
		return nil, fmt.Errorf("node %s does not provide tool %s", nodeID, toolName)
	}

	// 契约复核：节点现场契约必须在公开兼容集合内。
	published, ok := g.fleet.Published(userID, toolName)
	if !ok {
		return nil, fmt.Errorf("tool %s contract not published", toolName)
	}
	descriptors, err := g.nodeSvc.ToolDescriptors(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	target, ok := findDescriptor(descriptors, toolName)
	if !ok {
		return nil, fmt.Errorf("node %s does not provide tool %s", nodeID, toolName)
	}
	nodeHash, err := fleet.ToolContractHash(target)
	if err != nil {
		return nil, err
	}
	if !containsHash(published.AcceptedSemanticHashes, nodeHash) {
		differences := fleet.ContractDifferences(published.Descriptor, target)
		return nil, &core.CodedError{
			Code:    "TOOL_CONTRACT_MISMATCH",
			Message: fmt.Sprintf("目标节点工具 %s 的契约不在当前兼容集合内（published=%s node=%s），请刷新工具列表或升级该设备的 AgentDock", toolName, published.ContractHash, nodeHash),
			Err:     contractMismatchError{differences: differences},
		}
	}

	// 并发在途上限：超过即快速失败，防止单节点拖垮该用户的整条连接。
	if !g.acquireInflight(nodeID) {
		return nil, &core.CodedError{Code: core.CodeTooManyRequests, Message: "节点并发请求已达上限"}
	}
	defer g.releaseInflight(nodeID)

	delete(arguments, "node_id")
	invokeCtx, cancel := context.WithTimeout(ctx, config.InvokeTimeout)
	defer cancel()
	invokeCtx = bridge.WithTraceparent(invokeCtx, traceparentOf(ctx))
	started := time.Now()
	result, err := g.hub.Invoke(invokeCtx, nodeID, protocol.OperationToolCall, map[string]any{"tool": toolName, "arguments": arguments})
	if g.invokes != nil {
		g.recordInvoke(ctx, userID, nodeID, toolName, time.Since(started), err)
	}
	return result, err
}

// recordInvoke 把调用结果按类别写入记录（只含元数据）。
func (g *Gateway) recordInvoke(ctx context.Context, userID, nodeID, toolName string, duration time.Duration, err error) {
	outcome := OutcomeSuccess
	errorCode := ""
	if err != nil {
		outcome = OutcomeError
		errorCode = string(core.ErrorCodeOf(err))
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = OutcomeTimeout
		} else if errors.Is(err, context.Canceled) {
			outcome = OutcomeCancelled
		}
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	// 记录失败不阻断调用结果，只打日志。
	if recordErr := g.invokes.RecordInvoke(recordCtx, userID, nodeID, toolName, outcome, duration, errorCode); recordErr != nil && g.logger != nil {
		g.logger.Warn("写入调用记录失败", "user", userID, "node", nodeID, "tool", toolName, "error", recordErr)
	}
}

// nodesContextHandler 返回用户节点清单。
func (g *Gateway) nodesContextHandler(userID string) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nodeList, err := g.nodeSvc.ListByOwner(ctx, userID)
		if err != nil {
			return gatewayToolResult(NodesContextToolName, nil, err)
		}
		type nodeView struct {
			NodeID       string   `json:"node_id"`
			Name         string   `json:"name"`
			Version      string   `json:"version,omitempty"`
			OS           string   `json:"os,omitempty"`
			Arch         string   `json:"arch,omitempty"`
			Online       bool     `json:"online"`
			Enabled      bool     `json:"enabled"`
			Capabilities []string `json:"capabilities"`
		}
		views := make([]nodeView, 0, len(nodeList))
		for _, node := range nodeList {
			views = append(views, nodeView{
				NodeID: node.ID, Name: node.Name, Version: node.Version,
				OS: node.OS, Arch: node.Arch,
				Online: g.hub.Online(node.ID), Enabled: node.Enabled,
				Capabilities: node.Capabilities,
			})
		}
		return gatewayToolResult(NodesContextToolName, map[string]any{"nodes": views}, nil)
	}
}

// acquireInflight / releaseInflight 每节点并发计数。
func (g *Gateway) acquireInflight(nodeID string) bool {
	g.inflightMu.Lock()
	defer g.inflightMu.Unlock()
	if g.inflight[nodeID] >= config.MaxInflightPerNode {
		return false
	}
	g.inflight[nodeID]++
	return true
}

func (g *Gateway) releaseInflight(nodeID string) {
	g.inflightMu.Lock()
	defer g.inflightMu.Unlock()
	if g.inflight[nodeID] > 0 {
		g.inflight[nodeID]--
	}
	if g.inflight[nodeID] == 0 {
		delete(g.inflight, nodeID)
	}
}

// ---- 小工具 ----

type mcpUserKey struct{}

func mcpContextWithUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, mcpUserKey{}, userID)
}

// UserIDFromContext 取 MCP 请求所属用户（invoke_logs 记录用）。
func UserIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(mcpUserKey{}).(string)
	return value
}

type contractMismatchError struct{ differences []fleet.ContractDifference }

func (contractMismatchError) Error() string { return "tool contract mismatch" }

// Differences 供上层把结构化差异写入错误详情。
func (e contractMismatchError) Differences() []fleet.ContractDifference { return e.differences }

func traceparentOf(ctx context.Context) string {
	value, _ := ctx.Value(traceparentKey{}).(string)
	return value
}

type traceparentKey struct{}

// WithTraceparent 记录入站 traceparent（httpx 层调用）。
func WithTraceparent(ctx context.Context, value string) context.Context {
	if strings.TrimSpace(value) == "" {
		return ctx
	}
	return context.WithValue(ctx, traceparentKey{}, value)
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if len(header) > 7 && strings.EqualFold(header[:7], "Bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func findDescriptor(descriptors []protocol.ToolDescriptor, name string) (protocol.ToolDescriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.Name == name {
			return descriptor, true
		}
	}
	return protocol.ToolDescriptor{}, false
}

func containsHash(hashes []string, target string) bool {
	for _, hash := range hashes {
		if hash == target {
			return true
		}
	}
	return false
}

func writeMCPError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

// gatewayToolResult 把节点结果包装为 MCP CallToolResult（照搬 nexusdock 形状）。
func gatewayToolResult(name string, result map[string]any, err error) (*mcp.CallToolResult, error) {
	if err == nil && result != nil {
		if _, hasContent := result["content"]; hasContent {
			if _, hasErrorFlag := result["isError"]; hasErrorFlag {
				encoded, encodeErr := json.Marshal(result)
				if encodeErr != nil {
					return nil, encodeErr
				}
				var proxied mcp.CallToolResult
				if decodeErr := json.Unmarshal(encoded, &proxied); decodeErr != nil {
					return nil, decodeErr
				}
				return &proxied, nil
			}
		}
	}
	if result == nil {
		result = map[string]any{}
	}
	if err != nil {
		result["tool"] = name
		result["error"] = err.Error()
		if mismatch, ok := err.(*core.CodedError); ok && mismatch.Code == "TOOL_CONTRACT_MISMATCH" {
			if cm, canCast := mismatch.Err.(contractMismatchError); canCast {
				result["contract_differences"] = cm.Differences()
			}
		}
	}
	encoded, encodeErr := json.Marshal(map[string]any{
		"isError":           err != nil,
		"structuredContent": result,
		"content":           []map[string]any{{"type": "text", "text": prettyJSON(result)}},
	})
	if encodeErr != nil {
		return nil, encodeErr
	}
	var response mcp.CallToolResult
	if decodeErr := json.Unmarshal(encoded, &response); decodeErr != nil {
		return nil, decodeErr
	}
	return &response, nil
}

func prettyJSON(value any) string {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}
