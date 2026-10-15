import { useState } from "react";
import { api, setCSRF } from "../api";

export function Login({ onLoggedIn }: { onLoggedIn: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  return (
    <div className="login-wrap">
      <div className="card narrow">
        <h1>AgentHub</h1>
        <p className="muted">多用户 AgentDock 汇聚网关</p>
        <input placeholder="用户名（邮箱形态）" value={username} onChange={(e) => setUsername(e.target.value)} />
        <input type="password" placeholder="密码" value={password}
          onChange={(e) => setPassword(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && void submit()} />
        {error && <p className="error">{error}</p>}
        <button disabled={busy} onClick={() => void submit()}>{busy ? "登录中…" : "登录"}</button>
      </div>
    </div>
  );

  async function submit() {
    setBusy(true);
    setError("");
    try {
      const response = await fetch("/v1/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      const data = await response.json();
      if (!response.ok) {
        setError(data.message ?? "登录失败");
        return;
      }
      setCSRF(data.csrf_token);
      onLoggedIn();
    } catch {
      setError("网络错误");
    } finally {
      setBusy(false);
    }
  }
}
