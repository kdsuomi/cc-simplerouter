#!/usr/bin/env sh
set -eu

repo_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
profile="${1:-${SIMPLEROUTER_CODEX_PROFILE:-dev-small}}"
source_dir="${SIMPLEROUTER_CODEX_SOURCE:-$repo_dir/.build/codex-rust-v0.153.4/codex-rs}"
target_dir="${SIMPLEROUTER_CODEX_TARGET:-$repo_dir/.build/codex-target}"
install_root="${SIMPLEROUTER_CODEX_INSTALL_ROOT:-$HOME/.local/share/simplerouter/simplerouter-codex}"
cargo_home="${CARGO_HOME:-$repo_dir/.build/cargo-home}"
rustup_home="${RUSTUP_HOME:-$repo_dir/.build/rustup-home}"
bridge_source="$repo_dir/codex/macos-node-repl-signed-parent.sh"
bridge_name="node-repl-signed-parent"

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

rustc_bin="$(dirname -- "$cargo_bin")/rustc"
if [ ! -x "$rustc_bin" ]; then
    rustc_bin="$(command -v rustc 2>/dev/null || true)"
fi
target="unknown"
if [ -n "$rustc_bin" ]; then
    target="$(CARGO_HOME="$cargo_home" RUSTUP_HOME="$rustup_home" "$rustc_bin" -vV | sed -n 's/^host: //p')"
fi

if [ -n "${SIMPLEROUTER_CODEX_OFFICIAL_PACKAGE:-}" ]; then
    official_package="$SIMPLEROUTER_CODEX_OFFICIAL_PACKAGE"
else
    pinned_official_package="$HOME/.codex/packages/standalone/releases/0.153.4-$target"
    if [ -d "$pinned_official_package" ]; then
        official_package="$pinned_official_package"
    else
        official_package="$HOME/.codex/packages/standalone/current"
    fi
fi

mkdir -p "$target_dir"
build_args="--package codex-cli --bin codex"
code_mode_host="$official_package/bin/codex-code-mode-host"
if [ ! -x "$code_mode_host" ]; then
    build_args="$build_args --package codex-code-mode-host --bin codex-code-mode-host"
    code_mode_host="$target_dir/$profile/codex-code-mode-host"
fi
if [ "$(uname -s)" = "Linux" ]; then
    build_args="$build_args --package codex-bwrap --bin bwrap"
fi
(
    cd "$source_dir"
    CARGO_HOME="$cargo_home" \
    RUSTUP_HOME="$rustup_home" \
    CARGO_TARGET_DIR="$target_dir" \
        "$cargo_bin" build --locked --profile "$profile" \
        $build_args
)

output="$target_dir/$profile/codex"
if [ ! -x "$output" ] || [ ! -x "$code_mode_host" ]; then
    echo "Missing Codex package build outputs under $target_dir/$profile" >&2
    exit 1
fi

rg_source=""
if [ -x "$official_package/codex-path/rg" ]; then
    rg_source="$official_package/codex-path/rg"
elif command -v rg >/dev/null 2>&1; then
    rg_source="$(command -v rg)"
fi
if [ -z "$rg_source" ]; then
    echo "Could not find ripgrep for the canonical Codex package." >&2
    exit 1
fi

zsh_source=""
if [ -x "$official_package/codex-resources/zsh/bin/zsh" ]; then
    zsh_source="$official_package/codex-resources/zsh/bin/zsh"
elif [ "$(uname -s)" = "Darwin" ] && command -v zsh >/dev/null 2>&1; then
    zsh_source="$(command -v zsh)"
fi

install_bin="$install_root/bin"
install_resources="$install_root/codex-resources"
install_path="$install_root/codex-path"
destination="$install_bin/codex"
mkdir -p "$install_bin" "$install_resources" "$install_path"

cp "$output" "$destination.tmp.$$"
cp "$code_mode_host" "$install_bin/codex-code-mode-host.tmp.$$"
cp "$rg_source" "$install_path/rg.tmp.$$"
chmod 0755 \
    "$destination.tmp.$$" \
    "$install_bin/codex-code-mode-host.tmp.$$" \
    "$install_path/rg.tmp.$$"
mv "$destination.tmp.$$" "$destination"
mv "$install_bin/codex-code-mode-host.tmp.$$" "$install_bin/codex-code-mode-host"
mv "$install_path/rg.tmp.$$" "$install_path/rg"

if [ -n "$zsh_source" ]; then
    mkdir -p "$install_resources/zsh/bin"
    cp "$zsh_source" "$install_resources/zsh/bin/zsh.tmp.$$"
    chmod 0755 "$install_resources/zsh/bin/zsh.tmp.$$"
    mv "$install_resources/zsh/bin/zsh.tmp.$$" "$install_resources/zsh/bin/zsh"
fi

if [ "$(uname -s)" = "Linux" ]; then
    bwrap="$target_dir/$profile/bwrap"
    if [ ! -x "$bwrap" ]; then
        echo "Missing Linux Codex sandbox helper: $bwrap" >&2
        exit 1
    fi
    cp "$bwrap" "$install_resources/bwrap.tmp.$$"
    chmod 0755 "$install_resources/bwrap.tmp.$$"
    mv "$install_resources/bwrap.tmp.$$" "$install_resources/bwrap"
fi

if [ "$(uname -s)" = "Darwin" ]; then
    if [ ! -x "$bridge_source" ]; then
        echo "Missing macOS node_repl signed-parent launcher: $bridge_source" >&2
        exit 1
    fi
    cp "$bridge_source" "$install_bin/$bridge_name.tmp.$$"
    chmod 0755 "$install_bin/$bridge_name.tmp.$$"
    mv "$install_bin/$bridge_name.tmp.$$" "$install_bin/$bridge_name"
fi

ln -sfn codex "$install_bin/codex-simplerouter"
ln -sfn bin/codex "$install_root/codex"

cat >"$install_root/codex-package.json.tmp.$$" <<EOF
{
  "layoutVersion": 1,
  "version": "0.153.4",
  "target": "$target",
  "variant": "codex",
  "entrypoint": "bin/codex",
  "resourcesDir": "codex-resources",
  "pathDir": "codex-path"
}
EOF
mv "$install_root/codex-package.json.tmp.$$" "$install_root/codex-package.json"

if ! cmp -s "$output" "$destination"; then
    echo "Installed companion verification failed: $destination" >&2
    exit 1
fi

echo "Built and installed the canonical $profile Codex companion package to $install_root"
