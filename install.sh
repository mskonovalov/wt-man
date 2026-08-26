#!/bin/sh

set -eu

repository=https://github.com/mskonovalov/wt-man
install_dir=${WT_MAN_INSTALL_DIR:-"$HOME/.local/bin"}

for command_name in curl tar install awk; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		printf 'wt-man installer: %s is required\n' "$command_name" >&2
		exit 1
	fi
done

case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*)
		printf 'wt-man installer: unsupported operating system: %s\n' "$(uname -s)" >&2
		exit 1
		;;
esac

case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*)
		printf 'wt-man installer: unsupported architecture: %s\n' "$(uname -m)" >&2
		exit 1
		;;
esac

latest_url=$(curl --proto '=https' --tlsv1.2 -fsSL -o /dev/null -w '%{url_effective}' "$repository/releases/latest")
tag=${latest_url##*/}
case "$tag" in
	v[0-9]*) ;;
	*)
		printf 'wt-man installer: could not determine the latest release\n' >&2
		exit 1
		;;
esac

version=${tag#v}
archive=wt-man_${version}_${os}_${arch}.tar.gz
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/wt-man.XXXXXX")
trap 'rm -rf "$temporary_dir"' 0 1 2 15

curl --proto '=https' --tlsv1.2 -fsSL -o "$temporary_dir/$archive" "$repository/releases/download/$tag/$archive"
curl --proto '=https' --tlsv1.2 -fsSL -o "$temporary_dir/checksums.txt" "$repository/releases/download/$tag/checksums.txt"

expected_checksum=$(awk -v name="$archive" '$2 == name { print $1 }' "$temporary_dir/checksums.txt")
if [ -z "$expected_checksum" ]; then
	printf 'wt-man installer: %s is missing from checksums.txt\n' "$archive" >&2
	exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
	actual_checksum=$(sha256sum "$temporary_dir/$archive" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	actual_checksum=$(shasum -a 256 "$temporary_dir/$archive" | awk '{ print $1 }')
else
	printf 'wt-man installer: sha256sum or shasum is required\n' >&2
	exit 1
fi

if [ "$actual_checksum" != "$expected_checksum" ]; then
	printf 'wt-man installer: checksum verification failed for %s\n' "$archive" >&2
	exit 1
fi

tar -xzf "$temporary_dir/$archive" -C "$temporary_dir"
mkdir -p "$install_dir"
install -m 0755 "$temporary_dir/wt-man" "$install_dir/wt-man"

printf 'Installed wt-man %s to %s/wt-man\n' "$version" "$install_dir"
case ":${PATH:-}:" in
	*":$install_dir:"*) ;;
	*) printf 'Add %s to PATH to run wt-man directly.\n' "$install_dir" ;;
esac
