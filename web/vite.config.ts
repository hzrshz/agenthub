import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// 开发代理到本地 serve；构建产物输出 internal/httpx/web_dist 供 go:embed。
export default defineConfig({
  plugins: [react()],
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
