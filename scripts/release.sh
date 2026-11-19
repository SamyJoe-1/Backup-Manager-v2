#!/usr/bin/env bash
# Build the release files for every supported CPU into dist/:
#   backup-manager-linux-amd64, backup-manager-linux-arm64, SHA256SUMS, VERSION
# Then publish them as a GitHub release (see docs/DEVELOPMENT.md):
#   scripts/release.sh && scripts/publish-release.sh      (needs GITHUB_TOKEN with write access)
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="${GOROOT_BIN:-/root/sdk/go/bin}:$PATH" CGO_ENABLED=0
VERSION="$(cat VERSION)"
rm -rf dist && mkdir dist
cap=(); command -v systemd-run >/dev/null && [ "$(id -u)" = 0 ] && cap=(systemd-run --scope -q -p MemoryMax=900M -p MemorySwapMax=0 -p CPUWeight=20)
for arch in amd64 arm64; do
  echo "building linux/$arch"
  GOOS=linux GOARCH=$arch "${cap[@]}" nice -n 15 go build -p 1 -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "dist/backup-manager-linux-$arch" ./cmd/backup-manager
done
echo "$VERSION" > dist/VERSION
(cd dist && sha256sum backup-manager-linux-amd64 backup-manager-linux-arm64 > SHA256SUMS)
ls -la dist && cat dist/SHA256SUMS
