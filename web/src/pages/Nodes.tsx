import { useCallback, useEffect, useState } from "react";
import { api, ApiError } from "../api";

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
      // 明文只出现一次：展示后要求用户复制。
      setResultToken(result.token);
    } catch (e) {
      setError(String(e));
    }
  };

  const [resultToken, setResultToken] = useState("");

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
      <div className="card">
        <h2>MCP 接入</h2>
        <p className="muted">
          Agent 平台的 MCP endpoint：<code>{tokenInfo?.mcp_endpoint ?? "…"}</code>
        </p>
        <button onClick={() => void resetToken()}>重置 MCP Token</button>
        {resultToken && (
          <div className="token-result">
            <p>新 Token（仅显示一次，请立即复制）：</p>
            <code>{resultToken}</code>
          </div>
        )}
      </div>

      <div className="card">
        <h2>我的节点</h2>
        <button onClick={() => void createPairingCode()}>生成配对码</button>
        {pairing && (
          <div className="pairing-result">
            <p>
              配对码（10 分钟内有效，单次使用）：
              <code>{pairing.code}</code>
            </p>
            <p className="muted">在目标设备执行：<code>agentdock nexus pair --endpoint &lt;本站地址&gt; --code {pairing.code}</code></p>
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
                <td>{node.name}</td>
                <td><code>{node.device_id}</code></td>
                <td>
                  <span className={node.enabled ? (node.online ? "online" : "offline") : "disabled"}>
                    {node.enabled ? (node.online ? "在线" : "离线") : "已禁用"}
                  </span>
                </td>
                <td>{node.version}</td>
                <td>{node.capabilities.join(", ")}</td>
                <td>{node.last_seen_at ? new Date(node.last_seen_at).toLocaleString() : "—"}</td>
                <td>
                  {node.enabled
                    ? <button onClick={() => void disableNode(node.id)}>禁用</button>
                    : <button onClick={() => void enableNode(node.id)}>启用</button>}
                  <button className="danger" onClick={() => void deleteNode(node.id)}>删除</button>
                </td>
              </tr>
            ))}
            {nodes.length === 0 && <tr><td colSpan={7}>还没有节点，先在设备上用配对码完成配对。</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  );
}

export { ApiError };
