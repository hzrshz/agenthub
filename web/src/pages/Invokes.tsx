import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import { Pagination, PageMeta } from "../components/Pagination";

interface InvokeLog {
  id: string;
  occurred_at: string;
  user_id: string;
  username?: string;
  node_id: string;
  tool_name: string;
  outcome: string;
  duration_ms: number;
  error_code: string;
}

interface InvokePage {
  items: InvokeLog[];
  page: number;
  per_page: number;
  total: number;
}

const OUTCOMES = [
  { value: "", label: "全部结果" },
  { value: "success", label: "成功" },
  { value: "error", label: "失败" },
  { value: "timeout", label: "超时" },
  { value: "cancelled", label: "已取消" },
];

export function Invokes({ admin = false }: { admin?: boolean }) {
  const [logs, setLogs] = useState<InvokeLog[]>([]);
  const [meta, setMeta] = useState<PageMeta>({ page: 1, per_page: 20, total: 0 });
  const [tool, setTool] = useState("");
  const [toolInput, setToolInput] = useState("");
  const [outcome, setOutcome] = useState("");
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    try {
      const params = new URLSearchParams({
        page: String(meta.page),
        limit: String(meta.per_page),
      });
      if (tool) params.set("tool", tool);
      if (outcome) params.set("outcome", outcome);
      const resp = await api.get<InvokePage>(`${admin ? "/v1/admin/invokes" : "/v1/invokes"}?${params}`);
      setLogs(resp.items);
      setMeta({ page: resp.page, per_page: resp.per_page, total: resp.total });
      setError("");
    } catch (e) {
      setError(String(e));
    }
  }, [admin, meta.page, meta.per_page, tool, outcome]);

  useEffect(() => {
    void load();
    const timer = setInterval(() => void load(), 30000);
    return () => clearInterval(timer);
  }, [load]);

  const gotoPage = (page: number) => setMeta((m) => ({ ...m, page }));
  const setPerPage = (perPage: number) => setMeta((m) => ({ ...m, per_page: perPage, page: 1 }));

  return (
    <div className="card">
      <h2>{admin ? "全局调用流水" : "调用记录"}</h2>
      <p className="muted">仅记录时间、节点、工具、结果与耗时；工具参数与返回内容不落库。</p>
      <div className="filter-row">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            setTool(toolInput.trim());
            setMeta((m) => ({ ...m, page: 1 }));
          }}
        >
          <input
            placeholder="按工具名过滤（前缀）"
            value={toolInput}
            onChange={(e) => setToolInput(e.target.value)}
          />
          <button type="submit">查询</button>
          {(tool || outcome) && (
            <button
              type="button"
              onClick={() => {
                setTool("");
                setToolInput("");
                setOutcome("");
                setMeta((m) => ({ ...m, page: 1 }));
              }}
            >
              重置
            </button>
          )}
        </form>
        <select
          value={outcome}
          onChange={(e) => {
            setOutcome(e.target.value);
            setMeta((m) => ({ ...m, page: 1 }));
          }}
        >
          {OUTCOMES.map((o) => (
            <option key={o.value} value={o.value}>{o.label}</option>
          ))}
        </select>
      </div>
      {error && <p className="error">{error}</p>}
      <table>
        <thead>
          <tr>
            <th>时间</th>
            {admin && <th>用户</th>}
            <th>节点</th>
            <th>工具</th>
            <th>结果</th>
            <th>耗时</th>
            <th>错误码</th>
          </tr>
        </thead>
        <tbody>
          {logs.map((log) => (
            <tr key={log.id}>
              <td>{new Date(log.occurred_at).toLocaleString()}</td>
              {admin && <td>{log.username || <code>{log.user_id.slice(0, 12)}…</code>}</td>}
              <td><code>{log.node_id.slice(0, 16)}…</code></td>
              <td>{log.tool_name}</td>
              <td><span className={`outcome-${log.outcome}`}>{outcomeLabel(log.outcome)}</span></td>
              <td>{log.duration_ms} ms</td>
              <td>{log.error_code || "—"}</td>
            </tr>
          ))}
          {logs.length === 0 && (
            <tr><td colSpan={admin ? 7 : 6}>暂无调用记录。</td></tr>
          )}
        </tbody>
      </table>
      <Pagination
        meta={meta}
        onPage={gotoPage}
        onPerPage={setPerPage}
      />
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
