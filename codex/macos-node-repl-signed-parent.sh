#!/bin/sh
set -eu

# Chrome's native host accepts browser protocol clients only when node_repl has
# an OpenAI-signed Codex parent. The unrestricted outer profile applies no
# sandbox; node_repl still launches its JavaScript kernel in the normal
# per-call Codex sandbox.
signed_codex="${CODEX_CLI_PATH:-/Applications/ChatGPT.app/Contents/Resources/codex}"
resources_dir="$(CDPATH= cd -- "$(dirname -- "$signed_codex")" && pwd)"
node_repl="$resources_dir/cua_node/bin/node_repl"

if [ ! -x "$signed_codex" ]; then
    echo "OpenAI-signed Codex CLI not found: $signed_codex" >&2
    exit 1
fi
if [ ! -x "$node_repl" ]; then
    echo "ChatGPT node_repl host not found: $node_repl" >&2
    exit 1
fi

exec "$signed_codex" sandbox -P :danger-full-access -- "$node_repl"
