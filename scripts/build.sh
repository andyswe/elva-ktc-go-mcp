#!/usr/bin/env bash
# Build bin/elva and bin/elva-mcp-stdio for this machine, with the version
# (git describe) built in.
# Run as a normal user; installing is deploy/install.sh.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v go >/dev/null || { echo "build.sh: go not found; install Go 1.25+ (see README)" >&2; exit 1; }
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

for cmd in elva elva-mcp-stdio; do
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "bin/$cmd" "./cmd/$cmd"
    echo "built bin/$cmd $("bin/$cmd" -version)"
done
