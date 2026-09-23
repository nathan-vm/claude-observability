# Collector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port `transcript-scan.mjs` (1013 lines) and a simplified `usage-truth.mjs` to a self-contained Go binary (`collector`) with byte-for-byte equivalent behavior, then cut over from `collector-old/`.

**Architecture:** A Go module at `collector/` with small, independently testable packages (`accounts`, `shelltok`, `state`, `lokiclient`, `usagetruth`) plus one larger `transcriptscan` package (built across several tasks, given its size) composed by `cmd/collector/main.go`'s single poll loop.

**Tech Stack:** Go 1.22 (standard library only). CI jobs for this module already exist in `.github/workflows/test.yml` and `release.yml` (added during the dash-generator work) — no workflow changes needed here.

**Spec:** `docs/superpowers/specs/2026-09-23-collector-design.md`

## Global Constraints

- No third-party Go modules — standard library only.
- Byte-for-byte equivalent behavior to `transcript-scan.mjs` for token attribution, MCP/skill/bash-command resolution, email resolution, dedup, batching/retry — this is a port, not a redesign. Every task below cites exact `collector-old/transcript-scan.mjs` line numbers for the logic it ports.
- One known, deliberate divergence: JS string `.length` counts UTF-16 code units; Go's `len(string)` counts UTF-8 bytes. `blockSize`/`BlockSize` uses Go byte length. This only affects the proportional *split* of tokens across parallel tool calls in rare non-ASCII-heavy results — not an absolute count that must match exactly (the JS comment on `attribute` already frames this as an approximation: "what matters is the marginal cost of the call, not its drag on the turns that follow").
- `usage-meter.mjs`'s calibration math is not ported (dead — see the dash-generator spec's Context section).
- Must be run from the `claude-observability` repo root only for its default `STATE_FILE` location (unlike the wizard/dash-generator, everything else comes from env vars, not repo-relative paths).

---

## File Structure

```
collector/
  go.mod
  cmd/collector/main.go
  internal/accounts/accounts.go              (port of accounts.mjs)
  internal/accounts/accounts_test.go
  internal/shelltok/shelltok.go               (port of tokenizeShell/heredocEnd/extractBashCommands)
  internal/shelltok/shelltok_test.go
  internal/state/state.go                     (persisted JSON state)
  internal/state/state_test.go
  internal/lokiclient/lokiclient.go            (log-query client — see spec correction)
  internal/lokiclient/lokiclient_test.go
  internal/transcriptscan/transcriptscan.go    (built across Tasks 6-10)
  internal/transcriptscan/transcriptscan_test.go
  internal/usagetruth/usagetruth.go            (simplified port of usage-truth.mjs)
  internal/usagetruth/usagetruth_test.go
```

`collector-old/` is deleted in the final task, once this binary is verified end to end.

---

### Task 1: Go module scaffold

**Files:**
- Create: `collector/go.mod`
- Create: `collector/cmd/collector/main.go`

**Interfaces:**
- Produces: a `collector` Go module (`module claude-observability-collector`) later tasks add packages under.

- [ ] **Step 1: Create the Go module**

```bash
mkdir -p collector/cmd/collector
cat > collector/go.mod <<'EOF'
module claude-observability-collector

go 1.22
EOF
```

- [ ] **Step 2: Placeholder main**

`collector/cmd/collector/main.go`:
```go
package main

import "fmt"

func main() {
	fmt.Println("collector")
}
```

- [ ] **Step 3: Verify it builds and runs**

Run: `cd collector && go build ./... && go run ./cmd/collector`
Expected: prints `collector`, no errors.

- [ ] **Step 4: Verify the pre-existing CI job picks it up**

`.github/workflows/test.yml` already has a `collector` job pointed at `working-directory: collector` (added during the dash-generator work, ahead of this module existing). No workflow file changes needed — confirm by reading the job, don't recreate it.

- [ ] **Step 5: Commit**

```bash
git add collector/go.mod collector/cmd/collector/main.go
git commit -m "$(cat <<'EOF'
Scaffold collector Go module

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: `internal/accounts` — port of `accounts.mjs`

**Files:**
- Create: `collector/internal/accounts/accounts.go`
- Test: `collector/internal/accounts/accounts_test.go`

**Interfaces:**
- Produces: `accounts.ResolveConfigDirs(homeDir, claudeDirEnv, extraDirsEnv string) []string`.

Ports `collector-old/accounts.mjs` in full (it's 33 lines — the whole file).

- [ ] **Step 1: Write the failing tests**

`collector/internal/accounts/accounts_test.go`:
```go
package accounts

import "testing"

func TestResolveConfigDirs_DefaultsToHomeClaudeWhenUnset(t *testing.T) {
	got := ResolveConfigDirs("/home/nathan", "", "")
	if len(got) != 1 || got[0] != "/home/nathan/.claude" {
		t.Errorf("got %v", got)
	}
}

func TestResolveConfigDirs_ExpandsHomeVarInClaudeDir(t *testing.T) {
	got := ResolveConfigDirs("/home/nathan", "${HOME}/.claude-work", "")
	if len(got) != 1 || got[0] != "/home/nathan/.claude-work" {
		t.Errorf("got %v", got)
	}
}

func TestResolveConfigDirs_ExpandsBareDollarHomeInExtraDirs(t *testing.T) {
	got := ResolveConfigDirs("/home/nathan", "/home/nathan/.claude", "$HOME/.claude-work:$HOME/.claude-personal")
	want := []string{"/home/nathan/.claude", "/home/nathan/.claude-work", "/home/nathan/.claude-personal"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveConfigDirs_PrimaryFirstAndDeduped(t *testing.T) {
	got := ResolveConfigDirs("/home/nathan", "/home/nathan/.claude", "/home/nathan/.claude: :/home/nathan/.claude-work")
	want := []string{"/home/nathan/.claude", "/home/nathan/.claude-work"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd collector && go test ./internal/accounts/... -v`
Expected: FAIL — `ResolveConfigDirs` undefined.

- [ ] **Step 3: Implement accounts.go**

`collector/internal/accounts/accounts.go`:
```go
// Package accounts resolves every Claude Code config directory this
// machine monitors — port of accounts.mjs.
package accounts

import (
	"path/filepath"
	"strings"
)

func expandHome(value, homeDir string) string {
	value = strings.ReplaceAll(value, "${HOME}", homeDir)
	value = strings.ReplaceAll(value, "$HOME", homeDir)
	return value
}

// ResolveConfigDirs returns every Claude Code config directory to scan:
// claudeDirEnv (default "<homeDir>/.claude") plus the colon-separated
// extraDirsEnv, each with "${HOME}"/"$HOME" expanded, deduplicated,
// primary first.
func ResolveConfigDirs(homeDir, claudeDirEnv, extraDirsEnv string) []string {
	primary := claudeDirEnv
	if primary == "" {
		primary = filepath.Join(homeDir, ".claude")
	} else {
		primary = expandHome(primary, homeDir)
	}

	seen := map[string]bool{primary: true}
	out := []string{primary}
	for _, d := range strings.Split(extraDirsEnv, ":") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		d = expandHome(d, homeDir)
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd collector && go test ./internal/accounts/... -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add collector/internal/accounts
git commit -m "$(cat <<'EOF'
Add accounts package: config dir resolution, port of accounts.mjs

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `internal/shelltok` — port of the shell tokenizer + Bash command extractor

**Files:**
- Create: `collector/internal/shelltok/shelltok.go`
- Test: `collector/internal/shelltok/shelltok_test.go`

**Interfaces:**
- Produces: `shelltok.ExtractBashCommands(commandStr string) []string`.

Ports `collector-old/transcript-scan.mjs` lines 200-349 exactly: `heredocEnd`
(210-239), `tokenizeShell` (261-299), `SHELL_KEYWORDS`/`looksLikeCommandName`
(301-323), `extractBashCommands` (325-349). `ParseToolName` (200-208) is a
separate, tiny function — it goes in `transcriptscan` (Task 6), not here,
since it isn't shell-tokenizing logic.

- [ ] **Step 1: Write the failing tests**

`collector/internal/shelltok/shelltok_test.go`:
```go
package shelltok

import (
	"reflect"
	"testing"
)

func TestExtractBashCommands_SimpleAndChained(t *testing.T) {
	got := ExtractBashCommands("git status && ls -la")
	want := []string{"git", "ls"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_PipeAndSemicolonAndNewline(t *testing.T) {
	got := ExtractBashCommands("cat foo.txt | grep bar; echo done\npwd")
	want := []string{"cat", "grep", "echo", "pwd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_SkipsEnvAssignment(t *testing.T) {
	got := ExtractBashCommands("FOO=bar git status")
	want := []string{"git"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_SubshellSurvivesAsOneAtomicWord(t *testing.T) {
	// The whole FROM=$(...) assignment must be skipped as ONE env-assignment
	// token, not split into fragments that leak a bogus second "command".
	got := ExtractBashCommands(`FROM=$(date -v-6d +%F || date -d '6 days ago' +%F) && echo "$FROM"`)
	want := []string{"echo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_HeredocBodyDoesNotLeakSegments(t *testing.T) {
	got := ExtractBashCommands("cat <<EOF\nif this; then\n  echo leaked\nfi\nEOF\npwd")
	want := []string{"cat", "pwd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_IndentedHeredocDelimiter(t *testing.T) {
	got := ExtractBashCommands("cat <<-EOF\n\ttext\n\tEOF\npwd")
	want := []string{"cat", "pwd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_UnterminatedHeredocFallsThroughAsText(t *testing.T) {
	// heredocEnd returns -1 (no closing line), so the remainder is scanned
	// as ORDINARY text, not swallowed: it still segments on the embedded
	// newline, and "some" (a plausible-looking word) is picked up as a
	// second, spurious candidate — "falls through as ordinary text" means
	// "gets tokenized normally," not "produces no extra segments."
	got := ExtractBashCommands("cat <<EOF\nsome text with no closer")
	want := []string{"cat", "some"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_LineContinuationJoinsWithoutBreak(t *testing.T) {
	got := ExtractBashCommands("git \\\n  status")
	want := []string{"git"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_RtkUnwrapsToRealCommand(t *testing.T) {
	got := ExtractBashCommands("rtk git status")
	want := []string{"git"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractBashCommands_RejectsShellKeywords(t *testing.T) {
	// Only the FIRST word of each ;-split segment is ever a candidate —
	// "do" (segment 2's first word) is rejected as a keyword, so "echo"
	// (same segment) never gets examined; the whole line yields nothing.
	// Matches the JS's `let name = tokens[i]` picking exactly one
	// candidate per segment, with no fallback to a later word.
	got := ExtractBashCommands("for f in *; do echo $f; done")
	if len(got) != 0 {
		t.Errorf("got %v, want none (keyword rejection drops the whole segment)", got)
	}
}

func TestExtractBashCommands_RejectsNamesWithDisallowedChars(t *testing.T) {
	got := ExtractBashCommands(`$(echo hidden)`)
	if len(got) != 0 {
		t.Errorf("got %v, want none (name starts with disallowed char)", got)
	}
}

func TestExtractBashCommands_EmptyInput(t *testing.T) {
	if got := ExtractBashCommands(""); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd collector && go test ./internal/shelltok/... -v`
Expected: FAIL — `ExtractBashCommands` undefined.

- [ ] **Step 3: Implement shelltok.go**

`collector/internal/shelltok/shelltok.go`:
```go
// Package shelltok names every recognizable sub-command in a Bash tool's
// command string — port of transcript-scan.mjs's tokenizeShell/
// extractBashCommands (collector-old/transcript-scan.mjs lines 200-349).
package shelltok

import (
	"regexp"
	"strings"
)

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// heredocEnd finds where a heredoc body (the "<<" is at str[i]) ends: a
// line that is exactly the delimiter, optionally indented when the
// redirect is "<<-". Returns -1 when this isn't actually a heredoc (a
// bare "<<" with no delimiter word, or one whose closing line never
// appears — the safer fallback then is ordinary text).
func heredocEnd(str string, i int) int {
	j := i + 2
	if j < len(str) && str[j] == '-' {
		j++
	}
	for j < len(str) && (str[j] == ' ' || str[j] == '\t') {
		j++
	}
	var delim strings.Builder
	if j < len(str) && (str[j] == '"' || str[j] == '\'') {
		q := str[j]
		j++
		for j < len(str) && str[j] != q {
			delim.WriteByte(str[j])
			j++
		}
		j++
	} else {
		for j < len(str) && !isSpaceByte(str[j]) && str[j] != ';' && str[j] != '&' && str[j] != '|' {
			delim.WriteByte(str[j])
			j++
		}
	}
	if delim.Len() == 0 {
		return -1
	}
	bodyStart := strings.IndexByte(str[j:], '\n')
	if bodyStart < 0 {
		return -1
	}
	bodyStart += j
	closeRe := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(delim.String()) + `[ \t]*$`)
	loc := closeRe.FindStringIndex(str[bodyStart+1:])
	if loc == nil {
		return -1
	}
	return bodyStart + 1 + loc[1]
}

// tokenizeShell tokenizes a shell command into words, grouped into
// top-level segments split on &&, ||, ; and |. Quotes/backticks and
// parens (what makes $(...) / (...) atomic) suppress splitting inside
// them; a heredoc gets the same atomic treatment for its whole body.
func tokenizeShell(str string) [][]string {
	segments := [][]string{nil}
	var word strings.Builder
	var quote byte
	depth := 0
	endWord := func() {
		if word.Len() > 0 {
			last := len(segments) - 1
			segments[last] = append(segments[last], word.String())
			word.Reset()
		}
	}
	newSegment := func() { segments = append(segments, nil) }

	for i := 0; i < len(str); i++ {
		ch := str[i]
		if quote != 0 {
			word.WriteByte(ch)
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '"', '\'', '`':
			quote = ch
			word.WriteByte(ch)
			continue
		case '(':
			depth++
			word.WriteByte(ch)
			continue
		case ')':
			if depth > 0 {
				depth--
			}
			word.WriteByte(ch)
			continue
		}
		if depth > 0 {
			word.WriteByte(ch)
			continue
		}
		if ch == '<' && i+1 < len(str) && str[i+1] == '<' {
			if end := heredocEnd(str, i); end >= 0 {
				word.WriteString(str[i:end])
				i = end - 1
				continue
			}
		}
		if ch == '\\' && i+1 < len(str) && str[i+1] == '\n' {
			i++
			continue
		}
		if ch == '\n' {
			endWord()
			newSegment()
			continue
		}
		if isSpaceByte(ch) {
			endWord()
			continue
		}
		if ch == '&' && i+1 < len(str) && str[i+1] == '&' {
			endWord()
			newSegment()
			i++
			continue
		}
		if ch == '|' && i+1 < len(str) && str[i+1] == '|' {
			endWord()
			newSegment()
			i++
			continue
		}
		if ch == ';' || ch == '|' {
			endWord()
			newSegment()
			continue
		}
		word.WriteByte(ch)
	}
	endWord()
	return segments
}

var shellKeywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true,
	"case": true, "esac": true, "function": true, "select": true, "in": true, "time": true,
}

var commandNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./@:-]*$`)

func looksLikeCommandName(name string) bool {
	return !shellKeywords[name] && commandNameRe.MatchString(name)
}

var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// ExtractBashCommands names every recognizable sub-command in a Bash
// tool's `input.command` string ("git status && ls -la" -> ["git","ls"]).
// The "rtk" PreToolUse hook on this machine rewrites recognized commands
// before they execute ("git status" -> "rtk git status") — that prefix is
// unwrapped so calls attribute to the real command, not "rtk".
func ExtractBashCommands(commandStr string) []string {
	if commandStr == "" {
		return nil
	}
	var names []string
	for _, tokens := range tokenizeShell(commandStr) {
		i := 0
		for i < len(tokens) && (tokens[i] == `\` || envAssignRe.MatchString(tokens[i])) {
			i++
		}
		if i >= len(tokens) {
			continue
		}
		name := tokens[i]
		if name == "rtk" && i+1 < len(tokens) {
			name = tokens[i+1]
		}
		if name != "" && looksLikeCommandName(name) {
			names = append(names, name)
		}
	}
	return names
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd collector && go test ./internal/shelltok/... -v`
Expected: PASS (12 tests).

- [ ] **Step 5: Commit**

```bash
git add collector/internal/shelltok
git commit -m "$(cat <<'EOF'
Add shelltok package: shell tokenizer + Bash command extractor

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: `internal/state` — persisted JSON state

**Files:**
- Create: `collector/internal/state/state.go`
- Test: `collector/internal/state/state_test.go`

**Interfaces:**
- Produces: `state.Group{SessionID, RequestID, Model, Effort, GitBranch, Project string, IsSidechain bool, AtMs int64, Calls []Call}`, `state.Call{ToolUseID, ToolName string, ResultBytes int, Timestamp string, BashCommands []string, ToolSource, MCPServer, MCPTool string}`, `state.FileState{Offset int64, PendingMain, PendingSub *Group, ActiveSkillMain, ActiveSkillSub string}`, `state.PluginSkillRequest{Skill string, TsMs int64}`, `state.State{Version int, Files map[string]*FileState, SessionEmail map[string]string, Seen map[string]int64, PluginSkills map[string][]string, PluginSkillByRequest map[string]PluginSkillRequest}`, `state.Load(path string) (*State, error)`, `state.Save(path string, st *State) error`.

Ports `collector-old/transcript-scan.mjs`'s `emptyState()`/`loadState()`/
`saveState()` (lines 115-157) — dropping `otelSkills` (confirmed dead)
and `ratePublished` (now dash-generator's own state file, per the
dash-generator spec).

- [ ] **Step 1: Write the failing tests**

`collector/internal/state/state_test.go`:
```go
package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_MissingFileReturnsZeroValue(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != 2 {
		t.Errorf("Version = %d, want 2", st.Version)
	}
	if st.Files == nil || st.SessionEmail == nil || st.Seen == nil ||
		st.PluginSkills == nil || st.PluginSkillByRequest == nil {
		t.Error("all maps must be non-nil on a fresh state")
	}
}

func TestSaveThenLoad_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := &State{
		Version: 2,
		Files: map[string]*FileState{
			"/t.jsonl": {
				Offset: 42,
				PendingMain: &Group{
					SessionID: "s1", RequestID: "r1", Model: "claude", Effort: "high",
					GitBranch: "main", Project: "repo", IsSidechain: false, AtMs: 1700000000000,
					Calls: []Call{{ToolUseID: "tu1", ToolName: "Bash", ResultBytes: 10, Timestamp: "2026-01-01T00:00:00Z", BashCommands: []string{"git"}, ToolSource: "builtin"}},
				},
				ActiveSkillMain: "owner:skill",
			},
		},
		SessionEmail:         map[string]string{"s1": "a@example.com"},
		Seen:                 map[string]int64{"tu1": 1700000000000},
		PluginSkills:         map[string][]string{"s1": {"owner:skill"}},
		PluginSkillByRequest: map[string]PluginSkillRequest{"r1": {Skill: "owner:skill", TsMs: 1700000000000}},
	}
	if err := Save(path, st); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	fs := loaded.Files["/t.jsonl"]
	if fs == nil || fs.Offset != 42 || fs.PendingMain == nil || fs.PendingMain.SessionID != "s1" {
		t.Fatalf("loaded file state = %+v", fs)
	}
	if fs.PendingMain.Calls[0].ToolUseID != "tu1" || len(fs.PendingMain.Calls[0].BashCommands) != 1 {
		t.Fatalf("loaded call = %+v", fs.PendingMain.Calls[0])
	}
	if loaded.SessionEmail["s1"] != "a@example.com" {
		t.Errorf("SessionEmail = %v", loaded.SessionEmail)
	}
	if loaded.PluginSkillByRequest["r1"].Skill != "owner:skill" {
		t.Errorf("PluginSkillByRequest = %v", loaded.PluginSkillByRequest)
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "state.json")
	if err := Save(path, &State{Version: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd collector && go test ./internal/state/... -v`
Expected: FAIL — types/`Load`/`Save` undefined.

- [ ] **Step 3: Implement state.go**

`collector/internal/state/state.go`:
```go
// Package state persists transcript-scan progress between passes: per-file
// byte offsets, in-flight tool-call groups, dedup, and skill-name
// resolution — port of transcript-scan.mjs's emptyState/loadState/saveState.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Group is one assistant message's worth of tool calls, still waiting to
// be settled by the NEXT assistant message's input-token usage.
type Group struct {
	SessionID   string `json:"sessionId"`
	RequestID   string `json:"requestId"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	GitBranch   string `json:"gitBranch"`
	Project     string `json:"project"`
	IsSidechain bool   `json:"isSidechain"`
	AtMs        int64  `json:"at"`
	Calls       []Call `json:"calls"`
}

// Call is one tool_use block within a Group.
type Call struct {
	ToolUseID    string   `json:"toolUseId"`
	ToolName     string   `json:"toolName"`
	ResultBytes  int      `json:"resultBytes"`
	Timestamp    string   `json:"timestamp"`
	BashCommands []string `json:"bashCommands,omitempty"`
	ToolSource   string   `json:"toolSource"`
	MCPServer    string   `json:"mcpServer"`
	MCPTool      string   `json:"mcpTool"`
}

// FileState is per-transcript-file progress: consumed byte offset, at
// most one open group per track (main/subagent), and each track's
// currently-active skill name (sticky until the next "Skill" tool call).
type FileState struct {
	Offset          int64  `json:"offset"`
	PendingMain     *Group `json:"pendingMain,omitempty"`
	PendingSub      *Group `json:"pendingSub,omitempty"`
	ActiveSkillMain string `json:"activeSkillMain"`
	ActiveSkillSub  string `json:"activeSkillSub"`
}

// PluginSkillRequest records which plugin skill was active at the moment
// of one specific request — the precise twin of the session-level
// PluginSkills set, needed when a session used 2+ plugin skills (the
// session-level set alone can't say which request used which).
type PluginSkillRequest struct {
	Skill string `json:"skill"`
	TsMs  int64  `json:"ts"`
}

// State is transcript-scan's entire persisted state.
type State struct {
	Version              int                            `json:"version"`
	Files                map[string]*FileState          `json:"files"`
	SessionEmail         map[string]string              `json:"sessionEmail"`
	Seen                 map[string]int64               `json:"seen"`
	PluginSkills         map[string][]string             `json:"pluginSkills"`
	PluginSkillByRequest map[string]PluginSkillRequest   `json:"pluginSkillByRequest"`
}

func empty() *State {
	return &State{
		Version:              2,
		Files:                map[string]*FileState{},
		SessionEmail:         map[string]string{},
		Seen:                 map[string]int64{},
		PluginSkills:         map[string][]string{},
		PluginSkillByRequest: map[string]PluginSkillRequest{},
	}
}

// Load reads path, or returns a fresh empty State if it doesn't exist yet.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return empty(), nil
		}
		return nil, err
	}
	st := empty()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	if st.Files == nil {
		st.Files = map[string]*FileState{}
	}
	if st.SessionEmail == nil {
		st.SessionEmail = map[string]string{}
	}
	if st.Seen == nil {
		st.Seen = map[string]int64{}
	}
	if st.PluginSkills == nil {
		st.PluginSkills = map[string][]string{}
	}
	if st.PluginSkillByRequest == nil {
		st.PluginSkillByRequest = map[string]PluginSkillRequest{}
	}
	st.Version = 2
	return st, nil
}

// Save writes st to path via write-temp-then-rename, so a kill mid-write
// never leaves truncated state that would make the next pass re-scan
// everything.
func Save(path string, st *State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd collector && go test ./internal/state/... -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add collector/internal/state
git commit -m "$(cat <<'EOF'
Add state package: persisted transcript-scan progress

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: `internal/lokiclient` — log-query Loki client

**Files:**
- Create: `collector/internal/lokiclient/lokiclient.go`
- Test: `collector/internal/lokiclient/lokiclient_test.go`

**Interfaces:**
- Produces: `lokiclient.StreamResult{Labels map[string]string, Values [][2]string}`, `lokiclient.QueryRange(lokiURL, query string, startNs, endNs int64, limit int, direction string) ([]StreamResult, error)`, `lokiclient.Stream{Labels map[string]string, Values []StreamValue}`, `lokiclient.StreamValue{TimestampNs, Line string, Metadata map[string]string}`, `lokiclient.Push(lokiURL string, streams []Stream) error`.

**Not** identical to `dash-generator/internal/lokiclient` — see the
spec's correction. This client parses Loki's `"streams"` result shape
(JSON key `"stream"`, not `"metric"`), and `QueryRange` takes nanosecond
timestamps plus optional `limit`/`direction`, matching exactly what
`resolveEmails`/`seedSeenFromLoki`/`projectOwners`/`rebuildSkillRecords`
send (collector-old/transcript-scan.mjs lines 544-551 `lokiQuery`, and
each call site).

- [ ] **Step 1: Write the failing tests**

`collector/internal/lokiclient/lokiclient_test.go`:
```go
package lokiclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQueryRange_ParsesStreamsResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("limit") != "1" || q.Get("direction") != "backward" {
			t.Errorf("params = %v", q)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"user_email":"a@example.com","session_id":"s1"},"values":[["1700000000000000000","line text"]]}
		]}}`))
	}))
	defer srv.Close()

	results, err := QueryRange(srv.URL, `{service_name="claude-code"}`, 1, 2, 1, "backward")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Labels["user_email"] != "a@example.com" {
		t.Fatalf("got %+v", results)
	}
	if results[0].Values[0][1] != "line text" {
		t.Errorf("values = %v", results[0].Values)
	}
}

func TestQueryRange_OmitsLimitAndDirectionWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("limit") || q.Has("direction") {
			t.Errorf("expected no limit/direction params, got %v", q)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	if _, err := QueryRange(srv.URL, "up", 1, 2, 0, ""); err != nil {
		t.Fatal(err)
	}
}

func TestQueryRange_NonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error"}`))
	}))
	defer srv.Close()

	if _, err := QueryRange(srv.URL, "up", 1, 2, 0, ""); err == nil {
		t.Error("want error for status=error")
	}
}

func TestQueryRange_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := QueryRange(srv.URL, "up", 1, 2, 0, ""); err == nil {
		t.Error("want error for 500")
	}
}

func TestPush_SendsExpectedBody(t *testing.T) {
	var received map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/push" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := Push(srv.URL, []Stream{{
		Labels: map[string]string{"service_name": "x"},
		Values: []StreamValue{{TimestampNs: "123000000", Line: "Bash", Metadata: map[string]string{"tool_use_id": "t1"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	streams, _ := received["streams"].([]interface{})
	if len(streams) != 1 {
		t.Fatalf("received = %v", received)
	}
}

func TestPush_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad"))
	}))
	defer srv.Close()

	if err := Push(srv.URL, []Stream{{Labels: map[string]string{"a": "b"}}}); err == nil {
		t.Error("want error for 400")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd collector && go test ./internal/lokiclient/... -v`
Expected: FAIL — package has no exported symbols yet.

- [ ] **Step 3: Implement lokiclient.go**

`collector/internal/lokiclient/lokiclient.go`:
```go
// Package lokiclient talks to Loki for the collector: log-line range
// queries (Loki's "streams" result type) and pushing new log lines. Not
// the same shape as dash-generator's client — see the spec correction in
// docs/superpowers/specs/2026-09-23-collector-design.md.
package lokiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// StreamResult is one Loki log-query result group: its labels (including
// any structured-metadata fields Loki surfaces here) plus its
// (timestamp_ns, line) pairs.
type StreamResult struct {
	Labels map[string]string
	Values [][2]string
}

type queryStreamsResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// QueryRange runs a log-query range request. start/end are nanosecond
// Unix timestamps. limit <= 0 omits the "limit" param; direction == ""
// omits "direction".
func QueryRange(lokiURL, query string, startNs, endNs int64, limit int, direction string) ([]StreamResult, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(startNs, 10))
	params.Set("end", strconv.FormatInt(endNs, 10))
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if direction != "" {
		params.Set("direction", direction)
	}
	u, err := url.Parse(lokiURL)
	if err != nil {
		return nil, fmt.Errorf("invalid loki url %q: %w", lokiURL, err)
	}
	u.Path = "/loki/api/v1/query_range"
	u.RawQuery = params.Encode()

	resp, err := http.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("loki %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var qr queryStreamsResponse
	if err := json.Unmarshal(body, &qr); err != nil {
		return nil, fmt.Errorf("loki response decode: %w", err)
	}
	if qr.Status != "success" {
		return nil, fmt.Errorf("loki status=%s", qr.Status)
	}
	out := make([]StreamResult, 0, len(qr.Data.Result))
	for _, r := range qr.Data.Result {
		out = append(out, StreamResult{Labels: r.Stream, Values: r.Values})
	}
	return out, nil
}

// Stream is one Loki push stream: labels plus (timestamp_ns, line, metadata) triples.
type Stream struct {
	Labels map[string]string
	Values []StreamValue
}

// StreamValue is one pushed line.
type StreamValue struct {
	TimestampNs string
	Line        string
	Metadata    map[string]string
}

type pushBody struct {
	Streams []pushStream `json:"streams"`
}
type pushStream struct {
	Stream map[string]string `json:"stream"`
	Values []pushValue       `json:"values"`
}
type pushValue [3]interface{}

// Push posts to /loki/api/v1/push.
func Push(lokiURL string, streams []Stream) error {
	body := pushBody{Streams: make([]pushStream, 0, len(streams))}
	for _, s := range streams {
		values := make([]pushValue, 0, len(s.Values))
		for _, v := range s.Values {
			values = append(values, pushValue{v.TimestampNs, v.Line, v.Metadata})
		}
		body.Streams = append(body.Streams, pushStream{Stream: s.Labels, Values: values})
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	u, err := url.Parse(lokiURL)
	if err != nil {
		return fmt.Errorf("invalid loki url %q: %w", lokiURL, err)
	}
	u.Path = "/loki/api/v1/push"

	resp, err := http.Post(u.String(), "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("loki push %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd collector && go test ./internal/lokiclient/... -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add collector/internal/lokiclient
git commit -m "$(cat <<'EOF'
Add lokiclient package: log-query Loki client for the collector

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: `internal/transcriptscan` — discovery, tool-name parsing, token attribution, per-file scan

**Files:**
- Create: `collector/internal/transcriptscan/transcriptscan.go`
- Test: `collector/internal/transcriptscan/transcriptscan_test.go`

**Interfaces:**
- Consumes: `state.{Group,Call,FileState}` (Task 4), `shelltok.ExtractBashCommands` (Task 3).
- Produces: `transcriptscan.FindTranscripts(configDirs []string, log func(string, ...any)) ([]string, error)`, `transcriptscan.ParseToolName(name string) (source, server, tool string)`, `transcriptscan.BlockSize(block map[string]interface{}) int`, `transcriptscan.Record{TimestampNs, Line, DedupKey string, StreamLabels map[string]string, Meta map[string]string}`, `transcriptscan.Settle(group state.Group, inputTokens int64) []Record`, `transcriptscan.ProcessTranscript(path string, fs state.FileState, pluginSkills map[string]map[string]bool, pluginSkillByRequest map[string]state.PluginSkillRequest) ([]Record, state.FileState, error)`.

Ports `collector-old/transcript-scan.mjs`: `findTranscripts`/`findAllTranscripts`
(159-198), `parseToolName` (200-208), `blockSize` (351-362), `attribute`/
`settle` (364-421), `processTranscript` (423-540).

- [ ] **Step 1: Write the failing tests**

`collector/internal/transcriptscan/transcriptscan_test.go`:
```go
package transcriptscan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"claude-observability-collector/internal/state"
)

func TestFindTranscripts_WalksDepthAndFindsSubagentFiles(t *testing.T) {
	root := t.TempDir()
	// session file at depth 1, subagent file one level deeper (the
	// specific bug transcript-scan.mjs's own comments document).
	must(t, os.MkdirAll(filepath.Join(root, "projects", "proj1", "session1", "subagents"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "projects", "proj1", "session1.jsonl"), []byte("{}"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "projects", "proj1", "session1", "subagents", "sub1.jsonl"), []byte("{}"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "projects", "proj1", "notes.txt"), []byte("x"), 0o644))

	found, err := FindTranscripts([]string{filepath.Join(root, "projects")}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("got %d files, want 2 (session + subagent): %v", len(found), found)
	}
}

func TestFindTranscripts_MissingDirIsNonFatal(t *testing.T) {
	found, err := FindTranscripts([]string{filepath.Join(t.TempDir(), "nope")}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("got %v, want empty", found)
	}
}

func TestParseToolName(t *testing.T) {
	cases := []struct {
		name, wantSource, wantServer, wantTool string
	}{
		{"mcp__github__search_code", "mcp", "github", "search_code"},
		{"mcp__github", "mcp", "github", ""},
		{"Bash", "builtin", "", ""},
		{"", "builtin", "", ""},
	}
	for _, c := range cases {
		source, server, tool := ParseToolName(c.name)
		if source != c.wantSource || server != c.wantServer || tool != c.wantTool {
			t.Errorf("ParseToolName(%q) = %q,%q,%q want %q,%q,%q", c.name, source, server, tool, c.wantSource, c.wantServer, c.wantTool)
		}
	}
}

func TestParseToolName_SplitsOnlyOnFirstDoubleUnderscore(t *testing.T) {
	// The tool name itself may legitimately contain "__".
	_, server, tool := ParseToolName("mcp__github__search__code")
	if server != "github" || tool != "search__code" {
		t.Errorf("server=%q tool=%q, want github, search__code", server, tool)
	}
}

func TestBlockSize_StringContent(t *testing.T) {
	block := map[string]interface{}{"content": "hello"}
	if got := BlockSize(block); got != 5 {
		t.Errorf("got %d, want 5", got)
	}
}

func TestBlockSize_ArrayContentMixedParts(t *testing.T) {
	block := map[string]interface{}{"content": []interface{}{
		"abc",
		map[string]interface{}{"type": "text", "text": "de"},
		map[string]interface{}{"type": "other", "foo": "bar"},
	}}
	// "abc" (3) + "de" (2) + JSON-encoded {"foo":"bar","type":"other"} length.
	other := map[string]interface{}{"type": "other", "foo": "bar"}
	encoded, _ := json.Marshal(other)
	want := 3 + 2 + len(encoded)
	if got := BlockSize(block); got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

func TestBlockSize_NilContentFallsBackToEmptyStringJSON(t *testing.T) {
	block := map[string]interface{}{}
	if got := BlockSize(block); got != 2 { // json.Marshal("") == `""`
		t.Errorf("got %d, want 2", got)
	}
}

func TestSettle_ProportionalSplitAcrossParallelCalls(t *testing.T) {
	group := state.Group{
		SessionID: "s1", RequestID: "r1", Model: "claude", IsSidechain: false,
		Calls: []state.Call{
			{ToolUseID: "a", ToolName: "Read", ResultBytes: 30, Timestamp: "2026-01-01T00:00:00Z", ToolSource: "builtin"},
			{ToolUseID: "b", ToolName: "Read", ResultBytes: 10, Timestamp: "2026-01-01T00:00:00Z", ToolSource: "builtin"},
		},
	}
	records := Settle(group, 400)
	if len(records) != 2 {
		t.Fatalf("got %d records", len(records))
	}
	byID := map[string]Record{}
	for _, r := range records {
		byID[r.Meta["tool_use_id"]] = r
	}
	if byID["a"].Meta["tokens_attributed"] != "300" { // 400 * 30/40
		t.Errorf("call a tokens_attributed = %v", byID["a"].Meta["tokens_attributed"])
	}
	if byID["b"].Meta["tokens_attributed"] != "100" { // 400 * 10/40
		t.Errorf("call b tokens_attributed = %v", byID["b"].Meta["tokens_attributed"])
	}
}

func TestSettle_EvenSplitWhenAllResultsEmpty(t *testing.T) {
	group := state.Group{
		SessionID: "s1", Calls: []state.Call{
			{ToolUseID: "a", ToolName: "Read", ResultBytes: 0, Timestamp: "2026-01-01T00:00:00Z"},
			{ToolUseID: "b", ToolName: "Read", ResultBytes: 0, Timestamp: "2026-01-01T00:00:00Z"},
		},
	}
	records := Settle(group, 100)
	for _, r := range records {
		if r.Meta["tokens_attributed"] != "50" {
			t.Errorf("tokens_attributed = %v, want 50 (even split)", r.Meta["tokens_attributed"])
		}
	}
}

func TestSettle_BashFanoutSharesTokensAndHasDistinctDedupKeys(t *testing.T) {
	group := state.Group{
		SessionID: "s1", Calls: []state.Call{
			{ToolUseID: "bash1", ToolName: "Bash", ResultBytes: 100, Timestamp: "2026-01-01T00:00:00Z",
				BashCommands: []string{"git", "ls"}, ToolSource: "builtin"},
		},
	}
	records := Settle(group, 200)
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (one per sub-command)", len(records))
	}
	if records[0].Meta["tokens_attributed"] != "200" || records[1].Meta["tokens_attributed"] != "200" {
		t.Error("each fanout row must reuse the full call's tokens_attributed, not split further")
	}
	if records[0].DedupKey != "bash:bash1:0" || records[1].DedupKey != "bash:bash1:1" {
		t.Errorf("dedup keys = %q, %q", records[0].DedupKey, records[1].DedupKey)
	}
	if records[0].Meta["mcp_server"] != "bash" || records[0].Meta["mcp_tool"] != "git" {
		t.Errorf("meta = %v", records[0].Meta)
	}
}

func writeJSONL(t *testing.T, path string, lines []string) {
	t.Helper()
	var content string
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestProcessTranscript_SettlesOnNextAssistantMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	writeJSONL(t, path, []string{
		`{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":"2026-01-01T00:00:00Z","message":{"model":"claude","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"git status"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}`,
		`{"type":"assistant","sessionId":"s1","requestId":"r2","timestamp":"2026-01-01T00:01:00Z","message":{"model":"claude","usage":{"input_tokens":100,"cache_creation_input_tokens":0},"content":[]}}`,
	})

	records, fs, err := ProcessTranscript(path, state.FileState{}, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1 (settled by the second assistant message)", len(records))
	}
	if records[0].Meta["tokens_attributed"] != "100" {
		t.Errorf("tokens_attributed = %v", records[0].Meta["tokens_attributed"])
	}
	if fs.PendingMain != nil {
		t.Error("pending group should be cleared after settling")
	}
}

func TestProcessTranscript_ResumesFromOffsetAcrossTwoPasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	line1 := `{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":"2026-01-01T00:00:00Z","message":{"model":"claude","usage":{"input_tokens":10},"content":[]}}`
	writeJSONL(t, path, []string{line1})

	// A line's bytes are only folded into the consumed offset once the
	// NEXT line proves it ended in "\n" — with only one line in the file,
	// pass 1 must NOT advance the offset yet, even though this line does
	// have a real trailing newline (the algorithm can't tell the
	// difference without a following line; see ProcessTranscript's doc
	// comment).
	_, fs1, err := ProcessTranscript(path, state.FileState{}, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if fs1.Offset != 0 {
		t.Fatalf("offset after pass 1 = %d, want 0 (line 1's bytes aren't proven consumed yet)", fs1.Offset)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	must(t, err)
	line2 := `{"type":"assistant","sessionId":"s1","requestId":"r2","timestamp":"2026-01-01T00:01:00Z","message":{"model":"claude","content":[{"type":"tool_use","id":"tu2","name":"Read"}]}}`
	f.WriteString(line2 + "\n")
	f.Close()

	// Pass 2 re-reads from offset 0 (line 1 is re-parsed — harmless here,
	// it had no tool_use and nothing was pending to settle), and NOW
	// line 1's bytes fold into the offset once line 2 is seen after it.
	// Line 2 itself is the new last line, so ITS bytes stay pending in
	// turn — the same held-back-last-line rule applying again, one line
	// later.
	records2, fs2, err := ProcessTranscript(path, fs1, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records2) != 0 {
		t.Fatalf("got %d records on pass 2, want 0 (tu2 not yet settled)", len(records2))
	}
	if fs2.Offset != int64(len(line1)+1) {
		t.Errorf("offset after pass 2 = %d, want %d (line 1 now proven consumed)", fs2.Offset, len(line1)+1)
	}
	if fs2.PendingMain == nil || fs2.PendingMain.RequestID != "r2" {
		t.Fatalf("pending after pass 2 = %+v", fs2.PendingMain)
	}
}

func TestProcessTranscript_ShrunkFileResetsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	writeJSONL(t, path, []string{`{"type":"assistant","sessionId":"s1","timestamp":"2026-01-01T00:00:00Z","message":{"model":"claude","content":[{"type":"tool_use","id":"tu1","name":"Read"}]}}`})
	info, _ := os.Stat(path)
	bigOffset := info.Size() + 1000

	// After the shrink reset, the file gets properly re-scanned from
	// offset 0 — its one real line DOES have an unsettled tool_use, so a
	// fresh pending group is legitimately (re)built. What must be true is
	// that it's the REAL group (session "s1"), not the stale leftover
	// ("stale") the caller passed in.
	_, fs, err := ProcessTranscript(path, state.FileState{Offset: bigOffset, PendingMain: &state.Group{SessionID: "stale"}}, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if fs.PendingMain == nil || fs.PendingMain.SessionID != "s1" {
		t.Errorf("pending after shrink+rescan = %+v, want a fresh group from session s1, not the stale one", fs.PendingMain)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd collector && go test ./internal/transcriptscan/... -v`
Expected: FAIL — package has no exported symbols yet.

- [ ] **Step 3: Implement transcriptscan.go**

`collector/internal/transcriptscan/transcriptscan.go`:
```go
// Package transcriptscan ports transcript-scan.mjs: exports Claude Code
// tool calls from local transcripts into Loki, attributing tokens and
// recovering the real MCP/skill/bash-command names OTel redacts.
package transcriptscan

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"claude-observability-collector/internal/shelltok"
	"claude-observability-collector/internal/state"
)

// Record is one line ready to push to Loki — either a tool record
// (StreamLabels nil, caller applies the default tools-stream labels) or a
// skill record (StreamLabels set explicitly to the skills stream).
type Record struct {
	TimestampNs  string
	Line         string
	DedupKey     string
	StreamLabels map[string]string
	Meta         map[string]string
}

// FindTranscripts walks every configDir recursively (depth <= 6),
// collecting every *.jsonl file — including "<session>/subagents/*.jsonl"
// one level deeper than session files. A missing configDir is logged and
// skipped, not fatal (matches collector-old/transcript-scan.mjs:176-178).
func FindTranscripts(configDirs []string, log func(string, ...any)) ([]string, error) {
	var all []string
	for _, dir := range configDirs {
		found, err := findTranscriptsRec(dir, 0, log)
		if err != nil {
			return nil, err
		}
		all = append(all, found...)
	}
	return all, nil
}

func findTranscriptsRec(dir string, depth int, log func(string, ...any)) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if depth == 0 {
				log("transcript directory does not exist: %s", dir)
			}
			return nil, nil
		}
		return nil, err
	}
	var found []string
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if depth < 6 {
				sub, err := findTranscriptsRec(full, depth+1, log)
				if err != nil {
					return nil, err
				}
				found = append(found, sub...)
			}
		} else if strings.HasSuffix(e.Name(), ".jsonl") {
			found = append(found, full)
		}
	}
	return found, nil
}

// ParseToolName splits only on the FIRST "__" after "mcp__" (the tool
// name itself may legitimately contain "__").
func ParseToolName(name string) (source, server, tool string) {
	if !strings.HasPrefix(name, "mcp__") {
		return "builtin", "", ""
	}
	rest := name[len("mcp__"):]
	sep := strings.Index(rest, "__")
	if sep < 0 {
		return "mcp", rest, ""
	}
	return "mcp", rest[:sep], rest[sep+2:]
}

// BlockSize measures a tool_result content block the same way the JS
// does: string content by length, array content by summing string parts'
// length / {type:"text"} parts' text length / JSON-encoded length of
// anything else, and anything else by JSON-encoded length. Uses Go's
// byte length, not JS's UTF-16 code-unit length — see Global Constraints.
func BlockSize(block map[string]interface{}) int {
	content, ok := block["content"]
	if !ok || content == nil {
		encoded, _ := json.Marshal("")
		return len(encoded)
	}
	switch c := content.(type) {
	case string:
		return len(c)
	case []interface{}:
		sum := 0
		for _, part := range c {
			switch p := part.(type) {
			case string:
				sum += len(p)
			case map[string]interface{}:
				if p["type"] == "text" {
					if text, ok := p["text"].(string); ok {
						sum += len(text)
					}
					continue
				}
				encoded, _ := json.Marshal(p)
				sum += len(encoded)
			default:
				encoded, _ := json.Marshal(p)
				sum += len(encoded)
			}
		}
		return sum
	default:
		encoded, _ := json.Marshal(content)
		return len(encoded)
	}
}

func toTimestampNs(timestamp string) string {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "0000000000000"
	}
	return strconv.FormatInt(t.UnixMilli(), 10) + "000000"
}

func parseTimestampMs(timestamp string) int64 {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// attribute computes one call's share of the group's input tokens, and
// the meta fields every record for that call shares regardless of how
// many rows it fans out into (mcp_server/mcp_tool are added by the
// caller, per-row, since a Bash fanout gives each row a different one).
func attribute(call state.Call, group state.Group, inputTokens, total int64) map[string]string {
	var tokensAttributed int64
	if total > 0 {
		tokensAttributed = int64(math.Round(float64(inputTokens) * float64(call.ResultBytes) / float64(total)))
	} else if len(group.Calls) > 0 {
		tokensAttributed = int64(math.Round(float64(inputTokens) / float64(len(group.Calls))))
	}
	querySource := "main"
	if group.IsSidechain {
		querySource = "subagent"
	}
	return map[string]string{
		"session_id":        group.SessionID,
		"request_id":        group.RequestID,
		"tool_use_id":       call.ToolUseID,
		"tool_name":         call.ToolName,
		"tool_source":       call.ToolSource,
		"model":             group.Model,
		"effort":            group.Effort,
		"query_source":      querySource,
		"git_branch":        group.GitBranch,
		"project":           group.Project,
		"result_bytes":      strconv.Itoa(call.ResultBytes),
		"tokens_attributed": strconv.FormatInt(tokensAttributed, 10),
	}
}

// Settle attributes the group's input tokens to its tool calls,
// proportionally to each one's result size (or evenly if every result was
// empty). A Bash call with recognized sub-commands fans out into one row
// per sub-command, each reusing the SAME tokens_attributed/result_bytes —
// token cost belongs to the LLM turn, not a proportional split within it.
func Settle(group state.Group, inputTokens int64) []Record {
	var total int64
	for _, c := range group.Calls {
		total += int64(c.ResultBytes)
	}
	var out []Record
	for _, call := range group.Calls {
		timestampNs := toTimestampNs(call.Timestamp)
		meta := attribute(call, group, inputTokens, total)
		if len(call.BashCommands) == 0 {
			m := cloneMeta(meta)
			m["mcp_server"] = call.MCPServer
			m["mcp_tool"] = call.MCPTool
			out = append(out, Record{TimestampNs: timestampNs, Line: call.ToolName, Meta: m})
			continue
		}
		for i, name := range call.BashCommands {
			m := cloneMeta(meta)
			m["mcp_server"] = "bash"
			m["mcp_tool"] = name
			out = append(out, Record{
				TimestampNs: timestampNs, Line: call.ToolName,
				DedupKey: fmt.Sprintf("bash:%s:%d", call.ToolUseID, i),
				Meta:     m,
			})
		}
	}
	return out
}

func cloneMeta(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func asFloat(v interface{}) float64 {
	f, _ := v.(float64)
	return f
}

// ProcessTranscript reads path starting at fs.Offset, line by line, and
// returns newly-settled records plus the updated FileState. A line's byte
// length is only folded into the consumed count once the NEXT line is
// seen (proving the previous one ended in "\n") — Claude Code writes to
// these files concurrently, so the final line commonly has no trailing
// newline yet, and counting it as consumed would skip the first byte
// written next. If the file shrank since fs.Offset (truncated/replaced),
// everything resets.
func ProcessTranscript(path string, fs state.FileState, pluginSkills map[string]map[string]bool, pluginSkillByRequest map[string]state.PluginSkillRequest) ([]Record, state.FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, state.FileState{}, err
	}
	size := info.Size()
	offset := fs.Offset
	pendingMain, pendingSub := fs.PendingMain, fs.PendingSub
	activeSkillMain, activeSkillSub := fs.ActiveSkillMain, fs.ActiveSkillSub

	if size < offset {
		offset = 0
		pendingMain, pendingSub = nil, nil
		activeSkillMain, activeSkillSub = "", ""
	}
	if size == offset {
		return nil, state.FileState{
			Offset: offset, PendingMain: pendingMain, PendingSub: pendingSub,
			ActiveSkillMain: activeSkillMain, ActiveSkillSub: activeSkillSub,
		}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, state.FileState{}, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, state.FileState{}, err
	}

	var records []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var consumed, pendente int64

	for scanner.Scan() {
		line := scanner.Text()
		consumed += pendente
		pendente = int64(len(line)) + 1

		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		message, _ := entry["message"].(map[string]interface{})
		if message == nil {
			continue
		}
		content, _ := message["content"].([]interface{})
		isSidechain, _ := entry["isSidechain"].(bool)
		track := "main"
		if isSidechain {
			track = "sub"
		}
		entryType, _ := entry["type"].(string)
		sessionID, _ := entry["sessionId"].(string)
		requestID, _ := entry["requestId"].(string)
		timestamp, _ := entry["timestamp"].(string)

		if entryType == "assistant" {
			usage, hasUsage := message["usage"].(map[string]interface{})
			activeSkill := activeSkillMain
			pending := pendingMain
			if track == "sub" {
				activeSkill = activeSkillSub
				pending = pendingSub
			}

			if sessionID != "" && strings.Contains(activeSkill, ":") {
				set := pluginSkills[sessionID]
				if set == nil {
					set = map[string]bool{}
					pluginSkills[sessionID] = set
				}
				set[activeSkill] = true
				if requestID != "" {
					ts := parseTimestampMs(timestamp)
					if ts == 0 {
						ts = time.Now().UnixMilli()
					}
					pluginSkillByRequest[requestID] = state.PluginSkillRequest{Skill: activeSkill, TsMs: ts}
				}
			}

			if hasUsage && pending != nil {
				inputTokens := int64(asFloat(usage["input_tokens"])) + int64(asFloat(usage["cache_creation_input_tokens"]))
				records = append(records, Settle(*pending, inputTokens)...)
				pending = nil
			}

			var calls []state.Call
			for _, blockRaw := range content {
				block, ok := blockRaw.(map[string]interface{})
				if !ok || block["type"] != "tool_use" {
					continue
				}
				name, _ := block["name"].(string)
				id, _ := block["id"].(string)
				source, server, tool := ParseToolName(name)
				var bashCommands []string
				if name == "Bash" {
					input, _ := block["input"].(map[string]interface{})
					cmd, _ := input["command"].(string)
					bashCommands = shelltok.ExtractBashCommands(cmd)
				}
				calls = append(calls, state.Call{
					ToolUseID: id, ToolName: name, ResultBytes: 0, Timestamp: timestamp,
					BashCommands: bashCommands, ToolSource: source, MCPServer: server, MCPTool: tool,
				})
			}
			for _, blockRaw := range content {
				block, ok := blockRaw.(map[string]interface{})
				if !ok || block["type"] != "tool_use" || block["name"] != "Skill" {
					continue
				}
				input, _ := block["input"].(map[string]interface{})
				if skill, ok := input["skill"].(string); ok && skill != "" {
					activeSkill = skill
				}
			}

			if len(calls) > 0 {
				project := ""
				if cwd, ok := entry["cwd"].(string); ok && cwd != "" {
					project = filepath.Base(cwd)
				}
				effort, _ := entry["perTurnEffort"].(string)
				if effort == "" {
					effort, _ = entry["effort"].(string)
				}
				gitBranch, _ := entry["gitBranch"].(string)
				model, _ := message["model"].(string)
				at := parseTimestampMs(timestamp)
				if at == 0 {
					at = time.Now().UnixMilli()
				}
				pending = &state.Group{
					SessionID: sessionID, RequestID: requestID, Model: model, Effort: effort,
					GitBranch: gitBranch, Project: project, IsSidechain: isSidechain, AtMs: at, Calls: calls,
				}
			}

			if track == "sub" {
				activeSkillSub, pendingSub = activeSkill, pending
			} else {
				activeSkillMain, pendingMain = activeSkill, pending
			}
		} else if entryType == "user" {
			pending := pendingMain
			if track == "sub" {
				pending = pendingSub
			}
			if pending != nil {
				for _, blockRaw := range content {
					block, ok := blockRaw.(map[string]interface{})
					if !ok || block["type"] != "tool_result" {
						continue
					}
					toolUseID, _ := block["tool_use_id"].(string)
					for i := range pending.Calls {
						if pending.Calls[i].ToolUseID == toolUseID {
							pending.Calls[i].ResultBytes = BlockSize(block)
							break
						}
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, state.FileState{}, err
	}

	return records, state.FileState{
		Offset: offset + consumed, PendingMain: pendingMain, PendingSub: pendingSub,
		ActiveSkillMain: activeSkillMain, ActiveSkillSub: activeSkillSub,
	}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd collector && go test ./internal/transcriptscan/... -v`
Expected: PASS (13 tests).

- [ ] **Step 5: Commit**

```bash
git add collector/internal/transcriptscan
git commit -m "$(cat <<'EOF'
Add transcriptscan core: discovery, tool-name parsing, token attribution

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: `internal/transcriptscan` — email resolution and dedup

**Files:**
- Modify: `collector/internal/transcriptscan/transcriptscan.go`
- Modify: `collector/internal/transcriptscan/transcriptscan_test.go`

**Interfaces:**
- Consumes: `lokiclient.QueryRange` (Task 5).
- Produces: `transcriptscan.ResolveEmails(lokiURL string, emailLookbackHours int, sessionEmail map[string]string, sessionIDs []string, log func(string, ...any))`, `transcriptscan.ProjectOwners(lokiURL, exporterStream string, dedupDays int, batch []Record) (map[string]string, error)`, `transcriptscan.DropAlreadySeen(seen map[string]int64, dedupDays int, records []Record) (fresh []Record, novos map[string]int64)`.

Ports `collector-old/transcript-scan.mjs`: `resolveEmails` (553-578),
`projectOwners` (619-661), `dropAlreadySeen` (836-856).

- [ ] **Step 1: Add the failing tests**

Append to `collector/internal/transcriptscan/transcriptscan_test.go` (add
`net/http`, `net/http/httptest`, `strings` to imports):
```go
func TestResolveEmails_ResolvesAndCachesEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.Contains(q, "s1") {
			w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
				{"stream":{"user_email":"a@example.com"},"values":[["1","x"]]}
			]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	sessionEmail := map[string]string{}
	ResolveEmails(srv.URL, 720, sessionEmail, []string{"s1", "s2"}, t.Logf)
	if sessionEmail["s1"] != "a@example.com" {
		t.Errorf("s1 = %q", sessionEmail["s1"])
	}
	if _, ok := sessionEmail["s2"]; !ok {
		t.Error("s2 must be cached as empty, not left unresolved (so it's never re-queried)")
	}
	if sessionEmail["s2"] != "" {
		t.Errorf("s2 = %q, want empty", sessionEmail["s2"])
	}
}

func TestResolveEmails_SkipsAlreadyCachedSessions(t *testing.T) {
	queried := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queried = true
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	sessionEmail := map[string]string{"s1": "cached@example.com"}
	ResolveEmails(srv.URL, 720, sessionEmail, []string{"s1"}, t.Logf)
	if queried {
		t.Error("must not re-query an already-cached session")
	}
}

func TestProjectOwners_OnlyMapsUnambiguousProjects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"project":"solo","user_email":"a@example.com"},"values":[["1","x"]]},
			{"stream":{"project":"shared","user_email":"a@example.com"},"values":[["1","x"]]},
			{"stream":{"project":"shared","user_email":"b@example.com"},"values":[["1","x"]]}
		]}}`))
	}))
	defer srv.Close()

	owners, err := ProjectOwners(srv.URL, "s", 90, nil)
	if err != nil {
		t.Fatal(err)
	}
	if owners["solo"] != "a@example.com" {
		t.Errorf("solo = %q", owners["solo"])
	}
	if _, ok := owners["shared"]; ok {
		t.Error("a project with 2+ distinct emails must be left unmapped")
	}
}

func TestProjectOwners_IncludesCurrentBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	batch := []Record{{Meta: map[string]string{"project": "fresh", "user_email": "a@example.com"}}}
	owners, err := ProjectOwners(srv.URL, "s", 90, batch)
	if err != nil {
		t.Fatal(err)
	}
	if owners["fresh"] != "a@example.com" {
		t.Errorf("owners = %v, want the batch-only project mapped", owners)
	}
}

func TestDropAlreadySeen_FiltersCrossBatchAndSameBatchDuplicates(t *testing.T) {
	seen := map[string]int64{"already": time.Now().UnixMilli()}
	records := []Record{
		{Meta: map[string]string{"tool_use_id": "already"}},
		{Meta: map[string]string{"tool_use_id": "fresh1"}},
		{Meta: map[string]string{"tool_use_id": "fresh1"}}, // duplicate within this same batch
	}
	fresh, novos := DropAlreadySeen(seen, 90, records)
	if len(fresh) != 1 {
		t.Fatalf("got %d fresh, want 1", len(fresh))
	}
	if _, ok := novos["fresh1"]; !ok {
		t.Error("novos must stage the newly-seen key")
	}
}

func TestDropAlreadySeen_PrunesEntriesOlderThanDedupDays(t *testing.T) {
	oldMs := time.Now().AddDate(0, 0, -100).UnixMilli()
	seen := map[string]int64{"stale": oldMs}
	DropAlreadySeen(seen, 90, nil)
	if _, ok := seen["stale"]; ok {
		t.Error("entries older than dedupDays must be pruned")
	}
}

func TestDropAlreadySeen_RecordWithNoIDAlwaysPasses(t *testing.T) {
	fresh, _ := DropAlreadySeen(map[string]int64{}, 90, []Record{{Meta: map[string]string{}}})
	if len(fresh) != 1 {
		t.Error("a record with neither dedupKey nor tool_use_id must always pass through")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd collector && go test ./internal/transcriptscan/... -v`
Expected: FAIL — `ResolveEmails`/`ProjectOwners`/`DropAlreadySeen` undefined.

- [ ] **Step 3: Implement the three functions**

Append to `collector/internal/transcriptscan/transcriptscan.go` (add
`"claude-observability-collector/internal/lokiclient"` to imports):
```go
// ResolveEmails fills sessionEmail (in place) for every sessionID in
// sessionIDs not already cached, by querying the session's own OTel data
// for the most recent user_email it carries. A session with no match is
// cached as "" too — otherwise every pass re-queries sessions that never
// sent OTel telemetry at all.
func ResolveEmails(lokiURL string, emailLookbackHours int, sessionEmail map[string]string, sessionIDs []string, log func(string, ...any)) {
	var missing []string
	for _, id := range sessionIDs {
		if id == "" {
			continue
		}
		if _, ok := sessionEmail[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return
	}
	endNs := time.Now().UnixNano()
	startNs := endNs - int64(emailLookbackHours)*3600*1e9
	for _, sessionID := range missing {
		query := fmt.Sprintf("{service_name=\"claude-code\"} | session_id = `%s` | user_email != ``", sessionID)
		results, err := lokiclient.QueryRange(lokiURL, query, startNs, endNs, 1, "backward")
		if err != nil {
			short := sessionID
			if len(short) > 8 {
				short = short[:8]
			}
			log("could not resolve the account for session %s: %v", short, err)
			continue
		}
		email := ""
		if len(results) > 0 {
			email = results[0].Labels["user_email"]
		}
		sessionEmail[sessionID] = email
	}
}

// ProjectOwners builds a project->email map from both Loki history and
// the current batch (critical for a from-scratch import, where Loki
// history is empty). A project maps to an email only if every observed
// pairing used the same single email — ambiguous projects are left
// unmapped.
func ProjectOwners(lokiURL, exporterStream string, dedupDays int, batch []Record) (map[string]string, error) {
	endNs := time.Now().UnixNano()
	startNs := endNs - int64(dedupDays)*24*3600*1e9
	query := fmt.Sprintf(`{service_name="%s", kind="tools"}`, exporterStream)
	results, err := lokiclient.QueryRange(lokiURL, query, startNs, endNs, 5000, "backward")
	if err != nil {
		results = nil // fall back to batch-only, matching the JS's try/catch
	}

	seen := map[string]map[string]bool{}
	note := func(project, email string) {
		if project == "" || email == "" {
			return
		}
		if seen[project] == nil {
			seen[project] = map[string]bool{}
		}
		seen[project][email] = true
	}
	for _, r := range results {
		note(r.Labels["project"], r.Labels["user_email"])
	}
	for _, record := range batch {
		note(record.Meta["project"], record.Meta["user_email"])
	}

	owners := map[string]string{}
	for project, emails := range seen {
		if len(emails) == 1 {
			for email := range emails {
				owners[project] = email
			}
		}
	}
	return owners, nil
}

// DropAlreadySeen prunes seen entries older than dedupDays, then splits
// records into fresh (not previously exported, and not a duplicate
// within this same batch) and the staged dedup keys (novos) the caller
// commits to seen only after a successful push. A record with neither
// DedupKey nor a tool_use_id in Meta always passes through.
func DropAlreadySeen(seen map[string]int64, dedupDays int, records []Record) (fresh []Record, novos map[string]int64) {
	cutoff := time.Now().Add(-time.Duration(dedupDays) * 24 * time.Hour).UnixMilli()
	for id, ts := range seen {
		if ts < cutoff {
			delete(seen, id)
		}
	}
	novos = map[string]int64{}
	for _, record := range records {
		id := record.DedupKey
		if id == "" {
			id = record.Meta["tool_use_id"]
		}
		if id == "" {
			fresh = append(fresh, record)
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		if _, ok := novos[id]; ok {
			continue
		}
		ts, _ := strconv.ParseInt(record.TimestampNs, 10, 64)
		novos[id] = ts / 1e6
		fresh = append(fresh, record)
	}
	return fresh, novos
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd collector && go test ./internal/transcriptscan/... -v`
Expected: PASS (19 tests total).

- [ ] **Step 5: Commit**

```bash
git add collector/internal/transcriptscan
git commit -m "$(cat <<'EOF'
Add transcriptscan email resolution and dedup

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: `internal/transcriptscan` — `RebuildSkillRecords`

**Files:**
- Modify: `collector/internal/transcriptscan/transcriptscan.go`
- Modify: `collector/internal/transcriptscan/transcriptscan_test.go`

**Interfaces:**
- Consumes: `lokiclient.QueryRange` (Task 5).
- Produces: `transcriptscan.RebuildSkillRecords(lokiURL, exporterStream string, dedupDays int, pluginSkills map[string]map[string]bool, pluginSkillByRequest map[string]state.PluginSkillRequest, fullHistory bool, log func(string, ...any)) ([]Record, error)`.

Ports `collector-old/transcript-scan.mjs` lines 663-760. Re-reads OTel's
own `api_request` events (not transcript data — OTel's per-skill token
counts are the source of truth; transcript-based attribution measured
87-100% off on some skills since subagents/rotated sessions never appear
in transcripts). The one subtlety not in the earlier design doc: a
saturated 5000-line window comes back truncated in silence (this is how
39% of the volume went missing once, per the JS's own comment) — a
window at/over the limit is split in half and retried, recursively, up
to depth 12 or until the window is under 60 seconds wide.

- [ ] **Step 1: Add the failing tests**

Append to `collector/internal/transcriptscan/transcriptscan_test.go`:
```go
func TestRebuildSkillRecords_PerRequestOverridesThirdParty(t *testing.T) {
	// fullHistory=false makes RebuildSkillRecords loop 2 day-windows; a
	// real Loki would only return this event within its true window, but
	// this fixture doesn't filter by start/end, so it only answers the
	// FIRST call to avoid an artificial duplicate.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) > 1 {
			w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"session_id":"s1","request_id":"r1","skill_name":"third-party","model":"claude","input_tokens":"10","output_tokens":"20","cache_creation_tokens":"0"},"values":[["1700000000000000000","x"]]}
		]}}`))
	}))
	defer srv.Close()

	pluginSkills := map[string]map[string]bool{"s1": {"superpowers:brainstorming": true, "other:thing": true}}
	pluginSkillByRequest := map[string]state.PluginSkillRequest{"r1": {Skill: "superpowers:brainstorming", TsMs: 1}}

	records, err := RebuildSkillRecords(srv.URL, "claude-code-exporter-1", 90, pluginSkills, pluginSkillByRequest, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records", len(records))
	}
	if records[0].Line != "superpowers:brainstorming" {
		t.Errorf("skill = %q, want the per-request resolved name (ambiguous session-level set correctly bypassed)", records[0].Line)
	}
	if records[0].Meta["skill_owner"] != "superpowers" {
		t.Errorf("owner = %q", records[0].Meta["skill_owner"])
	}
	if records[0].Meta["tokens"] != "30" {
		t.Errorf("tokens = %q, want 30 (10+20+0)", records[0].Meta["tokens"])
	}
	if records[0].DedupKey != "skill:r1" {
		t.Errorf("dedupKey = %q", records[0].DedupKey)
	}
}

func TestRebuildSkillRecords_SessionLevelFallbackWhenUnambiguous(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) > 1 {
			w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"session_id":"s1","request_id":"r-unseen","skill_name":"third-party"},"values":[["1700000000000000000","x"]]}
		]}}`))
	}))
	defer srv.Close()

	pluginSkills := map[string]map[string]bool{"s1": {"owner:only-one": true}}
	records, err := RebuildSkillRecords(srv.URL, "s", 90, pluginSkills, map[string]state.PluginSkillRequest{}, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Line != "owner:only-one" {
		t.Fatalf("got %+v, want session-level unambiguous fallback", records)
	}
}

func TestRebuildSkillRecords_NonThirdPartyUsesOTelNameAsIs(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) > 1 {
			w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"session_id":"s1","request_id":"r1","skill_name":"local-skill"},"values":[["1700000000000000000","x"]]}
		]}}`))
	}))
	defer srv.Close()

	records, err := RebuildSkillRecords(srv.URL, "s", 90, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{}, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Line != "local-skill" || records[0].Meta["skill_owner"] != "local" {
		t.Fatalf("got %+v", records)
	}
}

func TestRebuildSkillRecords_SplitsSaturatedWindow(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		start := r.URL.Query().Get("start")
		end := r.URL.Query().Get("end")
		startNs, _ := strconv.ParseInt(start, 10, 64)
		endNs, _ := strconv.ParseInt(end, 10, 64)
		// The first call spans a full day (way over 60s) — return exactly
		// 5000 lines to force a split. Recursive halves return few lines.
		if endNs-startNs > 60_000*1e6 {
			values := make([]string, 5000)
			for i := range values {
				values[i] = fmt.Sprintf(`["%d","x"]`, startNs+int64(i))
			}
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[
				{"stream":{"session_id":"s1","request_id":"r-full","skill_name":"local"},"values":[%s]}
			]}}`, strings.Join(values, ","))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"session_id":"s1","request_id":"r-half","skill_name":"local"},"values":[["` + start + `","x"]]}
		]}}`))
	}))
	defer srv.Close()

	_, err := RebuildSkillRecords(srv.URL, "s", 1, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{}, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if calls < 3 {
		t.Errorf("got %d Loki calls, want the saturated window to have split into at least 2 sub-windows (3+ total calls)", calls)
	}
}
```

Add `"fmt"` and `"sync/atomic"` to the test file's imports (`"strconv"` is
already present from earlier tasks). `sync/atomic` is needed by the
call-counter pattern in three of the tests above: `RebuildSkillRecords`
genuinely loops 2 day-windows when `fullHistory=false`, and since these
fixtures don't filter by the requested start/end (unlike a real Loki,
which would only return an event within its true window), they only
answer the first call and return empty afterward, to avoid an artificial
duplicate that the real implementation wouldn't produce against real data.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd collector && go test ./internal/transcriptscan/... -v`
Expected: FAIL — `RebuildSkillRecords` undefined.

- [ ] **Step 3: Implement RebuildSkillRecords**

Append to `collector/internal/transcriptscan/transcriptscan.go`:
```go
const skillFetchLimit = 5000

// fetchSkillWindow queries one time window for skill-tagged api_request
// events. Loki caps the response at skillFetchLimit and a saturated
// window comes back truncated in silence — when a window is at/over the
// limit, it's split in half and retried (depth-limited, and never below
// 60s wide, to bound the recursion).
func fetchSkillWindow(lokiURL, startNs, endNs int64, depth int, log func(string, ...any)) []lokiclient.StreamResult {
	results, err := lokiclient.QueryRange(lokiURL,
		"{service_name=\"claude-code\"} | event_name = `api_request` | skill_name != ``",
		startNs, endNs, skillFetchLimit, "")
	if err != nil {
		log("OTel skills unavailable for that window: %v", err)
		return nil
	}
	total := 0
	for _, r := range results {
		total += len(r.Values)
	}
	if total >= skillFetchLimit && endNs-startNs > 60_000*1_000_000 && depth < 12 {
		mid := (startNs + endNs) / 2
		out := fetchSkillWindow(lokiURL, startNs, mid, depth+1, log)
		out = append(out, fetchSkillWindow(lokiURL, mid, endNs, depth+1, log)...)
		return out
	}
	return results
}

// RebuildSkillRecords re-reads OTel's own api_request events tagged with
// a skill (rather than the transcript) and republishes them with ONE
// adjustment: when OTel redacted the name to "third-party" (any plugin
// skill), the transcript-derived real name is substituted in, using the
// per-request resolution first and falling back to the session-level set
// only when it has exactly one candidate (unambiguous).
func RebuildSkillRecords(lokiURL, exporterStream string, dedupDays int, pluginSkills map[string]map[string]bool, pluginSkillByRequest map[string]state.PluginSkillRequest, fullHistory bool, log func(string, ...any)) ([]Record, error) {
	days := 2
	if fullHistory {
		days = dedupDays
	}
	const step = 24 * 3600 * 1_000_000_000 // 1 day, in nanoseconds
	nowNs := time.Now().UnixNano()

	var out []Record
	for offset := int64(0); offset < int64(days)*step; offset += step {
		end := nowNs - offset
		results := fetchSkillWindow(lokiURL, end-step, end, 0, log)
		for _, r := range results {
			sessionID := r.Labels["session_id"]
			requestID := r.Labels["request_id"]
			skillName := r.Labels["skill_name"]

			var real string
			if req, ok := pluginSkillByRequest[requestID]; ok {
				real = req.Skill
			} else if candidates := pluginSkills[sessionID]; len(candidates) == 1 {
				for s := range candidates {
					real = s
				}
			}
			skill := skillName
			if skillName == "third-party" && real != "" {
				skill = real
			}
			if skill == "" || requestID == "" {
				continue
			}
			owner := "local"
			if idx := strings.Index(skill, ":"); idx >= 0 {
				owner = skill[:idx]
			}

			inputT := r.Labels["input_tokens"]
			outputT := r.Labels["output_tokens"]
			cacheT := r.Labels["cache_creation_tokens"]
			tokens := parseIntOr0(inputT) + parseIntOr0(outputT) + parseIntOr0(cacheT)

			for _, v := range r.Values {
				out = append(out, Record{
					TimestampNs:  v[0],
					Line:         skill,
					DedupKey:     "skill:" + requestID,
					StreamLabels: map[string]string{"service_name": exporterStream, "kind": "skills"},
					Meta: map[string]string{
						"skill":        skill,
						"skill_owner":  owner,
						"session_id":   sessionID,
						"request_id":   requestID,
						"model":        r.Labels["model"],
						"effort":       r.Labels["effort"],
						"query_source": r.Labels["query_source"],
						"project":      "",
						"tokens":       strconv.FormatInt(tokens, 10),
						"user_email":   r.Labels["user_email"],
						"account_source": "otel",
					},
				})
			}
		}
	}
	return out, nil
}

func parseIntOr0(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd collector && go test ./internal/transcriptscan/... -v`
Expected: PASS (23 tests total).

- [ ] **Step 5: Commit**

```bash
git add collector/internal/transcriptscan
git commit -m "$(cat <<'EOF'
Add RebuildSkillRecords: OTel skill re-read with saturated-window split

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: `internal/transcriptscan` — `PushToLoki`

**Files:**
- Modify: `collector/internal/transcriptscan/transcriptscan.go`
- Modify: `collector/internal/transcriptscan/transcriptscan_test.go`

**Interfaces:**
- Consumes: `lokiclient.Push`/`Stream`/`StreamValue` (Task 5).
- Produces: `transcriptscan.PushToLoki(lokiURL string, batchSize int, defaultLabels map[string]string, records []Record, log func(string, ...any)) (pushed, skipped []Record, err error)`.

Ports `collector-old/transcript-scan.mjs` lines 773-832 exactly: sort by
timestamp, chunk by `batchSize`, group each chunk by exact stream labels,
retry up to 5 times with `2000ms * attempt` backoff, a `400` containing
"too far behind" (case-insensitive) goes to `skipped` (not retried), a
`429` retries like a 5xx, any other 4xx aborts the chunk immediately, a
300ms pace gap follows every successful non-final chunk.

Uses raw `net/http` directly here (not `lokiclient.Push`) because the
retry/backoff/skip logic needs the raw response status and body per
attempt — `lokiclient.Push` already collapses that into a single error,
which is right for `ratemeter`'s simpler needs but not for this.

- [ ] **Step 1: Add the failing tests**

Append to `collector/internal/transcriptscan/transcriptscan_test.go` (add
`"sync/atomic"` to imports):
```go
func TestPushToLoki_SplitsIntoBatchSizeChunks(t *testing.T) {
	var pushCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pushCalls, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	records := make([]Record, 5)
	for i := range records {
		records[i] = Record{TimestampNs: strconv.Itoa(1700000000000000000 + i), Line: "x", Meta: map[string]string{"tool_use_id": strconv.Itoa(i)}}
	}
	pushed, skipped, err := PushToLoki(srv.URL, 2, map[string]string{"service_name": "s", "kind": "tools"}, records, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 5 || len(skipped) != 0 {
		t.Fatalf("pushed=%d skipped=%d", len(pushed), len(skipped))
	}
	if pushCalls != 3 { // ceil(5/2)
		t.Errorf("push calls = %d, want 3", pushCalls)
	}
}

func TestPushToLoki_TooFarBehindGoesToSkippedNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("entry too far behind"))
	}))
	defer srv.Close()

	records := []Record{{TimestampNs: "1700000000000000000", Line: "x", Meta: map[string]string{"tool_use_id": "a"}}}
	pushed, skipped, err := PushToLoki(srv.URL, 100, map[string]string{"service_name": "s"}, records, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || len(pushed) != 0 {
		t.Fatalf("pushed=%d skipped=%d", len(pushed), len(skipped))
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (not retried)", attempts)
	}
}

func TestPushToLoki_OtherFourHundredAbortsImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("nope"))
	}))
	defer srv.Close()

	records := []Record{{TimestampNs: "1700000000000000000", Line: "x", Meta: map[string]string{"tool_use_id": "a"}}}
	_, _, err := PushToLoki(srv.URL, 100, map[string]string{"service_name": "s"}, records, t.Logf)
	if err == nil {
		t.Error("want error for a non-retryable, non-too-old 4xx")
	}
}

func TestPushToLoki_RetriesOnServerErrorThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	records := []Record{{TimestampNs: "1700000000000000000", Line: "x", Meta: map[string]string{"tool_use_id": "a"}}}
	pushed, _, err := PushToLoki(srv.URL, 100, map[string]string{"service_name": "s"}, records, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 1 {
		t.Fatalf("pushed=%d, want 1 after the retry succeeded", len(pushed))
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want a retry to have happened", attempts)
	}
}

func TestPushToLoki_RetriesOn429LikeServerError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	records := []Record{{TimestampNs: "1700000000000000000", Line: "x", Meta: map[string]string{"tool_use_id": "a"}}}
	pushed, _, err := PushToLoki(srv.URL, 100, map[string]string{"service_name": "s"}, records, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 1 {
		t.Errorf("pushed=%d, want 1 (429 retried like a 5xx, not aborted)", len(pushed))
	}
}

func TestPushToLoki_GroupsByExplicitStreamLabelsWhenSet(t *testing.T) {
	var receivedStreams int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		streams, _ := body["streams"].([]interface{})
		receivedStreams = len(streams)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	records := []Record{
		{TimestampNs: "1700000000000000000", Line: "x", Meta: map[string]string{"tool_use_id": "a"}}, // default (tools) labels
		{TimestampNs: "1700000000000000001", Line: "y", DedupKey: "skill:r1",
			StreamLabels: map[string]string{"service_name": "s", "kind": "skills"}, Meta: map[string]string{}},
	}
	_, _, err := PushToLoki(srv.URL, 100, map[string]string{"service_name": "s", "kind": "tools"}, records, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if receivedStreams != 2 {
		t.Errorf("got %d streams in the push payload, want 2 (tools and skills grouped separately)", receivedStreams)
	}
}

func TestPushToLoki_EmptyRecordsIsNoop(t *testing.T) {
	pushed, skipped, err := PushToLoki("http://unused.invalid", 100, nil, nil, t.Logf)
	if err != nil || len(pushed) != 0 || len(skipped) != 0 {
		t.Errorf("pushed=%v skipped=%v err=%v", pushed, skipped, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd collector && go test ./internal/transcriptscan/... -v`
Expected: FAIL — `PushToLoki` undefined.

- [ ] **Step 3: Implement PushToLoki**

Append to `collector/internal/transcriptscan/transcriptscan.go` (add
`"bytes"`, `"io"`, "net/http"`, `"net/url"`, `"sort"`, `"time"` (already
present) to imports):
```go
type pushPayload struct {
	Streams []pushStreamEntry `json:"streams"`
}
type pushStreamEntry struct {
	Stream map[string]string  `json:"stream"`
	Values []pushValueEntry   `json:"values"`
}
type pushValueEntry [3]interface{}

// PushToLoki sorts records by timestamp (Loki rejects too-far-out-of-order
// writes within a stream, and the initial backfill interleaves many files
// in time), chunks by batchSize, and pushes each chunk — grouped by its
// exact stream labels, since tool and skill records can't share one push.
// Each chunk retries up to 5 times with linear backoff to tolerate Loki's
// cold-start "empty ring" error. A 400 containing "too far behind" is NOT
// retried (that chunk can never succeed later either) — those records go
// to skipped, not pushed, so the caller's dedup doesn't lock them out once
// the blocking window ages out. A 429 gets the same retry/backoff as a
// 5xx (seen during full reimports outrunning per-user ingest limits). Any
// other 4xx aborts immediately. A fixed 300ms pace gap follows every
// successful non-final chunk.
func PushToLoki(lokiURL string, batchSize int, defaultLabels map[string]string, records []Record, log func(string, ...any)) (pushed, skipped []Record, err error) {
	if len(records) == 0 {
		return nil, nil, nil
	}
	ordered := append([]Record{}, records...)
	sort.Slice(ordered, func(i, j int) bool {
		a, _ := strconv.ParseInt(ordered[i].TimestampNs, 10, 64)
		b, _ := strconv.ParseInt(ordered[j].TimestampNs, 10, 64)
		return a < b
	})

	pushURL, urlErr := url.Parse(lokiURL)
	if urlErr != nil {
		return nil, nil, fmt.Errorf("invalid loki url %q: %w", lokiURL, urlErr)
	}
	pushURL.Path = "/loki/api/v1/push"

	for i := 0; i < len(ordered); i += batchSize {
		end := i + batchSize
		if end > len(ordered) {
			end = len(ordered)
		}
		chunk := ordered[i:end]

		byLabelsKey := map[string]*pushStreamEntry{}
		var order []string
		for _, r := range chunk {
			labels := r.StreamLabels
			if labels == nil {
				labels = defaultLabels
			}
			key := labelsKey(labels)
			entry, ok := byLabelsKey[key]
			if !ok {
				entry = &pushStreamEntry{Stream: labels}
				byLabelsKey[key] = entry
				order = append(order, key)
			}
			entry.Values = append(entry.Values, pushValueEntry{r.TimestampNs, r.Line, r.Meta})
		}
		payload := pushPayload{}
		for _, key := range order {
			payload.Streams = append(payload.Streams, *byLabelsKey[key])
		}
		body, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return nil, nil, marshalErr
		}

		var lastErr error
		tooOld := false
		for attempt := 0; attempt < 5; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(2000*attempt) * time.Millisecond)
			}
			resp, reqErr := http.Post(pushURL.String(), "application/json", bytes.NewReader(body))
			if reqErr != nil {
				lastErr = fmt.Errorf("push failed: %v", reqErr)
				continue
			}
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				lastErr = nil
				break
			}
			lastErr = fmt.Errorf("push failed %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
			if resp.StatusCode == 400 && strings.Contains(strings.ToLower(string(respBody)), "too far behind") {
				tooOld = true
				break
			}
			if resp.StatusCode != 429 && resp.StatusCode >= 400 && resp.StatusCode < 500 {
				break
			}
		}

		if !tooOld && lastErr == nil && end < len(ordered) {
			time.Sleep(300 * time.Millisecond)
		}
		if tooOld {
			log("%d record(s) rejected as too old by Loki, skipped: %v", len(chunk), lastErr)
			skipped = append(skipped, chunk...)
			continue
		}
		if lastErr != nil {
			return pushed, skipped, lastErr
		}
		pushed = append(pushed, chunk...)
	}
	return pushed, skipped, nil
}

func labelsKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(';')
	}
	return b.String()
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd collector && go test ./internal/transcriptscan/... -v`
Expected: PASS (30 tests total). This one is slower than the others
(real `time.Sleep` backoff on the retry tests) — that's expected, not a
bug; don't shrink the backoff durations just to make the test faster,
since that would stop testing the real timing the production code uses.

- [ ] **Step 5: Commit**

```bash
git add collector/internal/transcriptscan
git commit -m "$(cat <<'EOF'
Add PushToLoki: batched push with cold-start retry and too-old skip

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: `internal/transcriptscan` — `RunPass`, `InitState`, `SeedSeenFromLoki`

**Files:**
- Modify: `collector/internal/transcriptscan/transcriptscan.go`
- Modify: `collector/internal/transcriptscan/transcriptscan_test.go`

**Interfaces:**
- Consumes: everything from Tasks 6-9, `state.{State,Load,Save}` (Task 4).
- Produces: `transcriptscan.Config{ConfigDirs []string, LokiURL, ExporterStream string, BatchSize int, OrphanAfterMs int64, DedupDays, EmailLookbackHours int, DryRun, Rescan bool, Log func(string, ...any)}`, `transcriptscan.InitState(cfg Config) (*state.State, error)`, `transcriptscan.RunPass(cfg Config, st *state.State) (int, error)`.

Ports `collector-old/transcript-scan.mjs`: `seedSeenFromLoki` (580-617),
`runPass` (858-962), `initState` (997-1012). `cfg.ConfigDirs` must already
have `/projects` appended by the caller (matching the JS's own
`CONFIG_DIRS = resolveConfigDirs().map(dir => join(dir, 'projects'))` —
that suffixing happens once at the top level, not inside this package).

`summarize()` (964-993, the `--dry-run` console tally) is a debug
convenience with no correctness impact and is **not** ported
byte-for-byte here — what matters and *is* preserved and tested is
`DRY_RUN`'s actual data-safety contract: no Loki push, no state save.

- [ ] **Step 1: Add the failing tests**

Append to `collector/internal/transcriptscan/transcriptscan_test.go`:
```go
func TestSeedSeenFromLoki_PopulatesFromToolUseIDLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"tool_use_id":"tu1"},"values":[["1700000000000000000","x"]]}
		]}}`))
	}))
	defer srv.Close()

	seen := map[string]int64{}
	SeedSeenFromLoki(srv.URL, "claude-code-exporter-1", 1, seen, t.Logf)
	if seen["tu1"] == 0 {
		t.Errorf("seen = %v, want tu1 populated", seen)
	}
}

func TestSeedSeenFromLoki_ContinuesPastAFailedDay(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"tool_use_id":"tu2"},"values":[["1700000000000000000","x"]]}
		]}}`))
	}))
	defer srv.Close()

	seen := map[string]int64{}
	SeedSeenFromLoki(srv.URL, "s", 2, seen, t.Logf)
	if seen["tu2"] == 0 {
		t.Error("a failed day must not stop the remaining days from seeding")
	}
}

func TestRunPass_EndToEndSettlesAndPushes(t *testing.T) {
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects")
	must(t, os.MkdirAll(projectsDir, 0o755))
	writeJSONL(t, filepath.Join(projectsDir, "s.jsonl"), []string{
		`{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":"2026-01-01T00:00:00Z","message":{"model":"claude","content":[{"type":"tool_use","id":"tu1","name":"Read"}]}}`,
		`{"type":"assistant","sessionId":"s1","requestId":"r2","timestamp":"2026-01-01T00:01:00Z","message":{"model":"claude","usage":{"input_tokens":50},"content":[]}}`,
	})

	var pushed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/push" {
			pushed = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	statePath := filepath.Join(root, "state.json")
	cfg := Config{
		ConfigDirs: []string{projectsDir}, LokiURL: srv.URL, ExporterStream: "claude-code-exporter-1",
		BatchSize: 2000, OrphanAfterMs: 15 * 60 * 1000, DedupDays: 90, EmailLookbackHours: 720,
		StatePath: statePath, Log: t.Logf,
	}
	st, err := state.Load(statePath)
	must(t, err)

	n, err := RunPass(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pushed count = %d, want 1", n)
	}
	if !pushed {
		t.Error("Loki push endpoint was never called")
	}
	if len(st.Files) != 1 {
		t.Errorf("state.Files = %v, want the transcript's offset recorded", st.Files)
	}
}

func TestRunPass_OrphanGroupSettlesWithZeroTokensAfterTimeout(t *testing.T) {
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects")
	must(t, os.MkdirAll(projectsDir, 0o755))
	oldTimestamp := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	writeJSONL(t, filepath.Join(projectsDir, "s.jsonl"), []string{
		fmt.Sprintf(`{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":%q,"message":{"model":"claude","content":[{"type":"tool_use","id":"tu1","name":"Read"}]}}`, oldTimestamp),
	})

	var pushedTokens string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/push" {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			streams := body["streams"].([]interface{})
			s0 := streams[0].(map[string]interface{})
			values := s0["values"].([]interface{})
			v0 := values[0].([]interface{})
			meta := v0[2].(map[string]interface{})
			pushedTokens = meta["tokens_attributed"].(string)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	cfg := Config{
		ConfigDirs: []string{projectsDir}, LokiURL: srv.URL, ExporterStream: "s",
		BatchSize: 2000, OrphanAfterMs: 1000, DedupDays: 90, EmailLookbackHours: 720, // 1s orphan timeout, easily tripped by the 1h-old fixture
		StatePath: filepath.Join(root, "state.json"), Log: t.Logf,
	}
	st, err := state.Load(cfg.StatePath)
	must(t, err)
	if _, err := RunPass(cfg, st); err != nil {
		t.Fatal(err)
	}
	if pushedTokens != "0" {
		t.Errorf("orphaned group's tokens_attributed = %q, want \"0\"", pushedTokens)
	}
}

func TestInitState_RescanZeroesFilesButKeepsDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	seeded := &state.State{
		Version: 2,
		Files:   map[string]*state.FileState{"/old.jsonl": {Offset: 999}},
		Seen:    map[string]int64{"tu1": 1},
	}
	must(t, state.Save(path, seeded))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	st, err := InitState(Config{StatePath: path, LokiURL: srv.URL, ExporterStream: "s", DedupDays: 90, Rescan: true, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Files) != 0 {
		t.Errorf("Files = %v, want zeroed by --rescan", st.Files)
	}
	if st.Seen["tu1"] != 1 {
		t.Error("--rescan must keep the dedup map")
	}
}

func TestInitState_SeedsWhenSeenIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var seedQueried bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seedQueried = true
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	_, err := InitState(Config{StatePath: path, LokiURL: srv.URL, ExporterStream: "s", DedupDays: 1, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if !seedQueried {
		t.Error("a fresh state (empty Seen) must trigger seedSeenFromLoki")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd collector && go test ./internal/transcriptscan/... -v`
Expected: FAIL — `Config`/`InitState`/`RunPass`/`SeedSeenFromLoki` undefined.

- [ ] **Step 3: Implement the three functions**

Append to `collector/internal/transcriptscan/transcriptscan.go`:
```go
// Config bundles everything RunPass/InitState need. ConfigDirs must
// already have "/projects" appended by the caller.
type Config struct {
	ConfigDirs         []string
	LokiURL            string
	ExporterStream     string
	BatchSize          int
	OrphanAfterMs      int64
	DedupDays          int
	EmailLookbackHours int
	DryRun             bool
	Rescan             bool
	StatePath          string
	Log                func(string, ...any)
}

func (cfg Config) defaultLabels() map[string]string {
	return map[string]string{"service_name": cfg.ExporterStream, "kind": "tools"}
}

// SeedSeenFromLoki rebuilds seen from what's already in Loki — runs when
// the map is empty (an upgrade, a lost state volume, or a deleted file).
// Scanned one day at a time to stay under Loki's per-query line limit,
// continuing past a single failed day rather than aborting the whole seed.
func SeedSeenFromLoki(lokiURL, exporterStream string, dedupDays int, seen map[string]int64, log func(string, ...any)) {
	const step = 24 * 3600 * 1_000_000_000 // 1 day, ns
	windowNs := int64(dedupDays) * step / (24 * 3600)
	_ = windowNs
	total := 0
	windowTotalNs := int64(dedupDays) * 24 * 3600 * 1_000_000_000
	nowNs := time.Now().UnixNano()
	query := fmt.Sprintf(`{service_name="%s"}`, exporterStream)
	for offset := int64(0); offset < windowTotalNs; offset += step {
		end := nowNs - offset
		start := end - step
		results, err := lokiclient.QueryRange(lokiURL, query, start, end, 5000, "backward")
		if err != nil {
			log("seed window failed (%v); continuing to the next", err)
			continue
		}
		for _, r := range results {
			id := r.Labels["tool_use_id"]
			if id == "" {
				continue
			}
			for _, v := range r.Values {
				ns, _ := strconv.ParseInt(v[0], 10, 64)
				seen[id] = ns / 1e6
				total++
			}
		}
	}
	if total > 0 {
		log("dedup seeded with %d call(s) already in Loki", len(seen))
	}
}

// InitState loads state (or starts fresh), honors --rescan (zeros
// state.Files only — keeps the dedup/plugin-skill maps, a non-destructive
// re-read), and seeds state.Seen from Loki when it's empty.
func InitState(cfg Config) (*state.State, error) {
	cfg.Log("transcripts: %s", strings.Join(cfg.ConfigDirs, ", "))
	if cfg.DryRun {
		cfg.Log("loki: %s  (dry-run)", cfg.LokiURL)
	} else {
		cfg.Log("loki: %s", cfg.LokiURL)
	}
	st, err := state.Load(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	if cfg.Rescan {
		cfg.Log("RESCAN: zeroing offsets, keeping the dedup map")
		st.Files = map[string]*state.FileState{}
	}
	if len(st.Files) == 0 {
		cfg.Log("first run: importing the full transcript history")
	}
	if len(st.Seen) == 0 {
		SeedSeenFromLoki(cfg.LokiURL, cfg.ExporterStream, cfg.DedupDays, st.Seen, cfg.Log)
	}
	return st, nil
}

// RunPass does one full pass: scan every configured directory's new
// transcript lines, settle/orphan groups, rebuild skill records from
// OTel, dedup, resolve emails, push to Loki, persist state. Returns the
// count of records actually pushed.
func RunPass(cfg Config, st *state.State) (int, error) {
	firstPass := len(st.Files) == 0
	files, err := FindTranscripts(cfg.ConfigDirs, cfg.Log)
	if err != nil {
		return 0, err
	}

	type update struct {
		fs state.FileState
	}
	updates := map[string]update{}

	pluginSkills := map[string]map[string]bool{}
	for sessionID, names := range st.PluginSkills {
		set := map[string]bool{}
		for _, n := range names {
			set[n] = true
		}
		pluginSkills[sessionID] = set
	}
	pluginSkillByRequest := map[string]state.PluginSkillRequest{}
	cutoffMs := time.Now().AddDate(0, 0, -cfg.DedupDays).UnixMilli()
	for reqID, info := range st.PluginSkillByRequest {
		if info.TsMs >= cutoffMs {
			pluginSkillByRequest[reqID] = info
		}
	}

	var collected []Record
	for _, path := range files {
		fs := state.FileState{}
		if existing, ok := st.Files[path]; ok && existing != nil {
			fs = *existing
		}
		records, newFs, err := ProcessTranscript(path, fs, pluginSkills, pluginSkillByRequest)
		if err != nil {
			cfg.Log("failed reading %s: %v", filepath.Base(path), err)
			continue
		}

		for _, track := range []string{"main", "sub"} {
			var group **state.Group
			if track == "main" {
				group = &newFs.PendingMain
			} else {
				group = &newFs.PendingSub
			}
			if *group != nil && time.Now().UnixMilli()-(*group).AtMs > cfg.OrphanAfterMs {
				records = append(records, Settle(**group, 0)...)
				*group = nil
			}
		}

		collected = append(collected, records...)
		updates[path] = update{fs: newFs}
	}

	skillRecords, err := RebuildSkillRecords(cfg.LokiURL, cfg.ExporterStream, cfg.DedupDays, pluginSkills, pluginSkillByRequest, firstPass, cfg.Log)
	if err != nil {
		return 0, err
	}
	collected = append(collected, skillRecords...)

	fresh, novos := DropAlreadySeen(st.Seen, cfg.DedupDays, collected)
	if skipped := len(collected) - len(fresh); skipped > 0 {
		cfg.Log("%d call(s) skipped: already exported (resumed session)", skipped)
	}

	commit := func() {
		for path, u := range updates {
			fs := u.fs
			st.Files[path] = &fs
		}
		for id, ts := range novos {
			st.Seen[id] = ts
		}
		for sessionID, set := range pluginSkills {
			names := make([]string, 0, len(set))
			for n := range set {
				names = append(names, n)
			}
			st.PluginSkills[sessionID] = names
		}
		st.PluginSkillByRequest = pluginSkillByRequest
	}

	if len(fresh) == 0 {
		if len(collected) > 0 {
			commit()
			if !cfg.DryRun {
				if err := state.Save(cfg.StatePath, st); err != nil {
					return 0, err
				}
			}
		}
		return 0, nil
	}
	collected = fresh

	sessionIDs := map[string]bool{}
	for _, r := range collected {
		if id := r.Meta["session_id"]; id != "" {
			sessionIDs[id] = true
		}
	}
	ids := make([]string, 0, len(sessionIDs))
	for id := range sessionIDs {
		ids = append(ids, id)
	}
	ResolveEmails(cfg.LokiURL, cfg.EmailLookbackHours, st.SessionEmail, ids, cfg.Log)
	for i := range collected {
		if collected[i].Meta["user_email"] != "" {
			continue
		}
		email := st.SessionEmail[collected[i].Meta["session_id"]]
		collected[i].Meta["user_email"] = email
		if email != "" {
			collected[i].Meta["account_source"] = "otel"
		} else {
			collected[i].Meta["account_source"] = ""
		}
	}

	needsProject := false
	for _, r := range collected {
		if r.Meta["user_email"] == "" {
			needsProject = true
			break
		}
	}
	if needsProject {
		owners, err := ProjectOwners(cfg.LokiURL, cfg.ExporterStream, cfg.DedupDays, collected)
		if err != nil {
			return 0, err
		}
		for i := range collected {
			if collected[i].Meta["user_email"] != "" {
				continue
			}
			if owner, ok := owners[collected[i].Meta["project"]]; ok {
				collected[i].Meta["user_email"] = owner
				collected[i].Meta["account_source"] = "project"
			}
		}
	}

	if cfg.DryRun {
		commit()
		return 0, nil
	}

	pushedRecords, rejected, err := PushToLoki(cfg.LokiURL, cfg.BatchSize, cfg.defaultLabels(), collected, cfg.Log)
	if err != nil {
		return 0, err
	}
	for _, r := range rejected {
		id := r.DedupKey
		if id == "" {
			id = r.Meta["tool_use_id"]
		}
		delete(novos, id)
	}
	commit()
	if err := state.Save(cfg.StatePath, st); err != nil {
		return 0, err
	}
	return len(pushedRecords), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd collector && go test ./internal/transcriptscan/... -v`
Expected: PASS (36 tests total).

- [ ] **Step 5: Commit**

```bash
git add collector/internal/transcriptscan
git commit -m "$(cat <<'EOF'
Add RunPass/InitState/SeedSeenFromLoki, completing the transcriptscan port

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: `internal/usagetruth` — simplified port of `usage-truth.mjs`

**Files:**
- Create: `collector/internal/usagetruth/usagetruth.go`
- Test: `collector/internal/usagetruth/usagetruth_test.go`

**Interfaces:**
- Produces: `usagetruth.Usage{SessionPct, WeekPct int, SessionReset, WeekReset string}`, `usagetruth.AccountEmail(configDir string) (string, error)`, `usagetruth.FetchUsage(configDir string) (Usage, error)`, `usagetruth.PublishUsageTruth(lokiURL string, configDirs []string, log func(string, ...any)) error`.

Ports the capture-and-publish core of `collector-old/usage-truth.mjs`
(`runClaude` 97-104, `accountEmail` 106-110, `parseUsage` 112-131,
`fetchUsage` 133-139, `publishTruth` 161-183, the loop inside
`publishUsageTruth` 209-235 minus the calibration block 236-266). No
calibration — its only data source (`meterTokens`, reading a stream
`usage-meter.mjs` published) died with `usage-meter.mjs`; see the
Context section of the collector spec.

- [ ] **Step 1: Write the failing tests**

`collector/internal/usagetruth/usagetruth_test.go`:
```go
package usagetruth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeClaudeOnPath writes a small shell script named "claude" that
// echoes canned JSON based on its arguments, and prepends its directory
// to PATH for the duration of the test — a real subprocess, not a mock
// of os/exec internals, so this exercises AccountEmail/FetchUsage exactly
// as they'll really run.
func fakeClaudeOnPath(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude fixture is a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAccountEmail_LoggedIn(t *testing.T) {
	fakeClaudeOnPath(t, `echo '{"loggedIn":true,"email":"a@example.com"}'`)
	email, err := AccountEmail("/tmp/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if email != "a@example.com" {
		t.Errorf("email = %q", email)
	}
}

func TestAccountEmail_NotLoggedInReturnsEmpty(t *testing.T) {
	fakeClaudeOnPath(t, `echo '{"loggedIn":false}'`)
	email, err := AccountEmail("/tmp/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if email != "" {
		t.Errorf("email = %q, want empty", email)
	}
}

func TestAccountEmail_PassesConfigDirAsEnvVar(t *testing.T) {
	fakeClaudeOnPath(t, `if [ "$CLAUDE_CONFIG_DIR" = "/expected/dir" ]; then echo '{"loggedIn":true,"email":"ok@example.com"}'; else echo '{"loggedIn":false}'; fi`)
	email, err := AccountEmail("/expected/dir")
	if err != nil {
		t.Fatal(err)
	}
	if email != "ok@example.com" {
		t.Errorf("email = %q, want the fixture to have seen CLAUDE_CONFIG_DIR", email)
	}
}

func TestFetchUsage_ParsesBothLines(t *testing.T) {
	result := `Current session: 13% used · resets Sep 22 at 8:39pm (America/Sao_Paulo)
Current week (all models): 42% used · resets Sep 24 at 7:59pm (America/Sao_Paulo)`
	payload, _ := json.Marshal(map[string]interface{}{"is_error": false, "result": result})
	fakeClaudeOnPath(t, "cat <<'EOF'\n"+string(payload)+"\nEOF")

	usage, err := FetchUsage("/tmp/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if usage.SessionPct != 13 || usage.WeekPct != 42 {
		t.Errorf("usage = %+v", usage)
	}
	if usage.SessionReset != "resets Sep 22 at 8:39pm (America/Sao_Paulo)" {
		t.Errorf("SessionReset = %q", usage.SessionReset)
	}
}

func TestFetchUsage_ZeroPercentHasNoResetClause(t *testing.T) {
	result := `Current session: 0% used
Current week (all models): 0% used`
	payload, _ := json.Marshal(map[string]interface{}{"is_error": false, "result": result})
	fakeClaudeOnPath(t, "cat <<'EOF'\n"+string(payload)+"\nEOF")

	usage, err := FetchUsage("/tmp/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if usage.SessionPct != 0 || usage.SessionReset != "" {
		t.Errorf("usage = %+v", usage)
	}
}

func TestFetchUsage_IsErrorReturnsError(t *testing.T) {
	payload, _ := json.Marshal(map[string]interface{}{"is_error": true, "result": "boom"})
	fakeClaudeOnPath(t, "cat <<'EOF'\n"+string(payload)+"\nEOF")

	if _, err := FetchUsage("/tmp/cfg"); err == nil {
		t.Error("want error when is_error is true")
	}
}

func TestPublishUsageTruth_PublishesOneLinePerLoggedInAccount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-claude fixture is a POSIX shell script")
	}
	dir := t.TempDir()
	// Built via json.Marshal + a quoted heredoc, not an echo with an
	// embedded \n: macOS's /bin/sh echo interprets \n as a real newline
	// byte, which then sits UNESCAPED inside the JSON string and makes it
	// invalid JSON. A heredoc prints its body verbatim, so json.Marshal's
	// own correct "\n" (backslash-n) escaping survives untouched.
	usagePayload, _ := json.Marshal(map[string]interface{}{
		"is_error": false,
		"result":   "Current session: 5% used\nCurrent week (all models): 6% used",
	})
	script := "#!/bin/sh\n" +
		`if [ "$1" = "auth" ]; then` + "\n" +
		`  echo '{"loggedIn":true,"email":"a@example.com"}'` + "\n" +
		"else\n" +
		"  cat <<'EOF'\n" + string(usagePayload) + "\nEOF\n" +
		"fi\n"
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var pushed map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&pushed)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := PublishUsageTruth(srv.URL, []string{"/tmp/cfg"}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	streams, _ := pushed["streams"].([]interface{})
	if len(streams) != 1 {
		t.Fatalf("pushed = %v", pushed)
	}
	stream := streams[0].(map[string]interface{})["stream"].(map[string]interface{})
	if stream["user_email"] != "a@example.com" || stream["service_name"] != "claude-code-usage-truth" {
		t.Errorf("stream = %v", stream)
	}
}

func TestPublishUsageTruth_SkipsNotLoggedInWithoutError(t *testing.T) {
	fakeClaudeOnPath(t, `echo '{"loggedIn":false}'`)
	err := PublishUsageTruth("http://unused.invalid", []string{"/tmp/cfg"}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
}

var _ = exec.Command // keep exec imported for future use if needed
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd collector && go test ./internal/usagetruth/... -v`
Expected: FAIL — package has no exported symbols yet.

- [ ] **Step 3: Implement usagetruth.go**

`collector/internal/usagetruth/usagetruth.go`:
```go
// Package usagetruth captures the real numbers behind Anthropic's 5h/week
// limits by running `/usage` via the `claude` CLI itself, per account, and
// publishing them into Loki. Simplified port of usage-truth.mjs — no
// calibration (its data source died with usage-meter.mjs).
package usagetruth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"claude-observability-collector/internal/lokiclient"
)

const claudeTimeout = 30 * time.Second

// Usage is one account's current session/week percentages and Anthropic's
// own "resets at" text for each (empty when the window reads 0% used —
// no open block carries no expiry to report).
type Usage struct {
	SessionPct   int
	SessionReset string
	WeekPct      int
	WeekReset    string
}

func runClaude(configDir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), claudeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+configDir)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// AccountEmail runs `claude auth status` with CLAUDE_CONFIG_DIR=configDir
// and returns the logged-in email, or "" if not logged in.
func AccountEmail(configDir string) (string, error) {
	out, err := runClaude(configDir, "auth", "status")
	if err != nil {
		return "", err
	}
	var status struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", fmt.Errorf("auth status decode: %w", err)
	}
	if !status.LoggedIn {
		return "", nil
	}
	return status.Email, nil
}

var sessionRe = regexp.MustCompile(`(?i)Current session:\s*(\d+)%\s*used(?:\s*·\s*(.+))?`)
var weekRe = regexp.MustCompile(`(?i)Current week[^:]*:\s*(\d+)%\s*used(?:\s*·\s*(.+))?`)

func parseUsage(text string) (Usage, error) {
	session := sessionRe.FindStringSubmatch(text)
	week := weekRe.FindStringSubmatch(text)
	if session == nil || week == nil {
		short := text
		if len(short) > 200 {
			short = short[:200]
		}
		return Usage{}, fmt.Errorf("/usage output not in the expected shape: %s", short)
	}
	sessionPct, _ := strconv.Atoi(session[1])
	weekPct, _ := strconv.Atoi(week[1])
	return Usage{
		SessionPct: sessionPct, SessionReset: strings.TrimSpace(session[2]),
		WeekPct: weekPct, WeekReset: strings.TrimSpace(week[2]),
	}, nil
}

// FetchUsage runs `claude -p /usage --output-format json
// --no-session-persistence` and parses the two top-line percentages/reset
// clauses Anthropic always leads with. Only these two numbers are used —
// the rest of /usage's output is explicitly scoped by Anthropic to "local
// sessions on this machine".
func FetchUsage(configDir string) (Usage, error) {
	out, err := runClaude(configDir, "-p", "/usage", "--output-format", "json", "--no-session-persistence")
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

func publish(lokiURL, email string, usage Usage) error {
	nowMs := time.Now().UnixMilli()
	return lokiclient.Push(lokiURL, []lokiclient.Stream{{
		Labels: map[string]string{"service_name": "claude-code-usage-truth", "user_email": email},
		Values: []lokiclient.StreamValue{{
			TimestampNs: strconv.FormatInt(nowMs, 10) + "000000",
			Line:        "usage-truth",
			Metadata: map[string]string{
				"session_pct":        strconv.Itoa(usage.SessionPct),
				"session_reset_text": usage.SessionReset,
				"week_pct":           strconv.Itoa(usage.WeekPct),
				"week_reset_text":    usage.WeekReset,
			},
		}},
	}})
}

// PublishUsageTruth runs AccountEmail+FetchUsage for every configDir and
// publishes one line per logged-in account. A per-account failure (not
// logged in, claude timeout, publish failure) is logged and skipped — one
// account's failure must not stop the others.
func PublishUsageTruth(lokiURL string, configDirs []string, log func(string, ...any)) error {
	for _, configDir := range configDirs {
		email, err := AccountEmail(configDir)
		if err != nil {
			log("usage-truth: auth status for %s failed: %v", configDir, err)
			continue
		}
		if email == "" {
			log("usage-truth: %s is not logged in, skipping", configDir)
			continue
		}
		usage, err := FetchUsage(configDir)
		if err != nil {
			log("usage-truth: /usage for %s failed: %v", email, err)
			continue
		}
		if err := publish(lokiURL, email, usage); err != nil {
			log("usage-truth: publish for %s failed: %v", email, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd collector && go test ./internal/usagetruth/... -v`
Expected: PASS (8 tests; the two Windows-skipped ones show as `SKIP`).

- [ ] **Step 5: Commit**

```bash
git add collector/internal/usagetruth
git commit -m "$(cat <<'EOF'
Add usagetruth package: simplified port of usage-truth.mjs

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 12: `cmd/collector/main.go` — wire the single poll loop

**Files:**
- Modify: `collector/cmd/collector/main.go`

**Interfaces:**
- Consumes: `accounts.ResolveConfigDirs` (Task 2), `transcriptscan.{Config,InitState,RunPass}` (Tasks 6-10), `usagetruth.PublishUsageTruth` (Task 11).
- Produces: the `collector` binary's end-to-end behavior.

- [ ] **Step 1: Replace main.go with the full orchestration**

`collector/cmd/collector/main.go`:
```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"claude-observability-collector/internal/accounts"
	"claude-observability-collector/internal/transcriptscan"
	"claude-observability-collector/internal/usagetruth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		return err
	}

	configDirs := accounts.ResolveConfigDirs(home, os.Getenv("CLAUDE_DIR"), os.Getenv("CLAUDE_OBSERVABILITY_EXTRA_DIRS"))
	projectDirs := make([]string, len(configDirs))
	for i, d := range configDirs {
		projectDirs[i] = filepath.Join(d, "projects")
	}

	lokiURL := envOr("LOKI_URL", "http://localhost:47100")
	exporterStream := envOr("EXPORTER_STREAM", "claude-code-exporter-1")
	pollSeconds := envInt("POLL_SECONDS", 60)
	statePath := envOr("STATE_FILE", filepath.Join(repoRoot, ".state", "collector-state.json"))

	once := hasArg("--once")
	dryRun := hasArg("--dry-run")
	rescan := hasArg("--rescan")

	log := func(format string, args ...any) { fmt.Printf(time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...) }

	cfg := transcriptscan.Config{
		ConfigDirs: projectDirs, LokiURL: lokiURL, ExporterStream: exporterStream,
		BatchSize: envInt("BATCH_SIZE", 2000), OrphanAfterMs: int64(envInt("ORPHAN_AFTER_MS", 15*60*1000)),
		DedupDays: envInt("DEDUP_DAYS", 90), EmailLookbackHours: envInt("EMAIL_LOOKBACK_HOURS", 720),
		DryRun: dryRun, Rescan: rescan, StatePath: statePath, Log: log,
	}

	st, err := transcriptscan.InitState(cfg)
	if err != nil {
		return err
	}

	pass := func() error {
		count, err := transcriptscan.RunPass(cfg, st)
		if err != nil {
			log("transcript scan failed: %v", err)
		} else if count > 0 {
			log("%d tool call(s) exported", count)
		}
		if err := usagetruth.PublishUsageTruth(lokiURL, configDirs, log); err != nil {
			log("usage-truth failed: %v", err)
		}
		return nil
	}

	if once {
		return pass()
	}
	for {
		if err := pass(); err != nil {
			return err
		}
		time.Sleep(time.Duration(pollSeconds) * time.Second)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func hasArg(name string) bool {
	for _, a := range os.Args[1:] {
		if a == name {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Verify it builds**

Run: `cd collector && go build ./...`
Expected: no errors.

- [ ] **Step 3: Run the full test suite**

Run: `cd collector && go test ./...`
Expected: PASS across all packages (accounts, shelltok, state, lokiclient, transcriptscan, usagetruth).

- [ ] **Step 4: Manual smoke test — `--once --dry-run` against a fake Loki, isolated `$HOME`**

Never against the real repo or real `$HOME` — a lesson paid for earlier
in this project. Use a scratch directory throughout:
```bash
rm -rf /tmp/collector-smoke
mkdir -p /tmp/collector-smoke/fakehome/.claude/projects
mkdir -p /tmp/collector-smoke/.state
touch /tmp/collector-smoke/docker-compose.yaml
cd collector && go build -o /tmp/collector-smoke/collector ./cmd/collector

python3 -c "
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers()
        self.wfile.write(b'{\"status\":\"success\",\"data\":{\"resultType\":\"streams\",\"result\":[]}}')
    def do_POST(self):
        self.send_response(204); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', 47198), H).serve_forever()
" &
FAKE_PID=$!
sleep 1
(
  export HOME=/tmp/collector-smoke/fakehome
  cd /tmp/collector-smoke
  LOKI_URL=http://127.0.0.1:47198 ./collector --once --dry-run
  echo "exit=$?"
)
kill $FAKE_PID
rm -rf /tmp/collector-smoke
```
Expected: logs `transcripts: .../fakehome/.claude/projects`,
`loki: http://127.0.0.1:47198  (dry-run)`, no tool calls (empty
transcripts dir), `usage-truth: ... is not logged in, skipping` (or a
`claude` CLI failure, since no real `claude` binary is on PATH in this
sandboxed run — either is fine, it must not crash the whole process),
exit 0.

- [ ] **Step 5: Commit**

```bash
git add collector/cmd/collector/main.go
git commit -m "$(cat <<'EOF'
Wire the collector's poll loop in main.go

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 13: Wizard — offer to install `collector` as a service too

**Files:**
- Modify: `setup/cmd/setup/main.go`

**Interfaces:**
- Consumes: the generalized `service.Config`/`service.Install` (already built in the dash-generator plan's Task 11 — no further service-package changes needed).

A separate prompt from "Install the dash generator...": a user might
legitimately want one without the other (e.g. a machine that only hosts
Loki/Grafana/dash-generator, with collectors running elsewhere once a
shared-collector project exists).

- [ ] **Step 1: Add a second install step after the dash-generator one**

In `setup/cmd/setup/main.go`, after the existing "Dash generator" block in
`run()`, add:
```go
	fmt.Fprintln(out, "\n── Collector ─────────────────────────────────────────────────")
	if wizard.AskYesNo(out, stdin, "  Install the collector as a background service now?", true) {
		if err := installCollectorService(repoRoot, collectorVars); err != nil {
			return fmt.Errorf("installing collector service: %w", err)
		}
		fmt.Fprintln(out, "  installed and started")
	}
```

And add the corresponding function, alongside the existing
`installService`/`findDashGeneratorBinary`/`dashGeneratorLabel`:
```go
func installCollectorService(repoRoot string, collectorVars []envwriter.Var) error {
	collectorBin, err := findCollectorBinary(repoRoot)
	if err != nil {
		return err
	}
	cfg := service.Config{
		Label:      collectorLabel(),
		Command:    collectorBin,
		WorkingDir: repoRoot,
		Env:        collectorVars,
		LogDir:     filepath.Join(repoRoot, ".state"),
	}
	return service.Install(cfg)
}

// findCollectorBinary looks for a collector binary built alongside this
// one (same directory as the running setup executable) or on PATH.
func findCollectorBinary(repoRoot string) (string, error) {
	name := "collector"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), name)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s not found next to this binary or on PATH — build it from collector/ or download it alongside claude-observability-setup", name)
}

func collectorLabel() string {
	switch runtime.GOOS {
	case "darwin":
		return "com.agents-observability.collector"
	case "windows":
		return "ClaudeObservabilityCollector"
	default:
		return "claude-observability-collector"
	}
}
```

- [ ] **Step 2: Build and test**

Run: `cd setup && go build ./... && go test ./...`
Expected: no errors; all existing tests still green (this task adds new
functions and a new prompt, doesn't touch anything under test).

- [ ] **Step 3: Manual smoke test — decline both prompts**

Same isolated-scratch-directory pattern as every earlier smoke test in
this project (never the real repo or real `$HOME`):
```bash
rm -rf /tmp/setup-smoke3
mkdir -p /tmp/setup-smoke3/fakehome
cd /tmp/setup-smoke3 && touch docker-compose.yaml
cd /path/to/claude-observability/setup && go build -o /tmp/setup-smoke3/claude-observability-setup ./cmd/setup

python3 -c "
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self): self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', 47397), H).serve_forever()
" &
FAKE_PID=$!
sleep 1
(
  export HOME=/tmp/setup-smoke3/fakehome
  cd /tmp/setup-smoke3
  printf 'http://127.0.0.1:47397\n\nn\nn\n' | ./claude-observability-setup
  echo "exit=$?"
)
kill $FAKE_PID
rm -rf /tmp/setup-smoke3
```
Expected: reaches both "Dash generator" and "Collector" sections in
order, both declined cleanly, exit 0, no attempt to locate either binary.

- [ ] **Step 4: Commit**

```bash
git add setup/cmd/setup/main.go
git commit -m "$(cat <<'EOF'
Offer to install collector as a background service too

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 14: Cutover — delete `collector-old/`, fix remaining references

**Files:**
- Delete: `collector-old/` (all 7 `.mjs` files)
- Modify: `bin/install-service.sh`
- Modify: `docker-compose.yaml`
- Modify: `README.md`

**Interfaces:**
- None (cleanup + documentation only).

**Only do this once collector has actually been run against a real
account's real transcripts and verified working** — not just unit tests.
If that verification hasn't happened yet, stop here and ask before
deleting `collector-old/`; it's the only remaining functional fallback.

- [ ] **Step 1: Verify collector works against real data (manual, not scripted)**

Build the real binary and run it once, by hand, from the actual repo root
with the actual `$HOME` (this is the one place in the whole project where
that's appropriate — verifying the real thing against real data is the
point):
```bash
cd collector && go build -o /tmp/collector-verify ./cmd/collector
cd /path/to/claude-observability
/tmp/collector-verify --once --dry-run
```
Confirm the log output looks sane: real config dirs found, some tool
calls scanned (if any exist), no crashes. Then run once for real
(without `--dry-run`) and spot-check a few rows landed in Loki/Grafana
looking right compared to what `collector-old` was producing.

- [ ] **Step 2: Remove the old Node implementation**

```bash
git rm -r collector-old
```

- [ ] **Step 3: Update `bin/install-service.sh`**

Change `COLLECTOR="$OBS_ROOT/collector-old/collector.mjs"` back to
pointing at nothing (it's retired) — replace the whole script's purpose
note, OR simplest: delete it too, since the wizard's `installCollectorService`
(Task 13) now covers what it did. Check whether anything else still
references `bin/install-service.sh` before deleting (grep the repo); if
README still walks through it as a manual fallback, update that section
instead of deleting the script outright — use judgment based on what's
actually still there by the time this task runs.

- [ ] **Step 4: Fix `docker-compose.yaml`'s comment**

Update the comment block (added during the `collector-old` rename) that
currently reads "now `collector-old/collector.mjs` (being ported to Go as
`collector/`...)" — the port is done now, so it should just describe the
real Go collector directly, matching how the file already describes
`otel-collector`/`loki`/`grafana`.

- [ ] **Step 5: Grep for stale references**

Run: `grep -rn "collector-old\|collector\.mjs\|collector/collector\.mjs" README.md docker-compose.yaml bin/ 2>/dev/null`
Expected: no matches (fix any remaining ones found).

- [ ] **Step 6: Final full verification**

```bash
cd setup && go build ./... && go test ./...
cd ../dash-generator && go build ./... && go test ./...
cd ../collector && go build ./... && go test ./...
```
Expected: all three green.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "$(cat <<'EOF'
Cut over from collector-old to the Go collector

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Testing summary

Every task above follows the same TDD cycle: write the failing test,
confirm it fails for the right reason, implement, confirm it passes,
commit. Cumulative test count by the end of Task 12: `accounts` (4),
`shelltok` (12), `state` (3), `lokiclient` (6), `transcriptscan` (36),
`usagetruth` (8) — 69 tests across the collector module, plus the
existing 28 (setup) and 36 (dash-generator) from the other two plans.

No manual/smoke-only verification substitutes for these — the smoke
tests in Tasks 12-13 check wiring and CLI behavior an automated unit test
can't easily reach (real process spawning, real prompts), not correctness
of the ported algorithms, which is what the 69 unit tests are for.
