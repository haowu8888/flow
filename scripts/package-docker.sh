#!/usr/bin/env bash
# 打包 Docker 部署目录：构建前端、交叉编译 flow-server / flow-runner（静态链接），连同 deploy/docker 下的文件输出到 OUT（默认 build/flow-docker）。
# 用法：scripts/package-docker.sh [OUT]，然后把 OUT 目录（或生成的 .tar.gz）放到服务器上执行 docker compose up -d --build
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=${1:-build/flow-docker}
VERSION=${VERSION:-$(date +%Y%m%d%H%M)}
LD="-s -w -X flow/internal/version.Version=$VERSION"
rm -rf "$OUT" && mkdir -p "$OUT/bin" "$OUT/dist"
if [ "${SKIP_WEB:-}" != 1 ]; then (cd web && npm run build); fi
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$LD" -o "$OUT/bin/flow-server" ./cmd/server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$LD" -o "$OUT/bin/flow-runner" ./cmd/runner
for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  CGO_ENABLED=0 GOOS=${p%/*} GOARCH=${p#*/} go build -trimpath -ldflags="$LD" -o "$OUT/dist/flow-runner-${p%/*}-${p#*/}" ./cmd/runner
done
cp deploy/docker/Dockerfile deploy/docker/docker-compose.yml deploy/docker/.env.example "$OUT/"
tar -czf "$OUT.tar.gz" -C "$(dirname "$OUT")" "$(basename "$OUT")"
echo "$OUT.tar.gz ($VERSION)"
