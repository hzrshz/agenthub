package httpx

import (
	"encoding/json"
	"errors"
	"net/http"

	protocol "github.com/uvwt/agentdock-protocol"

	"github.com/uvwt/agenthub/internal/core"
	"github.com/uvwt/agenthub/internal/nodes"
)

// ---- 用户侧节点 API ----

type pairingCodeResponse struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expires_at"`
}

func (s *Server) handleCreatePairingCode(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	code, err := s.nodes.CreatePairingCode(r.Context(), session.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "create pairing code failed")
		return
	}
	writeJSON(w, http.StatusCreated, pairingCodeResponse{
		Code: code.Code, ExpiresAt: code.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	list, err := s.nodes.ListByOwner(r.Context(), session.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "list nodes failed")
		return
	}
	// 在线状态以 Hub 实时连接为准，不落库。
	for i := range list {
		list[i].Online = s.hub.Online(list[i].ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": list})
}

func (s *Server) handleDisableNode(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	node, err := s.nodes.SetEnabled(r.Context(), session.UserID, r.PathValue("id"), false)
	if err != nil {
		writeNodeError(w, err)
		return
	}
	// 禁用即踢线：现有连接断开，新连接被 SetEnabled 后的存储查询拒绝。
	s.hub.Disconnect(node.ID)
	// 启停会改变 fleet 的 enabled provider 集，必须重算，
	// 否则公开契约残留指向已下线节点（DESIGN §3.3 离线即撤工具）。
	if s.fleet != nil {
		s.fleet.ReconcileNode(node.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": node})
}

func (s *Server) handleEnableNode(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	node, err := s.nodes.SetEnabled(r.Context(), session.UserID, r.PathValue("id"), true)
	if err != nil {
		writeNodeError(w, err)
		return
	}
	if s.fleet != nil {
		s.fleet.ReconcileNode(node.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": node})
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	nodeID := r.PathValue("id")
	if err := s.nodes.Delete(r.Context(), session.UserID, nodeID); err != nil {
		writeNodeError(w, err)
		return
	}
	// 删除即吊销：device token 哈希随行删除，旧 token 永久失效。
	s.hub.Disconnect(nodeID)
	// 节点消失后重算：其独占提供的工具应从工具面下架。
	if s.fleet != nil {
		s.fleet.ReconcileNode(nodeID)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeNodeError 节点域错误 → HTTP 状态；NotFound 不区分"不存在"与"不属于你"。
func writeNodeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, nodes.ErrNodeNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "node not found")
	case errors.Is(err, nodes.ErrNodeDisabled):
		writeError(w, http.StatusConflict, "NODE_DISABLED", "node is disabled")
	case errors.Is(err, nodes.ErrNodeLimitReached):
		writeError(w, http.StatusConflict, "NODE_LIMIT_REACHED", "node limit reached")
	case errors.Is(err, nodes.ErrPairingCodeInvalid):
		writeError(w, http.StatusBadRequest, "PAIRING_CODE_INVALID", "pairing code invalid or expired")
	case errors.Is(err, nodes.ErrNodeExists):
		// 同 device_id 重复配对是冲突而非服务端错误；
		// 裸 errors.New 会落 default 分支变成 500，误导 AgentDock 重试。
		writeError(w, http.StatusConflict, "NODE_EXISTS", "device already paired")
	default:
		writeAuthError(w, err)
	}
}

// ---- 节点侧（AgentDock）API ----

// handleNodePair 兑换配对码：POST /v1/nodes/pair。
// 公网无认证入口，配对码爆破防护由 pairRateLimiter 中间件承担。
func (s *Server) handleNodePair(w http.ResponseWriter, r *http.Request) {
	var request protocol.PairRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid pair body")
		return
	}
	node, err := s.nodes.Pair(r.Context(), nodes.PairInput(request))
	if err != nil {
		writeNodeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, protocol.PairResponse{
		Node:        protocol.PairedNode{ID: node.ID},
		DeviceToken: node.DeviceToken,
	})
}

// handleNodeConnect 是节点 WebSocket 入口：device token 认证后交给 Hub。
func (s *Server) handleNodeConnect(w http.ResponseWriter, r *http.Request) {
	node, err := s.nodes.AuthenticateDeviceToken(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		// 认证失败必须留痕（DESIGN §7）：设备 token 被盗用/爆破在这里露头。
		s.logger().Warn("节点连接认证失败", "remote", r.RemoteAddr, "error", err)
		switch core.ErrorCodeOf(err) {
		case core.CodeAuthRequired, core.CodeInvalidToken, core.CodeTokenRevoked:
			writeError(w, http.StatusUnauthorized, "INVALID_DEVICE_TOKEN", "device token invalid")
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "device authentication failed")
		}
		return
	}
	// Accept 成功后连接已升级为 WebSocket，不能再写 HTTP 响应，
	// 因此拒绝原因只能记日志（协议版本不符时已通过错误帧回传给节点）。
	if err := s.hub.Accept(w, r, node.ID, s.public); err != nil {
		s.logger().Warn("节点握手拒绝", "node", node.ID, "user", node.OwnerUserID, "error", err)
	}
}
