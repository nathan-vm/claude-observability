package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"claude-observability-wizard/internal/discovery"
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

func TestSettingsTargets_FromChosenOnlyDedupedInOrder(t *testing.T) {
	chosen := []discovery.Account{{Dir: "/a/.claude"}, {Dir: "/a/.claude-work/"}, {Dir: "/a/.claude"}}
	got := settingsTargets(chosen)
	want := []string{"/a/.claude", "/a/.claude-work"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != filepath.FromSlash(want[i]) {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if settingsTargets(nil) != nil {
		t.Error("nil chosen should yield no targets")
	}
}

func TestWriteClaudeSettings_SkipsBadFileAndContinues(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good")
	bad := filepath.Join(root, "bad")
	for _, d := range []string{good, bad} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	badContent := "{not json"
	if err := os.WriteFile(filepath.Join(bad, "settings.json"), []byte(badContent), 0o600); err != nil {
		t.Fatal(err)
	}

	settingsVars, _ := buildTelemetryVars("http://localhost:47317", "", "")
	var out bytes.Buffer
	updated, skipped := writeClaudeSettings(&out, []string{bad, good}, settingsVars)

	if len(updated) != 1 || updated[0] != filepath.Join(good, "settings.json") {
		t.Errorf("updated = %v", updated)
	}
	if len(skipped) != 1 || skipped[0] != filepath.Join(bad, "settings.json") {
		t.Errorf("skipped = %v", skipped)
	}
	got, _ := os.ReadFile(filepath.Join(bad, "settings.json"))
	if string(got) != badContent {
		t.Error("bad file was modified")
	}
	if _, err := os.Stat(filepath.Join(good, "settings.json")); err != nil {
		t.Errorf("good file not written: %v", err)
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Errorf("output missing skip notice: %q", out.String())
	}
}

func TestBuildTelemetryVars_SettingsMatchesEnvAndExcludesSecretsAndCollectorVars(t *testing.T) {
	settingsVars, telemetryVars := buildTelemetryVars("http://example:4317", "a@b.co", "s3cret")

	dir := t.TempDir()
	var out bytes.Buffer
	if _, skipped := writeClaudeSettings(&out, []string{dir}, settingsVars); len(skipped) != 0 {
		t.Fatalf("skipped: %v", skipped)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Env) != len(settingsVars) || len(settingsVars) != 7 {
		t.Fatalf("env = %v, want exactly the 7 settings vars", doc.Env)
	}
	for _, v := range settingsVars {
		if doc.Env[v.Name] != v.Value {
			t.Errorf("env[%s] = %q, want %q", v.Name, doc.Env[v.Name], v.Value)
		}
	}
	for _, banned := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "CLAUDE_DIR", "CLAUDE_OBSERVABILITY_EXTRA_DIRS", "EXPORTER_STREAM"} {
		if _, ok := doc.Env[banned]; ok {
			t.Errorf("%s must not be written to settings.json", banned)
		}
	}
	if strings.Contains(string(data), "s3cret") {
		t.Error("token leaked into settings.json")
	}

	last := telemetryVars[len(telemetryVars)-1]
	if len(telemetryVars) != 8 || last.Name != "OTEL_EXPORTER_OTLP_HEADERS" || last.Value != basicAuthHeaderValue("a@b.co", "s3cret") {
		t.Errorf("rc vars lost the headers entry: %v", telemetryVars)
	}
}

func TestBuildTelemetryVars_NoTokenNoHeaders(t *testing.T) {
	_, telemetryVars := buildTelemetryVars("http://x", "", "")
	if len(telemetryVars) != 7 {
		t.Errorf("got %d vars, want 7", len(telemetryVars))
	}
}

func TestStaleEndpointWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rc files are not used on windows")
	}
	t.Setenv("SHELL", "/bin/zsh")
	home := t.TempDir()

	if w := staleEndpointWarning(home, "http://new"); w != "" {
		t.Errorf("no rc yet, got warning %q", w)
	}

	rc, shell := shellRCPath(home)
	if _, err := envwriter.WriteBlock(rc, shell, []envwriter.Var{{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://old"}}); err != nil {
		t.Fatal(err)
	}
	if w := staleEndpointWarning(home, "http://old"); w != "" {
		t.Errorf("same endpoint, got warning %q", w)
	}
	w := staleEndpointWarning(home, "http://new")
	if !strings.Contains(w, "http://old") || !strings.Contains(w, "http://new") {
		t.Errorf("warning = %q", w)
	}
	if strings.Contains(w, "settings.json") {
		t.Errorf("warning must not make claims about settings.json: %q", w)
	}
}

func TestWriteClaudeSettings_ReportsOverwrittenValuesAndRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	existing := `{"env": {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://old", "MY_API_TOKEN": "hunter2"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := []envwriter.Var{
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://new"},
		{Name: "MY_API_TOKEN", Value: "newsecret"},
	}
	var out bytes.Buffer
	updated, _ := writeClaudeSettings(&out, []string{dir}, vars)
	if len(updated) != 1 {
		t.Fatalf("updated = %v", updated)
	}
	got := out.String()
	if !strings.Contains(got, "OTEL_EXPORTER_OTLP_ENDPOINT: http://old -> http://new") {
		t.Errorf("missing overwrite line:\n%s", got)
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "newsecret") {
		t.Errorf("secret value printed:\n%s", got)
	}
	if !strings.Contains(got, "MY_API_TOKEN: <redacted> -> <redacted>") {
		t.Errorf("secret key not reported as redacted:\n%s", got)
	}
}

func TestWriteClaudeSettings_UnchangedIsNotReportedUpdated(t *testing.T) {
	dir := t.TempDir()
	settingsVars, _ := buildTelemetryVars("http://x", "", "")
	var out bytes.Buffer
	writeClaudeSettings(&out, []string{dir}, settingsVars)
	out.Reset()
	updated, skipped := writeClaudeSettings(&out, []string{dir}, settingsVars)
	if len(updated) != 0 || len(skipped) != 0 || !strings.Contains(out.String(), "already up to date") {
		t.Errorf("updated=%v skipped=%v out=%q", updated, skipped, out.String())
	}
}

func TestPercentEncodeHeaderValue(t *testing.T) {
	cases := map[string]string{
		"Basic dGVzdA==": "Basic%20dGVzdA%3D%3D",
		"a,b":             "a%2Cb",
		"a=b":             "a%3Db",
		"simple":          "simple",
	}
	for in, want := range cases {
		if got := percentEncodeHeaderValue(in); got != want {
			t.Errorf("percentEncodeHeaderValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBasicAuthHeaderValue_RoundTrips(t *testing.T) {
	got := basicAuthHeaderValue("alice@example.com", "tok 123,=")
	const prefix = "Authorization="
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("got %q, want prefix %q", got, prefix)
	}
	decoded, err := url.QueryUnescape(strings.TrimPrefix(got, prefix))
	if err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice@example.com:tok 123,="))
	if decoded != want {
		t.Errorf("decoded = %q, want %q", decoded, want)
	}
}

func TestOtelHeaderVars_EmptyWhenEitherMissing(t *testing.T) {
	if got := otelHeaderVars("", "tok"); got != nil {
		t.Errorf("got %v, want nil", got)
	}
	if got := otelHeaderVars("alice@example.com", ""); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestOtelHeaderVars_BuildsBasicAuthWhenBothSet(t *testing.T) {
	got := otelHeaderVars("alice@example.com", "tok123")
	if len(got) != 1 || got[0].Name != "OTEL_EXPORTER_OTLP_HEADERS" {
		t.Fatalf("got %v, want one OTEL_EXPORTER_OTLP_HEADERS var", got)
	}
	want := basicAuthHeaderValue("alice@example.com", "tok123")
	if got[0].Value != want {
		t.Errorf("Value = %q, want %q", got[0].Value, want)
	}
}

func TestLokiIngestURL_UnchangedWhenNoCredentials(t *testing.T) {
	got, err := lokiIngestURL("http://localhost:47100", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://localhost:47100" {
		t.Errorf("got %q, want endpoint unchanged", got)
	}
}

func TestLokiIngestURL_EmbedsCredentials(t *testing.T) {
	got, err := lokiIngestURL("https://loki-ingest.example.com", "alice@example.com", "tok:123")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.User.Username() != "alice@example.com" {
		t.Errorf("username = %q, want %q", parsed.User.Username(), "alice@example.com")
	}
	password, ok := parsed.User.Password()
	if !ok || password != "tok:123" {
		t.Errorf("password = %q (set=%v), want %q", password, ok, "tok:123")
	}
	if parsed.Scheme != "https" || parsed.Host != "loki-ingest.example.com" {
		t.Errorf("got %q, want scheme/host preserved", got)
	}
}
