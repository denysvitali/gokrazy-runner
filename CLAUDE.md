# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

A gokrazy appliance image that runs a GitHub Actions self-hosted runner
inside a podman container. Target board: Raspberry Pi 4 / arm64.

## Common commands

```bash
make build           # host go build of perm-init + runner-init into ./dist
make build-arm64     # cross-build linux/arm64 statics into ./dist
make test            # go test -race -v ./...
make test-short      # go test -short ./...
make tidy            # go mod tidy
make ota             # full build pipeline -> ./ota (needs gok + a static arm64 mke2fs in $PATH)
make clean           # rm -rf dist ota gokrazy

# Single test
go test -run TestBootBlockDevice ./pkg/perminit
go test -v -run TestPartitionDevice ./pkg/perminit

# Shellcheck (CI uses -x -S warning)
shellcheck -x -S warning scripts/gok-common.sh scripts/build-ota-image.sh setup-gokrazy.sh .github/scripts/build-mke2fs-arm64.sh
```

`make ota` shells out to `scripts/build-ota-image.sh`. Honour these env vars
when reproducing CI locally: `GOKRAZY_INSTANCE`, `GOKRAZY_PARENT_DIR`,
`IMAGE_DIR`, `IMAGE_NAME`, `GOKRAZY_IMAGE_MODE` (`ota`|`full`),
`TARGET_STORAGE_BYTES` (required for `full`), `MKE2FS_BINARY`.

## Architecture

Five Go binaries are baked into the gokrazy root, plus stock gokrazy
packages (`podman`, `iptables`, `nsenter`, `breakglass`, `serial-busybox`,
`fbstatus`) and upstream `tailscale.com/cmd/tailscaled` +
`tailscale.com/cmd/tailscale`.

**`cmd/perm-init` — one-shot, runs every boot.** Uses `pkg/perminit` to
implement a three-step fixed-point: (1) if GPT partition 4 doesn't span
the disk, grow it and reboot — the kernel reads the new geometry on the
next boot; (2) if partition 4 has no filesystem, run mke2fs and reboot;
(3) gokrazy mounts `/perm` and perm-init exits 125 (one-shot). Refuses
to reformat a partition that already has an ext/FAT signature. The reboot
goes through `gokapi.ConnectOnDevice()` → on-device `/update/reboot`.

**`cmd/runner-init` — long-lived supervisor.** At startup, calls
`pkg/dnsfallback.Ensure("/tmp/resolv.conf", ...)` which writes
`nameserver 1.1.1.1` / `nameserver 9.9.9.9` if (and only if) the file is
missing, empty, or has no `nameserver` lines — DHCP/Tailscale-supplied
resolvers always win. The target is `/tmp/resolv.conf` (not
`/etc/resolv.conf`) because on gokrazy `/etc/resolv.conf` is a symlink to
`/tmp/resolv.conf`, which itself starts as a symlink to `/proc/net/pnp`;
writing through the chain returns EIO. `dnsfallback.Ensure` writes
atomically via temp-file + rename so the symlink is replaced with a
real file, mirroring `gokrazy/dhcp`'s use of `renameio`. Then waits for `/perm/runner.env`
to exist, then loops: parse the env file, read `/perm/runner.token` (a
GitHub *registration* token, not a PAT), `podman rm -f` any stale
container, `podman pull`, then `podman run` with these key flags:
`--privileged --network=host -v /perm/runner-data:/home/runner
--entrypoint /bin/bash <image> -c <bootstrap>`. The inline `bootstrap`
const calls `./config.sh --unattended --replace --disableupdate` only when
no `.runner` config is present in the persisted volume, then exec's
`./run.sh`. Crash → exponential backoff `5s..2min` → reload config and
retry. Reserved env keys (URL/REPO_URL, NAME/RUNNER_NAME, LABELS,
IMAGE/RUNNER_IMAGE) are consumed by runner-init; everything else in
`runner.env` is passed through to the container as `-e KEY=VALUE`.

**`cmd/runner-webui` — long-lived web UI service.** Vanilla `net/http`
server serving an embedded HTML/JS app behind HTTP Basic Auth. Listens
on `:8443` over HTTPS using the per-device cert that **gokrazy itself**
generates at `/perm/ssl/gokrazy-web.{pem,key.pem}` (driven by
`Update.TLSCertificateStorage = "perm-self-signed"` in `config.json`).
`pkg/tlsconfig` is read-only — it never writes to those files. We used
to generate a parallel cert at the same paths and it raced gokrazy on
every boot, causing the cert to flip between gokrazy's 10-year EC cert
and our 1-year PKCS8 cert. Don't reintroduce that. Falls back to `:8080`
plain HTTP if the cert isn't readable yet or if `WEBUI_LISTEN_HTTP_ONLY`
is set. The Basic-Auth password *is* the
gokrazy update password — read from `/perm/gokr-pw.txt`, falling back
to `/etc/gokr-pw.txt` (the build-time seed), and finally to a literal
`gokrazy-runner`. Changing the password from the UI rewrites
`/perm/gokr-pw.txt` in place, so the gokrazy `/update/` endpoint and
the runner UI stay in sync. Edits the same `/perm` files runner-init
reads (`runner.env`, `runner.token`, `breakglass/authorized_keys`).
Because runner-init polls `runner.env` every 10s, saves through the UI
are picked up without a restart. Endpoints: `GET /` + `/static/...`
(embedded), `GET|POST /api/config`, `POST /api/token`, `GET|POST
/api/keys`, `POST /api/password`, `GET|POST /api/tailscale`,
`POST /api/reboot` (gokapi), `GET /api/status`, `GET /api/ota/status`,
`POST /api/ota/{install,upload,token}` (see `pkg/ota` below), `GET /api/wifi/status`,
`POST /api/wifi/{scan,connect,forget,reorder}` (503 when the device has no
radio; the saved-network response carries `has_password`, never the PSK),
`GET /api/system`, `GET /api/logs`, `POST /api/runner/restart`.

The front end (`pkg/webui/assets/`) is a hand-written tabbed app —
Overview / Runner / Network / System, tab held in the URL fragment. No
build step and no dependencies: `index.html` + `app.js` + `style.css` are
embedded via `pkg/webui/static.go`, so editing them is enough. Overview
polls `/api/system` every 10s and skips the poll while
`document.hidden`, because a Pi 4 running a CI job has no spare cycles for
a background tab.

**`pkg/webui/system.go` — device telemetry.** `GET /api/system` reads
procfs/sysfs directly (`/proc/{uptime,loadavg,meminfo}`,
`/sys/class/thermal/thermal_zone0/temp`, `/proc/device-tree/model`, which
is NUL-terminated) plus `statfs` for `/` and `/perm`, and shells out to
`podman ps --format json` for the runner container's state. Anything that
fails degrades to an omitted field rather than an error: a missing thermal
zone yields a nil `cpu_temp_c` so the UI hides the tile instead of
claiming 0 °C. `GET /api/logs` tails `podman logs` or `/dev/kmsg`, clamped
to 2000 lines so a caller can't make the device buffer an unbounded log.
`POST /api/runner/restart` is `podman rm -f <container>` — runner-init's
supervisor loop turns that into a restart. All three take their paths and
subprocess runner from `SystemOptions`, which is what the tests inject.

**`pkg/ota` — GitHub-driven A/B updater.** Lists releases from
`https://api.github.com/repos/denysvitali/gokrazy-runner/releases`,
downloads the gzipped squashfs asset
(`gokrazy-runner-rpi4b-root.squashfs.gz` by default), and streams it
into the loopback gokrazy updater
(`http://gokrazy:<password>@127.0.0.1/update/root`) using
`github.com/gokrazy/updater`. After `StreamTo("root", ...)` it calls
`Switch()` (flips the active partition) then `Reboot()`. The current
gokrazy password is fetched via a `PasswordFunc` callback so the URL
stays in sync with `/perm/gokr-pw.txt` after a password change.
Install history persists at `/perm/ota-install-history.json` (capped
at 20 entries).

The release listing is cached in-memory for 15 minutes and revalidated
with an `ETag` (GitHub doesn't charge 304s), and a stale cache is served
whenever the API errors — anonymous requests are limited to 60/h/IP and
a polling browser tab used to exhaust that. An optional token at
`/perm/github.token` (falling back to `$GITHUB_TOKEN`) raises the limit
to 5000/h; it's only attached to GitHub hosts. Two GitHub-free install
paths exist: `StartWithURL` (any http(s) gzipped squashfs) and
`StartWithFile` (an upload the webui spools to `/perm`, then deletes).
Overrides via env: `OTA_GITHUB_OWNER`,
`OTA_GITHUB_REPO`, `OTA_RELEASE_ASSET`, `OTA_GITHUB_API_URL`,
`OTA_GOKRAZY_UPDATE_URL`, `OTA_GOKRAZY_INSECURE`.

**`cmd/tailscale-init` — one-shot, runs every boot.** Reads the auth key
from `/perm/tailscale.authkey` (path overridable via `TS_AUTH_KEY_PATH`)
and execs `/user/tailscale up --auth-key=<key> --hostname=$TS_HOSTNAME
$TS_TAILSCALE_UP_ARGS`. If the file is missing or empty it logs and exits
cleanly so the rest of the system stays usable without Tailscale.
tailscaled persists state under `/perm/tailscale/` (set via the
`-statedir` flag in PackageConfig), so re-running `tailscale up` on every
boot is idempotent. The auth key is intentionally a flat file at the
`/perm/` root, *not* under `/perm/tailscale/`: gokrazy bind-mounts the
declared `-statedir` read-only into other services' namespaces, so the
webui can't write inside `/perm/tailscale/`. The webui's
`POST /api/tailscale` validates the key (must start with `tskey-auth-`),
persists it, and runs `tailscale up` right away — no reboot needed.

**`cmd/wifi-init` — long-lived, runs every boot.** Two distinct jobs, and
keeping them separate is the whole point of the design:

*Bring the radio up — unconditionally.* gokrazy has no udev and no
modprobe, so `brcmutil` + `brcmfmac` are located under
`/lib/modules/<uname -r>/` and loaded via `finit_module` by hand; without
that, `wlan0` never exists on a kernel that ships them as modules.

Wi-Fi needs the driver, the chip firmware, and something to load them.
`github.com/gokrazy/wifi` already ships the firmware — its extra-files tar
carries the whole `/lib/firmware/brcm/` set for the 43455 and 43430 plus
`regulatory.db` — so the only piece that was ever missing is the driver.
That comes from the kernel package's `lib/modules` tree, which gok's
default kernel does not have; both build scripts therefore pin
`KernelPackage` to `github.com/gokrazy/kernel.rpi`. Don't add firmware via
`ExtraFilePaths`: it collides with `gokrazy/wifi` and fails the build.

Every module load is **best-effort**, and a device with no radio is not an
error. Kernels differ — some build brcmfmac in, some ship it as a `.ko`,
and some (an older `kernel.rpi` pin) ship no module tree at all — so
whether Wi-Fi works is decided by whether `wlan0` appears, never by
`finit_module`. wifi-init previously called `log.Fatalf` here, and because
the radio setup now runs on every boot rather than only when Ethernet was
absent, that turned into gokrazy respawning the service roughly once a
second forever. Never exit this path: on failure it logs once and retries
every `radioRetryInterval` (60s), which also picks up a USB adapter
plugged in after boot. It then sets `IFF_UP` via `SIOCSIFFLAGS`
(nl80211 refuses to scan on a down interface, which is how it comes up
after `finit_module`), sets the regulatory domain (`WIFI_COUNTRY`, default
`CH`; the world-roaming default forbids most 5 GHz channels), and disables
power save (brcmfmac defaults it on, which makes the Pi stop acking after
idle periods — the DHCP lease survives so the device looks healthy locally
while being unreachable from the LAN).

This step used to be skipped when `eth0` had a carrier, on the theory that
a CI runner belongs on Ethernet. That was a catch-22: the operator reaches
the web UI *over Ethernet*, so the radio was always off exactly when they
wanted to scan, and every scan returned "no Wi-Fi interfaces found". Don't
reintroduce it.

*Associate — only when there is something to associate to.* It then
supervises `/user/wifi` (the stock `github.com/gokrazy/wifi` client, added
with `"DontStart": true` so wifi-init owns its lifecycle) for as long as
`/perm/wifi.json` is non-empty, restarting it with `5s..2min` backoff and
re-polling the file every 10s so a network saved from the web UI takes
effect without a reboot. `WIFI_INIT_ETHERNET_FIRST=true` suppresses *only*
this half while `eth0` has a carrier.

**`pkg/wifimanager` — Wi-Fi scanning and saved networks.** Scans via raw
nl80211 (`NL80211_CMD_TRIGGER_SCAN`, then a `NL80211_CMD_GET_SCAN` dump we
parse ourselves) because `mdlayher/wifi`'s parser drops BSSes with
information elements it doesn't recognise; it falls back to the library, and
then to the kernel's cached results when the trigger fails (EBUSY during a
concurrent scan is normal). Duplicate SSIDs collapse to one entry, preferring
5 GHz and then the strongest signal. Saved networks live in two files:
`/perm/extra-wifi.json` (the full priority-ordered list, ours) and
`/perm/wifi.json` (the head entry only, in the single-object format
`gokrazy/wifi` reads). Both are written atomically at mode 0600; forgetting
the last network deletes `/perm/wifi.json` rather than leaving a stale SSID
the client keeps retrying. Mutations snapshot and roll back on a failed
save, so a PSK that never reached disk never appears in `GetNetworks`.
`HasRadio` (surfaced as `has_radio` in `/api/wifi/status`) lets the UI say
"the driver didn't load" instead of a bare "scan failed"; a zero-interface
scan returns the `ErrNoRadio` sentinel, which names wifi-init as the place
to look.

**`cmd/usbdev-init` — long-lived udev stand-in.** Modern USB libraries
(nusb, libusb) open devices via `/dev/bus/usb/<busnum>/<devnum>`. On a
stock distro udev creates those nodes from kernel uevents; gokrazy has
no udev, so the kernel populates `/sys/bus/usb/devices/` but the
`/dev/bus/usb/` tree never appears and tools like `probe-rs run` fail
with `os error 2`. usbdev-init walks `/sys/bus/usb/devices/*` every
five seconds, reads `busnum`/`devnum`/`dev` for each non-interface
entry, and `mknod`s the matching char device (mode 0666). Nodes for
vanished devices are pruned. runner-init bind-mounts the host
`/dev/bus/usb` and `/sys/bus/usb` into the runner container so probe
access works inside the actions-runner job. A periodic scan is
deliberately used in place of a netlink uevent listener — probes
typically stay plugged in across CI runs, so the simpler approach
suffices.

**Runtime config lives in `/perm`, not in the image.** `runner.env`
(KEY=VALUE), `runner.token` (chmod 0600, one-shot, only consumed on the
*first* boot — `.runner` in `/perm/runner-data` makes it idempotent
afterwards), `breakglass/authorized_keys`, `gokr-pw.txt` (the
gokrazy update password, also used by runner-webui; falls back to the
rootfs-baked `/etc/gokr-pw.txt` if the perm copy is missing), and
`wifi.json` + `extra-wifi.json` (chmod 0600; see `pkg/wifimanager`), and
`github.token` (chmod 0600; optional, lifts the OTA GitHub API rate
limit), and `tailscale.authkey` (chmod 0600; flat file at the `/perm/` root —
tailscaled's state lives in `/perm/tailscale/`, kept separate because
gokrazy's bind-mount of `-statedir` is read-only for other services).

## Two invariants to preserve

1. **`scripts/gok-packages.txt` is the single source of truth for the
   package set.** Both `setup-gokrazy.sh` (interactive) and
   `scripts/build-ota-image.sh` (CI) source `scripts/gok-common.sh`,
   which parses this file and exposes `gok_packages` (for `gok add`) +
   `gok_config_packages` (for `config.json`'s `Packages`). Keeping the
   two flows in sync depends on this — never inline package lists.

2. **No secrets in the image.** The image is a pure artifact of the
   source tree at HEAD; everything per-device is read from `/perm` at
   runtime.

## Container image

`ghcr.io/actions/actions-runner:latest` — the official runner image
published by the actions team. Override via `IMAGE=` in
`/perm/runner.env`. The image is bare (no docker, no toolchains); jobs
must install what they need, or you can pin a derivative.

## Image build pipeline

`scripts/build-ota-image.sh` regenerates the gokrazy instance from
scratch on every invocation (`rm -rf "$INSTANCE_DIR"`, `gok new`,
write `go.mod` with a replace directive pointing at this checkout, `gok
add` every package, write `config.json`, then `gok overwrite --root` for
OTA squashfs or `--full` for raw `.img`). The version is stamped via
`-ldflags='-X main.Version=… -X main.BuildDate=…'`. CI builds both image
modes back-to-back, gzips them, attests provenance, and publishes a
GitHub Release tagged `master-${SHA}` (prerelease) or the git tag.

The static arm64 `mke2fs` is built from e2fsprogs source by
`.github/scripts/build-mke2fs-arm64.sh` (cached by file hash). Use
`LDFLAGS=-static` (gcc-driver static link), **not** `-all-static` (libtool
flag, breaks the build).

## When changing things

- **New gokrazy package?** Add to `scripts/gok-packages.txt`. If it needs
  `CommandLineFlags`, `Environment`, or `ExtraFilePaths`, add a
  `PackageConfig` block in *both* `setup-gokrazy.sh` and
  `scripts/build-ota-image.sh`.
- **New runtime config in `/perm`?** Document it in README.md and surface
  a clear error in `runner-init` when missing.
- **Pre-1.0**: don't add backwards-compat shims. Change the code and the
  docs.

## Project conventions (from global CLAUDE.md)

- Conventional commits: `feat(runner): …`, `fix(perm-init): …`,
  `ci: …`, `chore: …`.
- Branches: `type/what` (e.g. `feature/foo`); don't create feature
  branches unless asked.
- Keep CI green; check status after pushing.
- Pin GitHub Actions to versions from
  https://raw.githubusercontent.com/simonw/actions-latest/refs/heads/main/versions.txt.
