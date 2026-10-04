package claudesettings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"claude-observability-wizard/internal/envwriter"
)

var testVars = []envwriter.Var{
	{Name: "CLAUDE_CODE_ENABLE_TELEMETRY", Value: "1"},
	{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://localhost:47317"},
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func envOf(t *testing.T, path string) map[string]string {
	t.Helper()
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Env
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, ".settings.json.*"))
	if len(matches) != 0 {
		t.Errorf("leftover temp files: %v", matches)
	}
}

func TestMergeEnv_CreatesFileAndDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", ".claude", "settings.json")
	changed, _, err := MergeEnv(path, testVars)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	env := envOf(t, path)
	if len(env) != 2 || env["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Errorf("env = %v", env)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode().Perm())
		}
	}
	if !strings.HasSuffix(readFile(t, path), "}\n") {
		t.Error("missing trailing newline")
	}
	assertNoTemp(t, filepath.Dir(path))
}

func TestMergeEnv_PreservesUnrelatedContentAndOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	orig := `{
  "zeta": {"nested": [1, 2.50, {"a": null}]},
  "alpha": 1e3,
  "env": {"KEEP": "yes", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://old"},
  "last": true
}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, overwritten, err := MergeEnv(path, testVars)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	wantOverwrite := Overwrite{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Old: "http://old", New: "http://localhost:47317"}
	if len(overwritten) != 1 || overwritten[0] != wantOverwrite {
		t.Errorf("overwritten = %+v, want [%+v]", overwritten, wantOverwrite)
	}
	got := readFile(t, path)
	for _, want := range []string{"2.50", "1e3", `"a": null`} {
		if !strings.Contains(got, want) {
			t.Errorf("output lost %q:\n%s", want, got)
		}
	}
	order := []string{`"zeta"`, `"alpha"`, `"env"`, `"last"`}
	prev := -1
	for _, k := range order {
		i := strings.Index(got, k)
		if i <= prev {
			t.Errorf("key %s out of order:\n%s", k, got)
		}
		prev = i
	}
	env := envOf(t, path)
	if env["KEEP"] != "yes" {
		t.Errorf("unrelated env entry lost: %v", env)
	}
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://localhost:47317" {
		t.Errorf("endpoint not overwritten: %v", env)
	}
	if env["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Errorf("new key missing: %v", env)
	}
	if strings.Index(got, "KEEP") > strings.Index(got, "OTEL_EXPORTER_OTLP_ENDPOINT") {
		t.Errorf("overwritten key should keep its position:\n%s", got)
	}
}

func TestMergeEnv_IdempotentLeavesFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, _, err := MergeEnv(path, testVars); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	changed, _, err := MergeEnv(path, testVars)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	info, _ := os.Stat(path)
	if info.ModTime().After(old.Add(time.Minute)) {
		t.Errorf("mtime bumped: %v", info.ModTime())
	}
	if readFile(t, path) != before {
		t.Error("content changed")
	}
}

func TestMergeEnv_PreservesExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := MergeEnv(path, testVars); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
}

func TestMergeEnv_InvalidInputLeavesFileUntouched(t *testing.T) {
	cases := map[string]string{
		"invalid json":     `{"a": `,
		"trailing garbage": `{} x`,
		"top-level array":  `[1]`,
		"top-level null":   `null`,
		"env string":       `{"env": "x"}`,
		"env array":        `{"env": []}`,
		"env null":         `{"env": null}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "settings.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			changed, _, err := MergeEnv(path, testVars)
			if err == nil || changed {
				t.Fatalf("changed=%v err=%v, want error", changed, err)
			}
			if readFile(t, path) != content {
				t.Error("file modified")
			}
			assertNoTemp(t, dir)
		})
	}
}

func TestMergeEnv_EmptyAndWhitespaceTreatedAsObject(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "whitespace": " \n\t\n"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			changed, _, err := MergeEnv(path, testVars)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if envOf(t, path)["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
				t.Error("env not written")
			}
		})
	}
}

func TestMergeEnv_WritesThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(`{"keep": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := MergeEnv(link, testVars); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v %v", info, err)
	}
	if envOf(t, real)["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Error("target not updated")
	}
	assertNoTemp(t, filepath.Dir(real))
	assertNoTemp(t, dir)
}

func TestMergeEnv_OnlyPassedVarsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, _, err := MergeEnv(path, testVars); err != nil {
		t.Fatal(err)
	}
	if _, ok := envOf(t, path)["OTEL_EXPORTER_OTLP_HEADERS"]; ok {
		t.Error("headers present although not passed")
	}
	with := append(append([]envwriter.Var{}, testVars...), envwriter.Var{Name: "OTEL_EXPORTER_OTLP_HEADERS", Value: "a=b"})
	if _, _, err := MergeEnv(path, with); err != nil {
		t.Fatal(err)
	}
	if envOf(t, path)["OTEL_EXPORTER_OTLP_HEADERS"] != "a=b" {
		t.Error("headers missing when passed")
	}
}

func TestMergeEnv_DoesNotEscapeHTML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, _, err := MergeEnv(path, []envwriter.Var{{Name: "X", Value: "http://h/?a=1&b=2"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), "a=1&b=2") {
		t.Errorf("value was escaped:\n%s", readFile(t, path))
	}
}

func TestMergeEnv_NewKeysAreNotReportedAsOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	_, overwritten, err := MergeEnv(path, testVars)
	if err != nil || len(overwritten) != 0 {
		t.Errorf("overwritten=%+v err=%v", overwritten, err)
	}
	_, overwritten, err = MergeEnv(path, testVars)
	if err != nil || len(overwritten) != 0 {
		t.Errorf("identical rerun: overwritten=%+v err=%v", overwritten, err)
	}
}
