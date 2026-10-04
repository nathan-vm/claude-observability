# Wizard: also write telemetry env vars into Claude Code `settings.json`

Module touched: `src/wizard` only. Suggested PR title / squash subject:
`feat(wizard): write telemetry env into Claude Code settings.json` (releasable `feat`, so it auto-publishes on merge; do not hand-edit any version).

## Problem

The wizard currently enables Claude Code telemetry only by appending a marker block
(`# claude-observability: telemetry + collector config`) to one shell rc file
(`shellRCPath` in `src/wizard/cmd/wizard/main.go`) or, on Windows, via `setx`.
GUI launchers such as Maestro start `claude` without sourcing any rc file, so sessions
started that way never get `CLAUDE_CODE_ENABLE_TELEMETRY` / `OTEL_*` and send nothing.
Claude Code's own `settings.json` `"env"` object is applied to every session regardless of
launcher, so the telemetry vars must also be merged there, for every Claude config dir the
user monitors (`~/.claude`, `~/.claude-work`, `~/.claude-personal`, `$CLAUDE_CONFIG_DIR`).

## What exists today (grounding)

- `main.go run()`: `discovery.Find(home)` -> `wizard.ChooseAccounts` (`chosen`/`rejected`) ->
  endpoint + token prompts -> `health.Dial` -> builds `telemetryVars` (the 7 vars from the
  request, plus `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20<token>` when a token is
  entered) and `collectorVars` (`CLAUDE_DIR`, `CLAUDE_OBSERVABILITY_EXTRA_DIRS`,
  `EXPORTER_STREAM`) -> `writeShellConfig(out, home, allVars)` -> ignore list -> collector service.
- `envwriter.WriteBlock` is **write-once**: if `Marker` is already in the file it is a no-op,
  so a later run with a different endpoint/token never reaches the rc block (documented as an
  accepted gap in `docs/plans/2026-09-26-exporter-stream-reuse.md`).
- `discovery.Find` only returns `~/.claude` and `~/.claude-*` dirs that contain `projects/`;
  it does not look at `$CLAUDE_CONFIG_DIR`.
- `main_test.go` (package `main`) tests `shellRCPath` and the stream resolvers; `envwriter_test.go`
  tests are plain `testing` + `t.TempDir()`.
- README describes the rc behavior at lines ~114-118 (quick start), ~151-152 (Configuration),
  ~157-163 (Multiple accounts), ~199-200 (re-run).
- `docker-compose.yaml` reads `EXPORTER_STREAM` from the shell environment (i.e. the rc file) at
  compose-up time.

## Design

### 1. New package `internal/claudesettings`

`MergeEnv(path string, vars []envwriter.Var) (changed bool, err error)` plus a pure helper
`mergeEnv(data []byte, vars []envwriter.Var) (out []byte, changed bool, err error)` so the JSON
logic is unit-testable without touching disk. Behavior:

- **Read**: resolve `path` through `filepath.EvalSymlinks` when it exists (dotfile managers
  commonly symlink `settings.json`; renaming a temp file over the symlink would replace the
  link with a regular file). Write to the resolved target.
- **Missing file**: start from `{}`; new file mode `0600`. **Missing dir**: `os.MkdirAll(dir, 0o755)`
  (only reachable for the no-accounts `~/.claude` fallback). **Empty / whitespace-only file**:
  treat as `{}` (nothing to clobber).
- **Parse safely**: decode top-level as an object. On invalid JSON, a non-object top level, or a
  non-object `"env"` value, return an error and leave the file byte-for-byte untouched. No
  silent repair, no backup file (rejected: clutter; the abort-on-parse-error rule plus atomic
  write is the safety net).
- **Preserve everything else**: walk the top-level with `json.Decoder` tokens, keeping key order
  and each value as `json.RawMessage`; same for the `"env"` object. Our keys are replaced in
  place if present (value always set as a JSON string) and appended at the end of `env` if new;
  `env` itself is appended at the end of the top level if absent. Unrelated keys, unrelated env
  entries, and their order are untouched. Output re-indented with 2 spaces, trailing newline
  (Claude Code's own style). Rejected alternative: `map[string]any` round-trip - it sorts keys
  and mangles numbers, i.e. a noisy diff on a file the user may hand-maintain.
- **Idempotent**: managed keys are *overwritten* on every run (so an endpoint change on re-run
  propagates, unlike the rc marker block). If every managed key already holds exactly the
  requested string value, return `changed=false` and do not touch the file at all (no rewrite,
  no mtime bump, no reformatting of a hand-formatted file).
- **Atomic write**: `os.CreateTemp(dir, ".settings.json.*")` in the same directory, write,
  `Chmod` to the existing file's mode (or `0600` for a new file), `Sync`, `Close`, `os.Rename`;
  remove the temp file on any failure. `os.Rename` replaces atomically on Unix and Windows.
- Never deletes anything: if the user previously had `OTEL_EXPORTER_OTLP_HEADERS` from us and
  now enters no token, we leave it (we cannot tell ours from theirs).

### 2. Which vars go in settings.json

> **Implementation deviation (user decision):** `OTEL_EXPORTER_OTLP_HEADERS` is NOT written to
> `settings.json` - only the 7 telemetry vars are. The rc file / Windows `setx` behavior for
> the header is unchanged. The decision paragraph below and the "Open items" entry on this
> are superseded.

`telemetryVars` only (the 7 from the request, plus `OTEL_EXPORTER_OTLP_HEADERS` when a token
was entered - same slice, built once). `CLAUDE_DIR`, `CLAUDE_OBSERVABILITY_EXTRA_DIRS` and
`EXPORTER_STREAM` are collector/compose concerns and must not leak into Claude's env.

Decision to confirm (not blocking): the request lists 7 vars; I include the auth header when a
token is set, because the rc block already does and omitting it would make GUI-launched
sessions 401 against a gateway-protected endpoint (see
`docs/specs/2026-09-29-production-auth-gateway-design.md`). The token is stored in plaintext in
`settings.json`, same exposure class as the rc file today (that spec already lists plaintext
token storage as a known pre-existing issue); file mode `0600` is preserved/created.

### 3. Discovering the config dirs, and prompting

The target set is derived from the accounts the user already confirmed - no second discovery
pass with different rules:

1. Add `discovery.WithConfigDir(accounts []Account, dir string) []Account`: if `dir` (from
   `os.Getenv("CLAUDE_CONFIG_DIR")`) is non-empty, exists, contains `projects/`, and is not
   already in `accounts` (compare after `filepath.Clean` + `EvalSymlinks`), append it (with
   email read as in `Find`). Call it right after `discovery.Find` in `run()`. Effect: the env
   dir flows through `ChooseAccounts` like any other account, so what gets telemetry and what
   the collector monitors stay the same set. (Rejected: treating `$CLAUDE_CONFIG_DIR` as a
   settings-only target - it would emit telemetry for an account the collector never
   scans, producing a half-populated dashboard.)
2. Targets = `<chosen[i].Dir>/settings.json`, de-duplicated (`settingsTargets(chosen)` in
   `main.go`, pure, unit-tested). **Rejected accounts are not touched** - they are on the ignore
   list, writing telemetry config into them contradicts that. In the no-accounts fallback
   (`~/.claude` synthesized), `chosen` already contains it, so it is covered.
   Dirs that match `~/.claude*` but lack `projects/` are not targeted (same rule `Find`
   applies everywhere; they re-qualify after their first session when the wizard is re-run).
3. **Prompt: one yes/no, default Yes**, after the rc block is written and before the ignore
   list: list the exact `settings.json` paths that will be edited, then
   `wizard.AskYesNo(out, stdin, "  Also enable telemetry in these Claude Code settings files?", true)`.
   Rationale: this edits another tool's config files, which is more invasive than appending to
   the user's own rc file, so the paths are shown and confirmable - but one prompt, not
   one per dir, since per-account consent was already given in `ChooseAccounts`. EOF/non-
   interactive input returns the default (existing `AskYesNo` behavior), so scripted runs
   still work.

### 4. Failure policy

Per-dir failures (invalid JSON, permission denied) are **non-fatal**: print
`  skipped <path>: <reason> (file left untouched - fix it and re-run)` and continue with the
other dirs and the rest of the wizard (rc block already written, collector install still
runs). A concise summary line at the end lists skipped paths. Fatal would abort a half-done
setup after the endpoint was already verified and the rc already written, which is worse
than a clear warning.

### 5. Keep the rc block - and avoid divergence

Keep it, unchanged in content: `docker compose` reads `EXPORTER_STREAM` from it, terminal
users in unselected dirs and existing installs depend on it, and Windows `setx` is the
equivalent. Divergence risk is real because rc is write-once and settings.json is
overwrite-on-rerun. Mitigations (all small):

- **Single source in code**: `telemetryVars` is built once and passed to both writers; no second
  hand-typed list. A unit test asserts the settings output contains exactly the names/values
  of that slice (guards against drift when someone adds a var later).
- **Stale-rc warning**: before writing, call `envwriter.ExistingVar(rcPath, shell,
  "OTEL_EXPORTER_OTLP_ENDPOINT")` (Unix only; Windows `setx` always overwrites so it cannot be
  stale). If present and different from the endpoint just chosen, print
  `warning: <rc> still has OTEL_EXPORTER_OTLP_ENDPOINT=<old>; the rc block is not rewritten on
  re-run - edit it by hand. Claude Code settings.json now uses <new>.` Not auto-rewriting the
  rc block: changing `WriteBlock`'s marker semantics is a separate, already-deferred gap.
- **Precedence note for QA (open, non-blocking)**: I believe values in settings.json `env`
  override the inherited process environment for Claude Code sessions, which makes
  settings.json the winner when both exist. This is not verified from the repo - confirm
  manually in QA (below) and, if wrong, word the warning/README accordingly. Either way both
  sources agree on a fresh install.

Update the final message in `run()` ("Open a new terminal ...") to also say that Claude Code
sessions/launchers already running (e.g. Maestro) must be restarted to pick up settings.json.

## Files to touch

| File | Change |
|---|---|
| `src/wizard/internal/claudesettings/claudesettings.go` (new) | `MergeEnv`, `mergeEnv` as designed in section 1. Standard library + `envwriter.Var` only. |
| `src/wizard/internal/claudesettings/claudesettings_test.go` (new) | Table/unit tests below. |
| `src/wizard/internal/discovery/discovery.go` | Add `WithConfigDir`. |
| `src/wizard/internal/discovery/discovery_test.go` | Tests for `WithConfigDir`. |
| `src/wizard/cmd/wizard/main.go` | Call `WithConfigDir` after `Find`; add `settingsTargets(chosen)` and `writeClaudeSettings(out io.Writer, dirs []string, vars []envwriter.Var) (skipped []string)`; new section + prompt after `writeShellConfig`; stale-rc endpoint warning; updated "Done" text. `telemetryVars` is the single slice fed to both writers. |
| `src/wizard/cmd/wizard/main_test.go` | Tests for `settingsTargets`, `writeClaudeSettings`, stale-endpoint check. |
| `README.md` | See below. |
| `CHANGELOG.md` | **Do not edit** - generated by `release.yml`. |

No changes to `envwriter`, `service`, `limits`, `health`, or other modules. No ADR needed
(bounded change, no new dependency or cross-module contract). The older
`docs/specs/2026-09-23-local-setup-wizard-design.md` stays as a historical record.

## README updates

- Quick start step 3 (~lines 114-118): say the wizard turns on telemetry in your shell/OS
  **and** in each monitored account's Claude Code `settings.json`.
- Configuration (~151-152): note that the telemetry vars are written to both the rc file
  (Windows environment) and `<claude dir>/settings.json` `env`, while `CLAUDE_DIR`,
  `CLAUDE_OBSERVABILITY_EXTRA_DIRS` and `EXPORTER_STREAM` go to the rc file only.
- Multiple accounts (~157-163): `settings.json` is edited only for accounts you pick; the
  wizard also considers `$CLAUDE_CONFIG_DIR`; other keys in the file are preserved.
- Re-run note (~199-200): re-running updates the `env` entries in `settings.json` (e.g. new
  endpoint) but does not rewrite the existing rc block.
- Troubleshooting: new bullet - "Sessions launched from a GUI (e.g. Maestro) send nothing":
  telemetry now comes from `settings.json`; restart the launcher/session after running the
  wizard; if `settings.json` was reported as skipped (invalid JSON), fix it and re-run.

## Test plan (`src/wizard` only; plain `testing`, `t.TempDir()`, `Test<Func>_<Behavior>` names)

`claudesettings`:
- creates file (and missing parent dir) with `{"env": {...}}`, mode `0600`
- merges into existing file: unrelated top-level keys (including nested objects, numbers, arrays)
  preserved with original order; unrelated `env` entries preserved; managed keys added
- existing managed key with an old value (e.g. old endpoint) is overwritten in place
- second identical run: `changed == false`, file bytes and mtime unchanged
- existing mode (e.g. `0640`) preserved after write
- invalid JSON, top-level array/`null`, `"env": "x"` / `"env": []`: error returned, file bytes
  unchanged, no leftover temp files in the dir
- empty and whitespace-only file treated as `{}`
- symlinked `settings.json`: link still a symlink afterwards, target updated
- no `.settings.json.*` temp files left after success
- header var present only when passed in

`discovery`: `WithConfigDir` adds an out-of-home dir with `projects/`; dedupes the same dir
given via symlink / trailing slash; ignores empty string, nonexistent dir, and dir lacking
`projects/`.

`cmd/wizard`: `settingsTargets` (order, dedupe, built only from `chosen`);
`writeClaudeSettings` over two temp dirs where one has invalid JSON -> the good one is written,
the bad one is untouched and reported in `skipped`, no error aborts the loop; telemetry
slice parity test (settings `env` == `telemetryVars`, no collector vars); stale-endpoint helper
returns a warning only when rc has a different endpoint (Unix-only test, `t.Skip` on windows
like the existing resolver tests, `t.Setenv("SHELL", ...)`).

## Verification

1. `cd src/wizard && go build ./... && go vet ./... && go test ./...` (other two modules are
   untouched; do not rebuild them).
2. Manual QA with isolated state (never the real `~/.claude*`): create a temp `HOME` with
   `.claude/projects`, `.claude-work/projects`, plus a pre-existing `.claude/settings.json`
   containing unrelated keys and env entries and `chmod 600`; run the built wizard with
   `HOME=<tmp> SHELL=/bin/zsh CLAUDE_CONFIG_DIR=<tmp>/.claude-work`, from a worktree repo root
   against the **dev** stack endpoint (`docker-compose.dev.yaml` ports, see CLAUDE.md "Testing
   against Loki"; decline the collector-service install, since the collector is a host
   process and must not be pointed at the real stack). Check: `jq` diff shows only the 7
   env keys added; second run reports unchanged; changing the endpoint on a re-run updates
   settings.json and prints the stale-rc warning; a corrupt `settings.json` is skipped with
   the file intact; mode still `0600`.
3. Precedence check (resolves the open question): with the shell exporting a different
   `OTEL_EXPORTER_OTLP_ENDPOINT` than settings.json, start a real `claude` session and see
   which endpoint receives data (dev OTel collector logs). Record the result in the PR.
4. GUI-launcher check, if Maestro is available: launch a session from it and confirm
   telemetry reaches the dev stack without sourcing the rc file.

## Constraints from CLAUDE.md that apply

- Implement in an isolated worktree via `scripts/worktree-add.sh feat wizard-settings-env-block`;
  bring up only that worktree's dev stack (`docker-compose.dev.yaml --env-file
  docker-worktree.local`); never run anything that writes against the 47xxx stack, and never
  `down -v` the real compose file.
- Only the `src/wizard` module is rebuilt/retested; no new linter or third-party Go modules.
- PR title must be a Conventional Commit (`feat(wizard): ...`); do not touch `CHANGELOG.md` or any
  version.
- Subagents stay on Sonnet / effort high; escalation needs the user.

## Open items / risks

- Settings-vs-process-env precedence unverified (QA step 3); affects only warning wording.
- Header-in-settings.json decision (section 2) - default is include; say so if you'd rather omit.
- Concurrent writes by Claude Code itself to `settings.json` during the read-modify-write window
  are possible but the window is milliseconds; accepted.
- `settings.json` containing comments (JSONC) will be treated as invalid and skipped, not
  rewritten.
- Pre-existing, intentionally not fixed: write-once rc marker, plaintext token storage, shell
  switching writing a second rc block.
