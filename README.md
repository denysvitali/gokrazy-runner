# gokrazy-runner

A [gokrazy](https://gokrazy.org) appliance image that boots straight into a
GitHub Actions self-hosted runner.

The runner itself is the official `actions/runner` (.NET, glibc-based), so it
runs inside a podman container — gokrazy provides a minimal Go userspace, the
container provides the runner's runtime dependencies. A small Go init binary
(`runner-init`) reads configuration from `/perm`, pulls the container image,
and supervises it.

Designed for **Raspberry Pi 4 / arm64**. Other arm64 SBCs work with a
different kernel package set.

## Architecture

```
            ┌──────────────────────────── gokrazy root (squashfs, ro) ────────────┐
            │                                                                     │
            │  perm-init     (one-shot: grow GPT part 4, mke2fs, reboot)          │
            │  runner-init   (long-lived: read /perm/runner.env, run podman)      │
            │  gokrazy/podman, /iptables, /nsenter   (CNI + container runtime)    │
            │  gokrazy/breakglass     (emergency SSH, key-only)                   │
            │  gokrazy/serial-busybox, /fbstatus                                  │
            │                                                                     │
            └─────────────────────────────────────────────────────────────────────┘

                                      │
                                      ▼

            ┌────────────────────── /perm (ext4, persistent) ─────────────────────┐
            │  /perm/runner.env                URL=… NAME=… LABELS=… IMAGE=…      │
            │  /perm/runner.token              one-shot GH registration token     │
            │  /perm/runner-data               container's /runner mount          │
            │  /perm/breakglass/authorized_keys                                   │
            └─────────────────────────────────────────────────────────────────────┘

                                      │ podman run
                                      ▼

            ┌─────── docker.io/myoung34/github-runner:latest (or your own) ──────┐
            │  config.sh + run.sh, registers with GitHub, runs jobs              │
            └────────────────────────────────────────────────────────────────────┘
```

## Repository layout

```
cmd/perm-init/        one-shot service: grow + format /perm on first boot
cmd/runner-init/      runs the GitHub runner container under podman
pkg/perminit/         GPT/partition helpers shared by perm-init
scripts/
  gok-packages.txt    canonical list of gokrazy packages
  gok-common.sh       shell helper sourced by setup + build scripts
  build-ota-image.sh  CI / OTA image builder
setup-gokrazy.sh      interactive local SD-card provisioning
.github/workflows/    CI + OTA image build
```

## Building an image locally

Prerequisites: `gok` CLI (`go install github.com/gokrazy/tools/cmd/gok@latest`)
and a static arm64 `mke2fs` binary on `$PATH` (or pointed at via
`MKE2FS_BINARY`).

```bash
# Provision a local instance
./setup-gokrazy.sh

# Initial flash (BE CAREFUL with the device path)
gok -i gokrazy-runner overwrite --full /dev/sdX

# Subsequent OTA updates
gok -i gokrazy-runner overwrite --update yes
```

## CI image builds

Every push to `master` triggers `.github/workflows/ota-image.yml`, which:

1. Runs `go vet` and `go test` on `ubuntu-latest`.
2. On `ubuntu-24.04-arm`, builds two artifacts via `make ota`:
   - `gokrazy-runner-rpi4b.img` — full flash image (initial SD card flash)
   - `gokrazy-runner-rpi4b-root.squashfs` — OTA root for `gok overwrite --update`
3. Compresses both with `gzip -9`, attests provenance, and publishes to a
   GitHub Release tagged `master-${SHA}` (prerelease) or `${TAG_NAME}` for
   tags.

## Configuring a runner

Once flashed and booted, write the following to `/perm` (e.g. via
breakglass SSH):

```bash
# /perm/runner.env
URL=https://github.com/<owner>/<repo>      # or https://github.com/<org>
NAME=my-pi4-runner
LABELS=self-hosted,linux,arm64,gokrazy
# Optional overrides:
# IMAGE=docker.io/myoung34/github-runner:latest
# EPHEMERAL=true
# RUNNER_GROUP=Default
```

```bash
# /perm/runner.token  (chmod 0600)
<one-shot registration token from GitHub Settings → Actions → Runners → New runner>
```

Reboot. `runner-init` will pull the image, register with GitHub, and start
running jobs. The runner's `_work` directory and registered identity persist
in `/perm/runner-data` across reboots — the registration token only needs to
be supplied once.

## Conventions

- **Branches**: `type/what` (`feature/foo`, `fix/bar`); no feature branches
  unless requested.
- **Commits**: conventional commits (`feat(runner): ...`,
  `fix(perm-init): ...`).
- **CI gate**: every push must keep CI green.
