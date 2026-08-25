#!/usr/bin/env bash
# build-release.sh — 交叉编译全部架构的 sbx-core 并生成 SHA256SUMS
# 用法: ./scripts/build-release.sh [输出目录，默认 dist]
set -euo pipefail
cd "$(dirname "$0")/.." || exit 1

OUT="${1:-dist}"
VERSION="$(go run ./cmd/sbx-core version 2>/dev/null | sed 's/^sbx-core v//')"
[[ -n "$VERSION" ]] || VERSION="dev"
LDFLAGS="-s -w -X github.com/k6nfmm7dbr-commits/sbx/internal/version.Version=${VERSION}"

rm -rf "$OUT"; mkdir -p "$OUT"

build_one() { # build_one <name> <goarch> [goarm]
  local name="$1" goarch="$2" goarm="${3:-}"
  echo "==> building linux/$goarch${goarm:+ (GOARM=$goarm)} -> sbx-core-linux-$name"
  if [[ -n "$goarm" ]]; then
    env GOOS=linux GOARCH="$goarch" GOARM="$goarm" CGO_ENABLED=0 \
      go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/sbx-core-linux-$name" ./cmd/sbx-core
  else
    env GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 \
      go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/sbx-core-linux-$name" ./cmd/sbx-core
  fi
}

build_one amd64 amd64
build_one arm64 arm64
build_one armv7 arm 7
build_one armv6 arm 6
build_one 386 386
build_one s390x s390x
build_one riscv64 riscv64

cd "$OUT" || exit 1
sha256sum sbx-core-linux-* > SHA256SUMS
echo "---- 产物 ----"
ls -la
echo "---- SHA256SUMS ----"
cat SHA256SUMS
