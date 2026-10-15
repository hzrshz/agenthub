import { useCallback, useEffect, useState } from "react";
import { api } from "../api";

interface User {
  id: string;
  username: string;
  role: string;
  status: string;
  must_change_password: boolean;
  created_at: string;
}

export function AdminUsers() {
  const [users, setUsers] = useState<User[]>([]);
  const [newUsername, setNewUsername] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [created, setCreated] = useState<{ token: string; endpoint: string } | null>(null);
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    try {
      const resp = await api.get<{ users: User[] }>("/v1/admin/users");
      setUsers(resp.users);
    } catch (e) {
      setError(String(e));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const create = async () => {
    setError("");
    setCreated(null);
    try {
      const resp = await api.post<{ user: User; mcp_token: string; mcp_endpoint: string }>("/v1/admin/users", {
        username: newUsername,
        password: newPassword,
      });
      setCreated({ token: resp.mcp_token, endpoint: resp.mcp_endpoint });
      setNewUsername("");
      setNewPassword("");
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  };

  const disable = async (id: string) => {
    if (!confirm("禁用后该用户全部节点断开、MCP Token 立即失效。确定？")) return;
    try {
      await api.post(`/v1/admin/users/${id}/disable`);
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  };
  const enable = async (id: string) => {
    try {
      await api.post(`/v1/admin/users/${id}/enable`);
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  };
  const resetPassword = async (id: string) => {
    const password = prompt("输入新密码（至少 12 字符）");
    if (!password) return;
    try {
      await api.post(`/v1/admin/users/${id}/reset-password`, { password });
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  };

  return (
    <div className="card">
      <h2>用户管理</h2>
      <div className="create-row">
        <input placeholder="用户名（邮箱形态）" value={newUsername} onChange={(e) => setNewUsername(e.target.value)} />
        <input placeholder="初始密码（至少 12 字符）" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} />
        <button onClick={() => void create()}>创建用户</button>
      </div>
      {created && (
        <div className="token-result">
          <p>用户已创建。MCP Token（仅显示一次）：<code>{created.token}</code></p>
          <p className="muted">MCP endpoint：<code>{created.endpoint}</code></p>
        </div>
      )}
      {error && <p className="error">{error}</p>}
      <table>
        <thead>
          <tr><th>用户名</th><th>角色</th><th>状态</th><th>待改密</th><th>创建时间</th><th>操作</th></tr>
        </thead>
        <tbody>
          {users.map((user) => (
            <tr key={user.id}>
              <td>{user.username}</td>
              <td>{user.role === "admin" ? "管理员" : "用户"}</td>
              <td>{user.status === "active" ? "正常" : "已禁用"}</td>
              <td>{user.must_change_password ? "是" : "否"}</td>
              <td>{new Date(user.created_at).toLocaleDateString()}</td>
              <td>
                {user.status === "active"
                  ? <button onClick={() => void disable(user.id)}>禁用</button>
                  : <button onClick={() => void enable(user.id)}>启用</button>}
                <button onClick={() => void resetPassword(user.id)}>重置密码</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
