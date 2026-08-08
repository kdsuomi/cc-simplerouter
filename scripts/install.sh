#!/usr/bin/env sh
set -eu

repo="${SIMPLEROUTER_REPO:-kdsuomi/cc-simplerouter}"
version="${SIMPLEROUTER_VERSION:-latest}"
install_dir="${SIMPLEROUTER_INSTALL_DIR:-$HOME/.local/bin}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"

case "$os" in
    darwin)
        os="darwin"
        ;;
    linux)
        os="linux"
        ;;
    *)
        echo "Unsupported operating system: $os" >&2
        exit 1
        ;;
esac

case "$arch" in
    arm64|aarch64)
        arch="arm64"
        ;;
    x86_64|amd64)
        if [ "$os" = "darwin" ]; then
            echo "Intel macOS is not supported. simplerouter provides macOS arm64 binaries only." >&2
            exit 1
        fi
        arch="amd64"
        ;;
    *)
        echo "Unsupported CPU architecture: $arch" >&2
        exit 1
        ;;
esac

asset="simplerouter_${os}_${arch}"
if [ "$version" = "latest" ]; then
    url="https://github.com/$repo/releases/latest/download/$asset"
else
    url="https://github.com/$repo/releases/download/$version/$asset"
fi

mkdir -p "$install_dir"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

echo "Downloading $asset from $repo ($version)"
if command -v curl >/dev/null 2>&1; then
    curl -fL "$url" -o "$tmp"
elif command -v wget >/dev/null 2>&1; then
    wget -O "$tmp" "$url"
else
    echo "Install requires curl or wget." >&2
    exit 1
fi

chmod +x "$tmp"
mv "$tmp" "$install_dir/simplerouter"
trap - EXIT

case ":$PATH:" in
    *":$install_dir:"*) ;;
    *)
        echo "Installed to $install_dir, which is not currently on PATH."
        echo "Add this to your shell profile:"
        echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
        ;;
esac

echo "Installed simplerouter to $install_dir/simplerouter"
