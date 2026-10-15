import { useEffect, useState } from "react";
import { api } from "../api";

interface InvokeLog {
  id: string;
  occurred_at: string;
  node_id: string;
  tool_name: string;
  outcome: string;
  duration_ms: number;
  error_code: string;
}

export function Invokes({ admin = false }: { admin?: boolean }) {
  const [logs, setLogs] = useState<InvokeLog[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    const load = async () => {
      try {
        const resp = await api.get<{ invokes: InvokeLog[] }>(admin ? "/v1/admin/invokes" : "/v1/invokes");
        setLogs(resp.invokes);
      } catch (e) {
        setError(String(e));
      }
    };
    void load();
    const timer = setInterval(() => void load(), 10000);
    return () => clearInterval(timer);
  }, [admin]);

  return (
    <div className="card">
      <h2>{admin ? "全局调用流水" : "调用记录"}</h2>
      <p className="muted">仅记录时间、节点、工具、结果与耗时；工具参数与返回内容不落库。</p>
      {error && <p className="error">{error}</p>}
      <table>
        <thead>
          <tr><th>时间</th><th>节点</th><th>工具</th><th>结果</th><th>耗时</th><th>错误码</th></tr>
        </thead>
        <tbody>
          {logs.map((log) => (
            <tr key={log.id}>
              <td>{new Date(log.occurred_at).toLocaleString()}</td>
              <td><code>{log.node_id.slice(0, 16)}…</code></td>
              <td>{log.tool_name}</td>
              <td><span className={`outcome-${log.outcome}`}>{outcomeLabel(log.outcome)}</span></td>
              <td>{log.duration_ms} ms</td>
              <td>{log.error_code || "—"}</td>
            </tr>
          ))}
          {logs.length === 0 && <tr><td colSpan={6}>暂无调用记录。</td></tr>}
        </tbody>
      </table>
    </div>
  );
}

function outcomeLabel(outcome: string): string {
  switch (outcome) {
    case "success": return "成功";
    case "error": return "失败";
    case "timeout": return "超时";
    case "cancelled": return "已取消";
    default: return outcome;
  }
}
