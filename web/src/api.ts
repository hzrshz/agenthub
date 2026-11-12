// API 客户端：统一携带 CSRF 头与错误处理。
const CSRF_KEY = "agenthub_csrf";

// API 根路径：从本模块加载地址推导，而不是写死 "/"。
//
// 为什么不能用 document.baseURI：子路径部署（nginx 把 /app/ 剥前缀转给
// hub）时，hub 的 SPA 回退会把 index.html 应答到任意深层路径，页面 URL
// 并不等于部署前缀。而生产 bundle 固定位于 <前缀>/assets/ 下，
// new URL("..", import.meta.url) 精确得到部署前缀：根路径部署是空串，
// /app 部署则是 "/app"；dev 模式模块在 /src/ 下同样成立。部署形态差异
// 全部留在反代层，编译/产物与部署无关。与 vite base="./" 配套。
const API_BASE = new URL("..", import.meta.url).pathname.replace(/\/+$/, "");

export function getCSRF(): string {
  return sessionStorage.getItem(CSRF_KEY) ?? "";
}

export function setCSRF(token: string) {
  sessionStorage.setItem(CSRF_KEY, token);
}

export function clearCSRF() {
  sessionStorage.removeItem(CSRF_KEY);
}

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const csrf = getCSRF();
  if (csrf) headers["X-AgentHub-CSRF"] = csrf;
  const response = await fetch(API_BASE + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
  });
  if (response.status === 401 && path !== "/v1/auth/login") {
    clearCSRF();
    window.location.hash = "#/login";
    throw new ApiError(401, "AUTH_REQUIRED", "登录已过期");
  }
  const data = response.headers.get("content-type")?.includes("application/json")
    ? await response.json()
    : {};
  if (!response.ok) {
    const err = data as { error?: string; message?: string };
    throw new ApiError(response.status, err.error ?? "UNKNOWN", err.message ?? "请求失败");
  }
  return data as T;
}

export const api = {
  get: <T>(path: string) => request<T>("GET", path),
  post: <T>(path: string, body?: unknown) => request<T>("POST", path, body),
  del: <T>(path: string) => request<T>("DELETE", path),
};
