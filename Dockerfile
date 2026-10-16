# AgentHub 容器镜像：装配方言——静态二进制来自构建上下文（不在镜像内编译）。
# 原因：Docker Hub 官方库（golang/alpine/node）全都不发布 loong64，
# 任何基于官方基础镜像的多架构编译在 loong64 上都解析失败；
# 而二进制由 Go 原生编译（make ci / release binaries job）覆盖全部架构。
#   CI 路径：release.yml image job 先下载 binaries 矩阵产物，再 docker build；
#   本地路径：make release-linux 产出 dist/ 后执行 docker build。
# 产物要求：构建上下文根下存在 bin/agenthub-<os>-<arch>[.exe]（CI 装配），
#   或单架构本地构建时用 build-arg BIN_NAME 指定（默认 bin/agenthub-linux-amd64）。

ARG BIN_NAME=bin/agenthub-linux-amd64

# 运行层用 scratch：静态二进制不依赖任何 libc/基础系统，scratch 架构无关。
# 手工装配两件运行时依赖（都来自官方发行物，架构无关）：
#   - CA 证书（HTTPS 出站）——由 CI 从 build 阶段导出或本地准备
#   - 时区数据
# 本地构建时需要 bin/ca-certificates.crt + bin/zoneinfo.zip（make release-linux 产出）。
FROM scratch
# ARG 在 FROM 前声明后，必须在阶段内重新声明才能被 COPY 引用。
ARG BIN_NAME
COPY ${BIN_NAME} /agenthub
COPY certs/ca-certificates.crt /etc/ssl/certs/
COPY certs/zoneinfo.zip /zoneinfo.zip
ENV TZ=Asia/Shanghai \
    ZONEINFO=/zoneinfo.zip
# 数据目录（SQLite 单文件）以卷挂载持久化。
# 容器内必须监听 0.0.0.0 才能被端口映射访问（默认 127.0.0.1 仅本机）。
# scratch 无用户数据库，65534 是通用 nobody UID/GID。
VOLUME ["/data"]
ENV HUB_DATA_DIR=/data \
    HUB_LISTEN=0.0.0.0:8787
EXPOSE 8787
USER 65534:65534
ENTRYPOINT ["/agenthub"]
CMD ["serve"]
