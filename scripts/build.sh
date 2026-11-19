#!/usr/bin/env bash
# Build (and optionally test) under a hard memory/CPU cap so a build can never
# starve other services on the same machine. Usage: scripts/build.sh [test]
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="${GOROOT_BIN:-/root/sdk/go/bin}:$PATH" CGO_ENABLED=0
VERSION="${VERSION:-0.1.0-dev}"
cap=(); command -v systemd-run >/dev/null && [ "$(id -u)" = 0 ] && cap=(systemd-run --scope -q -p MemoryMax=900M -p MemorySwapMax=0 -p CPUWeight=20)
if [ "${1:-}" = test ]; then exec "${cap[@]}" nice -n 15 go test -p 1 ./... ; fi
"${cap[@]}" nice -n 15 go build -p 1 -trimpath -ldflags "-s -w -X main.version=$VERSION" -o bin/backup-manager ./cmd/backup-manager
ls -la bin/backup-manager
