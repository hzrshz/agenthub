# AgentHub 容器镜像：多阶段构建。
# 产物为静态链接二进制（纯 Go 栈，CGO_ENABLED=0），运行层用 distroless static，
# 无 shell、无包管理器，攻击面最小；国产化架构（arm64/loong64/riscv64）由
# release workflow 的 docker/build-push-action 按 matrix 扩展。
# web 产物必须先于 go build 就位（go:embed），故第一阶段先做前端构建。
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# npm build 的 outDir 是 ../internal/httpx/web_dist（相对 web/），
# 上一阶段工作目录为 /src/web，产物已落在 /src/internal/httpx/web_dist。
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/agenthub ./cmd/agenthub

FROM gcr.io/distroless/static-debian12:nonroot
# 时区数据：界面/日志按本地时区展示。
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /zoneinfo.zip
ENV TZ=Asia/Shanghai \
    ZONEINFO=/zoneinfo.zip
COPY --from=build /out/agenthub /agenthub
# 数据目录（SQLite 单文件）以卷挂载持久化。
# 容器内必须监听 0.0.0.0 才能被端口映射访问（默认 127.0.0.1 仅本机）。
VOLUME ["/data"]
ENV HUB_DATA_DIR=/data \
    HUB_LISTEN=0.0.0.0:8787
EXPOSE 8787
USER nonroot:nonroot
ENTRYPOINT ["/agenthub"]
CMD ["serve"]
