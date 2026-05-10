#!/usr/bin/env bash

set -euo pipefail

GOKRAZY_INSTANCE="${GOKRAZY_INSTANCE:-gokrazy-runner}"
GOKRAZY_PARENT_DIR="${GOKRAZY_PARENT_DIR:-$HOME/.gokrazy/$GOKRAZY_INSTANCE}"
IMAGE_DIR="${IMAGE_DIR:-$PWD/ota}"
IMAGE_NAME="${IMAGE_NAME:-gokrazy-runner-rpi4b-root.squashfs}"
GOKRAZY_IMAGE_MODE="${GOKRAZY_IMAGE_MODE:-ota}"
TARGET_STORAGE_BYTES="${TARGET_STORAGE_BYTES:-}"
IMAGE_PATH="${IMAGE_DIR}/${IMAGE_NAME}"
MKE2FS_BINARY="${MKE2FS_BINARY:-}"
BUILD_DATE="${BUILD_DATE:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
INSTANCE_DIR="$GOKRAZY_PARENT_DIR/$GOKRAZY_INSTANCE"

# shellcheck source=gok-common.sh
source "$REPO_DIR/scripts/gok-common.sh"

export GOKRAZY_PARENT_DIR

if [ -n "${VERSION:-}" ]; then
  BUILD_VERSION="$VERSION"
elif [ -n "${TAG_NAME:-}" ]; then
  BUILD_VERSION="$TAG_NAME"
elif [ "${GITHUB_REF:-}" = "refs/heads/master" ]; then
  BUILD_VERSION="$(date -u +'%Y.%-m.%-d.%H%M')"
elif [[ "${GITHUB_REF:-}" == refs/tags/* ]] && [ -n "${GITHUB_REF_NAME:-}" ]; then
  BUILD_VERSION="$GITHUB_REF_NAME"
else
  BUILD_VERSION="$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)"
fi

VERSION_LDFLAGS="-s -w -X main.Version=${BUILD_VERSION} -X main.BuildDate=${BUILD_DATE}"

if [ -z "$MKE2FS_BINARY" ]; then
  MKE2FS_BINARY="$(command -v mke2fs || true)"
fi
if [ -z "$MKE2FS_BINARY" ] || [ ! -x "$MKE2FS_BINARY" ]; then
  echo "Error: MKE2FS_BINARY must point to an executable mke2fs binary for the target architecture"
  exit 1
fi

mkdir -p "$GOKRAZY_PARENT_DIR" "$IMAGE_DIR"

if [ -d "$INSTANCE_DIR" ]; then
  rm -rf "$INSTANCE_DIR"
fi

gok -i "$GOKRAZY_INSTANCE" new

cat > "$INSTANCE_DIR/go.mod" <<EOF
module gokrazy-instance

go 1.26

replace github.com/denysvitali/gokrazy-runner => $REPO_DIR
EOF

# shellcheck disable=SC2154 # gok_packages comes from gok-common.sh
for pkg in "${gok_packages[@]}"; do
  gok -i "$GOKRAZY_INSTANCE" add "$pkg"
done

cat > "$INSTANCE_DIR/config.json" <<EOF
{
  "Hostname": "$GOKRAZY_INSTANCE",
  "Update": {
    "HTTPPort": "80",
    "HTTPSPort": "443",
    "HTTPPassword": "gokrazy-runner",
    "UseTLS": "self-signed",
    "TLSCertificateStorage": "perm-self-signed"
  },
  "Packages": [
$(emit_packages_json '    ')
  ],
  "PackageConfig": {
    "github.com/gokrazy/podman": {
      "Environment": [
        "CNI_CONFIG_DIR=/etc/cni/net.d"
      ]
    },
    "github.com/greenpau/cni-plugins/cmd/cni-nftables-portmap": {},
    "github.com/greenpau/cni-plugins/cmd/cni-nftables-firewall": {},
    "github.com/gokrazy/breakglass": {
      "CommandLineFlags": [
        "-authorized_keys=/perm/breakglass/authorized_keys"
      ]
    },
    "github.com/denysvitali/gokrazy-runner/cmd/perm-init": {
      "GoBuildFlags": [
        "-trimpath",
        "-ldflags=${VERSION_LDFLAGS}"
      ],
      "ExtraFilePaths": {
        "/usr/local/bin/mke2fs": "$MKE2FS_BINARY"
      }
    },
    "github.com/denysvitali/gokrazy-runner/cmd/runner-init": {
      "GoBuildFlags": [
        "-trimpath",
        "-ldflags=${VERSION_LDFLAGS}"
      ]
    },
    "github.com/denysvitali/gokrazy-runner/cmd/runner-webui": {
      "GoBuildFlags": [
        "-trimpath",
        "-ldflags=${VERSION_LDFLAGS}"
      ]
    },
    "github.com/denysvitali/gokrazy-runner/cmd/tailscale-init": {
      "GoBuildFlags": [
        "-trimpath",
        "-ldflags=${VERSION_LDFLAGS}"
      ],
      "Environment": [
        "TS_AUTH_KEY_PATH=/perm/tailscale.authkey",
        "TS_HOSTNAME=${GOKRAZY_INSTANCE}",
        "TS_TAILSCALE_UP_ARGS=--ssh"
      ]
    },
    "github.com/denysvitali/gokrazy-runner/cmd/usbdev-init": {
      "GoBuildFlags": [
        "-trimpath",
        "-ldflags=${VERSION_LDFLAGS}"
      ]
    },
    "tailscale.com/cmd/tailscaled": {
      "CommandLineFlags": [
        "-statedir=/perm/tailscale"
      ]
    },
    "tailscale.com/cmd/tailscale": {}
  }
}
EOF

case "$GOKRAZY_IMAGE_MODE" in
  ota)
    gok -i "$GOKRAZY_INSTANCE" overwrite --root "$IMAGE_PATH"
    ;;
  full)
    if [ -z "$TARGET_STORAGE_BYTES" ]; then
      echo "Error: TARGET_STORAGE_BYTES is required when GOKRAZY_IMAGE_MODE=full"
      exit 1
    fi
    gok -i "$GOKRAZY_INSTANCE" overwrite --full="$IMAGE_PATH" --target_storage_bytes="$TARGET_STORAGE_BYTES"
    if [ ! -s "$IMAGE_PATH" ]; then
      echo "Error: expected full image at $IMAGE_PATH, but the file was not created"
      exit 1
    fi
    ;;
  *)
    echo "Error: invalid GOKRAZY_IMAGE_MODE '$GOKRAZY_IMAGE_MODE' (expected 'ota' or 'full')"
    exit 1
    ;;
esac

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "image_path=$IMAGE_PATH" >> "$GITHUB_OUTPUT"
fi

echo "Built image: $IMAGE_PATH"
