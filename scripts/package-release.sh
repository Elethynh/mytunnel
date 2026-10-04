#!/bin/sh
set -eu

release_tag=${1-}
if ! printf '%s\n' "$release_tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  printf 'Usage: %s vMAJOR.MINOR.PATCH\n' "$0" >&2
  exit 1
fi

repo_dir=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
cd "$repo_dir"
go_bin=${GO:-go}
output_dir="$repo_dir/dist/$release_tag"
mkdir -p "$output_dir"
build_dir=$(mktemp -d "${TMPDIR:-/tmp}/mytunnel-release.XXXXXX")
trap 'rm -rf "$build_dir"' 0
trap 'exit 1' INT TERM

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  target_os=${target%/*}
  target_arch=${target#*/}
  stage="$build_dir/${target_os}_${target_arch}"
  mkdir -p "$stage"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" "$go_bin" build \
    -trimpath -ldflags "-s -w -X main.version=$release_tag" -o "$stage/mytunnel" .
  cp LICENSE README.md THIRD_PARTY_NOTICES.md "$stage/"
  archive="mytunnel_${release_tag#v}_${target_os}_${target_arch}.tar.gz"
  tar -C "$stage" -czf "$output_dir/$archive" mytunnel LICENSE README.md THIRD_PARTY_NOTICES.md
  printf 'Built %s\n' "$archive"
done

cd "$output_dir"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum ./*.tar.gz > checksums.txt
else
  shasum -a 256 ./*.tar.gz > checksums.txt
fi
printf 'Archives and checksums: %s\n' "$output_dir"
