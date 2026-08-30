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

install_tmp="$install_dir/.simplerouter.install.$$"
trap 'rm -f "$install_tmp"' 0 HUP INT TERM

cp "$bin_dir/simplerouter" "$install_tmp"
chmod +x "$install_tmp"

# Re-sign the copied binary on macOS, then replace the installed executable by
# rename. Overwriting an existing Mach-O in place can leave the kernel with
# stale code-signing state and cause an immediate SIGKILL on the next launch.
if [ "$(uname -s)" = Darwin ] && command -v codesign >/dev/null 2>&1; then
    codesign --force --sign - --identifier simplerouter "$install_tmp"
fi

mv -f "$install_tmp" "$install_dir/simplerouter"
trap - 0 HUP INT TERM

case ":$PATH:" in
    *":$install_dir:"*) ;;
    *)
        echo "Installed to $install_dir, which is not currently on PATH."
        echo "Add this to your shell profile:"
        echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
        ;;
esac

echo "Built and installed simplerouter to $install_dir/simplerouter"
