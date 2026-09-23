#!/bin/sh
# Install a dboss release for this machine, and optionally set up the host it runs.
#
#   curl -fsSL https://raw.githubusercontent.com/dux/dboss/main/install.sh | sh -s -- --help
#
# --dev scaffolds a host directory you run in the foreground; --server scaffolds /srv/dboss,
# gives it to an existing user and installs the systemd unit, which runs the daemon as that
# user with CAP_NET_BIND_SERVICE instead of root. See usage() below.

set -eu

repo="dux/dboss"
version="${DBOSS_VERSION:-latest}"
install_dir="${DBOSS_INSTALL_DIR:-/usr/local/bin}"
service_user="${DBOSS_USER:-}"
host_dir="${DBOSS_DIR:-}"
mode=plain
if [ -n "${DBOSS_DEV:-}" ]; then mode=dev; fi
if [ -n "${DBOSS_SERVER:-}" ]; then mode=server; fi

script="https://raw.githubusercontent.com/dux/dboss/main/install.sh"

die() {
	printf 'dboss: %s\n' "$1" >&2
	exit 1
}

usage() {
	cat <<-EOF
		Install a dboss release for this machine.

		  binary only
		    curl -fsSL $script | sh

		  development host (macOS or Linux), run in the foreground
		    curl -fsSL $script | sh -s -- --dev

		  production host (Linux), systemd unit running as an existing user
		    curl -fsSL $script | sudo sh -s -- --server --user deploy

		Options:
		  --dev            scaffold a development host in --dir (default: ./dboss)
		  --server         production setup in --dir (default: /srv/dboss); Linux, needs root
		  --user <name>    user that owns the host and runs the service (default: \$SUDO_USER)
		  --dir <path>     host directory

		Environment:
		  DBOSS_VERSION      release tag, e.g. v0.1.0 (default: latest)
		  DBOSS_INSTALL_DIR  binary directory (default: /usr/local/bin)
		  DBOSS_DEV          set to use --dev, DBOSS_SERVER for --server
		  DBOSS_USER         same as --user, DBOSS_DIR same as --dir
	EOF
}

# need takes the value of an option that must not be empty, so `--user` at the end of the
# line fails here instead of swallowing the next option.
need() {
	[ -n "$2" ] || die "$1 needs a value"
	case "$2" in
		--*) die "$1 needs a value" ;;
	esac
}

while [ $# -gt 0 ]; do
	case "$1" in
		--dev) mode=dev ;;
		--server) mode=server ;;
		--user)
			need "$1" "${2:-}"
			service_user="$2"
			shift
			;;
		--dir)
			need "$1" "${2:-}"
			host_dir="$2"
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown option: $1" ;;
	esac
	shift
done

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

# The service mode is checked before anything is downloaded, so a wrong invocation cannot
# leave a half-installed box behind.
if [ "$mode" = server ]; then
	[ "$os" = linux ] || die "--server is Linux only; use --dev on $os"
	[ "$(id -u)" = 0 ] || die "--server needs root: pipe this script into \`sudo sh -s -- --server --user <name>\`"
	command -v systemctl >/dev/null 2>&1 || die "--server installs a systemd unit, but systemctl was not found"
	[ -n "$service_user" ] || service_user="${SUDO_USER:-}"
	[ -n "$service_user" ] || die "--server needs --user <name>: the account that deploys and owns the apps"
	id "$service_user" >/dev/null 2>&1 || die "user $service_user does not exist: useradd -m -s /bin/bash $service_user"
	[ -n "$host_dir" ] || host_dir=/srv/dboss
	# lsof is a runtime dependency: start clears the port range with it, and so does `dboss kill`.
	command -v lsof >/dev/null 2>&1 || printf 'dboss: warning: lsof not found; install it or the daemon cannot clear its port range\n' >&2
fi
if [ "$mode" = dev ] && [ -z "$host_dir" ]; then
	host_dir=./dboss
fi

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
# Create the target first when we may: DBOSS_INSTALL_DIR=$HOME/bin should not need sudo just
# because the directory is not there yet.
mkdir -p "$install_dir" 2>/dev/null || true
if [ -w "$install_dir" ]; then
	mv "$tmp/$asset" "$install_dir/dboss"
else
	sudo mkdir -p "$install_dir"
	sudo mv "$tmp/$asset" "$install_dir/dboss"
fi
bin="$install_dir/dboss"

printf 'installed dboss %s to %s\n' "$version" "$bin"

# scaffold creates the host directory and its config when they are not there yet. The config
# is written through the temp dir so a failed `dboss init` cannot leave a truncated file.
scaffold() {
	mkdir -p "$host_dir/apps"
	if [ ! -f "$host_dir/dboss.yaml" ]; then
		"$bin" init service >"$tmp/dboss.yaml"
		mv "$tmp/dboss.yaml" "$host_dir/dboss.yaml"
		printf 'wrote %s\n' "$host_dir/dboss.yaml"
	fi
}

case "$mode" in
	server)
		scaffold
		# Ownership before the first start, so the runtime dir, the ACME cache and the generated
		# pubsub secrets all belong to the service user from the beginning.
		chown -R "$service_user:" "$host_dir"
		"$bin" check -c "$host_dir/dboss.yaml"
		"$bin" systemd -c "$host_dir/dboss.yaml" --user "$service_user" --bin "$bin" --install
		cat <<-EOF

			dboss runs as $service_user from $host_dir, and binds :80 without root.

			next:
			  1. open the console by adding this to $host_dir/dboss.local.yaml
			     (server-only, gitignored, never touched by a deploy):

			       management:
			         host: dboss.example.com
			         admins: [you@example.com]

			  2. sudo systemctl restart dboss
			  3. dboss login        # prints a one-time console sign-in link

			check: systemctl status dboss
			       ps -o user= -p \$(systemctl show -p MainPID --value dboss)
			       dboss doctor
		EOF
		;;
	dev)
		scaffold
		# `dboss start` inside an app folder also serves HTTPS from a local certificate authority;
		# trusting it now saves the browser warning later. A release without the command, or a
		# refused prompt, only warns: the install itself is done.
		if "$bin" help trust >/dev/null 2>&1; then
			"$bin" trust || printf 'dboss: warning: the local certificate authority is not trusted; run `dboss trust` later\n' >&2
		fi
		cat <<-EOF

			next: cd $host_dir && dboss start

			Apps live in $host_dir/apps, one folder each with its own dboss.yaml.
			A proxy.listen port below 1024 that this session may not bind moves to the first
			free port of ports.range, so no sudo is needed; dboss logs the address it took.
		EOF
		;;
	*)
		printf '\nnext: dboss start   # runs the host in this folder, apps in ./apps\n\n'
		if [ "$os" = darwin ]; then
			printf 'or scaffold a development host:\n  curl -fsSL %s | sh -s -- --dev\n' "$script"
		else
			printf 'or set up this box as a production host:\n  curl -fsSL %s | sudo sh -s -- --server --user <name>\n' "$script"
		fi
		;;
esac
