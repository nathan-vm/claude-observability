# Local Setup Wizard — Design

Date: 2026-09-23
Status: approved-pending-review

## Context

`bin/setup` today is bash-only: it discovers `~/.claude*` config directories,
asks which accounts to monitor, writes telemetry env vars into the shell rc
(bash/zsh/fish), and — critically — runs `docker compose up -d` itself. This
only works on macOS/Linux with a POSIX shell, and it conflates "configure the
client" with "run the infrastructure."

The collector's background-service installer (`bin/install-service.sh`) is
macOS-only (launchd); there is no Linux or Windows equivalent.

This is the first of two sub-projects. This one ("local setup") covers a
single-user, single-machine, local-only stack — exactly what exists today,
minus the bash/macOS-only constraints, plus room for a second project to add
later without re-touching this one. The second project (tokens, multi-machine
shared ingest, external Grafana auto-provisioning, docker-compose profiles)
is out of scope here and gets its own spec.

## Goals

- Cross-platform (macOS, Linux, Windows), self-contained — no runtime
  dependency (no Node, Python, bash, or PowerShell required to run it).
- Setup never runs `docker compose up -d` itself. The user brings the stack
  up by hand, in whichever order they want; setup's job is to configure the
  client and verify it can reach what's already running.
- The OTel endpoint and an optional bearer token are user-editable fields
  (prefilled with the local default), not hardcoded — so this same wizard
  already knows how to point at a non-default endpoint (e.g. a different
  local port, or later, a project-2 shared collector) without being aware of
  what's actually on the other end.
- Tests.

## Non-goals (deferred to project #2)

- Generating or validating OTLP bearer tokens server-side.
- Exposing the otel-collector beyond 127.0.0.1 / multi-machine ingest.
- External Grafana auto-provisioning, docker-compose profiles.
- A "join an existing shared collector" flow distinct from this wizard.

## Distribution

A single Go module at `setup/` (new top-level directory, alongside
`collector/`), built into standalone binaries for macOS/Linux/Windows ×
amd64/arm64 via a GitHub Actions release workflow (triggered on tag push),
published on GitHub Releases. `go build ./cmd/setup` also works locally for
anyone with Go installed, but end users are expected to just download the
binary for their OS/arch and run it — no dependency beyond the OS itself.

`bin/setup`, `bin/install-service.sh`, `bin/claude-telemetry.sh`, and
`bin/claude-telemetry.fish` are removed; the Go binary replaces all four.
README.md's "Getting started" section is updated to point at the release
binary instead of `bin/setup`.

## Wizard flow

1. **Discover accounts** — same logic as today, ported to Go: scan
   `~/.claude` and `~/.claude-*` (Windows: `%USERPROFILE%\.claude*`) for a
   `projects/` subdirectory, read `oauthAccount.emailAddress` out of
   `.claude.json`. Ask which to monitor (same "monitor all? / one at a
   time" prompts as today).

2. **OTel endpoint + token** — one prompt each, prefilled, single line to
   accept-or-edit:
   ```
   OTel endpoint [http://localhost:47317]:
   OTel token    []:
   ```
   Enter accepts the default; typing replaces it. The token field is empty
   by default (no auth expected against the local stack) but is plumbed
   through so a value can be pasted in as-is — nothing on this side
   validates it.

3. **Health check** — before writing anything, TCP-dial the host:port parsed
   out of whatever endpoint was entered (`net.DialTimeout("tcp", ..., 3s)`),
   endpoint-agnostic — it doesn't know or care whether that address is the
   local docker stack or something else. On failure, print the endpoint that
   failed and stop without writing any config:
   ```
   ✗ Can't reach http://localhost:47317 — nothing is listening there.
     Run `docker compose up -d` first, then re-run this wizard.
   ```
   For a non-default endpoint (the user edited it away from
   `http://localhost:47317`), the second line changes to a host-agnostic
   hint instead of assuming docker:
   ```
   ✗ Can't reach http://203.0.113.9:4317 — nothing is listening there.
     Check the URL and that it's reachable from this machine.
   ```
   The dial itself (`net.DialTimeout`) is identical in both cases; only the
   hint line is picked based on whether the endpoint still matches the
   default. On success, continue.

4. **Write env vars** — idempotent marker-block write, same shape as today's
   bash version: `CLAUDE_CODE_ENABLE_TELEMETRY`, `OTEL_METRICS_EXPORTER`,
   `OTEL_LOGS_EXPORTER`, `OTEL_EXPORTER_OTLP_PROTOCOL`,
   `OTEL_EXPORTER_OTLP_ENDPOINT` (the value from step 2),
   `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_LOGS_EXPORT_INTERVAL`, plus
   `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20<token>` (space
   percent-encoded per the OTLP env var spec's key=value list format) only
   if a token was entered, plus the collector's own `CLAUDE_DIR`/
   `CLAUDE_OBSERVABILITY_EXTRA_DIRS`/`EXPORTER_STREAM`. Destination:
   - bash/zsh: `~/.bashrc` or `~/.zshrc`, chosen the same way as today
     (`$SHELL` basename)
   - fish: `~/.config/fish/config.fish`
   - Windows: persistent user environment variables (`setx`, one call per
     var) — there's no single rc file every shell sources, and `setx`
     covers cmd/PowerShell/anything launched fresh afterward
   Re-running is a no-op if the marker block is already present, same as
   today (edit the block by hand to change values).

5. **Account-limits ignore list** — same behavior as today (unmonitored
   discovered accounts go on `grafana/account-limits.json`'s `ignore` list),
   reimplemented with Go's `encoding/json` — drops the `jq` dependency the
   bash version had.

6. **Collector service (optional prompt)** — "Install the collector as a
   background service now? [Y/n]". If yes, installs/starts it:
   - macOS: launchd `LaunchAgent`, same plist shape as today's
     `bin/install-service.sh`
   - Linux: a `systemd --user` unit, enabled and started
   - Windows: a Scheduled Task (`schtasks`) registered to run at logon
   All three resolve `node`/`claude` via `PATH` lookup at install time (Go's
   `exec.LookPath`) and error clearly if either is missing, matching today's
   `require()` checks.

7. **Summary** — print what was configured and the next step ("open a new
   terminal, or reload your shell config").

## Testing

Go's standard `testing` package, no external test framework.

- **discovery**: fixture temp `$HOME` with fake `.claude*/projects` dirs and
  `.claude.json` files (valid, missing, malformed) — asserts the right
  dirs/emails come back.
- **envwriter**: writes into a temp file, asserts marker-block idempotency
  (second run is a no-op), correct syntax per shell target, and that a
  token only produces the `OTEL_EXPORTER_OTLP_HEADERS` line when non-empty.
- **health check**: spin up a real `net.Listen("tcp", "127.0.0.1:0")` in the
  test for the success case; dial a closed port for the failure case — no
  mocking needed, this is cheap and deterministic.
- **limits**: JSON merge logic (ignore list) against fixture
  `account-limits.json` files.
- **service installers**: assert the generated plist XML / systemd unit
  file / Scheduled Task XML content (golden-file style), gated so the actual
  OS-level registration calls only run when built for/on that OS — CI
  doesn't need to actually install a launchd agent to verify what would be
  written.
- **CI**: GitHub Actions matrix (`macos-latest`, `ubuntu-latest`,
  `windows-latest`) runs `go test ./...` on every push. A separate,
  tag-triggered release workflow cross-compiles and publishes the binaries.

## Open items for project #2 (not resolved here)

- Per-user OTLP bearer tokens via the otel-collector-contrib
  `bearertokenauth` extension's `tokens`/`filename` list support (confirmed
  feasible), token generation/rotation/revocation UX.
- Exposing the otel-collector beyond 127.0.0.1 for multi-machine ingest.
- `docker-compose.yaml` gaining `profiles: [grafana]` on the grafana service
  so it can be skipped when Grafana is external.
- External Grafana datasource + dashboard auto-provisioning via its HTTP API
  (user supplies an API key).
