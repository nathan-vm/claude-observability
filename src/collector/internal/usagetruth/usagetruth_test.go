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
