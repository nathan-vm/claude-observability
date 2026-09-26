package usagetruth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	// invalid JSON (confirmed by hexdumping the fixture's actual output).
	// A heredoc prints its body verbatim, so json.Marshal's own correct
	// "\n" (backslash-n) escaping survives untouched.
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

func TestWriteEmptyMCPConfigFile_ReusesDirWhenPresent(t *testing.T) {
	path1, err := writeEmptyMCPConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	path2, err := writeEmptyMCPConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if path1 != path2 {
		t.Errorf("path1 = %q, path2 = %q, want the same path when nothing removed it", path1, path2)
	}
}

func TestWriteEmptyMCPConfigFile_SelfHealsWhenDirectoryRemoved(t *testing.T) {
	path1, err := writeEmptyMCPConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Dir(path1)
	if err := os.RemoveAll(oldDir); err != nil {
		t.Fatal(err)
	}

	// Simulates the directory vanishing underneath a long-running
	// process (e.g. systemd-tmpfiles-clean) — must recreate and retry,
	// never latch the resulting dangling path or an error forever.
	path2, err := writeEmptyMCPConfigFile()
	if err != nil {
		t.Fatalf("writeEmptyMCPConfigFile did not self-heal after its directory vanished: %v", err)
	}
	if _, err := os.Stat(path2); err != nil {
		t.Fatalf("path2 %q not created after healing: %v", path2, err)
	}
	if _, err := os.Stat(oldDir); err == nil {
		t.Errorf("old directory %q still exists after healing; want it removed so repeated healing can't accumulate directories", oldDir)
	}
	content, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != emptyMCPConfig {
		t.Errorf("content = %q, want %q", content, emptyMCPConfig)
	}
}

// TestKillProcessGroup_AlreadyExitedReturnsErrProcessDone pins the
// os/exec Cmd.Cancel contract directly and deterministically, rather than
// racing a timeout against a fast-exiting real `claude` process end to
// end: killProcessGroup must map "group already gone" (ESRCH) to
// os.ErrProcessDone, the only sentinel Cmd.Cancel's contract treats as
// "not a real Cancel failure" — a raw ESRCH would otherwise turn an
// already-successful run into a spurious Wait() error.
func TestKillProcessGroup_AlreadyExitedReturnsErrProcessDone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group kill is POSIX-specific; see proc_windows.go")
	}
	cmd := exec.Command("true")
	setNewProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := killProcessGroup(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("killProcessGroup() on an already-reaped process = %v, want an error satisfying errors.Is(err, os.ErrProcessDone)", err)
	}
}

func TestRunClaudeWithTimeout_KillsGrandchildProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group kill is POSIX-specific; see proc_windows.go")
	}
	dir := t.TempDir()
	heartbeat := filepath.Join(dir, "heartbeat")
	// The fixture backgrounds a loop that appends one line every ~20ms —
	// simulating a grandchild like `docker run` that a plain
	// cmd.Process.Kill() (direct child only) would leave running — then
	// blocks in the foreground well past the test's short timeout, so the
	// only thing that can end this script is our own timeout-triggered
	// group kill. A shell-builtin line count, not a `date +%N` timestamp:
	// BSD/macOS date has no nanosecond field, and counting lines instead of
	// comparing wall-clock content also avoids forking an external `date`
	// binary per iteration, which is what made the first heartbeat arrive
	// late enough to make a short timeout flaky here.
	script := "#!/bin/sh\n" +
		`( i=0; while true; do i=$((i+1)); echo $i >> ` + heartbeat + `; sleep 0.02; done ) &` + "\n" +
		"sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := runClaudeWithTimeout("/tmp/cfg", 1*time.Second)
	if err == nil {
		t.Fatal("want a timeout error")
	}

	lineCount := func() int {
		b, _ := os.ReadFile(heartbeat)
		if len(b) == 0 {
			return 0
		}
		return len(strings.Split(strings.TrimSpace(string(b)), "\n"))
	}
	first := lineCount()
	if first == 0 {
		t.Fatal("grandchild never started heartbeating — fixture didn't run as expected")
	}
	time.Sleep(500 * time.Millisecond) // well past the 20ms heartbeat interval
	second := lineCount()
	if first != second {
		t.Errorf("heartbeat still advancing after timeout (%d -> %d lines); want the grandchild to have died with the process group, not outlived it", first, second)
	}
}
