import { useCallback, useEffect, useState } from "react";
import { UserPlus, Ban, CirclePlay, KeyRound, ShieldCheck, Eye, EyeOff, X } from "lucide-react";
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
  const [showCreatePwd, setShowCreatePwd] = useState(false);
  const [resetTarget, setResetTarget] = useState<User | null>(null);
  const [resetPwd, setResetPwd] = useState("");
  const [showResetPwd, setShowResetPwd] = useState(false);
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
  const submitResetPassword = async () => {
    if (!resetTarget) return;
    try {
      await api.post(`/v1/admin/users/${resetTarget.id}/reset-password`, { password: resetPwd });
      setResetTarget(null);
      setResetPwd("");
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  };

  return (
    <div className="card" data-panel="ADMIN/USERS">
      <h2><ShieldCheck size={16} />用户管理</h2>
      <div className="create-row">
        <input
          placeholder="用户名（邮箱形态）"
          style={{ width: 240 }}
          value={newUsername}
          onChange={(e) => setNewUsername(e.target.value)}
        />
        <div className="pwd-field">
          <input
            type={showCreatePwd ? "text" : "password"}
            placeholder="初始密码（至少 12 字符）"
            style={{ width: 240 }}
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
          />
          <button type="button" className="pwd-toggle" onClick={() => setShowCreatePwd((v) => !v)}
            aria-label={showCreatePwd ? "隐藏密码" : "显示密码"}>
            {showCreatePwd ? <EyeOff size={14} /> : <Eye size={14} />}
          </button>
        </div>
        <button className="primary" onClick={() => void create()}>
          <UserPlus size={14} />创建用户
        </button>
      </div>
      {created && (
        <div className="token-result">
          <p>用户已创建。MCP Token（仅显示一次）：</p>
          <code>{created.token}</code>
          <p className="muted" style={{ marginTop: 8 }}>MCP endpoint：<code>{created.endpoint}</code></p>
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
              <td style={{ fontWeight: 500 }}>{user.username}</td>
              <td>
                <span className="status-chip">
                  {user.role === "admin" ? (
                    <><span className="status-dot online" />管理员</>
                  ) : (
                    <><span className="status-dot offline" />用户</>
                  )}
                </span>
              </td>
              <td>
                <span className="status-chip">
                  <span className={`status-dot ${user.status === "active" ? "online" : "disabled"}`} />
                  {user.status === "active" ? "正常" : "已禁用"}
                </span>
              </td>
              <td className="muted">{user.must_change_password ? "是" : "否"}</td>
              <td className="muted">{new Date(user.created_at).toLocaleDateString()}</td>
              <td>
                {user.status === "active" ? (
                  <button onClick={() => void disable(user.id)}><Ban size={13} />禁用</button>
                ) : (
                  <button onClick={() => void enable(user.id)}><CirclePlay size={13} />启用</button>
                )}
                <button onClick={() => { setResetTarget(user); setResetPwd(""); }}><KeyRound size={13} />重置密码</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {resetTarget && (
        <div className="modal-mask" onClick={() => setResetTarget(null)}>
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <div className="modal-head">
              <h2 style={{ margin: 0 }}><KeyRound size={16} />重置密码 — {resetTarget.username}</h2>
              <button className="pwd-toggle" onClick={() => setResetTarget(null)} aria-label="关闭"><X size={15} /></button>
            </div>
            <p className="muted" style={{ margin: "10px 0 14px" }}>
              重置后该用户全部会话失效，下次登录强制改密。
            </p>
            <div className="pwd-field" style={{ width: "100%" }}>
              <input
                type={showResetPwd ? "text" : "password"}
                placeholder="新密码（至少 12 字符）"
                style={{ width: "100%" }}
                autoFocus
                value={resetPwd}
                onChange={(e) => setResetPwd(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && void submitResetPassword()}
              />
              <button type="button" className="pwd-toggle" onClick={() => setShowResetPwd((v) => !v)}
                aria-label={showResetPwd ? "隐藏密码" : "显示密码"}>
                {showResetPwd ? <EyeOff size={14} /> : <Eye size={14} />}
              </button>
            </div>
            <div style={{ display: "flex", gap: 10, justifyContent: "flex-end", marginTop: 14 }}>
              <button onClick={() => setResetTarget(null)}>取消</button>
              <button className="primary" onClick={() => void submitResetPassword()}>确认重置</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
