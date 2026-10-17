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

// CodeToolContractMismatch 是调用前契约复核失败的错误码。
const CodeToolContractMismatch core.ErrorCode = "TOOL_CONTRACT_MISMATCH"

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
		// 在途超限在这里预检并直接 429（DESIGN §6 "超限立即 429"）。
		// 为什么必须在 Handler 层：MCP 的一次 tools/call 响应状态码在请求开始时就
		// 确定了，进了工具处理器再想返回 429 只能得到 HTTP 200 + isError，
		// 客户端的 HTTP 层限流重试策略（按状态码）不会生效。
		if !g.userHasInflightCapacity(user.ID) {
			w.Header().Set("Retry-After", "1")
			writeMCPError(w, http.StatusTooManyRequests, string(core.CodeTooManyRequests), "concurrent request limit reached")
			return
		}
		// 每请求临时 server：按用户动态注册工具面（Stateless 模式下 handler
		// 只在本次请求内使用该 server，注册成本低且天然用户隔离）。
		server := mcp.NewServer(&mcp.Implementation{Name: "agenthub", Version: "1"}, &mcp.ServerOptions{})
		if err := g.registerUserTools(r.Context(), user.ID, server); err != nil {
			writeMCPError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "assemble tool surface failed")
			return
		}
		reqCtx := mcpContextWithUser(r.Context(), user.ID)
		reqCtx = WithTraceparent(reqCtx, r.Header.Get("traceparent"))
		g.handlerWithServer(server).ServeHTTP(w, r.WithContext(reqCtx))
	})
}

// handlerWithServer 为单次请求生成一个绑定该 server 的 handler。
// StreamableHTTPHandler 的 getServer 由构造时的闭包决定，这里为每请求
// 新建一个轻量 handler（Stateless 模式无会话存储，开销可忽略）。
func (g *Gateway) handlerWithServer(server *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:    true,
			JSONResponse: true,
			// hub 不是 localhost 本地服务：部署形态是反代之后、监听 127.0.0.1，
			// Host 头是反代/客户端使用的 IP 或域名。SDK 的 DNS rebinding 防护
			// 在"监听回环 + Host 非回环"时一律 403，会把所有经反代的访问全部
			// 拦下（实测 Cherry Studio 报 Forbidden: invalid Host header）。
			// hub 的访问控制完全由 Bearer token + 限流承担，故关闭该防护。
			DisableLocalhostProtection:   true,
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
			g.safeAddTool(server, nodeMCPTool(published.Descriptor), g.nodeToolHandler(userID, name))
		}
	}
	g.safeAddTool(server, nodesContextTool(), g.nodesContextHandler(userID))
	return nil
}

// safeAddTool 包装 SDK 的 AddTool：节点提供的 schema 是外部输入，
// SDK 对缺 type / 非 object 的 schema 直接 panic，而这里每请求都会装配工具面。
// 单个畸形 descriptor 不能打穿整个 /mcp 请求——跳过它并记日志，其余工具照常可用。
func (g *Gateway) safeAddTool(server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandler) {
	if tool == nil {
		return
	}
	name := tool.Name
	defer func() {
		if r := recover(); r != nil {
			if g.logger != nil {
				g.logger.Warn("跳过非法工具契约（节点 schema 畸形）", "tool", name, "panic", fmt.Sprint(r))
			}
		}
	}()
	server.AddTool(tool, handler)
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
//
// 安全边界：节点的 inputSchema 是外部输入，畸形契约（nil、缺 type、type 不是 object）
// 绝不能原样透传给 MCP SDK——AddTool 对非 object schema 直接 panic，而工具面在每个
// /mcp 请求内装配，单个畸形 descriptor 就能让该用户所有调用持续失败。因此这里强制
// 补齐 {"type":"object"} 底座；SDK 侧 panic 的最后一道防线见 safeAddTool。
//
// 深拷贝用 JSON round-trip：published.Descriptor 可能被多节点共享（fleet 合并结果），
// 浅拷贝会让注入的 node_id 污染内存中的公开契约。
func injectNodeIDSchema(schema map[string]any) map[string]any {
	encoded, _ := json.Marshal(schema)
	cloned := map[string]any{"type": "object"}
	// json.Unmarshal("null", &cloned) 会把整个 cloned 置为 nil map，
	// 之后任何赋值都是 "assignment to entry in nil map" panic——
	// nil schema 恰好序列化成 "null"。这里解码到临时变量再合并，
	// 保证 cloned 始终是非 nil map。
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err == nil {
		for key, value := range decoded {
			cloned[key] = value
		}
	}
	// `"properties": null` 或类型不是对象时，类型断言拿到的是 nil map，
	// 同样必须重建。
	if properties, ok := cloned["properties"].(map[string]any); ok && properties != nil {
		properties["node_id"] = map[string]any{
			"type":        "string",
			"description": "目标 AgentDock 节点 ID，来自 nodes_context 返回的节点列表。",
		}
	} else {
		cloned["properties"] = map[string]any{
			"node_id": map[string]any{
				"type":        "string",
				"description": "目标 AgentDock 节点 ID，来自 nodes_context 返回的节点列表。",
			},
		}
	}
	// 同理：required 必须是 []any 才能 append。
	required, _ := cloned["required"].([]any)
	for _, value := range required {
		if name, ok := value.(string); ok && name == "node_id" {
			// 已含 node_id：直接返回，避免 required 里出现重复项。
			return cloned
		}
	}
	cloned["required"] = append(required, "node_id")
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
		// retryable=true（DESIGN §3.3）：节点离线是暂态故障，平台应退避后重试，
		// 而不是当成永久错误放弃整个工具。
		return nil, core.NewRetryableError(core.CodeNodeOffline, "节点当前离线", nil)
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
	if !fleet.ContainsHash(published.AcceptedSemanticHashes, nodeHash) {
		return nil, &core.CodedError{
			Code:    CodeToolContractMismatch,
			Message: fmt.Sprintf("目标节点工具 %s 的契约不在当前兼容集合内，请刷新工具列表或升级该设备的 AgentDock", toolName),
			// 契约漂移不是"稍后重试能好"的错误：重试多少次结果都一样，
			// 必须让平台升级 AgentDock 或重新加载工具面。
			Retryable: false,
			Err: contractMismatchError{
				Code:          string(CodeToolContractMismatch),
				Message:       fmt.Sprintf("目标节点工具 %s 的契约不在当前兼容集合内，请刷新工具列表或升级该设备的 AgentDock", toolName),
				Tool:          toolName,
				NodeID:        node.ID,
				NodeName:      node.Name,
				NodeVersion:   node.Version,
				PublishedHash: published.ContractHash,
				NodeHash:      nodeHash,
				Differences:   fleet.ContractDifferences(published.Descriptor, target),
			},
		}
	}

	// 并发在途上限：超过即快速失败，防止单节点拖垮该用户的整条连接。
	// 两个维度都要查（DESIGN §6）：每节点 16 防单节点拖垮，
	// 每用户 16 防压力被分散到自己的多个节点上绕过单点限制。
	if !g.acquireInflight(userID, nodeID) {
		return nil, core.NewRetryableError(core.CodeTooManyRequests, "并发请求已达上限（节点或用户维度），请稍后重试", nil)
	}
	defer g.releaseInflight(userID, nodeID)

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

// userHasInflightCapacity 报告该用户当前在途请求是否还有余量。
// 只看用户维度：节点维度要到 callNodeTool 知道目标节点后才能判断。
func (g *Gateway) userHasInflightCapacity(userID string) bool {
	g.inflightMu.Lock()
	defer g.inflightMu.Unlock()
	return g.inflight["user:"+userID] < config.MaxInflightPerUser
}

// acquireInflight / releaseInflight 在途计数（节点维度 + 用户维度）。
// 任一维度已满即拒绝，且两者要么一起加要么一起不加，避免计数泄漏。
func (g *Gateway) acquireInflight(userID, nodeID string) bool {
	g.inflightMu.Lock()
	defer g.inflightMu.Unlock()
	userKey := "user:" + userID
	nodeKey := "node:" + nodeID
	if g.inflight[nodeKey] >= config.MaxInflightPerNode || g.inflight[userKey] >= config.MaxInflightPerUser {
		return false
	}
	g.inflight[nodeKey]++
	g.inflight[userKey]++
	return true
}

func (g *Gateway) releaseInflight(userID, nodeID string) {
	g.inflightMu.Lock()
	defer g.inflightMu.Unlock()
	for _, key := range []string{"user:" + userID, "node:" + nodeID} {
		if g.inflight[key] > 0 {
			g.inflight[key]--
		}
		if g.inflight[key] == 0 {
			delete(g.inflight, key)
		}
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

// contractMismatchError 是调用前契约复核失败的结构化详情，
// 字段与 nexusdock ToolContractMismatch 对齐（JSON 形状是 MCP 工具错误响应
// 的一部分，调整会直接影响调用方可见的契约）。
type contractMismatchError struct {
	Code          string                     `json:"code"`
	Message       string                     `json:"message"`
	Tool          string                     `json:"tool"`
	NodeID        string                     `json:"node_id"`
	NodeName      string                     `json:"node_name,omitempty"`
	NodeVersion   string                     `json:"node_version,omitempty"`
	PublishedHash string                     `json:"published_hash"`
	NodeHash      string                     `json:"node_hash"`
	Differences   []fleet.ContractDifference `json:"differences,omitempty"`
}

func (e contractMismatchError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "tool contract mismatch"
}

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
		// 错误文本分两类处理：
		//
		// 1. CodedError 是业务错误，Message 是我们自己写的、对调用方有意义的
		//    提示（如"节点当前离线""请指定 node_id"），可以透出。
		// 2. 其余是 fmt.Errorf 包装的内部错误（SQLite 表名/列名、文件路径、
		//    节点返回的原始报文），一律替换为固定文案——否则 /mcp 这个
		//    Bearer 认证的对外入口会把内部实现细节泄漏给任何持有 token 的调用方
		//    （与 httpx 层的 safeMessage 是两套独立机制，此前只有 httpx 做了过滤）。
		var coded *core.CodedError
		if errors.As(err, &coded) {
			result["error"] = coded.Message
			// 结构化错误字段：平台据此程序化分支，不解析 error 字符串。
			// DESIGN §3.3 要求 NODE_OFFLINE 带 retryable=true，让 Agent 知道可重试。

			result["code"] = string(coded.Code)
			result["retryable"] = coded.Retryable
			var mismatch contractMismatchError
			if coded.Code == CodeToolContractMismatch && errors.As(coded.Err, &mismatch) {
				// 用 errors.As 而非类型断言：CodedError 有 Unwrap，
				// 一旦加包装层类型断言会静默丢掉全部差异。
				result["tool_contract_mismatch"] = mismatch
			}
		} else {
			result["error"] = "internal error"
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
