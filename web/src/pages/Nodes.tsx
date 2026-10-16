import { useCallback, useEffect, useState } from "react";
import {
  Plus, RotateCcw, Trash2, Ban, CirclePlay, Copy, PlugZap, Radio, Link2,
} from "lucide-react";
import { api } from "../api";

interface Node {
  id: string;
  device_id: string;
  name: string;
  enabled: boolean;
  version: string;
  os: string;
  arch: string;
  capabilities: string[];
  online: boolean;
  last_seen_at: string | null;
}

interface PairingCode {
  code: string;
  expires_at: string;
}

interface TokenInfo {
  has_token: boolean;
  mcp_endpoint: string;
}

export function Nodes() {
  const [nodes, setNodes] = useState<Node[]>([]);
  const [pairing, setPairing] = useState<PairingCode | null>(null);
  const [tokenInfo, setTokenInfo] = useState<TokenInfo | null>(null);
  const [resultToken, setResultToken] = useState("");
  const [copied, setCopied] = useState("");
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    try {
      const nodesResp = await api.get<{ nodes: Node[] }>("/v1/nodes");
      setNodes(nodesResp.nodes);
      setTokenInfo(await api.get<TokenInfo>("/v1/mcp-token"));
    } catch (e) {
      setError(String(e));
    }
  }, []);

  useEffect(() => {
    void refresh();
    const timer = setInterval(() => void refresh(), 10000);
    return () => clearInterval(timer);
  }, [refresh]);

  const copy = async (label: string, value: string) => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(label);
      setTimeout(() => setCopied(""), 1500);
    } catch {
      // 剪贴板不可用时静默（http 环境常见）。
    }
  };

  const createPairingCode = async () => {
    setError("");
    try {
      setPairing(await api.post<PairingCode>("/v1/nodes/pairing-codes"));
    } catch (e) {
      setError(String(e));
    }
  };

  const resetToken = async () => {
    if (!confirm("重置 MCP Token 后旧 Token 立即失效，Agent 平台需要更新配置。确定？")) return;
    try {
      const result = await api.post<{ token: string; mcp_endpoint: string }>("/v1/mcp-token/reset");
      setTokenInfo({ has_token: true, mcp_endpoint: result.mcp_endpoint });
      setResultToken(result.token); // 明文只出现一次
    } catch (e) {
      setError(String(e));
    }
  };

  const disableNode = async (id: string) => {
    try {
      await api.post(`/v1/nodes/${id}/disable`);
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  };
  const enableNode = async (id: string) => {
    try {
      await api.post(`/v1/nodes/${id}/enable`);
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  };
  const deleteNode = async (id: string) => {
    if (!confirm("删除节点后 Device Token 永久失效，AgentDock 需要重新配对。确定？")) return;
    try {
      await api.del(`/v1/nodes/${id}`);
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  };

  return (
    <div>
      <div className="card" data-panel="MCP/ENDPOINT">
        <h2><PlugZap size={16} />MCP 接入</h2>
        <p className="muted" style={{ marginBottom: 10 }}>
          Agent 平台的 MCP endpoint（Streamable HTTP + Bearer Token）
        </p>
        <div style={{ display: "flex", gap: 10, alignItems: "center", flexWrap: "wrap" }}>
          <code>{tokenInfo?.mcp_endpoint ?? "…"}</code>
          <button onClick={() => void copy("endpoint", tokenInfo?.mcp_endpoint ?? "")}>
            <Copy size={13} />{copied === "endpoint" ? "已复制" : "复制"}
          </button>
          <button className="danger" onClick={() => void resetToken()}>
            <RotateCcw size={13} />重置 Token
          </button>
        </div>
        {resultToken && (
          <div className="token-result">
            <p>新 Token（仅显示一次，请立即复制）：</p>
            <code>{resultToken}</code>
            <div style={{ marginTop: 8 }}>
              <button onClick={() => void copy("token", resultToken)}>
                <Copy size={13} />{copied === "token" ? "已复制" : "复制 Token"}
              </button>
            </div>
          </div>
        )}
      </div>

      <div className="card" data-panel="FLEET/NODES">
        <h2><Radio size={16} />我的节点</h2>
        <button className="primary" onClick={() => void createPairingCode()}>
          <Plus size={14} />生成配对码
        </button>
        {pairing && (
          <div className="pairing-result">
            <p>
              配对码（10 分钟内有效，单次使用）：<code>{pairing.code}</code>
              <button style={{ marginLeft: 8 }} onClick={() => void copy("pairing", pairing.code)}>
                <Copy size={12} />{copied === "pairing" ? "已复制" : "复制"}
              </button>
            </p>
            <p className="muted">
              <Link2 size={12} style={{ verticalAlign: -2 }} /> 在 AgentDock 设备上执行：
              <code>agentdock nexus pair --endpoint {tokenInfo?.mcp_endpoint?.replace(/\/mcp$/, "")} --code {pairing.code}</code>
            </p>
          </div>
        )}
        {error && <p className="error">{error}</p>}
        <table>
          <thead>
            <tr>
              <th>名称</th><th>设备 ID</th><th>状态</th><th>版本</th><th>能力</th><th>最后在线</th><th>操作</th>
            </tr>
          </thead>
          <tbody>
            {nodes.map((node) => (
              <tr key={node.id}>
                <td style={{ fontWeight: 500 }}>{node.name}</td>
                <td><code>{node.device_id}</code></td>
                <td>
                  <span className="status-chip">
                    <span className={`status-dot ${node.enabled ? (node.online ? "online" : "offline") : "disabled"}`} />
                    {node.enabled ? (node.online ? "在线" : "离线") : "已禁用"}
                  </span>
                </td>
                <td style={{ fontFamily: "var(--font-mono)", fontSize: 12.5 }}>{node.version || "—"}</td>
                <td className="muted">{node.capabilities.length > 0 ? node.capabilities.join(" · ") : "—"}</td>
                <td className="muted">{node.last_seen_at ? new Date(node.last_seen_at).toLocaleString() : "—"}</td>
                <td>
                  {node.enabled ? (
                    <button onClick={() => void disableNode(node.id)}><Ban size={13} />禁用</button>
                  ) : (
                    <button onClick={() => void enableNode(node.id)}><CirclePlay size={13} />启用</button>
                  )}
                  <button className="danger" onClick={() => void deleteNode(node.id)}><Trash2 size={13} />删除</button>
                </td>
              </tr>
            ))}
            {nodes.length === 0 && <tr><td colSpan={7} className="muted">还没有节点 —— 生成配对码，在设备上完成配对。</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  );
}
