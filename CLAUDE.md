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

Two Go binaries are baked into the gokrazy root, plus stock gokrazy
packages (`podman`, `iptables`, `nsenter`, `breakglass`, `serial-busybox`,
`fbstatus`).

**`cmd/perm-init` — one-shot, runs every boot.** Uses `pkg/perminit` to
implement a three-step fixed-point: (1) if GPT partition 4 doesn't span
the disk, grow it and reboot — the kernel reads the new geometry on the
next boot; (2) if partition 4 has no filesystem, run mke2fs and reboot;
(3) gokrazy mounts `/perm` and perm-init exits 125 (one-shot). Refuses
to reformat a partition that already has an ext/FAT signature. The reboot
goes through `gokapi.ConnectOnDevice()` → on-device `/update/reboot`.

**`cmd/runner-init` — long-lived supervisor.** Waits for `/perm/runner.env`
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

**Runtime config lives in `/perm`, not in the image.** `runner.env`
(KEY=VALUE), `runner.token` (chmod 0600, one-shot, only consumed on the
*first* boot — `.runner` in `/perm/runner-data` makes it idempotent
afterwards), and `breakglass/authorized_keys`.

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
