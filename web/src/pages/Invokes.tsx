import { useCallback, useEffect, useState } from "react";
import { ScrollText } from "lucide-react";
import { api } from "../api";
import { Pagination, PageMeta, FilterBar, UserOption } from "../components/Pagination";

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

const OUTCOME_LABEL: Record<string, string> = {
  success: "成功",
  error: "失败",
  timeout: "超时",
  cancelled: "已取消",
};

export function Invokes({ admin = false }: { admin?: boolean }) {
  const [logs, setLogs] = useState<InvokeLog[]>([]);
  const [meta, setMeta] = useState<PageMeta>({ page: 1, per_page: 20, total: 0 });
  const [user, setUser] = useState("");
  const [outcome, setOutcome] = useState("");
  const [userOptions, setUserOptions] = useState<UserOption[]>([]);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    try {
      const params = new URLSearchParams({
        page: String(meta.page),
        limit: String(meta.per_page),
      });
      if (user) params.set("user", user);
      if (outcome) params.set("outcome", outcome);
      const resp = await api.get<InvokePage>(`${admin ? "/v1/admin/invokes" : "/v1/invokes"}?${params}`);
      setLogs(resp.items);
      setMeta({ page: resp.page, per_page: resp.per_page, total: resp.total });
      setError("");
      if (admin && userOptions.length === 0) {
        const stats = await api.get<{ stats: UserOption[] }>("/v1/admin/stats");
        setUserOptions(stats.stats.map((s) => ({ user_id: s.user_id, username: s.username })));
      }
    } catch (e) {
      setError(String(e));
    }
  }, [admin, meta.page, meta.per_page, user, outcome, userOptions.length]);

  useEffect(() => {
    void load();
    const timer = setInterval(() => void load(), 30000);
    return () => clearInterval(timer);
  }, [load]);

  const hasFilter = Boolean(user || outcome);
  const reset = () => {
    setUser("");
    setOutcome("");
    setMeta((m) => ({ ...m, page: 1 }));
  };

  return (
    <div className="card" data-panel={admin ? "ADMIN/INVOKES" : "USER/INVOKES"}>
      <h2><ScrollText size={16} />{admin ? "全局调用流水" : "调用记录"}</h2>
      <p className="muted" style={{ marginBottom: 10 }}>
        仅记录时间、节点、工具、结果与耗时；工具参数与返回内容不落库。
      </p>
      <FilterBar
        admin={admin}
        userOptions={userOptions}
        user={user}
        setUser={(v) => {
          setUser(v);
          setMeta((m) => ({ ...m, page: 1 }));
        }}
        outcome={outcome}
        setOutcome={(v) => {
          setOutcome(v);
          setMeta((m) => ({ ...m, page: 1 }));
        }}
        onReset={reset}
        hasFilter={hasFilter}
      />
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
              <td style={{ fontFamily: "var(--font-mono)", fontSize: 12.5 }}>
                {new Date(log.occurred_at).toLocaleString()}
              </td>
              {admin && <td>{log.username || <code>{log.user_id.slice(0, 12)}…</code>}</td>}
              <td><code>{log.node_id.slice(0, 16)}…</code></td>
              <td style={{ fontFamily: "var(--font-mono)", fontSize: 12.5 }}>{log.tool_name}</td>
              <td><span className={`outcome-${log.outcome} status-chip`}><span className={`status-dot ${log.outcome === "success" ? "online" : log.outcome === "error" ? "offline" : "disabled"}`} />{OUTCOME_LABEL[log.outcome] ?? log.outcome}</span></td>
              <td style={{ fontFamily: "var(--font-mono)", fontSize: 12.5 }}>{log.duration_ms} ms</td>
              <td className="muted">{log.error_code || "—"}</td>
            </tr>
          ))}
          {logs.length === 0 && (
            <tr><td colSpan={admin ? 7 : 6} className="muted">暂无调用记录。</td></tr>
          )}
        </tbody>
      </table>
      <Pagination
        meta={meta}
        onPage={(page) => setMeta((m) => ({ ...m, page }))}
        onPerPage={(per_page) => setMeta((m) => ({ ...m, per_page, page: 1 }))}
      />
    </div>
  );
}
