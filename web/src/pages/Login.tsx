import { useState } from "react";
import { ArrowRight, Terminal } from "lucide-react";
import { api, setCSRF } from "../api";

export function Login({ onLoggedIn }: { onLoggedIn: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  return (
    <div className="login-wrap">
      <div className="login-hero" aria-hidden>
        <span className="giant">AGENT</span>
        <span className="giant filled">HUB_</span>
      </div>
      <div className="login-meta">AGENTDOCK MULTI-TENANT GATEWAY / V1</div>
      <form
        className="login-card"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <h1>登录</h1>
        <p className="sub"><Terminal size={13} style={{ verticalAlign: -2 }} /> AUTH / AGENTHUB</p>
        <input
          placeholder="用户名（邮箱形态）"
          autoComplete="username"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />
        <input
          type="password"
          placeholder="密码"
          autoComplete="current-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        {error && <p className="error">{error}</p>}
        <button className="primary" type="submit" disabled={busy}>
          {busy ? "验证中" : "进入"}<ArrowRight size={15} />
        </button>
      </form>
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
