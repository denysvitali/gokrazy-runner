# gokrazy-runner — Claude notes

A gokrazy appliance image that runs a GitHub Actions self-hosted runner inside
a podman container. Target board: Raspberry Pi 4 / arm64.

## What lives where

- `cmd/perm-init`     — one-shot: grow GPT partition 4 + mke2fs on first boot.
                        Mirrors the proven pattern from `pictures-sync-s3`.
- `cmd/runner-init`   — long-lived: reads `/perm/runner.env` + `/perm/runner.token`,
                        pulls the runner image, runs `podman run` with the container's
                        `/runner` mounted onto `/perm/runner-data`. Backoff on crash.
- `pkg/perminit`      — vendored from `pictures-sync-s3` (GPT growth, fs probe).
- `scripts/`          — shared package list + image build script. Single source of
                        truth: `scripts/gok-packages.txt`.
- `setup-gokrazy.sh`  — interactive local provisioning.

## Two key invariants

1. **The package list (`scripts/gok-packages.txt`) is the only place packages are
   declared.** Both `setup-gokrazy.sh` and `scripts/build-ota-image.sh` source
   `gok-common.sh` and read this list, so local SD flashes and CI images always
   bundle the same components.
2. **No secrets in the image.** Everything that varies per-runner — repo URL,
   labels, registration token — lives in `/perm/runner.env` and `/perm/runner.token`.
   The image itself is a pure artifact of the source tree at HEAD.

## Container choice

The runner image is `ghcr.io/actions/actions-runner:latest` (the official
self-hosted runner image published by the actions team). Override via
`IMAGE=` in `/perm/runner.env` to pin a specific version or to use a
derivative image that bakes in extra tooling.

The official image's entrypoint is `run.sh` and assumes `config.sh` has
already been invoked. We override the entrypoint with `/bin/bash -c` and
the inline `bootstrap` script in `cmd/runner-init/main.go`, which:
  1. cd's into `/home/runner`,
  2. runs `./config.sh --url ... --token ... --name ... --labels ...
     --unattended --replace --disableupdate` if no `.runner` config exists,
  3. exec's `./run.sh`.

`/home/runner` is mounted onto `/perm/runner-data`, so the registration
token is only consumed on the very first boot — every subsequent reboot
reuses the persisted runner identity.

The container runs `--privileged --network=host` to support typical CI
workloads (docker-in-docker, raw devices). Tighten in `buildPodmanArgs`
if your jobs don't need it.

## CI

`.github/workflows/ota-image.yml` builds two images (`--full` and `--root`)
on `ubuntu-24.04-arm` and publishes them to a GitHub Release. The workflow
is modelled on `pictures-sync-s3`'s `ota-image.yml` but stripped of the
hostapd / exfat tooling — only `mke2fs` is needed here.

## When changing things

- New gokrazy package? Add to `scripts/gok-packages.txt` (and a
  `PackageConfig` block in both `setup-gokrazy.sh` and
  `scripts/build-ota-image.sh` if it needs flags / extra files).
- New `/perm` data the orchestrator reads? Document it in README.md and
  surface a clear error in `runner-init` when missing.
- Don't add backwards-compat shims; the project is pre-1.0. Just change
  the code and update the docs.
