#!/bin/sh
# use-browser installer for macOS and Linux.
#   curl -fsSL https://raw.githubusercontent.com/hoangvu12/use-browser/main/install.sh | sh
set -eu

REPO="hoangvu12/use-browser"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) echo "error: unsupported OS: $os" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "error: unsupported architecture: $arch" >&2; exit 1 ;;
esac

asset="use-browser_${os}_${arch}"
url="https://github.com/$REPO/releases/latest/download/$asset"

# prefer ~/.local/bin, fall back to /usr/local/bin
dir="$HOME/.local/bin"
if [ ! -d "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else mkdir -p "$dir"; fi
fi

echo "downloading $url"
tmp=$(mktemp)
if ! curl -fsSL -o "$tmp" "$url"; then
  echo "error: download failed. If no release exists yet, build from source:" >&2
  echo "  git clone https://github.com/$REPO && cd use-browser && go build -o use-browser ." >&2
  rm -f "$tmp"
  exit 1
fi

install -m 755 "$tmp" "$dir/use-browser"
rm -f "$tmp"

echo "installed $dir/use-browser"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "note: add $dir to your PATH" ;;
esac
echo "next: start Chrome with --remote-debugging-port=9222, then run: use-browser doctor"
