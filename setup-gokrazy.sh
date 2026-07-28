#!/usr/bin/env bash
# Interactive provisioning helper for a local SD-card flash of gokrazy-runner.
#
# Creates a gokrazy instance under $HOME/gokrazy/<instance>/ with a config.json
# that wires up perm-init + runner-init. After running this you typically:
#
#   gok -i <instance> overwrite --full /dev/sdX     (initial flash)
#   gok -i <instance> overwrite --update yes        (subsequent OTA updates)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

# shellcheck source=scripts/gok-common.sh
source "$SCRIPT_DIR/scripts/gok-common.sh"

echo "=== gokrazy-runner setup ==="

if ! command -v gok >/dev/null 2>&1; then
  echo "Error: gok CLI not found in PATH"
  echo "Install with:"
  echo "    go install github.com/gokrazy/tools/cmd/gok@latest"
  exit 1
fi

read -r -p "Instance name [gokrazy-runner]: " INSTANCE_NAME
INSTANCE_NAME="${INSTANCE_NAME:-gokrazy-runner}"

read -r -p "Wi-Fi regulatory country (ISO 3166-1 alpha-2) [CH]: " WIFI_COUNTRY
WIFI_COUNTRY="$(echo "${WIFI_COUNTRY:-CH}" | tr '[:lower:]' '[:upper:]')"

MKE2FS_BINARY="${MKE2FS_BINARY:-$(command -v mke2fs || true)}"
if [ -z "$MKE2FS_BINARY" ] || [ ! -x "$MKE2FS_BINARY" ]; then
  echo "Error: MKE2FS_BINARY must point to an executable mke2fs for the target architecture"
  echo "(set MKE2FS_BINARY=/path/to/mke2fs)"
  exit 1
fi

INSTANCE_DIR="$HOME/gokrazy/$INSTANCE_NAME"
ABSOLUTE_PROJECT_PATH="$(cd "$SCRIPT_DIR" && pwd -P)"

if [ ! -d "$INSTANCE_DIR" ]; then
  echo "Creating new gokrazy instance at $INSTANCE_DIR"
  gok -i "$INSTANCE_NAME" new
fi

cat > "$INSTANCE_DIR/go.mod" <<EOF
module gokrazy-instance

go 1.26

replace github.com/denysvitali/gokrazy-runner => $ABSOLUTE_PROJECT_PATH
EOF

echo "Adding packages..."
# shellcheck disable=SC2154 # gok_packages comes from gok-common.sh
for pkg in "${gok_packages[@]}"; do
  gok -i "$INSTANCE_NAME" add "$pkg"
done

CONFIG_FILE="$INSTANCE_DIR/config.json"
cat > "$CONFIG_FILE" <<EOF
{
  "Hostname": "$INSTANCE_NAME",
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
    "github.com/gokrazy/wifi": {
      "DontStart": true
    },
    "github.com/gokrazy/breakglass": {
      "CommandLineFlags": [
        "-authorized_keys=/perm/breakglass/authorized_keys"
      ]
    },
    "github.com/denysvitali/gokrazy-runner/cmd/perm-init": {
      "ExtraFilePaths": {
        "/usr/local/bin/mke2fs": "$MKE2FS_BINARY"
      }
    },
    "github.com/denysvitali/gokrazy-runner/cmd/runner-init": {},
    "github.com/denysvitali/gokrazy-runner/cmd/runner-webui": {},
    "github.com/denysvitali/gokrazy-runner/cmd/tailscale-init": {
      "Environment": [
        "TS_AUTH_KEY_PATH=/perm/tailscale.authkey",
        "TS_HOSTNAME=$INSTANCE_NAME",
        "TS_TAILSCALE_UP_ARGS=--ssh"
      ]
    },
    "github.com/denysvitali/gokrazy-runner/cmd/wifi-init": {
      "Environment": [
        "WIFI_COUNTRY=$WIFI_COUNTRY",
        "WIFI_INIT_ETHERNET_FIRST=false"
      ]
    },
    "github.com/denysvitali/gokrazy-runner/cmd/usbdev-init": {},
    "tailscale.com/cmd/tailscaled": {
      "CommandLineFlags": [
        "-statedir=/perm/tailscale"
      ]
    },
    "tailscale.com/cmd/tailscale": {}
  }
}
EOF

echo
echo "=== Setup complete ==="
echo "Instance:  $INSTANCE_DIR"
echo "Config:    $CONFIG_FILE"
echo
echo "Next steps:"
echo "  1. gok -i $INSTANCE_NAME overwrite --full /dev/sdX   (flash SD card)"
echo "  2. Boot the device and copy your runner config to /perm:"
echo "       /perm/breakglass/authorized_keys      ssh keys"
echo "       /perm/runner.env                      URL=, NAME=, LABELS=, [IMAGE=]"
echo "       /perm/runner.token                    GitHub registration token (chmod 0600)"
echo "     Or configure everything via the web UI at https://<device>:8443/"
echo "     (falls back to http://<device>:8080/ if no TLS cert is present)"
echo "     The web UI password is the same as the gokrazy update password."
echo "     (default password: gokrazy-runner)."
echo "  3. Reboot. runner-init will pick up the config and start the runner."
