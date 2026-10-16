# AgentHub 容器镜像：多阶段构建。
# 产物为静态链接二进制（纯 Go 栈，CGO_ENABLED=0），运行层用 alpine
# （全架构官方镜像，含 loong64 之外的国产化目标）；国产化架构
# （arm64/loong64/riscv64）由 release workflow 的 docker/build-push-action 扩展。
#
# web 产物不进镜像构建：node:22-alpine 没有 loong64/riscv64 官方镜像，
# 且前端产物对全架构完全相同。构建顺序固定为：
#   make web  →  internal/httpx/web_dist 就位  →  docker build（go:embed 直接吃）
# 本地/CI 都在 docker build 之前执行 make web。

# 编译层用 golang:1.26（debian 系，官方发布全架构含 loong64/riscv64）；
# golang:1.26-alpine 缺少这两个国产化架构变体。产物是 CGO_ENABLED=0
# 静态二进制，与编译基础镜像的 libc 无关，运行层仍用 alpine。
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/agenthub ./cmd/agenthub

# 运行层选 alpine 而非 distroless：distroless 不发布 loong64/riscv64，
# 多架构国产化镜像必须用有全架构官方支持的基础镜像。
# 产物是 CGO_ENABLED=0 静态二进制，alpine 的 musl 不参与链接，行为一致。
FROM alpine:3.20
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
# 非 root 运行（alpine 内置 nobody）。
USER nobody
ENTRYPOINT ["/agenthub"]
CMD ["serve"]
