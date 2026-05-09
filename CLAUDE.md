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

The runner image is `docker.io/myoung34/github-runner:latest` by default
(can be overridden via `IMAGE=` in `/perm/runner.env`). It accepts the env
vars `runner-init` produces (`REPO_URL`, `RUNNER_NAME`, `LABELS`,
`RUNNER_TOKEN`, `RUNNER_WORKDIR`).

The container needs `--privileged` and `--network=host` for typical CI
workloads (docker-in-docker, raw socket access). If your jobs don't need
that, narrow it down in `cmd/runner-init/main.go`.

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
