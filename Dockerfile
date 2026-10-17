# AgentHub 容器镜像：装配方言——静态二进制来自构建上下文（不在镜像内编译）。
# 原因：Docker Hub 官方库（golang/alpine/node）全都不发布 loong64，
# 任何基于官方基础镜像的多架构构建在 loong64 上都解析失败；
# 而二进制由 Go 原生编译（make ci / release binaries job）覆盖全部架构。
#   CI 路径：release.yml image job 先下载 binaries 矩阵产物，再 docker build；
#   本地路径：make release-linux 产出 dist/ 后执行 docker build。
# 产物要求：构建上下文根下存在 bin/agenthub-<os>-<arch>[.exe]（CI 装配），
#   或单架构本地构建时用 build-arg BIN_NAME 指定（默认 bin/agenthub-linux-amd64）。
#
# 因此本文件**不引用任何外部基础镜像**——包括 scratch 之外的。
# 曾尝试用 `FROM busybox AS datadir` 建带属主的 /data，但 busybox 官方 tag
# 只发布 amd64/arm/386，loong64 上 manifest 直接解析失败（与本文件头
# 声明的约束冲突）。改为从构建上下文复制一个目录，用 COPY --chown 设属主。

ARG BIN_NAME=bin/agenthub-linux-amd64

# 运行层用 scratch：静态二进制不依赖任何 libc/基础系统，scratch 架构无关。
# 手工装配两件运行时依赖（都来自官方发行物，架构无关）：
#   - CA 证书（HTTPS 出站）——由 CI 从 build 阶段导出或本地准备
#   - 时区数据
# 本地构建时需要 certs/ca-certificates.crt + certs/zoneinfo.zip。
FROM scratch
# ARG 在 FROM 前声明后，必须在阶段内重新声明才能被 COPY 引用。
ARG BIN_NAME
COPY ${BIN_NAME} /agenthub
COPY certs/ca-certificates.crt /etc/ssl/certs/
COPY certs/zoneinfo.zip /zoneinfo.zip
ENV TZ=Asia/Shanghai \
    ZONEINFO=/zoneinfo.zip

# 数据目录（SQLite 单文件）以卷挂载持久化。
# /data 必须在镜像内以 65534 属主预置：docker 自动创建的卷挂载点属主是
# root:root 0755，非 root 进程首次启动写 SQLite 会 EACCES；空卷会继承
# 镜像里该路径的属主与权限。COPY --chown 达成同样效果且不引入基础镜像。
# context/data/ 只是一个空目录（见 .gitkeep），实际内容由下面的 COPY 提供。
COPY --chown=65534:65534 context/data/ /data
VOLUME ["/data"]

# 容器内必须监听 0.0.0.0 才能被 Docker 网络访问（默认 127.0.0.1 仅本机）。
#
# 重要：容器里**不能**改成 127.0.0.1。容器有独立的网络命名空间，绑定回环
# 意味着只监听容器自身的 loopback，端口映射（-p）与同网络的其他容器（如
# 反向代理）都连不上——服务会表现为"启动了但访问不了"。
#
# 安全边界应落在**宿主侧的端口绑定**上，而不是容器内的监听地址：
#   - 反代在宿主机：docker run -p 127.0.0.1:8787:8787
#     端口只发布到宿主回环，公网与其他容器都访问不到，但反代能正常反代。
#   - 反代也在容器：让两者处于同一 Docker 网络，用服务名互访
#     （compose 里 ports 写 127.0.0.1:8787:8787 或完全不发布端口）。
#
# 监听值可用 -e HUB_LISTEN 覆盖，但不要改成回环地址。
ENV HUB_DATA_DIR=/data \
    HUB_LISTEN=0.0.0.0:8787
EXPOSE 8787
USER 65534:65534
ENTRYPOINT ["/agenthub"]
CMD ["serve"]