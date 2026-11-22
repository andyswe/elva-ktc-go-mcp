#!/usr/bin/env bash
# Open the MCP Inspector on bin/elva-mcp-stdio, connected to the local test database
# (the elva-test-pg container from AGENTS.md, role elva_ro). For development only.
#
#   scripts/mcp-inspect.sh                  # web UI: open the URL it prints
#   scripts/mcp-inspect.sh --cli --method tools/list
#   scripts/mcp-inspect.sh --cli --method tools/call --tool-name get_baseload --tool-arg from=-14d
#
# Environment:
#   ELVA_DATABASE_URL   database to use (default: the test container)
#   ELVA_TEST_SCHEMA    schema to read, e.g. perf for the 6-year synthetic data (default: public)
set -euo pipefail
cd "$(dirname "$0")/.."

# The Inspector needs Node.js 22 or newer; prefer a user install in ~/.local/node.
[[ -x $HOME/.local/node/bin/node ]] && PATH=$HOME/.local/node/bin:$PATH
command -v npx >/dev/null || { echo "mcp-inspect.sh: npx not found; install Node.js 22+" >&2; exit 1; }
major=$(node -p 'process.versions.node.split(".")[0]')
(( major >= 22 )) || { echo "mcp-inspect.sh: Node.js $(node --version) is too old, the Inspector needs 22+" >&2; exit 1; }

[[ -x bin/elva-mcp-stdio ]] || { echo "mcp-inspect.sh: bin/elva-mcp-stdio missing; run scripts/build.sh" >&2; exit 1; }

url=${ELVA_DATABASE_URL:-postgres://elva_ro:ro@127.0.0.1:55432/elva}
if [[ -n ${ELVA_TEST_SCHEMA:-} ]]; then
    [[ $url == *\?* ]] && url+="&search_path=$ELVA_TEST_SCHEMA" || url+="?search_path=$ELVA_TEST_SCHEMA"
fi

# Fail early with a clear message instead of an Inspector connection error.
if ! ELVA_DATABASE_URL=$url bin/elva-mcp-stdio -check >/dev/null 2>&1; then
    echo "mcp-inspect.sh: cannot reach the database ($url)." >&2
    echo "  Is the test container running? docker ps; see AGENTS.md, Database tests" >&2
    exit 1
fi

exec npx @modelcontextprotocol/inspector -e ELVA_DATABASE_URL="$url" "$PWD/bin/elva-mcp-stdio" "$@"
