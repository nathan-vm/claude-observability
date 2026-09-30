# EXPORTER_STREAM Reuse Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the wizard from re-minting `EXPORTER_STREAM` on every re-run and baking a fresh value into the collector's installed service definition (launchd plist / systemd unit / Windows scheduled task) — it must reuse an already-configured install's existing stream id, and mint a fresh one only for a genuinely new install.

**Architecture:** Add `envwriter.ExistingVar` to parse an already-written value out of a shell rc file's marker block. In `cmd/wizard/main.go`, extract the existing inline shell-detection switch into a shared `shellRCPath` helper, add a pure `resolveExporterStreamValue` (reuse-or-mint decision), and a `resolveExporterStream` wrapper that sources the "existing" value from the shell rc file on Unix (the same file `writeShellConfig` is about to write, and the same file `docker compose up` reads its shell environment from) or from the process environment on Windows (no persisted marker file exists there — `setx` always overwrites). Wire the result into `collectorVars` in place of the current unconditional `streamid.New()` call, and correct the now-stale explanatory comment.

**Tech Stack:** Go 1.22, standard library only (`regexp`, `runtime`, `os`), existing `testing` package style (no testify/mocking).

**Spec:** No separate spec file — bounded-path fix, design approved in-chat during planning (see plan generation session notes). Grounding source: `src/wizard/cmd/wizard/main.go`, `src/wizard/internal/envwriter/envwriter.go`, `src/wizard/internal/streamid/streamid.go`, `src/wizard/internal/service/service_darwin.go` / `service_linux.go` / `service_windows.go`.

## Global Constraints

- Go module is `src/wizard` (own `go.mod`, module `claude-observability-wizard`, `go 1.22`). Only this module needs rebuilding/retesting — do not touch `src/collector` or `src/dash-generator`.
- Verification bar per task: `cd src/wizard && go build ./... && go vet ./... && go test ./...`. No new linter.
- No third-party Go modules — standard library only, matching the rest of the wizard module.
- Do not fix the pre-existing, separate gap where `envwriter.WriteBlock`'s marker-based no-op means an account added in a later wizard run never gets its `CLAUDE_DIR`/`EXPORTER_STREAM` written into an *already-marker-having* shell rc file at all — only the EXPORTER_STREAM reuse-vs-mint decision is in scope.
- Do not touch the wizard's interactive prompt flow, release/version plumbing, or the live-machine upgrade itself.
- Do not add a linter or third-party test library — match `envwriter_test.go` / `streamid_test.go` / `service_windows_test.go`'s existing style: plain `testing`, `Test<Func>_<Behavior>` names, `t.TempDir()`, `t.Fatal`/`t.Errorf`.

---

## File Structure

```
src/wizard/
  internal/envwriter/
    envwriter.go        (add ExistingVar; add "regexp" import)
    envwriter_test.go    (add ExistingVar tests)
  cmd/wizard/
    main.go              (add shellRCPath, resolveExporterStreamValue, resolveExporterStream;
                           rewire writeShellConfig and run(); rewrite the stale comment)
    main_test.go         (NEW — first test file in this package)
```

---

### Task 1: `envwriter.ExistingVar` — read an already-written var out of a marker block

**Files:**
- Modify: `src/wizard/internal/envwriter/envwriter.go`
- Test: `src/wizard/internal/envwriter/envwriter_test.go`

**Interfaces:**
- Consumes: `Marker` (existing const), `Shell`/`Bash`/`Fish` (existing), `WriteBlock` (existing, used only by tests to set up fixtures).
- Produces: `envwriter.ExistingVar(path string, shell Shell, name string) (value string, ok bool, err error)` — read by Task 3's `resolveExporterStream`.

- [ ] **Step 1: Write the failing tests**

Append to `src/wizard/internal/envwriter/envwriter_test.go`:

```go
func TestExistingVar_ReturnsValueWhenPresent_Bash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rc")
	if _, err := WriteBlock(path, Bash, []Var{{"EXPORTER_STREAM", "claude-code-exporter-abc123"}}); err != nil {
		t.Fatal(err)
	}

	value, ok, err := ExistingVar(path, Bash, "EXPORTER_STREAM")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != "claude-code-exporter-abc123" {
		t.Errorf("ExistingVar() = (%q, %v), want (\"claude-code-exporter-abc123\", true)", value, ok)
	}
}

func TestExistingVar_ReturnsValueWhenPresent_Fish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.fish")
	if _, err := WriteBlock(path, Fish, []Var{{"EXPORTER_STREAM", "claude-code-exporter-abc123"}}); err != nil {
		t.Fatal(err)
	}

	value, ok, err := ExistingVar(path, Fish, "EXPORTER_STREAM")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != "claude-code-exporter-abc123" {
		t.Errorf("ExistingVar() = (%q, %v), want (\"claude-code-exporter-abc123\", true)", value, ok)
	}
}

func TestExistingVar_FalseWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist")

	_, ok, err := ExistingVar(path, Bash, "EXPORTER_STREAM")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("ok = true, want false for a missing file")
	}
}

func TestExistingVar_FalseWhenMarkerMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rc")
	if err := os.WriteFile(path, []byte("export SOMETHING_ELSE=\"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, ok, err := ExistingVar(path, Bash, "EXPORTER_STREAM")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("ok = true, want false when the file has no Marker block at all")
	}
}

func TestExistingVar_FalseWhenBlockExistsButVarMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rc")
	// Simulates an accounts-less first run: the Marker block exists (from
	// telemetry vars) but EXPORTER_STREAM was never written.
	if _, err := WriteBlock(path, Bash, []Var{{"CLAUDE_CODE_ENABLE_TELEMETRY", "1"}}); err != nil {
		t.Fatal(err)
	}

	_, ok, err := ExistingVar(path, Bash, "EXPORTER_STREAM")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("ok = true, want false when the Marker block exists but never assigned this var")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd src/wizard && go test ./internal/envwriter/... -v`
Expected: FAIL — `ExistingVar` undefined.

- [ ] **Step 3: Implement `ExistingVar`**

In `src/wizard/internal/envwriter/envwriter.go`, add `"regexp"` to the import block (alongside `"fmt"`, `"os"`, `"path/filepath"`, `"strings"`), then append:

```go
// ExistingVar reports the value already assigned to name inside path's
// Marker block, if any. ok is false — with err nil — when: path doesn't
// exist yet (nothing configured); path exists but has no Marker block yet
// (nothing configured); or the Marker block exists but never assigned name
// (e.g. an accounts-less first run wrote the telemetry vars but had no
// collector vars to write yet). Any other read error is returned as err.
func ExistingVar(path string, shell Shell, name string) (value string, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if !strings.Contains(string(data), Marker) {
		return "", false, nil
	}

	var pattern *regexp.Regexp
	switch shell {
	case Fish:
		pattern = regexp.MustCompile(`(?m)^set -gx ` + regexp.QuoteMeta(name) + ` (.+)$`)
	default:
		pattern = regexp.MustCompile(`(?m)^export ` + regexp.QuoteMeta(name) + `="(.*)"$`)
	}
	m := pattern.FindStringSubmatch(string(data))
	if m == nil {
		return "", false, nil
	}
	return m[1], true, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd src/wizard && go test ./internal/envwriter/... -v`
Expected: PASS (12 tests: 7 existing + 5 new).

- [ ] **Step 5: Build and vet the whole module**

Run: `cd src/wizard && go build ./... && go vet ./...`
Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add src/wizard/internal/envwriter/envwriter.go src/wizard/internal/envwriter/envwriter_test.go
git commit -m "$(cat <<'EOF'
feat: add envwriter.ExistingVar to read an already-written var from a marker block

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: `main.go` — extract `shellRCPath`, add `resolveExporterStreamValue`

**Files:**
- Modify: `src/wizard/cmd/wizard/main.go`
- Test: `src/wizard/cmd/wizard/main_test.go` (NEW)

**Interfaces:**
- Consumes: `envwriter.Shell`/`Bash`/`Fish` (existing), `streamid.New() (string, error)` (existing).
- Produces: `shellRCPath(home string) (path string, shell envwriter.Shell)` and `resolveExporterStreamValue(existing string) (string, error)` — both consumed by Task 3's `resolveExporterStream` and by `writeShellConfig`.

- [ ] **Step 1: Write the failing tests**

Create `src/wizard/cmd/wizard/main_test.go`:

```go
package main

import (
	"path/filepath"
	"regexp"
	"testing"

	"claude-observability-wizard/internal/envwriter"
)

func TestShellRCPath_Fish(t *testing.T) {
	t.Setenv("SHELL", "/usr/local/bin/fish")
	home := t.TempDir()
	path, shell := shellRCPath(home)
	want := filepath.Join(home, ".config", "fish", "config.fish")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if shell != envwriter.Fish {
		t.Errorf("shell = %v, want Fish", shell)
	}
}

func TestShellRCPath_Zsh(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	home := t.TempDir()
	path, shell := shellRCPath(home)
	want := filepath.Join(home, ".zshrc")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if shell != envwriter.Bash {
		t.Errorf("shell = %v, want Bash", shell)
	}
}

func TestShellRCPath_DefaultBash(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	home := t.TempDir()
	path, shell := shellRCPath(home)
	want := filepath.Join(home, ".bashrc")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if shell != envwriter.Bash {
		t.Errorf("shell = %v, want Bash", shell)
	}
}

// streamValuePattern matches a full EXPORTER_STREAM value freshly minted by
// resolveExporterStreamValue: "claude-code-exporter-" + a UUIDv4.
var streamValuePattern = regexp.MustCompile(`^claude-code-exporter-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestResolveExporterStreamValue_ReusesNonEmpty(t *testing.T) {
	got, err := resolveExporterStreamValue("claude-code-exporter-existing-id")
	if err != nil {
		t.Fatal(err)
	}
	if got != "claude-code-exporter-existing-id" {
		t.Errorf("got %q, want existing value reused unchanged", got)
	}
}

func TestResolveExporterStreamValue_MintsWhenEmpty(t *testing.T) {
	got, err := resolveExporterStreamValue("")
	if err != nil {
		t.Fatal(err)
	}
	if !streamValuePattern.MatchString(got) {
		t.Errorf("got %q, want a freshly-minted claude-code-exporter-<uuid> value", got)
	}
}

func TestResolveExporterStreamValue_MintsDifferentIDsEachTime(t *testing.T) {
	a, err := resolveExporterStreamValue("")
	if err != nil {
		t.Fatal(err)
	}
	b, err := resolveExporterStreamValue("")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("resolveExporterStreamValue(\"\") returned the same value twice: %q", a)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd src/wizard && go test ./cmd/wizard/... -v`
Expected: FAIL — `shellRCPath` and `resolveExporterStreamValue` undefined.

- [ ] **Step 3: Extract `shellRCPath` and add `resolveExporterStreamValue`**

In `src/wizard/cmd/wizard/main.go`, replace the `writeShellConfig` function (currently lines 166-198) with:

```go
// shellRCPath returns the rc file this OS/shell combination uses for
// persistent env vars, and which Shell syntax to render into it. Shared by
// writeShellConfig (writing) and resolveExporterStream (peeking at an
// existing value before writing) so both always agree on which file "this
// run" means.
func shellRCPath(home string) (path string, shell envwriter.Shell) {
	shellName := filepath.Base(os.Getenv("SHELL"))
	switch shellName {
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish"), envwriter.Fish
	case "zsh":
		return filepath.Join(home, ".zshrc"), envwriter.Bash
	default:
		return filepath.Join(home, ".bashrc"), envwriter.Bash
	}
}

func writeShellConfig(out *os.File, home string, vars []envwriter.Var) error {
	if runtime.GOOS == "windows" {
		return envwriter.WriteWindows(vars, func(name, value string) error {
			return exec.Command("setx", name, value).Run()
		})
	}

	path, shell := shellRCPath(home)

	wrote, err := envwriter.WriteBlock(path, shell, vars)
	if err != nil {
		return err
	}
	if wrote {
		fmt.Fprintf(out, "  telemetry + collector env vars added to %s\n", path)
	} else {
		fmt.Fprintf(out, "  already present in %s (edit the block by hand to change accounts)\n", path)
	}
	return nil
}

// resolveExporterStreamValue returns existing unchanged if it's non-empty
// (an already-configured install's stream id, found by resolveExporterStream
// below), or mints a fresh "claude-code-exporter-<uuid>" value otherwise (a
// genuinely new install).
func resolveExporterStreamValue(existing string) (string, error) {
	if existing != "" {
		return existing, nil
	}
	id, err := streamid.New()
	if err != nil {
		return "", err
	}
	return "claude-code-exporter-" + id, nil
}
```

This is a behavior-preserving refactor of `writeShellConfig` (identical logic, just routed through the new shared `shellRCPath`) plus one new pure helper — `run()` still calls `streamid.New()` directly at this point; that's rewired in Task 3.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd src/wizard && go test ./cmd/wizard/... -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Build and vet the whole module**

Run: `cd src/wizard && go build ./... && go vet ./...`
Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add src/wizard/cmd/wizard/main.go src/wizard/cmd/wizard/main_test.go
git commit -m "$(cat <<'EOF'
refactor: extract shellRCPath from writeShellConfig; add resolveExporterStreamValue

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `main.go` — `resolveExporterStream`, wire into `run()`, fix the stale comment

**Files:**
- Modify: `src/wizard/cmd/wizard/main.go`
- Test: `src/wizard/cmd/wizard/main_test.go`

**Interfaces:**
- Consumes: `shellRCPath` and `resolveExporterStreamValue` (Task 2), `envwriter.ExistingVar` (Task 1).
- Produces: `resolveExporterStream(home string) (string, error)` — the full reuse-or-mint decision used by `run()`.

- [ ] **Step 1: Write the failing tests**

Append to `src/wizard/cmd/wizard/main_test.go` (add `"runtime"` to the import block):

```go
func TestResolveExporterStream_ReusesExistingUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix rc-file path only")
	}
	t.Setenv("SHELL", "/bin/bash")
	home := t.TempDir()
	path, shell := shellRCPath(home)
	existing := []envwriter.Var{{Name: "EXPORTER_STREAM", Value: "claude-code-exporter-existing-id"}}
	if _, err := envwriter.WriteBlock(path, shell, existing); err != nil {
		t.Fatal(err)
	}

	got, err := resolveExporterStream(home)
	if err != nil {
		t.Fatal(err)
	}
	if got != "claude-code-exporter-existing-id" {
		t.Errorf("got %q, want reused existing value", got)
	}
}

func TestResolveExporterStream_MintsFreshUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix rc-file path only")
	}
	t.Setenv("SHELL", "/bin/bash")
	home := t.TempDir() // no rc file written yet — genuinely new install

	got, err := resolveExporterStream(home)
	if err != nil {
		t.Fatal(err)
	}
	if !streamValuePattern.MatchString(got) {
		t.Errorf("got %q, want a freshly-minted value", got)
	}
}

func TestResolveExporterStream_MintsFreshWhenBlockExistsButVarMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix rc-file path only")
	}
	t.Setenv("SHELL", "/bin/bash")
	home := t.TempDir()
	path, shell := shellRCPath(home)
	// Simulates an accounts-less first run: the marker block exists (from
	// telemetry vars) but EXPORTER_STREAM was never written because
	// collectorVars was empty that run.
	if _, err := envwriter.WriteBlock(path, shell, []envwriter.Var{{Name: "CLAUDE_CODE_ENABLE_TELEMETRY", Value: "1"}}); err != nil {
		t.Fatal(err)
	}

	got, err := resolveExporterStream(home)
	if err != nil {
		t.Fatal(err)
	}
	if !streamValuePattern.MatchString(got) {
		t.Errorf("got %q, want a freshly-minted value (nothing to reuse yet)", got)
	}
}

func TestResolveExporterStream_ReusesExistingWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows env-var path only")
	}
	t.Setenv("EXPORTER_STREAM", "claude-code-exporter-existing-id")

	got, err := resolveExporterStream(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != "claude-code-exporter-existing-id" {
		t.Errorf("got %q, want reused existing value", got)
	}
}

func TestResolveExporterStream_MintsFreshWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows env-var path only")
	}
	t.Setenv("EXPORTER_STREAM", "")

	got, err := resolveExporterStream(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !streamValuePattern.MatchString(got) {
		t.Errorf("got %q, want a freshly-minted value", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd src/wizard && go test ./cmd/wizard/... -v`
Expected: FAIL — `resolveExporterStream` undefined.

- [ ] **Step 3: Implement `resolveExporterStream`**

In `src/wizard/cmd/wizard/main.go`, append (after `resolveExporterStreamValue`, added in Task 2):

```go
// resolveExporterStream returns the EXPORTER_STREAM value this run should
// use: the id already committed for this install, if one is found, so a
// wizard re-run never orphans the collector's existing Loki stream — a
// fresh id (see resolveExporterStreamValue) is minted only when none is
// found, i.e. a genuinely new install.
//
// On Unix, the source of truth is the shell rc file this run would write to
// (shellRCPath) — not the installed collector service definition (launchd
// plist / systemd unit), even though that's what actually determines the
// running collector's env: the service definition is exactly the artifact
// this bug corrupts, so trusting it as a source would perpetuate any
// existing drift instead of healing it. The shell rc file is also literally
// what a manual `docker compose up` reads EXPORTER_STREAM from at
// compose-up time, so it's the more authoritative side to trust anyway. A
// side effect: running this fixed wizard on a machine that already drifted
// (mismatched shell rc vs. service definition, from a prior buggy run)
// re-derives the correct id from the shell rc and rewrites the service
// definition back into agreement.
//
// On Windows there's no persisted marker file (envwriter.WriteWindows calls
// `setx`, which always overwrites and keeps no marker — see envwriter), so
// the only available signal is this process's own environment: reused if
// the current session already picked up a prior `setx` (true once the user
// has opened a new terminal since installing), minted fresh otherwise — the
// same reload caveat CLAUDE_DIR already has today on Windows.
//
// Not handled, pre-existing and unchanged by this fix: switching shells
// between wizard runs (e.g. bash -> fish) writes a second, separate rc
// block with its own fresh id, same as it always has; and an accounts-less
// first run (no CLAUDE_DIR/EXPORTER_STREAM ever written, since
// collectorVars is only populated when len(chosen) > 0) is treated as
// "nothing to reuse yet" the first time accounts are later found — correct,
// since the collector is only being configured for the first time then.
func resolveExporterStream(home string) (string, error) {
	if runtime.GOOS == "windows" {
		return resolveExporterStreamValue(os.Getenv("EXPORTER_STREAM"))
	}
	path, shell := shellRCPath(home)
	existing, ok, err := envwriter.ExistingVar(path, shell, "EXPORTER_STREAM")
	if err != nil {
		return "", err
	}
	if !ok {
		existing = ""
	}
	return resolveExporterStreamValue(existing)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd src/wizard && go test ./cmd/wizard/... -v`
Expected: PASS (11 tests: 6 from Task 2 + 5 new; on any given CI runner, 2 of the 5 new tests will `SKIP` per the `runtime.GOOS` guard — that's expected, not a failure. Across the macos-latest/ubuntu-latest/windows-latest matrix in `.github/workflows/test.yml`, every test still runs somewhere.)

- [ ] **Step 5: Wire `resolveExporterStream` into `run()` and fix the stale comment**

In `src/wizard/cmd/wizard/main.go`, replace the current block (originally lines ~103-113, inside the `if len(chosen) > 0 { ... }` in `run()`):

```go
		// A random id per install rather than a shared sequential counter —
		// see internal/streamid. Discarded on a re-run: WriteBlock below is
		// a no-op once the marker exists, so this only ever takes effect
		// the first time (on Windows, WriteWindows has no such marker and
		// setx always overwrites, so a re-run there does mint a new one —
		// same as it already does for CLAUDE_DIR today).
		id, err := streamid.New()
		if err != nil {
			return err
		}
		collectorVars = append(collectorVars, envwriter.Var{Name: "EXPORTER_STREAM", Value: "claude-code-exporter-" + id})
```

with:

```go
		// See resolveExporterStream: reuses this install's existing
		// EXPORTER_STREAM if one is already configured, so a wizard re-run
		// never orphans the collector's existing Loki stream. Only mints a
		// fresh id (internal/streamid) for a genuinely new install.
		streamValue, err := resolveExporterStream(home)
		if err != nil {
			return err
		}
		collectorVars = append(collectorVars, envwriter.Var{Name: "EXPORTER_STREAM", Value: streamValue})
```

(`home` is already in scope in `run()` — it's the function's first local, from `os.UserHomeDir()`.)

- [ ] **Step 6: Run the full test suite, build, and vet**

Run: `cd src/wizard && go build ./... && go vet ./... && go test ./...`
Expected: all packages PASS, no build/vet errors. Confirm `streamid` is still imported in `main.go` (it's now only referenced inside `resolveExporterStreamValue`, not directly in `run()` — `goimports`/`go vet` will catch it if the import became unused, but it shouldn't, since `resolveExporterStreamValue` still calls `streamid.New()`).

- [ ] **Step 7: Manual smoke check of the reuse behavior (optional but recommended given this is blocking a live upgrade)**

Run from `src/wizard`:
```bash
go run ./cmd/wizard --version  # sanity: binary still builds/runs
```
Then, in a scratch `$HOME` (do NOT point this at a real `$HOME` with production config):
```bash
export HOME=/tmp/wizard-smoke-test
mkdir -p "$HOME/.claude/projects"
cd /path/to/claude-observability-repo-root   # must contain docker-compose.yaml
go run ./src/wizard/cmd/wizard   # first run: mints a fresh EXPORTER_STREAM, writes it to $HOME/.bashrc (or config.fish per $SHELL)
grep EXPORTER_STREAM "$HOME/.bashrc"          # note the value
go run ./src/wizard/cmd/wizard   # second run: answer the same prompts
grep EXPORTER_STREAM "$HOME/.bashrc"          # must be byte-identical to the first run's value
```
Expected: the `EXPORTER_STREAM` value is identical across both runs. (Skip the "Install the collector as a background service now?" prompt with `n` during this smoke test unless you also want to verify the installed plist/unit now carries the same reused value — if you do, `cat ~/Library/LaunchAgents/com.claude-observability.collector.plist` (darwin) after each run and confirm the `EXPORTER_STREAM` `<string>` value is unchanged.)

- [ ] **Step 8: Commit**

```bash
git add src/wizard/cmd/wizard/main.go src/wizard/cmd/wizard/main_test.go
git commit -m "$(cat <<'EOF'
fix: reuse existing EXPORTER_STREAM on wizard re-run instead of re-minting it

The wizard was minting a fresh EXPORTER_STREAM on every run and baking it
into the collector's installed service definition (plist/systemd
unit/scheduled task), which service.Install unconditionally rewrites every
call. A re-run therefore silently orphaned the collector's existing Loki
stream from what the shell config (and docker-compose, which reads
EXPORTER_STREAM from the shell environment at compose-up time) still had —
even though the shell rc block itself was correctly frozen after the first
run via envwriter's marker no-op.

resolveExporterStream now looks for an already-configured value first (the
shell rc marker block on Unix, this process's own environment on Windows,
where no persisted marker file exists) and only mints a fresh id
(internal/streamid) when none is found — a genuinely new install.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review Notes (already applied above)

- **Spec coverage:** all points from the approved design are implemented: `ExistingVar` (Task 1), `shellRCPath` extraction + pure `resolveExporterStreamValue` (Task 2), OS-dispatching `resolveExporterStream` + `run()` wiring + comment fix (Task 3). The accounts-less-first-run case and the Windows reload caveat are each covered by a named test and documented in the `resolveExporterStream` doc comment. Non-goals (shell-switching duplication, the separate WriteBlock-no-op-for-later-added-accounts gap) are explicitly called out as unchanged, not silently dropped.
- **Type consistency:** `resolveExporterStream(home string) (string, error)` (Task 3) matches its use in `run()` (Task 3, Step 5) and its consumption of `resolveExporterStreamValue(existing string) (string, error)` (Task 2) and `envwriter.ExistingVar(path string, shell Shell, name string) (value string, ok bool, err error)` (Task 1) exactly — verified signatures match across all three tasks.
- **No placeholders:** every step has real, complete code; no "TBD"/"handle appropriately" language anywhere.
