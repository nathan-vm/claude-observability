# Usage-Truth MCP Orphan-Container Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the collector's usage-truth probe from leaking Docker-based MCP server containers forever — make `FetchUsage`'s headless `claude -p /usage` invocation boot zero MCP servers, and make the shared `runClaude` helper kill a timed-out `claude` invocation's *entire* process group (not just the direct child), so nothing survives even if a future MCP config slips through.

**Architecture:** Two independent, additive changes to `src/collector/internal/usagetruth/usagetruth.go`, each addressing one link in the confirmed leak chain. (1) `FetchUsage` passes `--strict-mcp-config --mcp-config <path-to-empty-config>` so the probe never boots any MCP server in the first place — applied only to `FetchUsage`'s call, not `AccountEmail`'s `claude auth status`. (2) `runClaude` (delegating to a timeout-injectable `runClaudeWithTimeout`) starts `claude` in its own process group and overrides `cmd.Cancel`/`cmd.WaitDelay` (Go 1.20+ `os/exec` fields) to kill the whole group on timeout via a Unix syscall, with a direct-child-only fallback on Windows behind build tags — strictly no worse than today on Windows, meaningfully better everywhere else.

**Tech Stack:** Go 1.22 (module `claude-observability-collector`), standard library only (`os/exec`, `syscall`, `context`, `time`), existing `testing` package style (no testify/mocking) — real-subprocess-on-PATH fixtures, matching `usagetruth_test.go`'s existing pattern.

**Spec:** No separate spec file — bounded-path fix. Grounding source: `src/collector/internal/usagetruth/usagetruth.go`, `src/collector/internal/usagetruth/usagetruth_test.go`, `src/collector/cmd/collector/main.go`, `src/collector/go.mod`, `src/wizard/internal/service/*` (per-OS build-tag style reference), `docs/superpowers/plans/2026-09-26-exporter-stream-reuse.md` (format reference).

---

## PRE-VERIFIED BY THE ORCHESTRATOR — the planner's two open questions are now CLOSED

The planner had no Bash access and left two questions open. Both were verified live on this machine before this plan was handed off. **Do not re-litigate these; do not "fix" the argument order below.**

### 1. Both flags exist and are global

`claude --help` confirms:
```
--mcp-config <configs...>             Load MCP servers from JSON files or ...
--strict-mcp-config                   Only use MCP servers from --mcp-config, ...
```

### 2. The exact `FetchUsage` argument order PARSES CLEANLY — verified

```bash
CLAUDE_CONFIG_DIR=<dir> claude --strict-mcp-config --mcp-config <path> \
  -p /usage --output-format json --no-session-persistence
```
returned valid JSON with `"is_error":false`. This is the shape Task 1 Step 4 implements. It works because `-p` begins with `-`, which terminates `--mcp-config`'s variadic collection.

### 3. ⚠️ CRITICAL GOTCHA — `--mcp-config` is VARIADIC AND GREEDY

`--mcp-config <configs...>` accepts *multiple* paths and greedily consumes every following **bare word**. Proof:

```
$ claude --strict-mcp-config --mcp-config <path> auth status
Error: Invalid MCP configuration:
MCP config file not found: /Users/nathan/.../auth
MCP config file not found: /Users/nathan/.../status
```

It ate `auth` and `status` as config paths. Consequences, both binding:

- **Never add these flags to `AccountEmail`'s `claude auth status`.** It is not that `auth status` rejects them — it is that the subcommand words get swallowed and the command breaks outright. The plan's decision to scope the flags to `FetchUsage` only is therefore **mandatory**, not merely conservative. The regression test in Task 1 Step 2 that asserts `auth status` never receives `--mcp-config` is protecting against a real, demonstrated breakage.
- **Never reorder the flags to trail the argument list.** They must stay ahead of `-p`, and `-p` (or some other `-`-prefixed token) must immediately follow the config path.

---

## Global Constraints

- Go module is `src/collector` (own `go.mod`, module `claude-observability-collector`, `go 1.22`). Only this module needs rebuilding/retesting — do not touch `src/wizard` or `src/dash-generator`.
- Verification bar per task: `cd src/collector && go build ./... && go vet ./... && go test ./...`. No new linter.
- No third-party Go modules — standard library only.
- Do not change `POLL_SECONDS`/the collector's poll cadence, `claudeTimeout`'s 30-second value, or anything about what `/usage` data gets published (`parseUsage`, `publish`, the `Usage` struct). This fix is scoped to how the `claude` subprocess is launched and torn down, nothing else.
- Do not touch the user's `~/.claude-work/.claude.json` or any other live machine config.
- `release.yml` builds five targets: darwin/amd64, darwin/arm64, linux/amd64, linux/arm64, windows/amd64. OS-specific code must keep `go build` working for all five.
- Match `usagetruth_test.go`'s existing style: plain `testing`, `Test<Func>_<Behavior>` names, real-subprocess-on-PATH fixtures (`fakeClaudeOnPath`), `t.TempDir()`/`t.Setenv()`, `runtime.GOOS == "windows"` skips for POSIX-shell-script fixtures.

---

## File Structure

```
src/collector/internal/usagetruth/
  usagetruth.go       (MODIFY: runClaude delegates to new runClaudeWithTimeout;
                        add emptyMCPConfig const + writeEmptyMCPConfigFile;
                        FetchUsage passes new flags; runClaudeWithTimeout wires
                        setNewProcessGroup/cmd.Cancel/cmd.WaitDelay)
  usagetruth_test.go  (MODIFY: add MCP-config-flag tests + group-kill test;
                        add "strings", "time" imports)
  proc_unix.go        (NEW: //go:build unix — real Setpgid + killpg)
  proc_windows.go     (NEW: //go:build windows — no-op group-set, direct-child kill)
```

`proc_unix.go`/`proc_windows.go` is a 2-way split (unix vs. windows), not the 3-way per-GOOS split `src/wizard/internal/service` uses. That package's install mechanisms genuinely diverge per OS (launchd plist vs. systemd unit vs. Scheduled Task); here, `Setpgid`/process-group-kill behavior is identical on darwin and linux, so Go's `unix` build tag matches the real behavior boundary instead of copying the file-per-GOOS pattern unexamined.

---

### Task 1: Zero MCP servers on the `/usage` probe

**Files:**
- Modify: `src/collector/internal/usagetruth/usagetruth.go`
- Test: `src/collector/internal/usagetruth/usagetruth_test.go`

**Interfaces:**
- Consumes: `runClaude(configDir string, args ...string) ([]byte, error)` (existing, unchanged signature).
- Produces: `writeEmptyMCPConfigFile() (string, error)` — internal helper. `FetchUsage`'s argument list to `runClaude` changes; `AccountEmail`'s does not.

- [ ] **Step 1: CLI flag verification — ALREADY DONE**

See "PRE-VERIFIED BY THE ORCHESTRATOR" above. The flags exist, the argument order in Step 4 parses cleanly, and the variadic-greediness gotcha is documented. Skip straight to Step 2.

- [ ] **Step 2: Write the failing tests**

Add `"strings"` to `usagetruth_test.go`'s import block. Append:

```go
func TestFetchUsage_PassesStrictMCPConfigFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude fixture is a POSIX shell script")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	usagePayload, _ := json.Marshal(map[string]interface{}{
		"is_error": false,
		"result":   "Current session: 1% used\nCurrent week (all models): 2% used",
	})
	script := "#!/bin/sh\n" +
		`echo "$@" > ` + argsPath + "\n" +
		"cat <<'EOF'\n" + string(usagePayload) + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := FetchUsage("/tmp/cfg"); err != nil {
		t.Fatal(err)
	}

	seenArgs, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	argsLine := strings.TrimSpace(string(seenArgs))
	if !strings.Contains(argsLine, "--strict-mcp-config") {
		t.Errorf("args = %q, want --strict-mcp-config", argsLine)
	}
	fields := strings.Fields(argsLine)
	idx := -1
	for i, f := range fields {
		if f == "--mcp-config" {
			idx = i
			break
		}
	}
	if idx == -1 || idx+1 >= len(fields) {
		t.Fatalf("args = %q, want --mcp-config followed by a path", argsLine)
	}
	configContent, err := os.ReadFile(fields[idx+1])
	if err != nil {
		t.Fatalf("--mcp-config path %q not readable: %v", fields[idx+1], err)
	}
	if string(configContent) != `{"mcpServers":{}}` {
		t.Errorf("mcp config content = %q, want empty mcpServers", configContent)
	}
	// The flags must precede -p: --mcp-config is variadic and greedily eats
	// following bare words, so anything non-flag after the config path would
	// be swallowed as another config file. See the pre-verification notes.
	pIdx := -1
	for i, f := range fields {
		if f == "-p" {
			pIdx = i
			break
		}
	}
	if pIdx == -1 || pIdx != idx+2 {
		t.Errorf("args = %q, want -p immediately after the --mcp-config path", argsLine)
	}
}

func TestAccountEmail_DoesNotPassMCPConfigFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude fixture is a POSIX shell script")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	script := "#!/bin/sh\n" +
		`echo "$@" > ` + argsPath + "\n" +
		`echo '{"loggedIn":true,"email":"a@example.com"}'` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := AccountEmail("/tmp/cfg"); err != nil {
		t.Fatal(err)
	}

	seenArgs, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	// Not merely unnecessary — actively breaking. --mcp-config is variadic
	// and would swallow the `auth` and `status` subcommand words as config
	// file paths, failing the command outright. Verified live.
	if strings.Contains(string(seenArgs), "--mcp-config") {
		t.Errorf("args = %q, auth status must not receive --mcp-config", seenArgs)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd src/collector && go test ./internal/usagetruth/... -run 'MCPConfig' -v`
Expected: FAIL — `TestFetchUsage_PassesStrictMCPConfigFlags` fails because the fixture never sees `--strict-mcp-config`. `TestAccountEmail_DoesNotPassMCPConfigFlags` passes vacuously for now; it becomes a real regression guard once Step 4 lands.

- [ ] **Step 4: Implement `writeEmptyMCPConfigFile` and wire it into `FetchUsage`**

Add `"path/filepath"` to `usagetruth.go`'s import block. Add after the `claudeTimeout` const:

```go
// emptyMCPConfig is the literal file content passed to `claude` via
// --mcp-config, paired with --strict-mcp-config, so FetchUsage's headless
// `/usage` probe boots zero MCP servers instead of the user's full
// configured set (including Docker-based ones whose container survives a
// timed-out `claude` process — see runClaudeWithTimeout for the second half
// of that fix).
const emptyMCPConfig = `{"mcpServers":{}}`

// writeEmptyMCPConfigFile (re)writes emptyMCPConfig to a fixed path under
// os.TempDir() and returns that path. A file path, not an inline JSON
// string: --mcp-config's help text only documents "JSON files", and a path
// sidesteps the question of whether an inline string is also accepted.
// Rewritten unconditionally on every call rather than cached — the content
// is static and the write is a few bytes, far cheaper than the bug this
// exists to avoid.
func writeEmptyMCPConfigFile() (string, error) {
	path := filepath.Join(os.TempDir(), "claude-observability-empty-mcp-config.json")
	if err := os.WriteFile(path, []byte(emptyMCPConfig), 0o644); err != nil {
		return "", fmt.Errorf("writing empty MCP config: %w", err)
	}
	return path, nil
}
```

Replace `FetchUsage` with:

```go
func FetchUsage(configDir string) (Usage, error) {
	mcpConfigPath, err := writeEmptyMCPConfigFile()
	if err != nil {
		return Usage{}, err
	}
	// Flag order matters: --mcp-config is variadic and greedily consumes
	// following bare words, so -p must come immediately after the path.
	out, err := runClaude(configDir,
		"--strict-mcp-config", "--mcp-config", mcpConfigPath,
		"-p", "/usage", "--output-format", "json", "--no-session-persistence")
	if err != nil {
		return Usage{}, err
	}
	var result struct {
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return Usage{}, fmt.Errorf("claude -p /usage decode: %w", err)
	}
	if result.IsError {
		return Usage{}, fmt.Errorf(`claude -p "/usage" failed: %s`, result.Result)
	}
	return parseUsage(result.Result)
}
```

Everything after the `runClaude` call is unchanged. `AccountEmail` is untouched.

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd src/collector && go test ./internal/usagetruth/... -v`
Expected: PASS — existing tests plus the two new ones.

- [ ] **Step 6: Build and vet the whole module**

Run: `cd src/collector && go build ./... && go vet ./...`
Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add src/collector/internal/usagetruth/usagetruth.go src/collector/internal/usagetruth/usagetruth_test.go
git commit -m "$(cat <<'EOF'
fix: stop the usage-truth probe from booting MCP servers

FetchUsage's `claude -p /usage` invocation was booting the user's full
configured MCP server set on every poll (~71s cadence), including
Docker-based servers whose container survives well past a timed-out
`claude` process. Now passes --strict-mcp-config --mcp-config
<empty-config-path> so the probe boots zero MCP servers.

Scoped to FetchUsage only, never AccountEmail's `claude auth status`:
--mcp-config is variadic and would swallow the `auth` and `status`
subcommand words as config file paths, breaking the command outright.
A regression test asserts auth status never receives the flag.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Kill the whole process group on timeout, not just the direct child

**Files:**
- Create: `src/collector/internal/usagetruth/proc_unix.go`
- Create: `src/collector/internal/usagetruth/proc_windows.go`
- Modify: `src/collector/internal/usagetruth/usagetruth.go`
- Test: `src/collector/internal/usagetruth/usagetruth_test.go`

**Interfaces:**
- Produces: `setNewProcessGroup(cmd *exec.Cmd)` and `killProcessGroup(cmd *exec.Cmd) error` (OS-specific, called only from `runClaudeWithTimeout`); `runClaudeWithTimeout(configDir string, timeout time.Duration, args ...string) ([]byte, error)` — `runClaude` becomes a thin wrapper passing the untouched `claudeTimeout` constant, so this task changes no production timeout value.

- [ ] **Step 1: Write the failing test**

Append to `usagetruth_test.go` (add `"time"` to the import block):

```go
func TestRunClaudeWithTimeout_KillsGrandchildProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group kill is POSIX-specific; see proc_windows.go")
	}
	dir := t.TempDir()
	heartbeat := filepath.Join(dir, "heartbeat")
	// The fixture backgrounds a loop that writes a fresh heartbeat every
	// ~50ms — simulating a grandchild like `docker run` that a plain
	// cmd.Process.Kill() (direct child only) would leave running — then
	// blocks in the foreground well past the test's short timeout, so the
	// only thing that can end this script is our own timeout-triggered
	// group kill.
	script := "#!/bin/sh\n" +
		`( while true; do date +%s%N > ` + heartbeat + `; sleep 0.05; done ) &` + "\n" +
		"sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := runClaudeWithTimeout("/tmp/cfg", 200*time.Millisecond)
	if err == nil {
		t.Fatal("want a timeout error")
	}

	readHeartbeat := func() []byte {
		b, _ := os.ReadFile(heartbeat)
		return b
	}
	first := readHeartbeat()
	if len(first) == 0 {
		t.Fatal("grandchild never started heartbeating — fixture didn't run as expected")
	}
	time.Sleep(300 * time.Millisecond) // well past the 50ms heartbeat interval
	second := readHeartbeat()
	if string(first) != string(second) {
		t.Errorf("heartbeat still advancing after timeout (%q -> %q); want the grandchild to have died with the process group, not outlived it", first, second)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd src/collector && go test ./internal/usagetruth/... -run TestRunClaudeWithTimeout -v`
Expected: FAIL — `runClaudeWithTimeout` undefined (compile error).

- [ ] **Step 3: Create the OS-specific process-group helpers**

Create `src/collector/internal/usagetruth/proc_unix.go`:

```go
//go:build unix

package usagetruth

import (
	"os/exec"
	"syscall"
)

// setNewProcessGroup starts cmd in its own process group (pgid == its own
// pid), so killProcessGroup below can signal the whole tree — including a
// grandchild like `docker run` that reparents to PID 1 (and keeps running)
// the moment exec.CommandContext's default, child-only kill fires on
// timeout.
func setNewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to cmd's entire process group via the
// negative-PID convention (kill(2)) — reaching grandchildren that a plain
// cmd.Process.Kill() (PID only) leaves behind.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
```

Create `src/collector/internal/usagetruth/proc_windows.go`:

```go
//go:build windows

package usagetruth

import "os/exec"

// setNewProcessGroup is a no-op on Windows: grouping a process tree for a
// single kill signal needs a job object, a materially bigger mechanism than
// POSIX Setpgid, and this fix's motivating case (a `docker run` grandchild
// reparenting to PID 1 on kill) is a Unix process-model-specific failure
// mode.
func setNewProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup kills only the direct child, same as
// exec.CommandContext's own default Cancel would have done anyway — this
// platform is strictly no worse than before this fix, not improved.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
```

- [ ] **Step 4: Refactor `runClaude` to delegate to `runClaudeWithTimeout`, wired with the group kill**

In `usagetruth.go`, replace the existing `runClaude` with:

```go
func runClaude(configDir string, args ...string) ([]byte, error) {
	return runClaudeWithTimeout(configDir, claudeTimeout, args...)
}

// runClaudeWithTimeout is runClaude with an injectable timeout — the seam
// TestRunClaudeWithTimeout_KillsGrandchildProcessGroup uses to exercise the
// process-group-kill path in well under a second instead of the real 30s
// claudeTimeout. Production code only ever reaches this through runClaude,
// which always passes the untouched claudeTimeout constant, so this
// refactor changes no production timeout behavior.
func runClaudeWithTimeout(configDir string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+configDir)

	// exec.CommandContext's default Cancel kills only the direct `claude`
	// child; a grandchild it spawned (e.g. a Docker-based MCP server via
	// `docker run`) reparents to PID 1 and keeps running. Starting claude
	// in its own process group and overriding Cancel to kill that whole
	// group closes that gap. WaitDelay bounds how long Wait() can be stuck
	// after Cancel runs, in case a grandchild still holds an inherited
	// stdout/stderr pipe fd open.
	setNewProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd src/collector && go test ./internal/usagetruth/... -v`
Expected: PASS — all pre-existing tests plus the two from Task 1 plus this one.

- [ ] **Step 6: Cross-compile for every release target**

```bash
cd src/collector
GOOS=darwin  GOARCH=amd64 go build ./...
GOOS=darwin  GOARCH=arm64 go build ./...
GOOS=linux   GOARCH=amd64 go build ./...
GOOS=linux   GOARCH=arm64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
```

Expected: no errors on any of the five. This is the check that would catch a `syscall.Kill`/`Setpgid` reference leaking into a file without the `unix` build tag.

- [ ] **Step 7: `go vet` the whole module**

Run: `cd src/collector && go vet ./...`
Expected: no errors.

- [ ] **Step 8: Skip the live smoke check**

Leave runtime smoke testing to the `qa` stage. Do NOT run the collector against the real `~/.claude-work` config yourself — there is a live collector service running on this machine and a live docker stack; the orchestrator coordinates that verification separately.

- [ ] **Step 9: Commit**

```bash
git add src/collector/internal/usagetruth/usagetruth.go src/collector/internal/usagetruth/usagetruth_test.go src/collector/internal/usagetruth/proc_unix.go src/collector/internal/usagetruth/proc_windows.go
git commit -m "$(cat <<'EOF'
fix: kill the usage-truth probe's whole process group on timeout

exec.CommandContext's default timeout behavior kills only the direct
`claude` child; a grandchild it spawned (e.g. a Docker-based MCP server
launched via `docker run`) reparented to PID 1 and kept running, leaking
a container per timed-out poll. runClaude now starts `claude` in its own
process group (Unix) and overrides cmd.Cancel to kill the whole group via
killpg, with cmd.WaitDelay bounding how long Wait() can be stuck on a
grandchild still holding an inherited pipe open. Windows has no
equivalent mechanism here and falls back to the pre-existing
direct-child-only kill — strictly no worse than before.

Defense-in-depth on top of the companion fix (--strict-mcp-config
--mcp-config) that stops the probe from booting any MCP server in the
first place.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review Notes

- **Spec coverage:** Both halves implemented: MCP-config suppression scoped to `FetchUsage` only (Task 1), process-group kill applied to the shared `runClaude`/`runClaudeWithTimeout` (Task 2). The CLI-flag questions the planner left open were closed empirically by the orchestrator and recorded above, including the variadic-greediness gotcha that makes the `FetchUsage`-only scoping mandatory. Cross-platform covered by the `unix`/`windows` build-tag split plus a 5-target cross-compile check.
- **Type consistency:** `runClaude(configDir string, args ...string) ([]byte, error)` (unchanged) → `runClaudeWithTimeout(configDir string, timeout time.Duration, args ...string) ([]byte, error)`, matching its one caller and one test. `writeEmptyMCPConfigFile() (string, error)` matches its one caller. `setNewProcessGroup`/`killProcessGroup` match across both build-tagged files and their call sites.
- **Non-goals confirmed unchanged:** `claudeTimeout` stays `30 * time.Second`; the poll loop is untouched; `parseUsage`/`publish`/`Usage` untouched; `AccountEmail`'s argument list untouched, now with a regression test.
