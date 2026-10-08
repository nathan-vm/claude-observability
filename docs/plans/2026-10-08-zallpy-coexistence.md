# Coexist with a Zallpy fork installed side by side

Modules touched: `src/wizard` only (Go), plus `docker-compose.yaml` (comment only), `.env.example`, `.gitignore` (comment only), `README.md`.
Not touched: `src/collector`, `src/dash-generator`, `docker-compose.dev.yaml`, `docker-compose.server.yaml`.

Suggested PR title / squash subject: `feat(wizard): coexist with forked installs (namespaced launchd label, rc marker, end-of-file block)`.
It is a releasable `feat`, so it auto-publishes on merge. Never hand-edit a version (CLAUDE.md, trunk-based release rules).

## Problem

A Zallpy fork of this repo is installed on the same machine. It collides with this repo in three places.

| Collision | This repo today | Fork |
|---|---|---|
| launchd label | `com.claude-observability.collector` (`collectorLabel()`, `src/wizard/cmd/wizard/main.go:454`) | same label, so `bootstrap`/`bootout` of one clobbers the other |
| rc-file marker | `# claude-observability: telemetry + collector config` (`envwriter.Marker`) | same string. The block is write-once (`WriteBlock` no-ops when `Contains(Marker)`), so whichever wizard ran first owns the block and the other silently never writes its own |
| host port | otel-collector OTLP/HTTP published on `127.0.0.1:47318` | wants `127.0.0.1:47318` |

Goal: both installs run side by side, each with its own service, its own rc block, and its own ports. This wizard's values must win in new shells.

## What exists today (grounding) and what it means for the brief

I read the code first. Several items in the request do not match what is in the repo.

1. **There is no `--apply` or `--uninstall` flag.** `main.go` only handles `--version`. `run()` is a fully interactive stdin wizard (`wizard.AskLine` / `AskYesNo` read from a `bufio.Reader` on `os.Stdin`). Uninstall is documented in the README as manual commands (README line ~206), and no code removes anything. Decision: do not add flags in this change (scope).
   - "Re-run/--apply" means re-running the wizard. The operational step feeds answers on stdin (see the runbook).
   - "Uninstall never touches legacy" is implemented and tested at the library level as an exported `envwriter.RemoveBlock(path)`. It is the same primitive `WriteBlock` uses internally and the one a future `--uninstall` would call. It only ever matches the new marker. The README's manual uninstall text is updated to say the same.
   - Open question for the orchestrator: if you want a real `--uninstall` CLI flag, that is a separate, small follow-up. Nothing here blocks it.
2. **`docker-compose.yaml` already has `${OTEL_HTTP_PORT:-47318}`** (line 18), as does `docker-compose.dev.yaml` (`:-40318`). They were added for worktree isolation (`scripts/docker-worktree-env.sh`). So item 2 needs no compose logic change. What is missing is the local `.env`, its docs, and a comment.
3. **`.gitignore` already ignores `.env`** (line 46). `.env` is currently the production-secrets file used with `docker-compose.server.yaml` (template: `.env.example`). No `.env` exists on disk now. Only a comment edit is needed.
4. **Compose auto-loads `.env` for every `docker compose` call in that directory, including `-f docker-compose.dev.yaml`.** Setting `OTEL_HTTP_PORT=47328` in `.env` would therefore also move the dev stack's HTTP port off 40318 if someone runs the dev file bare in the main checkout. CLAUDE.md's documented invocation always passes `--env-file docker-worktree.local`. `--env-file` replaces the default `.env`, so the documented path is unaffected. I will leave `docker-compose.dev.yaml` alone and say so in `.env.example`. Worktree stacks are unaffected by the main checkout's `.env`.
5. **`ExistingVar` scans the whole file** once `Contains(Marker)` is true. With a coexisting Zallpy block earlier in the file it could return the Zallpy block's `EXPORTER_STREAM`. That would be a data-loss-adjacent bug (it orphans our Loki stream). It must be scoped to the managed block's own lines.
6. **`strings.Contains(Marker)` is the wrong match for a shared file.** Marker detection must be exact whole-line equality.
7. **`staleEndpointWarning` becomes false.** It tells the user "the rc block is not rewritten on re-run". Requirement 4 makes the block rewritten every run. README lines ~208-213 say the same thing and also need rewording.
8. **The macOS path lookup uses `os.UserHomeDir()`.** Fine, `$HOME` on darwin, so tests can use a temp HOME. The new migration code still takes explicit paths so tests do not depend on that.
9. **Out of scope, but it affects the goal.** Both wizards write the telemetry `env` block of `<claude dir>/settings.json` (`claudesettings.MergeEnv`), and last writer wins. If the Zallpy wizard also targets `~/.claude-personal/settings.json`, its endpoint or stream could override ours regardless of rc ordering. This plan does not change it. The runbook adds a check (`jq .env`) so the orchestrator sees it. Report back instead of fixing it silently.
10. **"`lsof` on 47318 is empty" assumes the Zallpy stack is not currently up.** Today only our own container should hold 47318. If the fork's stack is running, the correct check is that no container from project `claude-observability` publishes 47318. Both forms are in the runbook.

## Design

### 1. Namespaced service names plus ownership-checked migration

New names (`collectorLabel()`):

| OS | New | Legacy (what we migrate from) |
|---|---|---|
| macOS | `com.nathan-vm.claude-observability.collector` | `com.claude-observability.collector` |
| Linux | `nathan-vm-claude-observability-collector` | `claude-observability-collector` |
| Windows | `NathanVmClaudeObservabilityCollector` | `ClaudeObservabilityCollector` |

Implement as `collectorLabelFor(goos string)` and `legacyCollectorLabelFor(goos string)` with thin wrappers over `runtime.GOOS`, so a table test can cover all three OSes on any CI runner.

**Ownership test (the important part).** A legacy service definition is ours only if its executable resolves to a path this checkout owns. The brief suggests matching `.bin/claude-observability-collector`. A suffix match is not safe: the fork very likely builds into the same relative `.bin/claude-observability-collector` in its own checkout, so a suffix would claim the Zallpy install. Instead:

- `owned` = the exact absolute path being installed now (`collectorBin` from `findCollectorBinary`) plus `<repoRoot>/.bin/claude-observability-collector` (`.exe` on Windows).
- Compare with `filepath.Clean`, then `EvalSymlinks` best-effort on both sides (fall back to the cleaned path if resolution fails).
- Anything else, unparseable, binary-format plist, or missing: leave it alone. Never error the install over it.

Consequence for the runbook: the wizard must run from the main checkout (`findRepoRoot` uses the cwd) so that `repoRoot` equals the `WorkingDirectory`/path baked into the existing plist. Running from a worktree would (correctly, safely) skip migration.

**Order of operations** in `installCollectorService`: migrate legacy, then `service.Install` the new label.
- Alternative considered: install new first, remove legacy after. Rejected because the two collectors would briefly run against the same `.state/` offsets and dedup file (same working dir), which can race.
- Cost of migrate-first: if the new install fails, the legacy service is already stopped and removed. Re-running the wizard is idempotent and fixes it, and the error message already surfaces from `installCollectorService`.

**Migration implementation** (new untagged file `src/wizard/internal/service/legacy.go`, so its tests run on all three CI OSes):

```go
type MigrateOptions struct {
    LegacyLabel   string   // old service name on this OS
    OwnedCommands []string // absolute executable paths that count as "this checkout's collector"
}
type runner func(name string, args ...string) ([]byte, error) // injected; real = exec.Command(...).CombinedOutput

// pure, OS-independent:
func plistProgramArguments(data []byte) ([]string, error) // encoding/xml token walk: <key>ProgramArguments</key> then <array><string>...
func unitExecStart(data []byte) (string, bool)            // first token of the ExecStart= line
func taskOwned(xml []byte, owned []string) bool           // `schtasks /query /xml`: XML-unescape <Command>/<Arguments>, case-insensitive contains of "<owned>"
func pathsEqual(a, b string) bool

// injectable cores (paths/runner passed in, no os.UserHomeDir, no real launchctl):
func migrateLaunchAgent(plistPath, uid string, opts MigrateOptions, run runner) (migrated bool, err error)
func migrateSystemdUnit(unitPath string, opts MigrateOptions, run runner) (migrated bool, err error)
func migrateScheduledTask(opts MigrateOptions, run runner) (migrated bool, err error)
```

Per-OS exported entry points, one each in `service_darwin.go`, `service_linux.go`, `service_windows.go`: `func MigrateLegacy(opts MigrateOptions) (bool, error)`. They bind the real home path and `exec`, and call the injectable core.

- **darwin**: read `~/Library/LaunchAgents/<legacy>.plist`. If absent, return false. If `plistProgramArguments(...)[0]` is owned: run `launchctl bootout gui/<uid> <plist>` (error ignored, since it may not be loaded), then `os.Remove(plist)` (a failure is returned), return true. Otherwise leave it and return false.
- **linux**: read `~/.config/systemd/user/<legacy>.service`. If `ExecStart` is owned: `systemctl --user disable --now <legacy>.service`, remove the file, `systemctl --user daemon-reload`.
- **windows**: `schtasks /query /tn <legacy> /xml`. A non-zero exit means the task is absent, so return false. If owned: `schtasks /delete /tn <legacy> /f`.

**Linux/Windows decision: migrate both, best-effort.** Justification:
- Without migration, anyone upgrading on Linux or Windows ends up with two collectors (old name still enabled, new name added) pushing the same stream. That is a regression, not just a naming issue, and these platforms ship binaries (`release.yml`).
- The same ownership test makes the migration safe against a fork.
- It is a small shim over the pure helpers, which are tested on every OS.
- Limit: the real `systemctl`/`schtasks` calls cannot be exercised on this machine. They are compile-checked (`GOOS=linux`/`GOOS=windows go vet`), the CI matrix runs the pure-function tests on all three OSes, and any probe failure is non-fatal (it prints a note and continues to install).
- Rejected: darwin-only migration plus README manual-removal instructions. It leaves the duplicate-collector footgun for upgraders.

`main.go` prints one line per outcome, for example `migrated legacy service com.claude-observability.collector -> com.nathan-vm.claude-observability.collector` or `left legacy service <label> alone (it points at a different install)`.

### 2. Free host port 47318 (compose and docs only)

- `docker-compose.yaml` already honours `OTEL_HTTP_PORT`. Add only a comment above the `ports:` of `otel-collector`: both host ports are overridable via `OTEL_GRPC_PORT`/`OTEL_HTTP_PORT` (shell, `.env`, or `--env-file`), and why one might move 47318 (another install on the machine).
- `.env.example`: add a clearly separated, commented optional section (not set by default) with `# OTEL_HTTP_PORT=47318` and a note that plain `docker compose up -d` reads `.env` automatically, so the same file serves local port overrides and the server overlay. Add the dev-file caveat from finding 4. The default stays 47318 for everyone else.
- `.gitignore`: no functional change (`.env` is already ignored at line 46). Optionally widen the adjacent comment ("production secrets and local port overrides"). Comment only.
- The real `.env` (`OTEL_HTTP_PORT=47328`) is a local, gitignored file created by the orchestrator in the main checkout. It is not part of the PR (see runbook). Verify with `git check-ignore -v .env`.
- gRPC stays 47317, which is also the wizard's default OTel endpoint. Claude Code uses gRPC (`OTEL_EXPORTER_OTLP_PROTOCOL=grpc`), so nothing in the wizard needs to learn about the HTTP port.
- Rejected: changing the default to 47328 in the compose file. That would move every other user's port for a problem that exists on one machine.

### 3. New rc marker with one-time migration

`src/wizard/internal/envwriter/envwriter.go`:

```go
const Marker       = "# nathan-vm/claude-observability: telemetry + collector config"
const LegacyMarker = "# claude-observability: telemetry + collector config" // read/removed only during first migration
```

Neither string is a substring of the other. A test pins that, because the old `Contains` logic would have been unsafe if it were.

**Block model** (pure helpers on the file text, tested without disk):
- A marker line matches only if the whole line (trailing `\r` and trailing whitespace trimmed) equals the marker. This ignores a marker quoted in some other comment.
- A block = the marker line plus the contiguous lines after it that are managed assignments: `export NAME=...` or `set -gx NAME ...`. Both flavours are accepted regardless of which `Shell` was passed, because the parser works on whatever the file contains. The block ends at the first blank line, comment, or any other line, or at EOF.
- Line splitting keeps terminators (`\n`/`\r\n`), so CRLF rc files and a missing final newline survive byte-for-byte.
- Known limit, documented in a code comment: a hand-added `export X=...` directly adjacent to the block (no blank line or comment between) is treated as part of the block. `Render` output never has such a neighbour.
- Rejected: adding an end-marker line to new blocks. It would make the extent exact, but it changes the file format, legacy blocks do not have it, and the brief says nothing about it.

**API** (`WriteBlock` semantics change from write-once to "replace and append at end"):

```go
type WriteResult struct {
    Changed        bool // file content actually changed
    MigratedLegacy bool // a legacy block was consumed this call
}
func WriteBlock(path string, shell Shell, vars []Var) (WriteResult, error)
func RemoveBlock(path string) (removed bool, err error) // new marker only, never legacy
func ExistingVar(path string, shell Shell, name string) (value string, ok bool, err error) // see below
```

`WriteBlock` algorithm:
1. Read the file. A missing file is empty. `MkdirAll` the parent directory.
2. Remove **every** new-`Marker` block (a stray duplicate collapses into one).
3. If **no** new-marker block existed before step 2 **and** a legacy block exists, remove the **first** legacy block and set `MigratedLegacy`. Migration happens only when no new-marker block existed. If one did, any legacy block is the Zallpy install's and is never touched, so a coexisting Zallpy block is not re-migrated.
   - If there are several legacy blocks, only the first is consumed. This cannot happen from this repo's own write-once history, so it is an arbitrary but deterministic tie-break.
4. Append `Render(shell, vars)` at the end. If the remaining text is non-empty and lacks a trailing newline, prepend `\n`, as today.
5. If the resulting bytes equal the original, do not write (`Changed=false`, mtime untouched).
6. Otherwise write atomically: resolve symlinks with `filepath.EvalSymlinks` (dotfile managers symlink `config.fish`/`.zshrc`), write a temp file in the target's directory, copy the existing mode (new file: 0600, as the existing tests require), and rename over the resolved target. The current implementation appends with `O_APPEND`, which cannot corrupt the file. A rewrite can, so atomicity is now required. This is the same approach `claudesettings.MergeEnv` uses.

`vars` passed by the caller are authoritative. Only `EXPORTER_STREAM` has to be carried across the migration, because it cannot be re-derived (a fresh one orphans the Loki stream). Every other managed var is recomputed by the wizard from discovery and prompts on each run. The "other managed vars" in the brief are therefore handled by the caller re-supplying them. Reading them back is available through `ExistingVar`'s legacy fallback if a future caller needs it.

`ExistingVar` changes:
- Search only inside the block's own lines (finding 5).
- If a new-marker block exists, read from it only. If it lacks `name`, return `ok=false`; do **not** fall back to legacy.
- If no new-marker block exists but a legacy block does, read from the first legacy block (this is how the first post-upgrade run learns the existing `EXPORTER_STREAM` before `WriteBlock` migrates it).
- This stays consistent with `WriteBlock`'s "migrate only when no new block exists" rule.

**Accepted risk, stated explicitly.** Migration assumes a lone legacy block is ours, which is true for every pre-change release because the write was write-once. If someone has only a Zallpy legacy block and runs this wizard for the first time, we would adopt its `EXPORTER_STREAM` and remove its block. Mitigation: `main.go` prints `migrating legacy block from <path> (EXPORTER_STREAM <value>)`, and the Zallpy wizard re-appends its block when it next runs. We cannot do better from the file alone.

**`main.go` call sites:**
- `writeShellConfig` uses `WriteResult` and prints one of `added to`, `rewritten in (moved to end of file)`, `migrated legacy block in`, or `already up to date in`. The old "edit the block by hand to change accounts" line goes away.
- `staleEndpointWarning` becomes `rcRewriteWarnings(home, vars)`, computed *before* writing:
  - an info line when `OTEL_EXPORTER_OTLP_ENDPOINT` differs from the existing block's value;
  - a warning when the existing block has `OTEL_EXPORTER_OTLP_HEADERS` and the new var list does not. Previously a re-run left the block alone. Now a blank token prompt would silently drop the auth header, so we surface it. Values go through `displayForLog` and are never printed.
- `resolveExporterStream` is unchanged. It already calls `ExistingVar`, which now falls back to the legacy block. Update its doc comment to mention the fallback.
- Windows (`setx`) is untouched: no marker, no block.

### 4. Managed block always at the end of the rc file

Falls out of `WriteBlock` steps 2-4: remove, then append, every run. Everything outside the removed block lines is byte-identical (a test asserts this, with content before, between and after).

- bash, zsh, fish: later assignments win in all three, so end-of-file wins over a Zallpy block that sits earlier. The flavours the wizard supports are the three that `shellRCPath` returns: fish → `~/.config/fish/config.fish`, zsh → `~/.zshrc`, anything else → `~/.bashrc`.
- One normalisation: if the block was the only thing after a non-newline-terminated line, the separator `\n` we had inserted is not restored. This is equivalent to what we would append anyway, and a test pins the result.
- Pre-existing and out of scope: bash on macOS login shells reads `.bash_profile`, not `.bashrc`.
- Known limit: if the Zallpy wizard is re-run later and re-appends its block after ours, its values win again until we re-run. Documented in the README note, not code-fixable from this side.

## Files to touch

| File | Change |
|---|---|
| `src/wizard/internal/envwriter/envwriter.go` | New `Marker`, `LegacyMarker`; block parser helpers; `WriteBlock` → remove + append-at-end + legacy migration + atomic write, returns `WriteResult`; new `RemoveBlock`; `ExistingVar` scoped to the block with legacy fallback; update doc comments. |
| `src/wizard/internal/envwriter/envwriter_test.go` | Update the tests broken by the semantic change (`IdempotentOnSecondRun` becomes "same vars → unchanged, different vars → replaced and moved to the end"). Add the cases listed under Tests. |
| `src/wizard/internal/service/legacy.go` (new, no build tag) | `MigrateOptions`, `runner`, pure parsers, `pathsEqual`, the three injectable `migrate*` cores. |
| `src/wizard/internal/service/legacy_test.go` (new, no build tag) | Hermetic tests of the above (temp dirs, fake runner). |
| `src/wizard/internal/service/service_darwin.go`, `service_linux.go`, `service_windows.go` | Add `MigrateLegacy(opts) (bool, error)` wiring real paths and `exec`. `Install` is unchanged. |
| `src/wizard/internal/service/service_darwin_test.go` | Add one round-trip test: `plistProgramArguments(GeneratePlist(cfg))[0] == cfg.Command`. Existing fixture labels (`com.claude-observability.dash-generator`) are just test strings, so leave them. |
| `src/wizard/cmd/wizard/main.go` | `collectorLabelFor`/`legacyCollectorLabelFor` plus wrappers; `installCollectorService(out, repoRoot, vars)` migrates, then installs; `writeShellConfig` uses `WriteResult`; `staleEndpointWarning` → `rcRewriteWarnings`; doc comments. |
| `src/wizard/cmd/wizard/main_test.go` | Label table test; `resolveExporterStream` reuses the stream from a legacy-only block; migration then second run (idempotent, same stream); rewrite of `TestStaleEndpointWarning` → `rcRewriteWarnings` (endpoint change, dropped headers, no secrets printed). Existing resolve tests call `envwriter.WriteBlock`, so update them to the new return type. |
| `docker-compose.yaml` | Comment only (ports overridable via `OTEL_*_PORT`, why 47318 might move). No functional change. |
| `.env.example` | Commented optional `OTEL_HTTP_PORT` section plus the dev-file caveat. |
| `.gitignore` | Comment only. |
| `README.md` | See below. |

README edits (find by text, line numbers will drift):
- "Services" table: footnote that the HTTP port is `OTEL_HTTP_PORT`-overridable.
- "Configuration": the sentence "There's no `.env` file" is no longer true. Rewrite it: a `.env` is optional, compose reads it automatically, and it is also the server overlay's secrets file. Add `OTEL_GRPC_PORT`, `OTEL_HTTP_PORT` and friends (`GRAFANA_PORT`, `LOKI_PORT`) to the variables table as compose-only knobs.
- "Managing the stack" table: rename the Linux unit and Windows task in status/stop/uninstall (macOS `launchctl list | grep claude-observability` still matches the new label). Uninstall guidance for the rc file says to delete only the block under the `# nathan-vm/claude-observability: …` marker.
- Replace the paragraph "Re-running … does not rewrite an existing rc block; the wizard warns…" with the new behaviour: the block is replaced and moved to the end on every run, and values you hand-edited in it are overwritten.
- New short subsection "Running alongside another install (e.g. a fork)": namespaced service names, rc marker, one-time automatic migration of the legacy label and block (only if it is demonstrably this checkout's), the `OTEL_HTTP_PORT` override, and the caveat that the later-run wizard's block wins, with `settings.json` `env` shared.
- Do not edit historical `docs/plans/*` / `docs/specs/*` that mention the old names. They are records.

## Tests (all hermetic: `t.TempDir()`, no real `launchctl`/`systemctl`/`schtasks`, no real HOME)

`envwriter_test.go` (must pass on macOS, Linux and Windows CI; skip the permission and symlink cases on Windows as the existing ones do):
1. `Marker` equals the exact new string, and neither marker is a substring of the other. `Render` (bash and fish) starts with the new marker.
2. Fresh file is created with mode 0600. Existing content is preserved as a prefix. Existing mode (0644) is preserved on rewrite (keep the two existing mode tests, now through the new write path).
3. Re-run with the same vars: `Changed=false`, bytes and mtime identical.
4. Re-run with different vars with the block in the **middle** of the file: new block is at the end, and the text before and after the old block is byte-identical (compare against an expected string).
5. A file without a trailing newline, and a CRLF file: output is correct and the rest is byte-identical.
6. Migration (bash and fish): only a legacy block with `EXPORTER_STREAM` → new block at the end, legacy block gone, rest byte-identical, `MigratedLegacy=true`. Second call → `MigratedLegacy=false`, `Changed=false`.
7. **Coexistence**: Zallpy legacy block present **and** our new block → no migration (`MigratedLegacy=false`). The Zallpy block bytes are untouched. Our block is moved to the end, after the Zallpy block. Cover Zallpy-before-ours and Zallpy-after-ours.
8. **No legacy touch on removal**: `RemoveBlock` with both blocks removes only ours and leaves the Zallpy block byte-identical. `RemoveBlock` with only a legacy block returns `removed=false` and the file is byte-identical. `RemoveBlock` on a missing file is a no-op without error.
9. `ExistingVar`: reads the new block. Falls back to legacy only when no new block exists. Does **not** fall back when a new block exists without the var. Ignores a same-named var **outside** the block (a Zallpy `EXPORTER_STREAM` placed before ours). Works for bash and fish.
10. Marker matching is whole-line: a comment that merely quotes the marker is not a block.
11. Block extent: ends at a blank line, a comment, or an unrelated line. Two new-marker blocks collapse to one.
12. Symlinked rc (skip on Windows): content is written through, the link survives, and the target mode is preserved.

`service/legacy_test.go` (untagged, so it runs on all three OSes):
- `plistProgramArguments`: parses a literal fixture in the exact shape `GeneratePlist` emits. Garbage and binary input return an error.
- `migrateLaunchAgent` with temp plist path, fake `runner` recording calls:
  - absent plist → `(false, nil)`, zero runner calls;
  - owned via `<repoRoot>/.bin/claude-observability-collector` → one `bootout`, file removed, `true`;
  - owned via the explicit install `Command`;
  - **same binary name in a different checkout** (`/Users/other/zallpy/claude-observability/.bin/claude-observability-collector`) → untouched, zero runner calls, `false`. This is the Zallpy case;
  - unparseable plist → untouched, `(false, nil)`;
  - `bootout` returns an error (not loaded) → still removes the file;
  - `os.Remove` failure (read-only dir) → returned error, skip on Windows.
- `unitExecStart` and `migrateSystemdUnit`: owned / not owned / absent, fake runner call sequence (`disable --now`, `daemon-reload`).
- `taskOwned` and `migrateScheduledTask`: owned (case-insensitive path), different path, query fails (task absent).
- `pathsEqual`: cleaning, trailing separators, symlinked temp dir.

`cmd/wizard/main_test.go`:
- `collectorLabelFor` / `legacyCollectorLabelFor` table for darwin, linux, windows. New ≠ legacy. Darwin equals `com.nathan-vm.claude-observability.collector`.
- `resolveExporterStream` reuses the stream from a legacy-only rc block (temp HOME, `SHELL=/bin/zsh`) and from a new block. A Zallpy-only-after-migration block is ignored.
- End-to-end on the rc layer: legacy rc → `resolveExporterStream` → `WriteBlock` → the new block carries the same `EXPORTER_STREAM`.
- `rcRewriteWarnings`: endpoint change, dropped headers, no secrets printed.

## Verification

Per touched module (only `src/wizard`; collector and dash-generator are untouched, so do not rebuild or retest them):

```sh
cd src/wizard
go build ./...
go vet ./...
go test ./...
GOOS=linux   go vet ./... && GOOS=linux   go build ./...
GOOS=windows go vet ./... && GOOS=windows go build ./...
GOOS=darwin  go vet ./... && GOOS=darwin  go build ./...   # the per-OS service files are build-tagged
```

The CI matrix (`.github/workflows/test.yml`) runs `go test ./...` for the wizard on macOS, Linux and Windows. That is why the migration logic and its tests are in untagged files.

Compose and docs (from the repo root, no containers touched):

```sh
docker compose --env-file /dev/null config | grep -n '4318'   # still 47318 by default
OTEL_HTTP_PORT=47328 docker compose config | grep -n '4318'   # 47328
docker compose -f docker-compose.dev.yaml --env-file /dev/null config | grep -n '4318'   # still 40318
git check-ignore -v .env                                       # matched by .gitignore
```

No QA agent run against the real stack is needed for the code change itself. The live acceptance checks are in the runbook, performed by the orchestrator after merge.

## Operational runbook (orchestrator, after the code is merged; not for the developer)

All commands run from the **main checkout**, `/Users/nathan/Developer/github/nathan-vm/claude-observability`, not a worktree. The wizard's `findRepoRoot` and the plist ownership test both depend on that. `.env` is gitignored, so it only exists where you create it. CLAUDE.md applies: the real stack (`docker-compose.yaml`, 47xxx) is read-mostly. **Never** `docker compose down -v` there.

**0. Pre-flight (read only)**
```sh
ls -d ~/.claude*/projects          # expect exactly ~/.claude-personal/projects and ~/.claude-work/projects
launchctl list | grep claude-observability
plutil -p ~/Library/LaunchAgents/com.claude-observability.collector.plist | grep -A2 -E 'ProgramArguments|WorkingDirectory'
grep -n "claude-observability" ~/.config/fish/config.fish ~/.zshrc
grep -n "EXPORTER_STREAM\|OTEL_EXPORTER_OTLP_HEADERS\|OTEL_EXPORTER_OTLP_ENDPOINT" ~/.config/fish/config.fish ~/.zshrc
docker ps --format '{{.Names}} {{.Ports}}' | grep 4731
```
Confirm before proceeding:
- The plist's program path is `/Users/nathan/Developer/github/nathan-vm/claude-observability/.bin/claude-observability-collector`.
- `EXPORTER_STREAM` is `claude-code-exporter-4e6cf1b8-2b79-4cff-bb03-b6fb5a1898bc` in every rc file you will touch.
- Whether any rc block has `OTEL_EXPORTER_OTLP_HEADERS`. A blank token prompt will now **remove** it. For a local stack it should be absent.

If there is an unexpected third account dir or a differing stream, stop and report instead of improvising: the stdin script below is positional.

Back up what will be rewritten:
```sh
ts=$(date +%Y%m%d-%H%M%S)
cp ~/.config/fish/config.fish ~/.config/fish/config.fish.bak-$ts
cp ~/.zshrc ~/.zshrc.bak-$ts
cp ~/Library/LaunchAgents/com.claude-observability.collector.plist /tmp/legacy-collector.plist.bak-$ts
```

**1. Update and rebuild binaries into `.bin/`**
```sh
git pull --ff-only origin main
(cd src/wizard    && go build -o ../../.bin/claude-observability-wizard    ./cmd/wizard)
(cd src/collector && go build -o ../../.bin/claude-observability-collector ./cmd/collector)
```
The wizard finds the collector next to its own executable (`findCollectorBinary`), so both must be in `.bin/`.

**2. Free port 47318 on the real stack**
```sh
printf 'OTEL_HTTP_PORT=47328\n' > .env        # gitignored; do not commit
git check-ignore -v .env                       # must print the .gitignore rule
docker compose up -d --no-deps otel-collector  # recreates only that container; Loki/Grafana/dash-generator untouched
docker compose port otel-collector 4318        # expect 127.0.0.1:47328
docker compose port otel-collector 4317        # expect 127.0.0.1:47317 (unchanged)
```
This causes a short OTLP gap while the container is recreated, and the exporters retry. `.env` also affects bare `docker compose -f docker-compose.dev.yaml` calls in this checkout (see finding 4). Use `--env-file docker-worktree.local` for dev, as CLAUDE.md already says.

**3. Re-run the wizard.** There is no `--apply` flag. It is interactive, so feed answers on stdin. Prompt order with exactly two accounts: monitor-all, OTel endpoint, OTel ingest email, OTel ingest token, Loki endpoint, Loki email, Loki token, settings.json confirm, install collector. Blank lines accept the defaults:
- endpoint `http://localhost:47317`,
- Loki `http://localhost:47100`,
- email defaults to the first account's email, but with a blank token no auth header is written.

`CLAUDE_DIR=/Users/nathan/.claude-personal` and `CLAUDE_OBSERVABILITY_EXTRA_DIRS=/Users/nathan/.claude-work` come from discovery order (`.claude-personal` sorts before `.claude-work`), not from answers. `EXPORTER_STREAM` comes from the existing block via the legacy fallback. The rc file is picked by `$SHELL` (the Claude Code environment gives zsh, not fish), so set it explicitly per pass. Do the fish pass first, with settings and service:

```sh
printf '%s\n' y '' '' '' '' '' '' y y | SHELL=/opt/homebrew/bin/fish ./.bin/claude-observability-wizard
```

Then the zsh pass, **only if** `~/.zshrc` has the legacy block with the same stream (otherwise the wizard would mint a new stream). Say `n` to settings and service, since pass 1 already did both:

```sh
printf '%s\n' y '' '' '' '' '' '' n n | SHELL=/bin/zsh ./.bin/claude-observability-wizard
```

Expected wizard output: `migrating legacy block from <rc path>` per rc file, `migrated legacy service com.claude-observability.collector -> com.nathan-vm.claude-observability.collector`, and `installed and started`. If it prints `left legacy service ... alone`, stop. The ownership test did not match, usually because it was run from the wrong directory.

**4. Acceptance checks**
```sh
launchctl list | grep claude-observability
# expect: com.nathan-vm.claude-observability.collector with a PID, and NO com.claude-observability.collector
# (if a Zallpy install is loaded under the legacy label, that line may legitimately remain; confirm its plist points outside this checkout)
ls ~/Library/LaunchAgents | grep claude-observability     # only the new .plist (plus the Zallpy one if present)

lsof -iTCP:47318 -sTCP:LISTEN                              # expect empty
docker ps --format '{{.Names}} {{.Ports}}' | grep 47318    # expect empty for project claude-observability

grep -n "claude-observability" ~/.config/fish/config.fish ~/.zshrc
# expect: only the "# nathan-vm/claude-observability: telemetry + collector config" marker, at the end of each file,
# and EXPORTER_STREAM=claude-code-exporter-4e6cf1b8-2b79-4cff-bb03-b6fb5a1898bc in both
diff <(sed 's/^[[:space:]]*//' ~/.config/fish/config.fish.bak-$ts) ~/.config/fish/config.fish   # only the block moved/renamed
for d in ~/.claude-personal ~/.claude-work; do jq .env $d/settings.json; done
# expect OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:47317 and no foreign stream or endpoint (finding 9)
tail -n 20 .state/claude-observability-collector.log       # collector running and pushing to Loki on 47100

cd src/wizard && go vet ./... && go test ./...
```

Re-run step 3 once more and confirm it is a no-op: block already last, `diff` against the post-run file shows no change, and the same `EXPORTER_STREAM`.

**Rollback:** restore the `.bak-$ts` rc files, `launchctl bootout gui/$UID ~/Library/LaunchAgents/com.nathan-vm.claude-observability.collector.plist && rm` it, copy `/tmp/legacy-collector.plist.bak-$ts` back to `~/Library/LaunchAgents/` and `launchctl bootstrap gui/$UID` it, and remove `.env` plus `docker compose up -d --no-deps otel-collector` to put 47318 back.

## Conventions and constraints (CLAUDE.md)

- Do the work in an isolated worktree (`scripts/worktree-add.sh feat zallpy-coexistence`), docker-isolated by its generated `docker-worktree.local`. Per the global rules, the orchestrator asks the user whether to use a worktree before creating one. Only the operational runbook touches the main checkout and the real 47xxx stack.
- Squash-merge title must be a Conventional Commit (`pr-title.yml`). Version, `CHANGELOG.md` and tags are derived by `release.yml`; do not edit them.
- `go vet` plus `go test` is the bar, with no new linter. Match the existing style, which is plain `testing` with `t.TempDir()`.
- Wizard only: do not rebuild or retest `src/collector` or `src/dash-generator`.
- The collector stays a host process. Nothing here moves it into compose.
- The collector's own `launchd` log and state paths (`<repo>/.state/…`) are per-checkout, so they do not collide with the fork and are unchanged.

## Risks and open questions

- **Behaviour change for all users:** the rc block is now rewritten on every run (previously write-once), so hand-edits inside the block are overwritten, and a blank token prompt drops `OTEL_EXPORTER_OTLP_HEADERS`. This is surfaced by `rcRewriteWarnings` and the README, but it is deliberate and follows from requirement 4.
- **First-run adoption assumption** for the legacy rc block (see "Accepted risk" in section 3).
- **`settings.json` `env` is shared with the fork** and is not addressed here (finding 9).
- **Linux/Windows service migration is not exercisable on this machine.** It relies on compile checks, CI on the three-OS matrix, and pure-function tests.
- **No `--uninstall` flag exists** (finding 1). If you want one, say so and it becomes a follow-up plan built on `envwriter.RemoveBlock` and the three `migrate*` cores.
- No blocking ambiguities. Everything above was decided from the code as it stands.
