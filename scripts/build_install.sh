#!/usr/bin/env sh
set -eu

repo_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
install_dir="${SIMPLEROUTER_INSTALL_DIR:-$HOME/.local/bin}"
bin_dir="$repo_dir/bin"

if ! command -v go >/dev/null 2>&1; then
    echo "Could not find 'go'. Install Go from https://go.dev/dl/ and rerun this script." >&2
    exit 1
fi

mkdir -p "$bin_dir" "$install_dir"

(
    cd "$repo_dir"
    go build -buildvcs=false -o "$bin_dir/simplerouter" ./cmd/simplerouter
)

cp "$bin_dir/simplerouter" "$install_dir/simplerouter"
chmod +x "$install_dir/simplerouter"

case ":$PATH:" in
    *":$install_dir:"*) ;;
    *)
        echo "Installed to $install_dir, which is not currently on PATH."
        echo "Add this to your shell profile:"
        echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
        ;;
esac

echo "Built and installed simplerouter to $install_dir/simplerouter"
