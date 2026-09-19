#!/bin/sh
# Install a dboss release for this machine.
#
#   curl -fsSL https://raw.githubusercontent.com/dux/dboss/main/install.sh | sh
#
# Overrides:
#   DBOSS_VERSION      release tag to install, e.g. v0.1.0 (default: latest)
#   DBOSS_INSTALL_DIR  target directory (default: /usr/local/bin)

set -eu

repo="dux/dboss"
version="${DBOSS_VERSION:-latest}"
dir="${DBOSS_INSTALL_DIR:-/usr/local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
	linux | darwin) ;;
	*)
		printf 'dboss: unsupported OS: %s\n' "$os" >&2
		exit 1
		;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*)
		printf 'dboss: unsupported architecture: %s\n' "$arch" >&2
		exit 1
		;;
esac

if [ "$version" = latest ]; then
	version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
fi
if [ -z "$version" ]; then
	printf 'dboss: could not resolve a release for %s\n' "$repo" >&2
	exit 1
fi

asset="dboss_${os}_${arch}"
base="https://github.com/$repo/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

expected=$(awk -v asset="$asset" '$2 == asset {print $1}' "$tmp/checksums.txt")
if [ -z "$expected" ]; then
	printf 'dboss: no checksum for %s in %s\n' "$asset" "$version" >&2
	exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else
	actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
fi

if [ "$expected" != "$actual" ]; then
	printf 'dboss: checksum mismatch for %s\n  expected %s\n  actual   %s\n' "$asset" "$expected" "$actual" >&2
	exit 1
fi

chmod +x "$tmp/$asset"
if [ -w "$dir" ]; then
	mv "$tmp/$asset" "$dir/dboss"
else
	sudo mkdir -p "$dir"
	sudo mv "$tmp/$asset" "$dir/dboss"
fi

printf 'installed dboss %s to %s/dboss\n' "$version" "$dir"
printf 'next: dboss start   # runs the host in this folder, apps in ./apps\n'
