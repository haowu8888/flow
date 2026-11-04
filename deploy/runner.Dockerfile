# flow-runner 镜像：既用作 Kubernetes 执行器的控制进程，也用作 Job Pod 的 helper（init 容器）镜像。
# 构建：docker build -f deploy/runner.Dockerfile -t registry.example.com/flow-runner:latest .
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 静态链接：init 容器会把它复制进任意 Job 镜像中执行（包括基于 musl / glibc 的镜像）。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /flow-runner ./cmd/runner

FROM alpine:3.20
RUN apk add --no-cache git ca-certificates
COPY --from=build /flow-runner /flow-runner
ENTRYPOINT ["/flow-runner"]
