import { useCallback, useEffect, useState } from "react";
import { api, getCSRF, clearCSRF } from "./api";
import { Login } from "./pages/Login";
import { Nodes } from "./pages/Nodes";
import { Invokes } from "./pages/Invokes";
import { AdminUsers } from "./pages/AdminUsers";
import { AdminOverview } from "./pages/AdminOverview";

export interface Me {
  user_id: string;
  username: string;
  role: string;
  must_change_password: boolean;
}

export type Page = "nodes" | "invokes" | "admin-users" | "admin-overview";

export function App() {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);
  const [page, setPage] = useState<Page>("nodes");

  const refresh = useCallback(async () => {
    if (!getCSRF()) {
      setMe(null);
      setLoading(false);
      return;
    }
    try {
      setMe(await api.get<Me>("/v1/auth/me"));
    } catch {
      setMe(null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const logout = async () => {
    try {
      await api.post("/v1/auth/logout");
    } finally {
      clearCSRF();
      setMe(null);
    }
  };

  if (loading) return <div className="loading">加载中…</div>;
  if (!me) return <Login onLoggedIn={refresh} />;
  if (me.must_change_password) {
    return <ChangePassword onDone={logout} />;
  }

  const isAdmin = me.role === "admin";
  return (
    <div className="app">
      <header className="topbar">
        <span className="brand">AgentHub</span>
        <nav>
          <button className={page === "nodes" ? "active" : ""} onClick={() => setPage("nodes")}>我的节点</button>
          <button className={page === "invokes" ? "active" : ""} onClick={() => setPage("invokes")}>调用记录</button>
          {isAdmin && (
            <>
              <button className={page === "admin-users" ? "active" : ""} onClick={() => setPage("admin-users")}>用户管理</button>
              <button className={page === "admin-overview" ? "active" : ""} onClick={() => setPage("admin-overview")}>全局概览</button>
            </>
          )}
        </nav>
        <span className="whoami">
          {me.username}
          <button onClick={() => void logout()}>退出</button>
        </span>
      </header>
      <main>
        {page === "nodes" && <Nodes />}
        {page === "invokes" && <Invokes />}
        {isAdmin && page === "admin-users" && <AdminUsers />}
        {isAdmin && page === "admin-overview" && <AdminOverview />}
      </main>
    </div>
  );
}

function ChangePassword({ onDone }: { onDone: () => void }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [error, setError] = useState("");
  return (
    <div className="card narrow">
      <h2>首次登录，请修改密码</h2>
      <input type="password" placeholder="当前密码" value={current} onChange={(e) => setCurrent(e.target.value)} />
      <input type="password" placeholder="新密码（至少 12 字符）" value={next} onChange={(e) => setNext(e.target.value)} />
      {error && <p className="error">{error}</p>}
      <button
        onClick={async () => {
          try {
            await api.post("/v1/auth/password", { current_password: current, new_password: next });
            alert("密码已修改，请重新登录");
            onDone();
          } catch (e) {
            setError(String(e));
          }
        }}
      >
        修改密码
      </button>
    </div>
  );
}
