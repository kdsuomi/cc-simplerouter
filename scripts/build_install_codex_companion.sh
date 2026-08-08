#!/usr/bin/env sh
set -eu

repo_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
profile="${1:-${SIMPLEROUTER_CODEX_PROFILE:-dev-small}}"
source_dir="${SIMPLEROUTER_CODEX_SOURCE:-$repo_dir/.build/codex-rust-v0.145.0/codex-rs}"
target_dir="${SIMPLEROUTER_CODEX_TARGET:-$repo_dir/.build/codex-target}"
install_root="${SIMPLEROUTER_CODEX_INSTALL_ROOT:-$HOME/.local/share/simplerouter/simplerouter-codex}"
cargo_home="${CARGO_HOME:-$repo_dir/.build/cargo-home}"
rustup_home="${RUSTUP_HOME:-$repo_dir/.build/rustup-home}"

if [ "$#" -gt 1 ]; then
    echo "Usage: $0 [dev-small|release]" >&2
    exit 2
fi

case "$profile" in
    dev-small|release) ;;
    *)
        echo "Unsupported profile '$profile'; use dev-small or release." >&2
        exit 2
        ;;
esac

if [ ! -f "$source_dir/Cargo.toml" ]; then
    echo "Patched Codex source not found at $source_dir" >&2
    echo "Prepare and verify the pinned patchset before building." >&2
    exit 1
fi

if [ -x "$cargo_home/bin/cargo" ]; then
    cargo_bin="$cargo_home/bin/cargo"
elif command -v cargo >/dev/null 2>&1; then
    cargo_bin="$(command -v cargo)"
else
    echo "Could not find cargo. Install the pinned Rust toolchain first." >&2
    exit 1
fi

mkdir -p "$target_dir"
(
    cd "$source_dir"
    CARGO_HOME="$cargo_home" \
    RUSTUP_HOME="$rustup_home" \
    CARGO_TARGET_DIR="$target_dir" \
        "$cargo_bin" build --locked --profile "$profile" \
        --package codex-cli --bin codex
)

output="$target_dir/$profile/codex"
if [ ! -x "$output" ]; then
    echo "Missing build output: $output" >&2
    exit 1
fi

install_dir="$install_root/bin"
destination="$install_dir/codex-simplerouter"
temporary="$install_dir/.codex-simplerouter.$$"
mkdir -p "$install_dir"
trap 'rm -f "$temporary"' EXIT
cp "$output" "$temporary"
chmod 0755 "$temporary"
mv "$temporary" "$destination"
trap - EXIT

if ! cmp -s "$output" "$destination"; then
    echo "Installed companion verification failed: $destination" >&2
    exit 1
fi

echo "Built and installed the $profile Codex companion to $destination"
