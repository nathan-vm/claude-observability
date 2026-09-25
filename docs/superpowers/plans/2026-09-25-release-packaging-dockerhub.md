# Release Packaging + Dash-Generator on Docker Hub Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** GitHub Releases ship 5 platform zip archives (wizard+collector bundled) instead of 15 loose binaries, and `dash-generator` — which runs as "the server" — is published as a multi-arch Docker Hub image instead of a loose binary, with `docker-compose.yaml` pulling that pinned image and a new, fully standalone `docker-compose.dev.yaml` preserving the local-build workflow for development/QA.

**Architecture:** `release.yml`'s `wizard` and `collector` matrix jobs collapse into one `package` job that builds both binaries per platform leg and zips them together; a new `docker` job builds+pushes `dash-generator`'s multi-arch image via buildx; the old `dash-generator` binary-build job is deleted. `docker-compose.yaml` switches `dash-generator` from `build:` to a hardcoded `image:` reference; `docker-compose.dev.yaml` is a brand-new, independent file (not a `-f`-layered override) carrying all 4 services with `dash-generator` still building from source, on a completely different default port range (`40xxx` vs prod's `47xxx`) so the two can run side by side without colliding.

**Tech Stack:** GitHub Actions, `docker/build-push-action` + Buildx/QEMU, `zip`, Docker Compose.

**Spec:** `docs/superpowers/specs/2026-09-25-release-packaging-dockerhub-design.md`

## Global Constraints

- `vars.DOCKERHUB_USERNAME` (`nathanvm`) and `secrets.DOCKERHUB_TOKEN` are already configured on this GitHub repo (confirmed via `gh variable list`/`gh secret list`, 2026-09-25) — no secret-bootstrapping task in this plan.
- Docker image name: `nathanvm/claude-observability-dash-generator`, tags `:<version>` (no `v` prefix, matching existing artifact-naming convention) and `:latest`.
- Docker image platforms: `linux/amd64,linux/arm64` only — `dash-generator` runs in a container; no native macOS/Windows image is needed.
- Zip archive naming: `claude-observability-<goos>-<goarch>.zip`, containing the flat (no directory structure) `claude-observability-wizard-<goos>-<goarch>[.exe]` and `claude-observability-collector-<goos>-<goarch>[.exe]` binaries.
- `docker-compose.yaml` (prod) `dash-generator` image reference is hardcoded (`nathanvm/claude-observability-dash-generator:latest`) — no env var indirection.
- `docker-compose.dev.yaml` is a **complete, standalone** file — never invoked as `-f docker-compose.yaml -f docker-compose.dev.yaml`, always `-f docker-compose.dev.yaml` alone.
- `docker-compose.dev.yaml` default ports: OTLP gRPC `40317`, OTLP HTTP `40318`, Grafana `40300`, Loki `40100` — same env var names as prod (`OTEL_GRPC_PORT`, `OTEL_HTTP_PORT`, `GRAFANA_PORT`, `LOKI_PORT`) so `docker-worktree.local` still overrides correctly; only the `:-default` fallback differs. Top-level `name: claude-observability-dev` (prod stays `name: claude-observability`).
- `src/dash-generator/Dockerfile`: Go version `1.22` → `1.25`, matching the rest of this repo's toolchain.

---

### Task 1: Bump `src/dash-generator/Dockerfile`'s Go version, verify it still builds

**Files:**
- Modify: `src/dash-generator/Dockerfile`

- [ ] **Step 1: Bump the Go version**

In `src/dash-generator/Dockerfile`, change:
```dockerfile
FROM golang:1.22-alpine AS build
```
to:
```dockerfile
FROM golang:1.25-alpine AS build
```

- [ ] **Step 2: Build the image locally and verify it runs**

Run:
```bash
docker build -t claude-observability-dash-generator-test src/dash-generator
docker run --rm claude-observability-dash-generator-test --version
```
Expected: the build succeeds, and the run prints `dev` (no `-X main.version=...` ldflag is passed in this ad hoc local build, so it falls back to the default — this binary already has a `--version` flag from the trunk-based-release work).

- [ ] **Step 3 (best-effort): confirm the multi-arch build path works**

Run: `docker buildx build --platform linux/amd64,linux/arm64 src/dash-generator`
Expected: exits 0. If your local Docker setup doesn't have QEMU emulation registered for cross-arch builds, this step may fail for environment reasons unrelated to the Dockerfile itself — that's fine; the real multi-arch build in CI (Task 6) has `docker/setup-qemu-action` + `docker/setup-buildx-action` to guarantee this works, so don't spend time debugging local QEMU setup if this step fails for infra reasons. Only investigate if the *build itself* reports a Go/Dockerfile error.

- [ ] **Step 4: Clean up and commit**

```bash
docker rmi claude-observability-dash-generator-test
git add src/dash-generator/Dockerfile
git commit -m "fix(dash-generator): bump Dockerfile Go version to 1.25"
```

---

### Task 2: `docker-compose.yaml` — dash-generator switches from `build:` to `image:`

**Files:**
- Modify: `docker-compose.yaml`

- [ ] **Step 1: Replace the `build:` key with `image:`**

In `docker-compose.yaml`, find the `dash-generator` service:
```yaml
  dash-generator:
    build:
      context: ./src/dash-generator
    environment:
```
Replace with:
```yaml
  dash-generator:
    image: nathanvm/claude-observability-dash-generator:latest
    environment:
```
Everything else in that service block (`environment`, `volumes`, `depends_on`, `restart`) stays exactly as-is.

- [ ] **Step 2: Verify with `docker compose config`**

Run: `docker compose config | grep -A1 'dash-generator:'`
Expected: shows `image: nathanvm/claude-observability-dash-generator:latest` for the `dash-generator` service, and no `build:` key anywhere in its output.

- [ ] **Step 3: Commit**

```bash
git add docker-compose.yaml
git commit -m "feat: point docker-compose.yaml's dash-generator at the published Docker Hub image"
```

---

### Task 3: New `docker-compose.dev.yaml` — standalone dev/QA stack

**Files:**
- Create: `docker-compose.dev.yaml`

**Interfaces:**
- Produces: a compose file usable as `docker compose -f docker-compose.dev.yaml --env-file docker-worktree.local up -d --build`, consumed by Task 4's updates to `.claude/agents/qa.md` and `CLAUDE.md`.

- [ ] **Step 1: Create the file**

Create `docker-compose.dev.yaml`:

```yaml
name: claude-observability-dev

# Standalone dev/QA stack — NOT layered on top of docker-compose.yaml (never
# run as `-f docker-compose.yaml -f docker-compose.dev.yaml`, always
# `-f docker-compose.dev.yaml` alone). Same 4 services as docker-compose.yaml,
# except dash-generator builds from local source here instead of pulling the
# published image, and every default port is in the 40xxx range instead of
# 47xxx so this can run at the same time as the "prod" file without colliding
# on ports or on container/network/volume identity (distinct `name:` above).
# See docs/superpowers/specs/2026-09-25-release-packaging-dockerhub-design.md.
#
# Every port is published on 127.0.0.1 only, same reasoning as
# docker-compose.yaml: unauthenticated Grafana/Loki/OTLP access has no
# business being reachable from other devices on the network.

services:
  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.160.0
    command: ["--config=/etc/otel-collector-config.yaml"]
    volumes:
      - ./config/otel-collector-config.yaml:/etc/otel-collector-config.yaml:ro
    ports:
      - "127.0.0.1:${OTEL_GRPC_PORT:-40317}:4317"
      - "127.0.0.1:${OTEL_HTTP_PORT:-40318}:4318"
    depends_on:
      - loki
    restart: unless-stopped

  loki:
    image: grafana/loki:latest
    command: ["-config.file=/etc/loki/loki-config.yaml"]
    ports:
      - "127.0.0.1:${LOKI_PORT:-40100}:3100"
    volumes:
      - ./config/loki-config.yaml:/etc/loki/loki-config.yaml:ro
      - loki-data:/loki
    restart: unless-stopped

  grafana:
    image: grafana/grafana:latest
    environment:
      - GF_AUTH_ANONYMOUS_ENABLED=true
      - GF_AUTH_ANONYMOUS_ORG_NAME=Main Org.
      - GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer
      - GF_SECURITY_ADMIN_USER=admin
      - GF_SECURITY_ADMIN_PASSWORD=admin
      - GF_USERS_DEFAULT_THEME=dark
      - GF_UNIFIED_ALERTING_ENABLED=false
      - GF_ALERTING_ENABLED=false
      - GF_ANALYTICS_REPORTING_ENABLED=false
      - GF_ANALYTICS_CHECK_FOR_UPDATES=false
      - GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES=false
      - GF_NEWS_NEWS_FEED_ENABLED=false
      - GF_LOG_LEVEL=warn
    volumes:
      - ./grafana/provisioning:/etc/grafana/provisioning:ro
      - ./grafana/dashboards:/var/lib/grafana/dashboards:ro
      - grafana-data:/var/lib/grafana
    ports:
      - "127.0.0.1:${GRAFANA_PORT:-40300}:3000"
    depends_on:
      - loki
    restart: unless-stopped

  dash-generator:
    build:
      context: ./src/dash-generator
    environment:
      - LOKI_URL=http://loki:3100
      - EXPORTER_STREAM=${EXPORTER_STREAM:-claude-code-exporter-dev}
      - GRAFANA_DIR=/app/grafana
      - STATE_FILE=/app/.state/dash-generator-state.json
    volumes:
      - ./grafana:/app/grafana
      - dash-generator-state:/app/.state
    depends_on:
      - loki
    restart: unless-stopped

volumes:
  grafana-data:
  loki-data:
  dash-generator-state:
```

- [ ] **Step 2: Verify default ports differ from prod**

Run: `docker compose -f docker-compose.dev.yaml config | grep '127.0.0.1'`
Expected: shows `40317`, `40318`, `40300`, `40100` — not the `47xxx` prod defaults.

- [ ] **Step 3: Verify it needs nothing from `docker-compose.yaml`**

Run: `docker compose -f docker-compose.dev.yaml config >/dev/null && echo OK`
Expected: `OK` — the file parses and resolves entirely on its own (no `docker-compose.yaml` in the `-f` list).

- [ ] **Step 4: Commit**

```bash
git add docker-compose.dev.yaml
git commit -m "feat: add standalone docker-compose.dev.yaml for local dash-generator development"
```

---

### Task 4: Update `.claude/agents/qa.md` and `CLAUDE.md` for the new dev compose file

**Files:**
- Modify: `.claude/agents/qa.md`
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update `qa.md`'s compose command**

In `.claude/agents/qa.md`, change:
```
1. In the given worktree, bring up its isolated docker-compose stack: `docker compose --env-file docker-worktree.local up -d --build`. If it's not already generated, run `scripts/docker-worktree-env.sh` first.
```
to:
```
1. In the given worktree, bring up its isolated docker-compose stack: `docker compose -f docker-compose.dev.yaml --env-file docker-worktree.local up -d --build`. If it's not already generated, run `scripts/docker-worktree-env.sh` first.
```

- [ ] **Step 2: Update `CLAUDE.md`'s compose command**

In `CLAUDE.md`'s "Worktree + docker isolation" section, change:
```
- Bring up a worktree's stack with:
  `docker compose --env-file docker-worktree.local up -d --build`
```
to:
```
- Bring up a worktree's stack with:
  `docker compose -f docker-compose.dev.yaml --env-file docker-worktree.local up -d --build`
```

- [ ] **Step 3: Remove the stale sentence in `CLAUDE.md`'s "Release conventions" section**

Delete this now-false sentence (both `pr-title.yml` and `release.yml` have been on `main` since PR #3 merged):
```
- This describes the process landing with `feat/trunk-based-release`;
  until it merges, neither `pr-title.yml` nor that `release.yml` exist
  on this branch or `main` — `release.yml` here is tag-triggered only,
  with no changelog or PR-title enforcement yet.
```

- [ ] **Step 4: Verify no other reference to the old single-file compose command remains**

Run: `grep -rn "docker compose --env-file docker-worktree.local" .claude/ CLAUDE.md`
Expected: no matches (both occurrences were just updated).

- [ ] **Step 5: Commit**

```bash
git add .claude/agents/qa.md CLAUDE.md
git commit -m "docs: point qa agent and CLAUDE.md at docker-compose.dev.yaml, drop stale release note"
```

---

### Task 5: `release.yml` — collapse `wizard` + `collector` into one `package` job

**Files:**
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- Produces: artifact name pattern `package-<goos>-<goarch>` (was `binaries-wizard-<goos>-<goarch>` / `binaries-collector-<goos>-<goarch>`), each containing one `claude-observability-<goos>-<goarch>.zip`. Consumed by the `release` job's `needs` (updated in this task) and `download-artifact --merge-multiple` step (unchanged).

- [ ] **Step 1: Replace the `wizard` and `collector` jobs with one `package` job**

In `.github/workflows/release.yml`, delete the entire `wizard:` job block and the entire `collector:` job block, and insert this `package:` job in their place (position doesn't matter for GitHub Actions — job order in the file is cosmetic — but put it where `wizard:` was, immediately after `compute-version:`):

```yaml
  package:
    needs: compute-version
    if: needs.compute-version.outputs.bumped == 'true'
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - goos: darwin
            goarch: amd64
          - goos: darwin
            goarch: arm64
          - goos: linux
            goarch: amd64
          - goos: linux
            goarch: arm64
          - goos: windows
            goarch: amd64
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25'

      - name: build wizard
        working-directory: src/wizard
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CGO_ENABLED: '0'
        run: |
          ext=""
          if [ "${{ matrix.goos }}" = "windows" ]; then ext=".exe"; fi
          go build -ldflags "-X main.version=${{ needs.compute-version.outputs.version }}" -o "claude-observability-wizard-${{ matrix.goos }}-${{ matrix.goarch }}${ext}" ./cmd/wizard

      - name: build collector
        working-directory: src/collector
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CGO_ENABLED: '0'
        run: |
          ext=""
          if [ "${{ matrix.goos }}" = "windows" ]; then ext=".exe"; fi
          go build -ldflags "-X main.version=${{ needs.compute-version.outputs.version }}" -o "claude-observability-collector-${{ matrix.goos }}-${{ matrix.goarch }}${ext}" ./cmd/collector

      - name: zip
        run: |
          ext=""
          if [ "${{ matrix.goos }}" = "windows" ]; then ext=".exe"; fi
          zip -j "claude-observability-${{ matrix.goos }}-${{ matrix.goarch }}.zip" \
            "src/wizard/claude-observability-wizard-${{ matrix.goos }}-${{ matrix.goarch }}${ext}" \
            "src/collector/claude-observability-collector-${{ matrix.goos }}-${{ matrix.goarch }}${ext}"

      - uses: actions/upload-artifact@v4
        with:
          name: package-${{ matrix.goos }}-${{ matrix.goarch }}
          path: claude-observability-${{ matrix.goos }}-${{ matrix.goarch }}.zip
```

- [ ] **Step 2: Update the `release` job's `needs`**

Change:
```yaml
  release:
    needs: [compute-version, wizard, dash-generator, collector]
```
to (temporarily, `dash-generator` stays for now — Task 6 removes it):
```yaml
  release:
    needs: [compute-version, package, dash-generator]
```

- [ ] **Step 3: Validate YAML syntax**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))" && echo OK`
Expected: `OK`

- [ ] **Step 4: Locally exercise the build+zip logic for one platform**

This mirrors exactly what the `package` job's steps do for a single matrix leg, without needing GitHub Actions:

```bash
(cd src/wizard && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=9.9.9" -o claude-observability-wizard-linux-amd64 ./cmd/wizard)
(cd src/collector && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=9.9.9" -o claude-observability-collector-linux-amd64 ./cmd/collector)
zip -j claude-observability-linux-amd64.zip \
  src/wizard/claude-observability-wizard-linux-amd64 \
  src/collector/claude-observability-collector-linux-amd64
unzip -l claude-observability-linux-amd64.zip
```
Expected: the `unzip -l` listing shows exactly two flat entries — `claude-observability-wizard-linux-amd64` and `claude-observability-collector-linux-amd64` — no directory paths.

- [ ] **Step 5: Clean up**

```bash
rm -f src/wizard/claude-observability-wizard-linux-amd64 src/collector/claude-observability-collector-linux-amd64 claude-observability-linux-amd64.zip
```

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: bundle wizard+collector into one zip per platform instead of loose binaries"
```

---

### Task 6: `release.yml` — add the `docker` job, remove the old `dash-generator` job

**Files:**
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `src/dash-generator/Dockerfile` (Task 1), `vars.DOCKERHUB_USERNAME`/`secrets.DOCKERHUB_TOKEN` (already configured, see Global Constraints).
- Produces: Docker Hub image `nathanvm/claude-observability-dash-generator:<version>` and `:latest`.

- [ ] **Step 1: Delete the `dash-generator:` job block entirely**

Remove the whole job (the one with the 5-way matrix building `claude-observability-dash-generator-*` binaries and uploading them as `binaries-dash-generator-*` artifacts).

- [ ] **Step 2: Add the `docker` job**

Insert this job (position doesn't matter; put it where `dash-generator:` was):

```yaml
  docker:
    needs: compute-version
    if: needs.compute-version.outputs.bumped == 'true'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Set up QEMU
        uses: docker/setup-qemu-action@v3

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Log in to Docker Hub
        uses: docker/login-action@v3
        with:
          username: ${{ vars.DOCKERHUB_USERNAME }}
          password: ${{ secrets.DOCKERHUB_TOKEN }}

      - name: Build and push image
        uses: docker/build-push-action@v6
        with:
          context: src/dash-generator
          push: true
          platforms: linux/amd64,linux/arm64
          tags: |
            ${{ vars.DOCKERHUB_USERNAME }}/claude-observability-dash-generator:${{ needs.compute-version.outputs.version }}
            ${{ vars.DOCKERHUB_USERNAME }}/claude-observability-dash-generator:latest
          cache-from: type=gha
          cache-to: type=gha,mode=max
```

- [ ] **Step 3: Update the `release` job's `needs` again**

Change:
```yaml
  release:
    needs: [compute-version, package, dash-generator]
```
to:
```yaml
  release:
    needs: [compute-version, package, docker]
```

- [ ] **Step 4: Validate YAML syntax**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))" && echo OK`
Expected: `OK`

- [ ] **Step 5: Diff-review against the spec**

Re-read `docs/superpowers/specs/2026-09-25-release-packaging-dockerhub-design.md`'s "Docker publish: new `docker` job" section side by side with the file and confirm every piece matches: gated on `compute-version`'s `bumped` output, QEMU+Buildx+login+build-push steps in that order, `context: src/dash-generator`, both platforms, both tags, GHA cache.

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: publish dash-generator to Docker Hub instead of building it as a release binary"
```

---

### Task 7: End-to-end local validation of the compose port separation

**Files:** none (validation only — no new files)

- [ ] **Step 1: Confirm `docker-compose.yaml`'s change is dash-generator-only**

Compare this branch's `docker-compose.yaml` against `origin/main`'s (the version before this branch's work started) — robust regardless of how many commits ended up on this branch:

```bash
git fetch origin main --quiet
git diff origin/main -- docker-compose.yaml
```
Expected: the only changed lines are inside the `dash-generator` service (`build:`/`context:` removed, `image: nathanvm/claude-observability-dash-generator:latest` added) — `otel-collector`, `loki`, and `grafana` show no diff at all.

- [ ] **Step 2: Bring up prod (no env-file) and dev (no env-file) at the same time**

```bash
docker compose up -d
docker compose -f docker-compose.dev.yaml up -d --build
docker compose ps
docker compose -f docker-compose.dev.yaml ps
```
Expected: both `docker compose ps` calls show their own 4 healthy/running containers, under two different `COMPOSE_PROJECT_NAME`s (`claude-observability` and `claude-observability-dev`), on non-overlapping ports (`47xxx` vs `40xxx`) — neither `up` command reports a port-bind conflict.

- [ ] **Step 3: Tear both down**

```bash
docker compose -f docker-compose.dev.yaml down
docker compose down
```
(Leave `-v`/volumes alone — this is a validation run, not a reason to wipe real data if this happens to be run somewhere with existing volumes. If either stack was already running before this task for unrelated reasons, `down` without `-v` is still safe — it only removes containers/networks, not volumes.)

- [ ] **Step 4: No commit for this task** — pure validation. If Step 1 or Step 2 surfaced a real problem, go fix the relevant earlier task (2 or 3) and re-run this task.

---

## Post-implementation note (not a task — informational)

End-to-end Docker Hub publish verification — the `docker` job actually pushing an image and that image actually being pullable (`docker pull nathanvm/claude-observability-dash-generator:latest`) — can't be exercised locally; it needs real GitHub Actions credentials. The next release that runs after this plan merges (per the existing trunk-based pipeline, that's the next `feat:`/`fix:`/etc. push to `main`) is the natural first live exercise, same as the trunk-based release pipeline's own tagging was when it first shipped.
