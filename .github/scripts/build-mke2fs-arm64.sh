#!/usr/bin/env bash

set -euo pipefail

E2FSPROGS_VERSION="${E2FSPROGS_VERSION:-1.47.2}"
E2FSPROGS_SHA256="${E2FSPROGS_SHA256:-7a959221c1b1cc6e28b7d7a4e204a2ffd8ec6d8a2de4461c482b64c5f4463cca}"
OUTPUT_DIR="${OUTPUT_DIR:-$PWD/dist/e2fsprogs-arm64}"
BUILD_DIR="${BUILD_DIR:-$PWD/.e2fsprogs-build}"

sudo apt-get update
sudo apt-get install -y --no-install-recommends \
  ca-certificates \
  curl \
  gcc \
  make \
  pkg-config

rm -rf "$BUILD_DIR" "$OUTPUT_DIR"
mkdir -p "$BUILD_DIR" "$OUTPUT_DIR"

archive="$BUILD_DIR/e2fsprogs-${E2FSPROGS_VERSION}.tar.gz"
curl -fsSL "https://mirrors.edge.kernel.org/pub/linux/kernel/people/tytso/e2fsprogs/v${E2FSPROGS_VERSION}/e2fsprogs-${E2FSPROGS_VERSION}.tar.gz" -o "$archive"
printf '%s  %s\n' "$E2FSPROGS_SHA256" "$archive" | sha256sum -c -

tar -C "$BUILD_DIR" -xzf "$archive"
cd "$BUILD_DIR/e2fsprogs-${E2FSPROGS_VERSION}"

# Build mke2fs statically. e2fsprogs ships bundled libuuid/libblkid/libcom_err
# and links them in by default; --disable-shared keeps everything as .a.
./configure \
  --prefix=/usr \
  --disable-shared \
  --enable-static \
  --disable-nls \
  --disable-fuse2fs
make -j"$(nproc)" LDFLAGS=-static

install -m 0755 misc/mke2fs "$OUTPUT_DIR/mke2fs"
file "$OUTPUT_DIR/mke2fs"

if ! file "$OUTPUT_DIR/mke2fs" | grep -q 'ARM aarch64'; then
  echo "Error: built mke2fs is not an arm64 binary"
  exit 1
fi

if ! file "$OUTPUT_DIR/mke2fs" | grep -q 'statically linked'; then
  echo "Error: built mke2fs is not static; dynamic libraries would be missing on gokrazy"
  exit 1
fi

echo "MKE2FS_BINARY=$OUTPUT_DIR/mke2fs" >> "${GITHUB_ENV:-/dev/null}"
echo "mke2fs_binary=$OUTPUT_DIR/mke2fs" >> "${GITHUB_OUTPUT:-/dev/null}"
