#!/usr/bin/env bash
# Fetch the Broadcom/Cypress Wi-Fi firmware the Raspberry Pi 4's CYW43455
# needs, into $1 (default ./dist/firmware/brcm).
#
# Why this exists: brcmfmac is a driver, not firmware. gokrazy's kernel.rpi
# package ships brcmfmac.ko and gokrazy/firmware ships only the VideoCore
# bootloader blobs (start4.elf and friends) — neither carries the Wi-Fi
# firmware, and the kernel does not embed it either. Without these files the
# module loads and then fails, so wlan0 never appears.
#
# The files are redistributable but not open source, so they are downloaded
# at build time rather than vendored into this repository.

set -euo pipefail

DEST="${1:-$PWD/dist/firmware/brcm}"

# Pinned to a tag rather than a branch so an image build is reproducible.
FIRMWARE_REF="${FIRMWARE_REF:-bookworm}"
BASE="https://raw.githubusercontent.com/RPi-Distro/firmware-nonfree/${FIRMWARE_REF}/debian/config/brcm80211"

mkdir -p "$DEST"

fetch() {
  local url="$1" out="$2" min_size="$3"
  if [ -s "$out" ]; then
    echo "  cached: $(basename "$out")"
    return
  fi
  echo "  fetching: $(basename "$out")"
  curl -fsSL --retry 3 --retry-delay 2 -o "$out.tmp" "$url"
  # A repository symlink checks out as a tiny text file containing its
  # target, and a 404 page is small too. Either would be silently installed
  # as "firmware" and fail at runtime, so check the size we actually got.
  local size
  size=$(stat -c %s "$out.tmp")
  if [ "$size" -lt "$min_size" ]; then
    echo "Error: $url returned $size bytes, expected at least $min_size" >&2
    rm -f "$out.tmp"
    exit 1
  fi
  mv "$out.tmp" "$out"
}

echo "Fetching Raspberry Pi 4 Wi-Fi firmware into $DEST"
fetch "$BASE/cypress/cyfmac43455-sdio-standard.bin" "$DEST/brcmfmac43455-sdio.bin" 500000
fetch "$BASE/cypress/cyfmac43455-sdio.clm_blob" "$DEST/brcmfmac43455-sdio.clm_blob" 2000
fetch "$BASE/brcm/brcmfmac43455-sdio.txt" "$DEST/brcmfmac43455-sdio.txt" 1000

echo "Wi-Fi firmware ready:"
ls -la "$DEST"
