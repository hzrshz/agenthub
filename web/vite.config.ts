import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// 开发代理到本地 serve；构建产物输出 internal/httpx/web_dist 供 go:embed。
export default defineConfig({
  plugins: [react()],
  // base 相对化：产物里资源引用变成 ./assets/... 而非 /assets/...，
  // 同一份 web_dist 在根路径与反代子路径（nginx 把 /app/ 剥前缀转给 hub）
  // 部署下都能加载。API 前缀不在这里固定，由 api.ts 在运行时从加载地址
  // 推导——部署形态差异全部留在反代层，编译/产物/CI 与部署无关。
  base: "./",
  build: {
    outDir: "../internal/httpx/web_dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/v1": "http://127.0.0.1:8787",
      "/mcp": "http://127.0.0.1:8787",
    },
  },
});
