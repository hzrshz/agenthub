import { useEffect, useState } from "react";
import { api } from "../api";
import { Invokes } from "./Invokes";

interface UserStats {
  user_id: string;
  recent_calls: number;
}

export function AdminOverview() {
  const [stats, setStats] = useState<UserStats[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    const load = async () => {
      try {
        const resp = await api.get<{ stats: UserStats[] }>("/v1/admin/stats");
        setStats(resp.stats);
      } catch (e) {
        setError(String(e));
      }
    };
    void load();
    const timer = setInterval(() => void load(), 30000);
    return () => clearInterval(timer);
  }, []);

  return (
    <div>
      <div className="card">
        <h2>近 24 小时调用量</h2>
        {error && <p className="error">{error}</p>}
        <table>
          <thead><tr><th>用户</th><th>调用次数</th></tr></thead>
          <tbody>
            {stats.map((stat) => (
              <tr key={stat.user_id}>
                <td><code>{stat.user_id.slice(0, 20)}…</code></td>
                <td>{stat.recent_calls}</td>
              </tr>
            ))}
            {stats.length === 0 && <tr><td colSpan={2}>近 24 小时无调用。</td></tr>}
          </tbody>
        </table>
      </div>
      <Invokes admin />
    </div>
  );
}
