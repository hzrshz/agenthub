import { useState } from "react";
import { ArrowRight, Terminal } from "lucide-react";
import { api, setCSRF, ApiError } from "../api";

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
      // 登录也走统一 api 封装：API_BASE 前缀（子路径部署）与错误处理
      // 只有一份，避免再出现绕过封装的裸 fetch("/v1/...")。
      const data = await api.post<{ csrf_token: string }>("/v1/auth/login", { username, password });
      setCSRF(data.csrf_token);
      onLoggedIn();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "网络错误");
    } finally {
      setBusy(false);
    }
  }
}
