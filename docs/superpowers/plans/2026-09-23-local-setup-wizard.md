# Local Setup Wizard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the bash-only `bin/setup` + `bin/install-service.sh` + `bin/claude-telemetry.{sh,fish}` with a single self-contained Go binary that runs identically on macOS, Linux, and Windows, configures Claude Code telemetry (with an editable OTel endpoint/token and a reachability check before writing anything), and installs the collector as a background service on all three OSes.

**Architecture:** A Go module at `setup/` with small, independently testable packages (`discovery`, `health`, `envwriter`, `limits`, `wizard`, `service`) composed by `cmd/setup/main.go`. OS-specific behavior (the background service installer) lives in build-tagged files per package so each OS's logic only compiles — and its tests only run — on that OS. No third-party Go dependencies; standard library only.

**Tech Stack:** Go 1.22 (standard library only), GitHub Actions (test matrix + release build/publish).

**Spec:** `docs/superpowers/specs/2026-09-23-local-setup-wizard-design.md`

## Global Constraints

- No runtime dependency for end users: the built binary must run with nothing else installed (no Node, Python, bash, or PowerShell required to run *it* — Node/`claude` are still required to actually run the collector service, checked via `PATH` lookup with a clear error).
- No third-party Go modules — standard library only.
- Setup never runs `docker compose up -d` itself.
- The OTel endpoint and bearer token are user-editable fields, prefilled with the local default (`http://localhost:47317` / empty) — never hardcoded assumptions elsewhere in the code.
- The health check (`internal/health.Dial`) is endpoint-agnostic: a plain TCP dial, no branching on whether the host looks "local."
- Must be run from the `claude-observability` repo root (the binary itself doesn't bundle `docker-compose.yaml`/`collector/`/`grafana/` — those still come from the git checkout).

---

## File Structure

```
setup/
  go.mod
  cmd/setup/main.go
  internal/discovery/discovery.go
  internal/discovery/discovery_test.go
  internal/health/health.go
  internal/health/health_test.go
  internal/envwriter/envwriter.go
  internal/envwriter/envwriter_test.go
  internal/limits/limits.go
  internal/limits/limits_test.go
  internal/wizard/wizard.go
  internal/wizard/wizard_test.go
  internal/service/service.go              (Config type, shared, no build tag)
  internal/service/service_darwin.go
  internal/service/service_darwin_test.go
  internal/service/service_linux.go
  internal/service/service_linux_test.go
  internal/service/service_windows.go
  internal/service/service_windows_test.go
.github/workflows/test.yml
.github/workflows/release.yml
README.md                                   (updated "Getting started")
bin/setup                                    (removed)
bin/install-service.sh                       (removed)
bin/claude-telemetry.sh                      (removed)
bin/claude-telemetry.fish                    (removed)
```

---

### Task 1: Go module scaffold + CI test workflow

**Files:**
- Create: `setup/go.mod`
- Create: `setup/cmd/setup/main.go`
- Create: `.github/workflows/test.yml`

**Interfaces:**
- Produces: a `setup` Go module (`module claude-observability-setup`) that later tasks add packages under; a CI job that runs `go test ./...` inside `setup/` on macOS/Linux/Windows runners on every push.

- [ ] **Step 1: Create the Go module**

```bash
mkdir -p setup/cmd/setup
cat > setup/go.mod <<'EOF'
module claude-observability-setup

go 1.22
EOF
```

- [ ] **Step 2: Create a placeholder main package**

`setup/cmd/setup/main.go`:
```go
package main

import "fmt"

func main() {
	fmt.Println("claude-observability setup")
}
```

- [ ] **Step 3: Verify it builds and runs**

Run: `cd setup && go build ./... && go run ./cmd/setup`
Expected: prints `claude-observability setup`, no errors.

- [ ] **Step 4: Add the CI test workflow**

`.github/workflows/test.yml`:
```yaml
name: test
on:
  push:
  pull_request:
jobs:
  test:
    strategy:
      matrix:
        os: [macos-latest, ubuntu-latest, windows-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - run: go test ./...
        working-directory: setup
```

- [ ] **Step 5: Commit**

```bash
git add setup/go.mod setup/cmd/setup/main.go .github/workflows/test.yml
git commit -m "$(cat <<'EOF'
Scaffold Go setup module and CI test matrix

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: `internal/discovery` — find Claude Code config directories

**Files:**
- Create: `setup/internal/discovery/discovery.go`
- Test: `setup/internal/discovery/discovery_test.go`

**Interfaces:**
- Produces: `discovery.Account{Dir, Email string}`, `discovery.Find(home string) ([]Account, error)`.

- [ ] **Step 1: Write the failing tests**

`setup/internal/discovery/discovery_test.go`:
```go
package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func mkAccountDir(t *testing.T, home, name, email string) {
	t.Helper()
	dir := filepath.Join(home, name)
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if email != "" {
		content := `{"oauthAccount":{"emailAddress":"` + email + `"}}`
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFind_OrdersDefaultFirst(t *testing.T) {
	home := t.TempDir()
	mkAccountDir(t, home, ".claude-work", "work@example.com")
	mkAccountDir(t, home, ".claude", "personal@example.com")
	mkAccountDir(t, home, ".claude-aaa", "aaa@example.com")

	accounts, err := Find(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 {
		t.Fatalf("got %d accounts, want 3", len(accounts))
	}
	want := []string{".claude", ".claude-aaa", ".claude-work"}
	for i, w := range want {
		if got := filepath.Base(accounts[i].Dir); got != w {
			t.Errorf("accounts[%d] = %s, want %s", i, got, w)
		}
	}
}

func TestFind_ReadsEmail(t *testing.T) {
	home := t.TempDir()
	mkAccountDir(t, home, ".claude", "personal@example.com")

	accounts, err := Find(home)
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].Email != "personal@example.com" {
		t.Errorf("Email = %q, want personal@example.com", accounts[0].Email)
	}
}

func TestFind_SkipsDirsWithoutProjects_AndHandlesMalformedJSON(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude-broken")
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	accounts, err := Find(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1 (only .claude-broken has projects/)", len(accounts))
	}
	if accounts[0].Email != "" {
		t.Errorf("Email = %q, want empty for malformed json", accounts[0].Email)
	}
}

func TestFind_NoAccountsReturnsEmptySlice(t *testing.T) {
	home := t.TempDir()
	accounts, err := Find(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Errorf("got %d accounts, want 0", len(accounts))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd setup && go test ./internal/discovery/... -v`
Expected: FAIL — `Find` undefined.

- [ ] **Step 3: Implement discovery.go**

`setup/internal/discovery/discovery.go`:
```go
// Package discovery finds Claude Code configuration directories on this
// machine and identifies the account behind each one.
package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Account is one discovered Claude Code configuration directory.
type Account struct {
	Dir   string // absolute path to the config directory (contains "projects/")
	Email string // "" if oauthAccount.emailAddress is missing or unreadable
}

type claudeJSON struct {
	OauthAccount struct {
		EmailAddress string `json:"emailAddress"`
	} `json:"oauthAccount"`
}

// Find scans home for Claude Code config directories: "home/.claude" and
// "home/.claude-*", each of which must contain a "projects" subdirectory to
// count. Results are sorted with ".claude" first, then the ".claude-*"
// variants alphabetically. Each Account's Email is read from
// "<dir>/.claude.json"; a missing or unparsable file just leaves it "".
func Find(home string) ([]Account, error) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, err
	}

	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name != ".claude" && !strings.HasPrefix(name, ".claude-") {
			continue
		}
		full := filepath.Join(home, name)
		if info, err := os.Stat(filepath.Join(full, "projects")); err != nil || !info.IsDir() {
			continue
		}
		dirs = append(dirs, full)
	}

	sort.Slice(dirs, func(i, j int) bool {
		bi, bj := filepath.Base(dirs[i]), filepath.Base(dirs[j])
		if bi == ".claude" {
			return true
		}
		if bj == ".claude" {
			return false
		}
		return bi < bj
	})

	accounts := make([]Account, 0, len(dirs))
	for _, d := range dirs {
		accounts = append(accounts, Account{Dir: d, Email: readEmail(d)})
	}
	return accounts, nil
}

func readEmail(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		return ""
	}
	var cfg claudeJSON
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.OauthAccount.EmailAddress
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd setup && go test ./internal/discovery/... -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add setup/internal/discovery
git commit -m "$(cat <<'EOF'
Add discovery package for finding Claude Code config directories

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `internal/health` — endpoint-agnostic reachability check

**Files:**
- Create: `setup/internal/health/health.go`
- Test: `setup/internal/health/health_test.go`

**Interfaces:**
- Produces: `health.Dial(rawURL string, timeout time.Duration) error`.

- [ ] **Step 1: Write the failing tests**

`setup/internal/health/health_test.go`:
```go
package health

import (
	"net"
	"testing"
	"time"
)

func TestDial_Success(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if err := Dial("http://"+ln.Addr().String(), time.Second); err != nil {
		t.Errorf("Dial() = %v, want nil", err)
	}
}

func TestDial_Failure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // now nothing is listening on addr

	if err := Dial("http://"+addr, 500*time.Millisecond); err == nil {
		t.Error("Dial() = nil, want error for closed port")
	}
}

func TestDial_NoPort(t *testing.T) {
	if err := Dial("http://localhost", time.Second); err == nil {
		t.Error("Dial() = nil, want error for missing port")
	}
}

func TestDial_InvalidURL(t *testing.T) {
	if err := Dial("://not a url", time.Second); err == nil {
		t.Error("Dial() = nil, want error for invalid url")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd setup && go test ./internal/health/... -v`
Expected: FAIL — `Dial` undefined.

- [ ] **Step 3: Implement health.go**

`setup/internal/health/health.go`:
```go
// Package health provides a minimal, protocol-agnostic reachability check
// used by the setup wizard before it writes any configuration.
package health

import (
	"fmt"
	"net"
	"net/url"
	"time"
)

// Dial reports whether a TCP connection can be opened to the host:port
// encoded in rawURL, within timeout. It knows nothing about what's on the
// other end — this is the same check whether rawURL points at a local
// docker stack or a remote collector.
func Dial(rawURL string, timeout time.Duration) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid endpoint %q: %w", rawURL, err)
	}
	if u.Port() == "" {
		return fmt.Errorf("endpoint %q has no port", rawURL)
	}
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", rawURL, err)
	}
	conn.Close()
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd setup && go test ./internal/health/... -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add setup/internal/health
git commit -m "$(cat <<'EOF'
Add health package: TCP-dial reachability check for the OTel endpoint

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: `internal/envwriter` — write telemetry config into the shell/OS

**Files:**
- Create: `setup/internal/envwriter/envwriter.go`
- Test: `setup/internal/envwriter/envwriter_test.go`

**Interfaces:**
- Produces: `envwriter.Var{Name, Value string}`, `envwriter.Shell` (`Bash`, `Fish`), `envwriter.Marker string`, `envwriter.Render(shell Shell, vars []Var) string`, `envwriter.WriteBlock(path string, shell Shell, vars []Var) (wrote bool, err error)`, `envwriter.WriteWindows(vars []Var, run func(name, value string) error) error`.

- [ ] **Step 1: Write the failing tests**

`setup/internal/envwriter/envwriter_test.go`:
```go
package envwriter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender_Bash(t *testing.T) {
	out := Render(Bash, []Var{{"FOO", "bar"}, {"BAZ", "qux"}})
	want := Marker + "\nexport FOO=\"bar\"\nexport BAZ=\"qux\"\n"
	if out != want {
		t.Errorf("Render() = %q, want %q", out, want)
	}
}

func TestRender_Fish(t *testing.T) {
	out := Render(Fish, []Var{{"FOO", "bar"}})
	want := Marker + "\nset -gx FOO bar\n"
	if out != want {
		t.Errorf("Render() = %q, want %q", out, want)
	}
}

func TestWriteBlock_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "rc")

	wrote, err := WriteBlock(path, Bash, []Var{{"FOO", "bar"}})
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Error("wrote = false, want true on first write")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), Marker) {
		t.Error("written file missing marker")
	}
}

func TestWriteBlock_IdempotentOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rc")

	if _, err := WriteBlock(path, Bash, []Var{{"FOO", "bar"}}); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)

	wrote, err := WriteBlock(path, Bash, []Var{{"FOO", "different"}})
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("wrote = true on second run, want false (marker already present)")
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Error("file content changed on second run")
	}
}

func TestWriteBlock_PreservesExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rc")
	if err := os.WriteFile(path, []byte("existing line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := WriteBlock(path, Bash, []Var{{"FOO", "bar"}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "existing line\n") {
		t.Errorf("existing content not preserved: %q", data)
	}
}

func TestWriteWindows_CallsRunForEachVar(t *testing.T) {
	var got []string
	run := func(name, value string) error {
		got = append(got, name+"="+value)
		return nil
	}

	if err := WriteWindows([]Var{{"FOO", "bar"}, {"BAZ", "qux"}}, run); err != nil {
		t.Fatal(err)
	}
	want := []string{"FOO=bar", "BAZ=qux"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWriteWindows_PropagatesError(t *testing.T) {
	run := func(name, value string) error { return os.ErrPermission }
	if err := WriteWindows([]Var{{"FOO", "bar"}}, run); err == nil {
		t.Error("WriteWindows() = nil, want error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd setup && go test ./internal/envwriter/... -v`
Expected: FAIL — package has no exported symbols yet.

- [ ] **Step 3: Implement envwriter.go**

`setup/internal/envwriter/envwriter.go`:
```go
// Package envwriter writes the telemetry + collector configuration into a
// shell's startup file (bash/zsh/fish) or, on Windows, into persistent user
// environment variables.
package envwriter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Shell selects the rc-file syntax Render produces.
type Shell int

const (
	Bash Shell = iota
	Fish
)

// Var is one NAME=VALUE pair. Order is preserved in Render's output.
type Var struct {
	Name  string
	Value string
}

// Marker is written as the first line of the block and used to detect an
// already-configured file on a re-run.
const Marker = "# claude-observability: telemetry + collector config"

// Render formats vars as an rc-file block, prefixed by Marker.
func Render(shell Shell, vars []Var) string {
	var b strings.Builder
	b.WriteString(Marker)
	b.WriteString("\n")
	for _, v := range vars {
		switch shell {
		case Fish:
			fmt.Fprintf(&b, "set -gx %s %s\n", v.Name, v.Value)
		default:
			fmt.Fprintf(&b, "export %s=%q\n", v.Name, v.Value)
		}
	}
	return b.String()
}

// WriteBlock appends Render(shell, vars) to path unless Marker is already
// present in it. Creates the file (and its parent directory) if missing.
// wrote is false when the file already had the marker (no-op).
func WriteBlock(path string, shell Shell, vars []Var) (wrote bool, err error) {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if strings.Contains(string(existing), Marker) {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()

	block := Render(shell, vars)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		block = "\n" + block
	}
	if _, err := f.WriteString(block); err != nil {
		return false, err
	}
	return true, nil
}

// WriteWindows sets each var as a persistent user environment variable by
// calling run(name, value) for each — real callers pass a function that
// shells out to `setx`; tests pass a fake. setx overwrites rather than
// appending, so this is naturally idempotent and needs no marker.
func WriteWindows(vars []Var, run func(name, value string) error) error {
	for _, v := range vars {
		if err := run(v.Name, v.Value); err != nil {
			return fmt.Errorf("setting %s: %w", v.Name, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd setup && go test ./internal/envwriter/... -v`
Expected: PASS (7 tests).

- [ ] **Step 5: Commit**

```bash
git add setup/internal/envwriter
git commit -m "$(cat <<'EOF'
Add envwriter package for idempotent shell rc / Windows env writes

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: `internal/limits` — update account-limits.json's ignore list

**Files:**
- Create: `setup/internal/limits/limits.go`
- Test: `setup/internal/limits/limits_test.go`

**Interfaces:**
- Produces: `limits.Update(path, examplePath string, ignored []string) error`.

- [ ] **Step 1: Write the failing tests**

`setup/internal/limits/limits_test.go`:
```go
package limits

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdate_CreatesFromExampleIfMissing(t *testing.T) {
	dir := t.TempDir()
	example := filepath.Join(dir, "account-limits.example.json")
	os.WriteFile(example, []byte(`{"default":{"block_5h":1},"accounts":{"x@example.com":{}},"ignore":[]}`), 0o644)
	path := filepath.Join(dir, "account-limits.json")

	if err := Update(path, example, []string{"a@example.com"}); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	ignore := doc["ignore"].([]interface{})
	if len(ignore) != 1 || ignore[0] != "a@example.com" {
		t.Errorf("ignore = %v, want [a@example.com]", ignore)
	}
	accounts := doc["accounts"].(map[string]interface{})
	if len(accounts) != 0 {
		t.Errorf("accounts = %v, want empty (cleared)", accounts)
	}
	if _, ok := doc["default"]; !ok {
		t.Error("default field not preserved")
	}
}

func TestUpdate_DedupesAndSkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account-limits.json")
	os.WriteFile(path, []byte(`{"ignore":[],"accounts":{}}`), 0o644)

	err := Update(path, filepath.Join(dir, "unused-example.json"), []string{"a@example.com", "a@example.com", ""})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var doc map[string]interface{}
	json.Unmarshal(data, &doc)
	ignore := doc["ignore"].([]interface{})
	if len(ignore) != 1 {
		t.Errorf("ignore = %v, want 1 deduped entry", ignore)
	}
}

func TestUpdate_PreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account-limits.json")
	os.WriteFile(path, []byte(`{"_comment":["hi"],"default":{"block_5h":99},"accounts":{"old@example.com":{}},"ignore":["old@example.com"]}`), 0o644)

	if err := Update(path, filepath.Join(dir, "unused.json"), []string{"new@example.com"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var doc map[string]interface{}
	json.Unmarshal(data, &doc)
	if _, ok := doc["_comment"]; !ok {
		t.Error("_comment not preserved")
	}
	def := doc["default"].(map[string]interface{})
	if def["block_5h"].(float64) != 99 {
		t.Error("default.block_5h not preserved")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd setup && go test ./internal/limits/... -v`
Expected: FAIL — `Update` undefined.

- [ ] **Step 3: Implement limits.go**

`setup/internal/limits/limits.go`:
```go
// Package limits updates grafana/account-limits.json's "ignore" list — the
// accounts the setup wizard discovered but the user chose not to monitor.
package limits

import (
	"encoding/json"
	"os"
)

// Update sets the "ignore" field of the JSON file at path to ignored
// (deduped, skipping empty strings) and clears "accounts" to an empty
// object, preserving every other top-level field untouched. If path
// doesn't exist, it's first created as a copy of examplePath.
func Update(path, examplePath string, ignored []string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data, err = os.ReadFile(examplePath)
		if err != nil {
			return err
		}
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}

	seen := make(map[string]bool, len(ignored))
	deduped := make([]interface{}, 0, len(ignored))
	for _, email := range ignored {
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		deduped = append(deduped, email)
	}
	doc["ignore"] = deduped
	doc["accounts"] = map[string]interface{}{}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd setup && go test ./internal/limits/... -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add setup/internal/limits
git commit -m "$(cat <<'EOF'
Add limits package: update account-limits.json ignore list in Go, no jq

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: `internal/wizard` — interactive prompts

**Files:**
- Create: `setup/internal/wizard/wizard.go`
- Test: `setup/internal/wizard/wizard_test.go`

**Interfaces:**
- Consumes: `discovery.Account{Dir, Email string}` (Task 2).
- Produces: `wizard.AskLine(w io.Writer, r *bufio.Reader, label, def string) string`, `wizard.AskYesNo(w io.Writer, r *bufio.Reader, prompt string, defaultYes bool) bool`, `wizard.ChooseAccounts(w io.Writer, r *bufio.Reader, accounts []discovery.Account) (chosen, rejected []discovery.Account)`.

- [ ] **Step 1: Write the failing tests**

`setup/internal/wizard/wizard_test.go`:
```go
package wizard

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"claude-observability-setup/internal/discovery"
)

func TestAskLine_EmptyReturnsDefault(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\n"))
	var out bytes.Buffer
	got := AskLine(&out, r, "OTel endpoint", "http://localhost:47317")
	if got != "http://localhost:47317" {
		t.Errorf("got %q, want default", got)
	}
}

func TestAskLine_InputOverridesDefault(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("http://example.com:9999\n"))
	var out bytes.Buffer
	got := AskLine(&out, r, "OTel endpoint", "http://localhost:47317")
	if got != "http://example.com:9999" {
		t.Errorf("got %q, want typed value", got)
	}
}

func TestAskYesNo_DefaultOnEmptyInput(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\n"))
	var out bytes.Buffer
	if !AskYesNo(&out, r, "Monitor all?", true) {
		t.Error("expected default true")
	}
}

func TestAskYesNo_ExplicitNo(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("n\n"))
	var out bytes.Buffer
	if AskYesNo(&out, r, "Monitor all?", true) {
		t.Error("expected false for explicit 'n'")
	}
}

func TestChooseAccounts_SingleAccountAutoChosen(t *testing.T) {
	accounts := []discovery.Account{{Dir: "/home/.claude", Email: "a@example.com"}}
	r := bufio.NewReader(strings.NewReader(""))
	var out bytes.Buffer
	chosen, rejected := ChooseAccounts(&out, r, accounts)
	if len(chosen) != 1 || len(rejected) != 0 {
		t.Errorf("chosen=%v rejected=%v, want single auto-chosen", chosen, rejected)
	}
}

func TestChooseAccounts_MonitorAll(t *testing.T) {
	accounts := []discovery.Account{
		{Dir: "/home/.claude", Email: "a@example.com"},
		{Dir: "/home/.claude-work", Email: "b@example.com"},
	}
	r := bufio.NewReader(strings.NewReader("y\n"))
	var out bytes.Buffer
	chosen, rejected := ChooseAccounts(&out, r, accounts)
	if len(chosen) != 2 || len(rejected) != 0 {
		t.Errorf("chosen=%d rejected=%d, want 2/0", len(chosen), len(rejected))
	}
}

func TestChooseAccounts_PerAccountChoice(t *testing.T) {
	accounts := []discovery.Account{
		{Dir: "/home/.claude", Email: "a@example.com"},
		{Dir: "/home/.claude-work", Email: "b@example.com"},
	}
	// "n" to "monitor all?", then "y" for the first, "n" for the second.
	r := bufio.NewReader(strings.NewReader("n\ny\nn\n"))
	var out bytes.Buffer
	chosen, rejected := ChooseAccounts(&out, r, accounts)
	if len(chosen) != 1 || chosen[0].Email != "a@example.com" {
		t.Errorf("chosen = %v, want [a@example.com]", chosen)
	}
	if len(rejected) != 1 || rejected[0].Email != "b@example.com" {
		t.Errorf("rejected = %v, want [b@example.com]", rejected)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd setup && go test ./internal/wizard/... -v`
Expected: FAIL — package has no exported symbols yet.

- [ ] **Step 3: Implement wizard.go**

`setup/internal/wizard/wizard.go`:
```go
// Package wizard implements the interactive prompts the setup binary shows:
// which accounts to monitor, and the OTel endpoint/token to use.
package wizard

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"claude-observability-setup/internal/discovery"
)

// AskLine prints "label [def]: " to w, reads one line from r, and returns
// the trimmed input, or def if the input was empty.
func AskLine(w io.Writer, r *bufio.Reader, label, def string) string {
	fmt.Fprintf(w, "%s [%s]: ", label, def)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// AskYesNo prints prompt with a [y/N] or [Y/n] hint depending on
// defaultYes, reads one line from r, and returns true iff the input starts
// with 'y'/'Y' (empty input returns defaultYes).
func AskYesNo(w io.Writer, r *bufio.Reader, prompt string, defaultYes bool) bool {
	hint := "y/N"
	if defaultYes {
		hint = "Y/n"
	}
	fmt.Fprintf(w, "%s [%s] ", prompt, hint)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultYes
	}
	return line[0] == 'y' || line[0] == 'Y'
}

// ChooseAccounts prints the discovered accounts and asks which to monitor.
// A single discovered account is chosen automatically, without prompting.
func ChooseAccounts(w io.Writer, r *bufio.Reader, accounts []discovery.Account) (chosen, rejected []discovery.Account) {
	if len(accounts) == 0 {
		return nil, nil
	}
	if len(accounts) == 1 {
		fmt.Fprintln(w, "  Only one directory found — using it.")
		return accounts, nil
	}

	fmt.Fprintln(w, "  Found:")
	for i, a := range accounts {
		email := a.Email
		if email == "" {
			email = "(unidentified account)"
		}
		fmt.Fprintf(w, "    [%d] %-24s %s\n", i+1, a.Dir, email)
	}

	if AskYesNo(w, r, "  Monitor all of these accounts?", true) {
		return accounts, nil
	}
	for _, a := range accounts {
		email := a.Email
		if email == "" {
			email = "(unidentified account)"
		}
		if AskYesNo(w, r, fmt.Sprintf("    Monitor %s?", email), true) {
			chosen = append(chosen, a)
		} else {
			rejected = append(rejected, a)
		}
	}
	return chosen, rejected
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd setup && go test ./internal/wizard/... -v`
Expected: PASS (7 tests).

- [ ] **Step 5: Commit**

```bash
git add setup/internal/wizard
git commit -m "$(cat <<'EOF'
Add wizard package: account selection + endpoint/token prompts

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: `internal/service` — shared Config + macOS launchd installer

**Files:**
- Create: `setup/internal/service/service.go`
- Create: `setup/internal/service/service_darwin.go`
- Test: `setup/internal/service/service_darwin_test.go`

**Interfaces:**
- Consumes: `envwriter.Var` (Task 4).
- Produces: `service.Config{RepoRoot, NodeBin, ClaudeBin string, Env []envwriter.Var, LogDir string}`, `service.Config.CollectorScript() string`; on darwin: `service.GeneratePlist(cfg Config) string`, `service.Install(cfg Config) error`.

- [ ] **Step 1: Implement the shared Config type**

`setup/internal/service/service.go`:
```go
// Package service installs the collector (collector/collector.mjs) as a
// background process: a launchd LaunchAgent on macOS, a systemd --user unit
// on Linux, and a logon-triggered Scheduled Task on Windows.
package service

import (
	"path/filepath"

	"claude-observability-setup/internal/envwriter"
)

// Config holds everything an OS-specific installer needs to register the
// collector as a background service.
type Config struct {
	RepoRoot  string          // absolute path to the claude-observability checkout
	NodeBin   string          // absolute path to `node`, from exec.LookPath("node")
	ClaudeBin string          // absolute path to `claude`, from exec.LookPath("claude")
	Env       []envwriter.Var // collector env vars to bake in (CLAUDE_DIR, EXPORTER_STREAM, ...)
	LogDir    string          // absolute path for stdout/stderr logs
}

// CollectorScript returns the absolute path to collector/collector.mjs
// under cfg.RepoRoot.
func (c Config) CollectorScript() string {
	return filepath.Join(c.RepoRoot, "collector", "collector.mjs")
}
```

- [ ] **Step 2: Write the failing darwin tests**

`setup/internal/service/service_darwin_test.go`:
```go
//go:build darwin

package service

import (
	"strings"
	"testing"

	"claude-observability-setup/internal/envwriter"
)

func TestGeneratePlist_ContainsLabelAndEnv(t *testing.T) {
	cfg := Config{
		RepoRoot:  "/repo",
		NodeBin:   "/usr/local/bin/node",
		ClaudeBin: "/usr/local/bin/claude",
		Env:       []envwriter.Var{{Name: "CLAUDE_DIR", Value: "/home/.claude"}},
		LogDir:    "/repo/.state",
	}
	out := GeneratePlist(cfg)

	for _, want := range []string{
		"com.agents-observability.collector",
		"/usr/local/bin/node",
		"/repo/collector/collector.mjs",
		"<key>CLAUDE_DIR</key><string>/home/.claude</string>",
		"/repo/.state/collector.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestGeneratePlist_EscapesXML(t *testing.T) {
	cfg := Config{
		RepoRoot: "/repo", NodeBin: "/bin/node", ClaudeBin: "/bin/claude", LogDir: "/repo/.state",
		Env: []envwriter.Var{{Name: "FOO", Value: "a & b < c"}},
	}
	out := GeneratePlist(cfg)
	if !strings.Contains(out, "a &amp; b &lt; c") {
		t.Errorf("plist did not escape XML special chars: %s", out)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd setup && go test ./internal/service/... -v` (on macOS)
Expected: FAIL — `GeneratePlist` undefined.

- [ ] **Step 4: Implement service_darwin.go**

`setup/internal/service/service_darwin.go`:
```go
//go:build darwin

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const label = "com.agents-observability.collector"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// GeneratePlist renders the launchd LaunchAgent plist for cfg.
func GeneratePlist(cfg Config) string {
	var envXML strings.Builder
	for _, v := range cfg.Env {
		fmt.Fprintf(&envXML, "    <key>%s</key><string>%s</string>\n", v.Name, xmlEscape(v.Value))
	}
	pathEnv := filepath.Dir(cfg.NodeBin) + ":" + filepath.Dir(cfg.ClaudeBin) + ":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>%s</string>
  </array>
  <key>WorkingDirectory</key><string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>%s</string>
%s  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
`, label, cfg.NodeBin, cfg.CollectorScript(), filepath.Join(cfg.RepoRoot, "collector"),
		pathEnv, envXML.String(),
		filepath.Join(cfg.LogDir, "collector.log"), filepath.Join(cfg.LogDir, "collector.err.log"))
}

// Install writes the plist and (re)loads it via launchctl.
func Install(cfg Config) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(GeneratePlist(cfg)), 0o644); err != nil {
		return err
	}

	uid := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", uid, path).Run() // ignore error: fine if not loaded yet
	if out, err := exec.Command("launchctl", "bootstrap", uid, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
	}
	if out, err := exec.Command("launchctl", "enable", uid+"/"+label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl enable: %w: %s", err, out)
	}
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd setup && go test ./internal/service/... -v` (on macOS)
Expected: PASS (2 tests).

- [ ] **Step 6: Commit**

```bash
git add setup/internal/service/service.go setup/internal/service/service_darwin.go setup/internal/service/service_darwin_test.go
git commit -m "$(cat <<'EOF'
Add service package: shared Config + macOS launchd installer

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: `internal/service` — Linux systemd `--user` installer

**Files:**
- Create: `setup/internal/service/service_linux.go`
- Test: `setup/internal/service/service_linux_test.go`

**Interfaces:**
- Consumes: `service.Config` (Task 7).
- Produces: `service.GenerateUnit(cfg Config) string`, `service.Install(cfg Config) error` (Linux build).

- [ ] **Step 1: Write the failing tests**

`setup/internal/service/service_linux_test.go`:
```go
//go:build linux

package service

import (
	"strings"
	"testing"

	"claude-observability-setup/internal/envwriter"
)

func TestGenerateUnit_ContainsExecAndEnv(t *testing.T) {
	cfg := Config{
		RepoRoot: "/repo", NodeBin: "/usr/bin/node", ClaudeBin: "/usr/bin/claude", LogDir: "/repo/.state",
		Env: []envwriter.Var{{Name: "CLAUDE_DIR", Value: "/home/.claude"}},
	}
	out := GenerateUnit(cfg)

	for _, want := range []string{
		"ExecStart=/usr/bin/node /repo/collector/collector.mjs",
		"Environment=CLAUDE_DIR=/home/.claude",
		"WantedBy=default.target",
		"StandardOutput=append:/repo/.state/collector.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit missing %q, got:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd setup && go test ./internal/service/... -v` (on Linux)
Expected: FAIL — `GenerateUnit` undefined.

- [ ] **Step 3: Implement service_linux.go**

`setup/internal/service/service_linux.go`:
```go
//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const unitName = "claude-observability-collector.service"

func unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", unitName), nil
}

// GenerateUnit renders the systemd --user unit file for cfg.
func GenerateUnit(cfg Config) string {
	var envLines strings.Builder
	for _, v := range cfg.Env {
		fmt.Fprintf(&envLines, "Environment=%s=%s\n", v.Name, v.Value)
	}
	pathEnv := filepath.Dir(cfg.NodeBin) + ":" + filepath.Dir(cfg.ClaudeBin) + ":/usr/local/bin:/usr/bin:/bin"

	return fmt.Sprintf(`[Unit]
Description=claude-observability collector

[Service]
Type=simple
WorkingDirectory=%s
Environment=PATH=%s
%sExecStart=%s %s
Restart=always
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, filepath.Join(cfg.RepoRoot, "collector"), pathEnv, envLines.String(), cfg.NodeBin, cfg.CollectorScript(),
		filepath.Join(cfg.LogDir, "collector.log"), filepath.Join(cfg.LogDir, "collector.err.log"))
}

// Install writes the unit file and enables + starts it via systemctl --user.
func Install(cfg Config) error {
	path, err := unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(GenerateUnit(cfg)), 0o644); err != nil {
		return err
	}

	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, out)
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable: %w: %s", err, out)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd setup && go test ./internal/service/... -v` (on Linux)
Expected: PASS (1 test).

- [ ] **Step 5: Commit**

```bash
git add setup/internal/service/service_linux.go setup/internal/service/service_linux_test.go
git commit -m "$(cat <<'EOF'
Add Linux systemd --user installer for the collector service

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: `internal/service` — Windows Scheduled Task installer

**Files:**
- Create: `setup/internal/service/service_windows.go`
- Test: `setup/internal/service/service_windows_test.go`

**Interfaces:**
- Consumes: `service.Config` (Task 7).
- Produces: `service.GenerateTaskArgs(cfg Config) []string`, `service.Install(cfg Config) error` (Windows build).

- [ ] **Step 1: Write the failing tests**

`setup/internal/service/service_windows_test.go`:
```go
//go:build windows

package service

import (
	"strings"
	"testing"

	"claude-observability-setup/internal/envwriter"
)

func TestGenerateTaskArgs_ContainsTaskNameAndCommand(t *testing.T) {
	cfg := Config{
		RepoRoot: `C:\repo`, NodeBin: `C:\node\node.exe`, ClaudeBin: `C:\node\claude.exe`, LogDir: `C:\repo\.state`,
		Env: []envwriter.Var{{Name: "CLAUDE_DIR", Value: `C:\Users\me\.claude`}},
	}
	args := GenerateTaskArgs(cfg)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"ClaudeObservabilityCollector",
		"onlogon",
		`set CLAUDE_DIR=C:\Users\me\.claude`,
		`C:\node\node.exe`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q, got: %s", want, joined)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd setup && go test ./internal/service/... -v` (on Windows)
Expected: FAIL — `GenerateTaskArgs` undefined.

- [ ] **Step 3: Implement service_windows.go**

`setup/internal/service/service_windows.go`:
```go
//go:build windows

package service

import (
	"fmt"
	"os"
	"os/exec"
)

const taskName = "ClaudeObservabilityCollector"

// GenerateTaskArgs returns the `schtasks /create` argument list that
// registers the collector to run at logon. Env vars are passed as part of
// the command line (`set NAME=VALUE&& ...`) since schtasks has no native
// per-task environment block. Paths are wrapped in plain double quotes, not
// Go's %q (which would escape the backslashes in a Windows path and break
// cmd.exe's own quoting).
func GenerateTaskArgs(cfg Config) []string {
	cmd := ""
	for _, v := range cfg.Env {
		cmd += fmt.Sprintf("set %s=%s&& ", v.Name, v.Value)
	}
	cmd += fmt.Sprintf(`"%s" "%s"`, cfg.NodeBin, cfg.CollectorScript())

	return []string{
		"/create", "/f",
		"/tn", taskName,
		"/sc", "onlogon",
		"/tr", "cmd /c " + cmd,
	}
}

// Install registers the Scheduled Task via schtasks.
func Install(cfg Config) error {
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return err
	}
	out, err := exec.Command("schtasks", GenerateTaskArgs(cfg)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /create: %w: %s", err, out)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd setup && go test ./internal/service/... -v` (on Windows)
Expected: PASS (1 test).

- [ ] **Step 5: Commit**

```bash
git add setup/internal/service/service_windows.go setup/internal/service/service_windows_test.go
git commit -m "$(cat <<'EOF'
Add Windows Scheduled Task installer for the collector service

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: `cmd/setup/main.go` — wire the full wizard together

**Files:**
- Modify: `setup/cmd/setup/main.go`

**Interfaces:**
- Consumes: `discovery.Find` (Task 2), `health.Dial` (Task 3), `envwriter.{Var,Shell,Bash,Fish,WriteBlock,WriteWindows}` (Task 4), `limits.Update` (Task 5), `wizard.{AskLine,AskYesNo,ChooseAccounts}` (Task 6), `service.{Config,Install}` (Tasks 7-9).
- Produces: the `claude-observability-setup` binary's end-to-end behavior.

- [ ] **Step 1: Replace main.go with the full orchestration**

`setup/cmd/setup/main.go`:
```go
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"claude-observability-setup/internal/discovery"
	"claude-observability-setup/internal/envwriter"
	"claude-observability-setup/internal/health"
	"claude-observability-setup/internal/limits"
	"claude-observability-setup/internal/service"
	"claude-observability-setup/internal/wizard"
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
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}

	stdin := bufio.NewReader(os.Stdin)
	out := os.Stdout

	fmt.Fprintln(out, "claude-observability setup")
	fmt.Fprintln(out, "Configures Claude Code telemetry and the collector service. Does NOT start")
	fmt.Fprintln(out, "docker for you — bring the stack up yourself first, then run this.")

	fmt.Fprintln(out, "\n── Which accounts to monitor ──────────────────────────────")
	accounts, err := discovery.Find(home)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Fprintln(out, "  No config directory found (~/.claude*/projects).")
		fmt.Fprintln(out, "  Falling back to ~/.claude — re-run this after your first Claude Code session.")
		accounts = []discovery.Account{{Dir: filepath.Join(home, ".claude")}}
	}
	chosen, rejected := wizard.ChooseAccounts(out, stdin, accounts)

	fmt.Fprintln(out, "\n── OTel endpoint ───────────────────────────────────────────")
	endpoint := wizard.AskLine(out, stdin, "OTel endpoint", "http://localhost:47317")
	token := wizard.AskLine(out, stdin, "OTel token", "")

	fmt.Fprintf(out, "  checking %s ... ", endpoint)
	if err := health.Dial(endpoint, 3*time.Second); err != nil {
		fmt.Fprintln(out, "unreachable")
		if endpoint == "http://localhost:47317" {
			return fmt.Errorf("can't reach %s — nothing is listening there.\n  Run `docker compose up -d` first, then re-run this wizard", endpoint)
		}
		return fmt.Errorf("can't reach %s — nothing is listening there.\n  Check the URL and that it's reachable from this machine", endpoint)
	}
	fmt.Fprintln(out, "ok")

	telemetryVars := []envwriter.Var{
		{Name: "CLAUDE_CODE_ENABLE_TELEMETRY", Value: "1"},
		{Name: "OTEL_METRICS_EXPORTER", Value: "otlp"},
		{Name: "OTEL_LOGS_EXPORTER", Value: "otlp"},
		{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "grpc"},
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: endpoint},
		{Name: "OTEL_METRIC_EXPORT_INTERVAL", Value: "60000"},
		{Name: "OTEL_LOGS_EXPORT_INTERVAL", Value: "30000"},
	}
	if token != "" {
		telemetryVars = append(telemetryVars, envwriter.Var{
			Name:  "OTEL_EXPORTER_OTLP_HEADERS",
			Value: "Authorization=Bearer%20" + token,
		})
	}

	var collectorVars []envwriter.Var
	if len(chosen) > 0 {
		collectorVars = append(collectorVars, envwriter.Var{Name: "CLAUDE_DIR", Value: chosen[0].Dir})
		if len(chosen) > 1 {
			extra := chosen[1].Dir
			for _, a := range chosen[2:] {
				extra += ":" + a.Dir
			}
			collectorVars = append(collectorVars, envwriter.Var{Name: "CLAUDE_OBSERVABILITY_EXTRA_DIRS", Value: extra})
		}
		collectorVars = append(collectorVars, envwriter.Var{Name: "EXPORTER_STREAM", Value: "claude-code-exporter-1"})
	}

	allVars := append(append([]envwriter.Var{}, telemetryVars...), collectorVars...)

	fmt.Fprintln(out, "\n── Enabling telemetry in your shell ───────────────────────")
	if err := writeShellConfig(out, home, allVars); err != nil {
		return err
	}

	if len(rejected) > 0 {
		fmt.Fprintln(out, "\n── Updating ignore list ────────────────────────────────────")
		var emails []string
		for _, a := range rejected {
			if a.Email != "" {
				emails = append(emails, a.Email)
			}
		}
		limitsPath := filepath.Join(repoRoot, "grafana", "account-limits.json")
		examplePath := filepath.Join(repoRoot, "grafana", "account-limits.example.json")
		if err := limits.Update(limitsPath, examplePath, emails); err != nil {
			return err
		}
		fmt.Fprintf(out, "  not monitored: %v\n", emails)
	}

	fmt.Fprintln(out, "\n── Collector service ───────────────────────────────────────")
	if wizard.AskYesNo(out, stdin, "  Install the collector as a background service now?", true) {
		if err := installService(repoRoot, collectorVars); err != nil {
			return fmt.Errorf("installing collector service: %w", err)
		}
		fmt.Fprintln(out, "  installed and started")
	}

	fmt.Fprintln(out, "\n── Done ─────────────────────────────────────────────────────")
	fmt.Fprintln(out, "  Open a new terminal (or reload your shell config) to pick up the telemetry vars.")
	return nil
}

// findRepoRoot requires the binary to be run from the claude-observability
// repo root — it doesn't bundle docker-compose.yaml/collector/grafana, so
// those still have to come from the checkout it's invoked inside.
func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(wd, "docker-compose.yaml")); err != nil {
		return "", fmt.Errorf("must be run from the claude-observability repo root (docker-compose.yaml not found in %s)", wd)
	}
	return wd, nil
}

func writeShellConfig(out *os.File, home string, vars []envwriter.Var) error {
	if runtime.GOOS == "windows" {
		return envwriter.WriteWindows(vars, func(name, value string) error {
			return exec.Command("setx", name, value).Run()
		})
	}

	shellName := filepath.Base(os.Getenv("SHELL"))
	var path string
	var shell envwriter.Shell
	switch shellName {
	case "fish":
		path = filepath.Join(home, ".config", "fish", "config.fish")
		shell = envwriter.Fish
	case "zsh":
		path = filepath.Join(home, ".zshrc")
		shell = envwriter.Bash
	default:
		path = filepath.Join(home, ".bashrc")
		shell = envwriter.Bash
	}

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

func installService(repoRoot string, collectorVars []envwriter.Var) error {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("node not found on PATH: %w", err)
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found on PATH: %w", err)
	}
	cfg := service.Config{
		RepoRoot:  repoRoot,
		NodeBin:   nodeBin,
		ClaudeBin: claudeBin,
		Env:       collectorVars,
		LogDir:    filepath.Join(repoRoot, ".state"),
	}
	return service.Install(cfg)
}
```

- [ ] **Step 2: Verify it builds**

Run: `cd setup && go build ./...`
Expected: no errors.

- [ ] **Step 3: Manual smoke test — health check failure path**

Run from the repo root (a directory containing `docker-compose.yaml`):
```bash
cd setup && go build -o /tmp/claude-observability-setup ./cmd/setup
cd ..
printf 'n\nhttp://localhost:1\n\n' | /tmp/claude-observability-setup
```
Expected: prints the account section, then fails on the health check with
`can't reach http://localhost:1 — nothing is listening there.` and a
non-zero exit code (check with `echo $?`).

- [ ] **Step 4: Manual smoke test — health check success path**

```bash
python3 -m http.server 47317 --bind 127.0.0.1 &
SERVER_PID=$!
printf 'n\n\n\n\nn\n' | /tmp/claude-observability-setup
kill $SERVER_PID
```
Expected: the health check prints `ok`, the wizard writes the telemetry
block into your shell rc (check with `tail -20 ~/.bashrc` or the
appropriate rc file — remove the added block afterward if this was run on
a real machine, not a throwaway container/VM).

- [ ] **Step 5: Commit**

```bash
git add setup/cmd/setup/main.go
git commit -m "$(cat <<'EOF'
Wire the setup wizard end to end in cmd/setup/main.go

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: Release workflow — cross-compiled binaries on GitHub Releases

**Files:**
- Create: `.github/workflows/release.yml`

**Interfaces:**
- Produces: on pushing a tag matching `v*`, binaries `claude-observability-setup-<goos>-<goarch>[.exe]` attached to a GitHub Release for darwin/linux (amd64+arm64) and windows (amd64).

- [ ] **Step 1: Write the release workflow**

`.github/workflows/release.yml`:
```yaml
name: release
on:
  push:
    tags: ['v*']
jobs:
  build:
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
          go-version: '1.22'
      - name: build
        working-directory: setup
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CGO_ENABLED: '0'
        run: |
          ext=""
          if [ "${{ matrix.goos }}" = "windows" ]; then ext=".exe"; fi
          go build -o "claude-observability-setup-${{ matrix.goos }}-${{ matrix.goarch }}${ext}" ./cmd/setup
      - uses: actions/upload-artifact@v4
        with:
          name: binaries-${{ matrix.goos }}-${{ matrix.goarch }}
          path: setup/claude-observability-setup-*
  release:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v4
        with:
          path: dist
          merge-multiple: true
      - uses: softprops/action-gh-release@v2
        with:
          files: dist/*
```

- [ ] **Step 2: Validate the workflow YAML parses**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))"`
Expected: no error (adjust the `python3 -c` invocation to whatever YAML linter is available if `pyyaml` isn't installed — e.g. `yamllint .github/workflows/release.yml`).

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "$(cat <<'EOF'
Add release workflow: cross-compiled setup binaries on tag push

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 12: Remove old bash scripts and update README

**Files:**
- Delete: `bin/setup`, `bin/install-service.sh`, `bin/claude-telemetry.sh`, `bin/claude-telemetry.fish`
- Modify: `README.md` ("Getting started" and "Enabling telemetry in every session" sections)

**Interfaces:**
- None (documentation + cleanup only).

- [ ] **Step 1: Remove the old scripts**

```bash
git rm bin/setup bin/install-service.sh bin/claude-telemetry.sh bin/claude-telemetry.fish
```

- [ ] **Step 2: Update README's "Getting started" section**

In `README.md`, replace the "Getting started" section's description of
`bin/setup` and the "By hand" fallback with:

```markdown
## Getting started

Bring the stack up yourself first — setup never does this for you:

```sh
docker compose up -d               # otel-collector + loki + grafana
```

Then download the `claude-observability-setup` binary for your OS/arch from
this repo's [Releases page](../../releases) (or build it yourself:
`cd setup && go build ./cmd/setup`), and run it from the repo root:

```sh
./claude-observability-setup
```

It **discovers on its own** which Claude Code config directories you have,
identifies the account behind each one, asks which ones you want to
monitor, asks for the OTel endpoint and an optional token (prefilled with
the local defaults — edit them if you're running docker on different
ports), checks that the endpoint is actually reachable before writing
anything, enables telemetry in the right shell/OS, and offers to install
the collector as a background service.
```

Update the "One dashboard per account" / "By hand" bullet list further down
that still references `bin/install-service.sh` to instead describe the
binary's "Install the collector as a background service now?" prompt.

- [ ] **Step 3: Update "Enabling telemetry in every session" section**

Replace the `cat bin/claude-telemetry.sh >> ...` instructions with a note
that the setup binary does this automatically per-OS (bash/zsh rc, fish
config, or Windows persistent user env vars via `setx`), and that
`OTEL_EXPORTER_OTLP_ENDPOINT`/`OTEL_EXPORTER_OTLP_HEADERS` are the two
values the wizard's "OTel endpoint"/"OTel token" prompts control.

- [ ] **Step 4: Grep for stale references**

Run: `grep -rn "bin/setup\|bin/install-service\|bin/claude-telemetry" README.md`
Expected: no matches (fix any remaining ones found).

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "$(cat <<'EOF'
Remove old bash setup scripts, point README at the Go binary

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```
