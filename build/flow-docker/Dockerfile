# Flow 运行镜像（Server 与 Runner 共用），使用 scripts/package-docker.sh 预编译的二进制，服务器上无需 Go / Node 构建环境。
# 由 docker-compose.yml 构建：docker compose build
FROM alpine:3.20
# git：拉取代码；docker-cli：Runner 执行指定了 image 的任务（挂载宿主 docker.sock）；openssh-client：部署脚本里可用 ssh/scp
RUN apk add --no-cache ca-certificates tzdata git docker-cli openssh-client curl bash
COPY bin/flow-server bin/flow-runner /usr/local/bin/
# 安装脚本（/api/runner/install.sh）与 GitHub 节点从这里下载 flow-runner
COPY dist /dist
ENV FLOW_DATA_DIR=/data FLOW_RUNNER_DIST=/dist TZ=Asia/Shanghai
VOLUME /data
EXPOSE 8080
CMD ["flow-server"]
