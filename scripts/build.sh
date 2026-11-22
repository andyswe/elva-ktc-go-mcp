#!/usr/bin/env bash
# Build bin/elva for this machine, with the version (git describe) built in.
# Run as a normal user; installing is deploy/install.sh.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v go >/dev/null || { echo "build.sh: go not found; install Go 1.25+ (see README)" >&2; exit 1; }
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o bin/elva ./cmd/elva
echo "built bin/elva $(bin/elva -version)"
