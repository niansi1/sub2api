#!/usr/bin/env bash
# 本地构建镜像的快速脚本，避免在命令行反复输入构建参数。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

build_args=(
    --build-arg GOPROXY=https://goproxy.cn,direct
    --build-arg GOSUMDB=sum.golang.google.cn
)

# Forward the vite heap cap only when the caller actually set one. Passing an
# empty --build-arg would override the Dockerfile default with "" and make the
# build die on "JavaScript heap out of memory".
if [[ -n "${NODE_OPTIONS:-}" ]]; then
    build_args+=(--build-arg "NODE_OPTIONS=${NODE_OPTIONS}")
fi

docker build -t sub2api:latest \
    "${build_args[@]}" \
    -f "${REPO_ROOT}/Dockerfile" \
    "${REPO_ROOT}"
