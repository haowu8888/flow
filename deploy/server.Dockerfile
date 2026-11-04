# flow-server 镜像（含前端）。构建：docker build -f deploy/server.Dockerfile -t registry.example.com/flow-server:latest .
FROM node:20-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --include=dev
COPY web/ ./
COPY internal/webui/ /src/internal/webui/
RUN npm run build

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X flow/internal/version.Version=${VERSION}" -o /flow-server ./cmd/server
# 安装脚本（/api/runner/install.sh）与 GitHub 节点从 Server 下载 flow-runner。
RUN for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
      CGO_ENABLED=0 GOOS=${p%/*} GOARCH=${p#*/} go build -trimpath -ldflags="-s -w -X flow/internal/version.Version=${VERSION}" \
        -o /dist/flow-runner-${p%/*}-${p#*/} ./cmd/runner; \
    done

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /flow-server /flow-server
COPY --from=build /dist /dist
VOLUME /data
ENV FLOW_DATA_DIR=/data FLOW_RUNNER_DIST=/dist
EXPOSE 8080
ENTRYPOINT ["/flow-server"]
