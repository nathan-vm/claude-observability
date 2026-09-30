# Release packaging + dash-generator on Docker Hub — Design

Date: 2026-09-25
Status: approved-pending-review

## Context

`v0.1.0` shipped (via the newly-merged trunk-based release pipeline,
`2026-09-24-trunk-based-release-design.md`) with 15 loose binary assets on
the GitHub Release — `wizard`, `collector`, and `dash-generator`, each
cross-compiled for 5 platform/arch combinations and uploaded as separate
files. This is hard to consume: an end user installing on one machine has
to know to grab exactly 2 of the 15 files (`wizard` + `collector` for
their platform — `dash-generator` isn't even meant to run on their
machine, see below) with no bundling or guidance.

This spec reshapes the release into two things, confirmed with the repo
owner in brainstorming (2026-09-25):

1. **`wizard` + `collector`** — these run on the *end user's own
   machine* (the wizard configures telemetry and installs the collector
   as a background service; the collector reads local transcripts and
   runs the logged-in `claude` CLI). They ship together, bundled per
   platform, so "download this one file" is the whole install step.
2. **`dash-generator`** — this "stands in for the server" (see
   `docker-compose.yaml`'s own comment) even when that server is just
   the user's own laptop running docker-compose. It moves off the
   GitHub Release entirely and becomes a **Docker Hub image**, published
   the same way `obsidian-mcp` (reference repo) publishes its image, and
   consumed by `docker-compose.yaml` as a pinned image reference instead
   of building from local source.

**Explicit amendment to the prior spec:** `2026-09-24-trunk-based-
release-design.md`'s Non-goals section said "Docker Hub (or any
container registry) publishing — distribution stays GitHub Release
binaries only." This spec deliberately reverses that, scoped to
`dash-generator` only — `wizard`/`collector` distribution is unchanged
(GitHub Release). This isn't a silent contradiction; it's a scope the
owner asked for explicitly once the first real release exposed the
packaging problem.

## Goals

- GitHub Release assets shrink from 15 loose binaries to 5 zip archives,
  one per platform/arch (`darwin-amd64`, `darwin-arm64`, `linux-amd64`,
  `linux-arm64`, `windows-amd64`), each containing both the `wizard` and
  `collector` binaries for that platform.
- `dash-generator` is built as a multi-arch (`linux/amd64`,
  `linux/arm64`) Docker image and published to Docker Hub on every
  release, tagged `:<version>` and `:latest` — mirroring `obsidian-mcp`'s
  `release.yml` pattern.
- `docker-compose.yaml` (the file real end users run) references that
  published image directly (`nathanvm/claude-observability-dash-
  generator:latest`) instead of building from local source.
- A new, fully standalone `docker-compose.dev.yaml` preserves the local
  build-from-source workflow for development/QA — it does not layer on
  top of or depend on `docker-compose.yaml` in any way.
- `src/dash-generator/Dockerfile`'s pinned Go version (`1.22`) is bumped
  to `1.25`, matching the rest of this repo's toolchain (drifted since
  `ci: bump pinned Go to 1.25 to fix macOS test failures`, #2).

## Non-goals

- Any change to `compute-next-version.sh`'s version/changelog logic —
  this spec only changes what gets *built and published* once a version
  number already exists, not how that number is computed.
- Any change to the `otel-collector`/`loki`/`grafana` service
  definitions themselves (images, config) — only `dash-generator`'s
  service definition changes, in both compose files.

## Design

### Release packaging: `package` job replaces `wizard` + `collector` jobs

`release.yml` currently has two independent jobs, each with its own
5-way `goos`/`goarch` matrix, each producing and uploading its own loose
binary. These collapse into one job, `package`, keeping the same 5-way
matrix. Each matrix leg: builds the `wizard` binary (`working-directory:
src/wizard`, unchanged build step/ldflags), builds the `collector`
binary (`working-directory: src/collector`, unchanged build step/
ldflags), then zips both into a single archive named
`claude-observability-<goos>-<goarch>.zip` and uploads that one archive
as the leg's artifact. The final `release` job's `files: dist/*` glob
needs no change — it already uploads whatever `dist/` contains after
`download-artifact --merge-multiple`, which becomes 5 zips instead of 10
loose binaries.

`dash-generator`'s existing build job is deleted outright — it no longer
produces a GitHub Release asset at all.

### Docker publish: new `docker` job

Added to `release.yml`, gated the same way every other build job is
(`needs: compute-version`, `if: needs.compute-version.outputs.bumped ==
'true'`). Steps, mirroring `obsidian-mcp`'s `release.yml`:
`docker/setup-qemu-action`, `docker/setup-buildx-action`,
`docker/login-action` (`username: ${{ vars.DOCKERHUB_USERNAME }}`,
`password: ${{ secrets.DOCKERHUB_TOKEN }}`), then
`docker/build-push-action` with `context: src/dash-generator`,
`platforms: linux/amd64,linux/arm64`, `push: true`, and two tags:
`${{ vars.DOCKERHUB_USERNAME }}/claude-observability-dash-generator:${{
needs.compute-version.outputs.version }}` and `...:latest`. No secret
mount is needed (unlike obsidian-mcp's `HF_TOKEN` — dash-generator's
Dockerfile has no equivalent download-at-build-time step). GHA build
cache (`cache-from`/`cache-to: type=gha`) carried over from the
obsidian-mcp pattern since it's free and speeds up the multi-arch build.

`vars.DOCKERHUB_USERNAME` (`nathanvm`) and `secrets.DOCKERHUB_TOKEN` are
already configured on this repo (confirmed via `gh variable list`/`gh
secret list`, 2026-09-25) — no rollout-ordering step needed before this
merges.

### `docker-compose.yaml` (production/end-user file)

The `dash-generator` service's `build: context: ./src/dash-generator`
becomes:

```yaml
  dash-generator:
    image: nathanvm/claude-observability-dash-generator:latest
```

Hardcoded, no env var indirection — the owner was explicit that this
file doesn't need to be overridable for a different Docker Hub account.
Everything else about the service (`environment`, `volumes`,
`depends_on`, `restart`) is unchanged.

### `docker-compose.dev.yaml` (new, standalone)

A **complete, independent** compose file — not a `-f a -f b` override
layered on `docker-compose.yaml`. Contains all four services
(`otel-collector`, `loki`, `grafana`, `dash-generator`) with identical
definitions to `docker-compose.yaml`, except `dash-generator` keeps
`build: context: ./src/dash-generator` instead of `image:`, and its
**default host ports are entirely different from prod's**, per the
owner's explicit requirement that prod and dev never share default
ports:

| | prod (`docker-compose.yaml`) | dev (`docker-compose.dev.yaml`) |
|---|---|---|
| OTLP gRPC | `47317` | `40317` |
| OTLP HTTP | `47318` | `40318` |
| Grafana | `47300` | `40300` |
| Loki | `47100` | `40100` |

Same last-three-digits pattern, different hundred-thousands prefix
(`40xxx` vs `47xxx`) — easy to keep straight, and far outside the
worktree-isolation band (`47100`–`62318`, from
`scripts/docker-worktree-env.sh`'s offset×300 scheme), so a dev-file
default can never collide with a worktree-isolated instance either. The
env var *names* stay identical (`OTEL_GRPC_PORT`, `OTEL_HTTP_PORT`,
`GRAFANA_PORT`, `LOKI_PORT`) — only the `:-default` fallback differs —
so `docker-worktree.local` (which sets these same names) still overrides
correctly when used with the dev file.

Top-level `name: claude-observability-dev` (distinct from the prod
file's `name: claude-observability`), so a bare `docker compose -f
docker-compose.dev.yaml up` (no `--env-file`) collides with prod on
neither ports nor container/network/volume identity.

Usage: `docker compose -f docker-compose.dev.yaml --env-file
docker-worktree.local up -d --build`. This is what changes in the two
files that already reference the old single-file command:

- `.claude/agents/qa.md` step 1: `docker compose --env-file docker-
  worktree.local up -d --build` → `docker compose -f docker-
  compose.dev.yaml --env-file docker-worktree.local up -d --build`.
- `CLAUDE.md`'s "Bring up a worktree's stack with" line: same
  replacement.

**Trade-off, stated plainly:** this duplicates `otel-collector`/`loki`/
`grafana`'s definitions across two files. A future change to, say,
Loki's image version has to be made in both places. The owner chose
this explicitly over a layered override to keep the dev file fully
independent of the prod file's shape — recorded here so it isn't
mistaken for an oversight later.

### `src/dash-generator/Dockerfile`

`FROM golang:1.22-alpine AS build` → `FROM golang:1.25-alpine AS build`.
One-line change, no other Dockerfile behavior affected.

### `CLAUDE.md` cleanup (unrelated drift, fixed while already editing this file)

The "Worktree + docker isolation" section's compose command needs
updating regardless (see above). While in this file: its "Release
conventions" section still has a leftover line from before
`trunk-based-release` merged — "until it merges, neither `pr-title.yml`
nor that `release.yml` exist on this branch or `main`" — which is now
false (both merged in #3). Remove that stale sentence in the same
commit.

## Edge cases

- **Zip contents on Windows**: the Windows leg's zip contains
  `claude-observability-wizard-windows-amd64.exe` and `claude-
  observability-collector-windows-amd64.exe` — `.exe` suffix preserved
  inside the archive, matching today's loose-binary naming exactly, just
  bundled.
- **`docker-compose.dev.yaml` and `docker-compose.yaml` run
  simultaneously, both with no `--env-file`**: now safe by construction —
  distinct `name:` (no container/network/volume collision) *and* distinct
  default ports (`40xxx` vs `47xxx`, no port-bind collision either).

## Testing

- **Zip packaging**: buildable and verifiable locally without CI —
  cross-compile `wizard` and `collector` for one platform combo, zip
  them, unzip into a scratch dir, run both binaries' `--version` flag to
  confirm they're intact and executable.
- **Docker image build**: `docker buildx build --platform
  linux/amd64,linux/arm64 src/dash-generator` locally (no `--push`)
  confirms the multi-arch build itself succeeds and the Go 1.25 bump
  doesn't break anything, without needing real Docker Hub credentials.
- **`docker-compose.dev.yaml` standalone**: `docker compose -f
  docker-compose.dev.yaml --env-file <a test env file> up -d --build`
  brings up all 4 services healthy, confirms it needs nothing from
  `docker-compose.yaml`. Separately, `docker compose -f docker-
  compose.dev.yaml config` with no env-file confirms the `40xxx` default
  ports, and running it alongside a `docker compose up` (prod, no
  env-file) confirms both come up with no port or project-name collision.
- **`docker-compose.yaml` unchanged behavior for the other 3 services**:
  `docker compose config` before/after this change, diffed, shows only
  the `dash-generator` service's `build`→`image` change — nothing else
  moved.
- End-to-end Docker Hub publish (`docker` job actually pushing, image
  actually pullable) is only verifiable by a real release running in
  CI with the secret configured — same category of "first real merge is
  the test" as the trunk-based release pipeline itself.
