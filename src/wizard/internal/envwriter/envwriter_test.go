package envwriter

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	newMarker    = "# nathan-vm/claude-observability: telemetry + collector config"
	legacyMarker = "# claude-observability: telemetry + collector config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustWrite(t *testing.T, path string, shell Shell, vars []Var) WriteResult {
	t.Helper()
	res, err := WriteBlock(path, shell, vars)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

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

func TestMarkers(t *testing.T) {
	if Marker != newMarker {
		t.Errorf("Marker = %q, want %q", Marker, newMarker)
	}
	if LegacyMarker != legacyMarker {
		t.Errorf("LegacyMarker = %q, want %q", LegacyMarker, legacyMarker)
	}
	if strings.Contains(Marker, LegacyMarker) || strings.Contains(LegacyMarker, Marker) {
		t.Error("neither marker may be a substring of the other")
	}
	for _, shell := range []Shell{Bash, Fish} {
		if !strings.HasPrefix(Render(shell, nil), newMarker+"\n") {
			t.Errorf("Render(%v) does not start with the new marker", shell)
		}
	}
}

func TestWriteBlock_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "rc")

	res := mustWrite(t, path, Bash, []Var{{"CLAUDE_DIR", "bar"}})
	if !res.Changed || res.Replaced || res.MigratedLegacy {
		t.Errorf("res = %+v, want only Changed", res)
	}
	if got := readFile(t, path); got != Render(Bash, []Var{{"CLAUDE_DIR", "bar"}}) {
		t.Errorf("file = %q", got)
	}
}

func TestWriteBlock_SameVarsIsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc")
	writeFile(t, path, "before\n")
	mustWrite(t, path, Bash, []Var{{"CLAUDE_DIR", "bar"}})
	first := readFile(t, path)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	infoBefore, _ := os.Stat(path)

	res := mustWrite(t, path, Bash, []Var{{"CLAUDE_DIR", "bar"}})
	if res.Changed || res.Replaced || res.MigratedLegacy {
		t.Errorf("res = %+v, want zero value", res)
	}
	if readFile(t, path) != first {
		t.Error("file content changed on identical re-run")
	}
	infoAfter, _ := os.Stat(path)
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Error("mtime changed on identical re-run")
	}
}

func TestWriteBlock_DifferentVarsMovesBlockToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc")
	writeFile(t, path, "top\n"+Render(Bash, []Var{{"CLAUDE_DIR", "old"}})+"\n# middle\nexport KEEP=\"1\"\n")

	res := mustWrite(t, path, Bash, []Var{{"CLAUDE_DIR", "new"}})
	if !res.Changed || !res.Replaced {
		t.Errorf("res = %+v, want Changed and Replaced", res)
	}
	want := "top\n\n# middle\nexport KEEP=\"1\"\n" + Render(Bash, []Var{{"CLAUDE_DIR", "new"}})
	if got := readFile(t, path); got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestWriteBlock_NoTrailingNewlineAndCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc")
	writeFile(t, path, "alias a=b")
	mustWrite(t, path, Bash, []Var{{"CLAUDE_DIR", "bar"}})
	if got, want := readFile(t, path), "alias a=b\n"+Render(Bash, []Var{{"CLAUDE_DIR", "bar"}}); got != want {
		t.Errorf("no-newline file = %q, want %q", got, want)
	}

	crlf := "one\r\n" + newMarker + "\r\nexport CLAUDE_DIR=\"old\"\r\ntwo\r\n"
	writeFile(t, path, crlf)
	mustWrite(t, path, Bash, []Var{{"CLAUDE_DIR", "new"}})
	want := "one\r\ntwo\r\n" + Render(Bash, []Var{{"CLAUDE_DIR", "new"}})
	if got := readFile(t, path); got != want {
		t.Errorf("crlf file = %q, want %q", got, want)
	}
}

func TestWriteBlock_MigratesLegacyBlock(t *testing.T) {
	cases := []struct {
		name   string
		shell  Shell
		legacy string
	}{
		{"bash", Bash, legacyMarker + "\nexport EXPORTER_STREAM=\"s1\"\n"},
		{"fish", Fish, legacyMarker + "\nset -gx EXPORTER_STREAM s1\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rc")
			writeFile(t, path, "head\n"+c.legacy+"\ntail\n")
			vars := []Var{{"EXPORTER_STREAM", "s1"}}

			res := mustWrite(t, path, c.shell, vars)
			if !res.Changed || !res.MigratedLegacy || res.Replaced {
				t.Errorf("res = %+v", res)
			}
			want := "head\n\ntail\n" + Render(c.shell, vars)
			if got := readFile(t, path); got != want {
				t.Errorf("file = %q, want %q", got, want)
			}

			res = mustWrite(t, path, c.shell, vars)
			if res.MigratedLegacy || res.Changed {
				t.Errorf("second call res = %+v, want zero value", res)
			}
		})
	}
}

func TestWriteBlock_CoexistsWithForkLegacyBlock(t *testing.T) {
	fork := legacyMarker + "\nexport EXPORTER_STREAM=\"zallpy\"\nexport OTEL_EXPORTER_OTLP_ENDPOINT=\"http://zallpy\"\n"
	ours := Render(Bash, []Var{{"EXPORTER_STREAM", "mine"}})
	updated := Render(Bash, []Var{{"EXPORTER_STREAM", "mine2"}})

	t.Run("fork before ours", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rc")
		writeFile(t, path, fork+"\n"+ours)
		res := mustWrite(t, path, Bash, []Var{{"EXPORTER_STREAM", "mine2"}})
		if res.MigratedLegacy {
			t.Error("migrated the fork's legacy block")
		}
		if got, want := readFile(t, path), fork+"\n"+updated; got != want {
			t.Errorf("file = %q, want %q", got, want)
		}
	})

	t.Run("fork after ours", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rc")
		writeFile(t, path, ours+"\n"+fork)
		res := mustWrite(t, path, Bash, []Var{{"EXPORTER_STREAM", "mine2"}})
		if res.MigratedLegacy {
			t.Error("migrated the fork's legacy block")
		}
		if got, want := readFile(t, path), "\n"+fork+updated; got != want {
			t.Errorf("file = %q, want %q", got, want)
		}
	})
}

func TestRemoveBlock(t *testing.T) {
	fork := legacyMarker + "\nexport EXPORTER_STREAM=\"zallpy\"\n"
	ours := Render(Bash, []Var{{"CLAUDE_DIR", "bar"}})

	t.Run("removes only ours", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rc")
		writeFile(t, path, "a\n"+fork+"\nb\n"+ours+"c\n")
		removed, err := RemoveBlock(path)
		if err != nil || !removed {
			t.Fatalf("removed=%v err=%v", removed, err)
		}
		if got, want := readFile(t, path), "a\n"+fork+"\nb\nc\n"; got != want {
			t.Errorf("file = %q, want %q", got, want)
		}
	})

	t.Run("legacy only is untouched", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rc")
		content := "a\n" + fork + "b\n"
		writeFile(t, path, content)
		removed, err := RemoveBlock(path)
		if err != nil || removed {
			t.Fatalf("removed=%v err=%v", removed, err)
		}
		if readFile(t, path) != content {
			t.Error("file changed")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		removed, err := RemoveBlock(filepath.Join(t.TempDir(), "nope"))
		if err != nil || removed {
			t.Errorf("removed=%v err=%v, want false, nil", removed, err)
		}
	})
}

func TestPendingMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc")
	if pending, err := PendingMigration(path); err != nil || pending {
		t.Errorf("missing file: pending=%v err=%v", pending, err)
	}
	writeFile(t, path, legacyMarker+"\nexport CLAUDE_DIR=\"1\"\n")
	if pending, _ := PendingMigration(path); !pending {
		t.Error("legacy-only file should be pending")
	}
	writeFile(t, path, legacyMarker+"\nexport CLAUDE_DIR=\"1\"\n\n"+Render(Bash, nil))
	if pending, _ := PendingMigration(path); pending {
		t.Error("file with a new block should not be pending")
	}
}

func TestMarkerMatchIsWholeLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc")
	content := "# see \"" + newMarker + "\" for details\nexport CLAUDE_DIR=\"1\"\n" + newMarker + " extra\n"
	writeFile(t, path, content)
	if removed, _ := RemoveBlock(path); removed {
		t.Error("a line that merely contains the marker was treated as a block")
	}
	if _, ok, _ := ExistingVar(path, Bash, "CLAUDE_DIR"); ok {
		t.Error("ExistingVar read from a non-block")
	}
}

func TestBlockExtent(t *testing.T) {
	block := newMarker + "\nexport CLAUDE_DIR=\"1\"\nset -gx LOKI_URL 2\n"
	for _, tail := range []string{"\nafter\n", "# comment\n", "alias x=y\n", ""} {
		path := filepath.Join(t.TempDir(), "rc")
		writeFile(t, path, "before\n"+block+tail)
		if removed, err := RemoveBlock(path); err != nil || !removed {
			t.Fatalf("tail %q: removed=%v err=%v", tail, removed, err)
		}
		if got, want := readFile(t, path), "before\n"+tail; got != want {
			t.Errorf("tail %q: file = %q, want %q", tail, got, want)
		}
	}
}

func TestForeignLineAfterBlockIsPreserved(t *testing.T) {
	foreign := map[Shell]string{
		Bash: "export PATH=\"/x:$PATH\"\n",
		Fish: "set -gx PATH /x $PATH\n",
	}
	for _, shell := range []Shell{Bash, Fish} {
		old := Render(shell, []Var{{"CLAUDE_DIR", "old"}})
		tail := foreign[shell]
		newBlock := Render(shell, []Var{{"CLAUDE_DIR", "new"}})

		t.Run("rewrite", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rc")
			writeFile(t, path, "head\n"+old+tail+"more\n")
			mustWrite(t, path, shell, []Var{{"CLAUDE_DIR", "new"}})
			if got, want := readFile(t, path), "head\n"+tail+"more\n"+newBlock; got != want {
				t.Errorf("shell %v: file = %q, want %q", shell, got, want)
			}
		})

		t.Run("remove", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rc")
			writeFile(t, path, "head\n"+old+tail)
			removed, err := RemoveBlock(path)
			if err != nil || !removed {
				t.Fatalf("removed=%v err=%v", removed, err)
			}
			if got, want := readFile(t, path), "head\n"+tail; got != want {
				t.Errorf("shell %v: file = %q, want %q", shell, got, want)
			}
		})

		t.Run("legacy migration", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rc")
			legacy := strings.Replace(old, Marker, LegacyMarker, 1)
			writeFile(t, path, "head\n"+legacy+tail)
			res := mustWrite(t, path, shell, []Var{{"CLAUDE_DIR", "new"}})
			if !res.MigratedLegacy {
				t.Error("legacy block not migrated")
			}
			if got, want := readFile(t, path), "head\n"+tail+newBlock; got != want {
				t.Errorf("shell %v: file = %q, want %q", shell, got, want)
			}
		})
	}
}

func TestIsManagedName(t *testing.T) {
	for _, n := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_X", "CLAUDE_CODE_ENABLE_TELEMETRY", "CLAUDE_DIR", "CLAUDE_OBSERVABILITY_EXTRA_DIRS", "EXPORTER_STREAM", "LOKI_URL"} {
		if !IsManagedName(n) {
			t.Errorf("%s should be managed", n)
		}
	}
	for _, n := range []string{"PATH", "FOO", "CLAUDE_CONFIG_DIR", "MY_OTEL_X"} {
		if IsManagedName(n) {
			t.Errorf("%s should not be managed", n)
		}
	}
}

func TestWriteBlock_CollapsesDuplicateBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc")
	old := Render(Bash, []Var{{"CLAUDE_DIR", "old"}})
	writeFile(t, path, old+"\nmid\n"+old)
	mustWrite(t, path, Bash, []Var{{"CLAUDE_DIR", "new"}})
	if got, want := readFile(t, path), "\nmid\n"+Render(Bash, []Var{{"CLAUDE_DIR", "new"}}); got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestWriteBlock_FollowsSymlinkAndKeepsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks and unix permission bits")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "zshrc")
	link := filepath.Join(dir, "zshrc")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, target, "keep\n")
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skip("cannot create symlink:", err)
	}

	mustWrite(t, link, Bash, []Var{{"CLAUDE_DIR", "bar"}})
	mustWrite(t, link, Bash, []Var{{"CLAUDE_DIR", "baz"}})

	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced: %v %v", info, err)
	}
	if got, want := readFile(t, target), "keep\n"+Render(Bash, []Var{{"CLAUDE_DIR", "baz"}}); got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
	tinfo, _ := os.Stat(target)
	if got := tinfo.Mode().Perm(); got != 0o640 {
		t.Errorf("target mode = %o, want 640", got)
	}
}

func TestWriteBlock_PreservesExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rc")
	if err := os.WriteFile(path, []byte("existing line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := WriteBlock(path, Bash, []Var{{"CLAUDE_DIR", "bar"}}); err != nil {
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
	if err := WriteWindows([]Var{{"CLAUDE_DIR", "bar"}}, run); err == nil {
		t.Error("WriteWindows() = nil, want error")
	}
}

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

func TestExistingVar_BlockScoping(t *testing.T) {
	for _, shell := range []Shell{Bash, Fish} {
		assign := func(name, value string) string {
			if shell == Fish {
				return "set -gx " + name + " " + value + "\n"
			}
			return "export " + name + "=\"" + value + "\"\n"
		}
		fork := legacyMarker + "\n" + assign("EXPORTER_STREAM", "zallpy")
		ours := newMarker + "\n" + assign("EXPORTER_STREAM", "mine")
		oursNoStream := newMarker + "\n" + assign("LOKI_URL", "1")

		check := func(name, content, want string, wantOK bool) {
			t.Helper()
			path := filepath.Join(t.TempDir(), "rc")
			writeFile(t, path, content)
			got, ok, err := ExistingVar(path, shell, "EXPORTER_STREAM")
			if err != nil || ok != wantOK || got != want {
				t.Errorf("shell %v %s: got (%q, %v, %v), want (%q, %v)", shell, name, got, ok, err, want, wantOK)
			}
		}
		check("new block", ours, "mine", true)
		check("legacy only", fork, "zallpy", true)
		check("new wins over legacy", fork+"\n"+ours, "mine", true)
		check("no fallback when new block lacks var", fork+"\n"+oursNoStream, "", false)
		check("ignores var outside block", assign("EXPORTER_STREAM", "stray")+"\n"+oursNoStream, "", false)
		check("ignores fork var before ours", fork+"\n"+ours, "mine", true)
	}
}

func TestWriteBlock_NewFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "rc")
	if _, err := WriteBlock(path, Bash, []Var{{"CLAUDE_DIR", "bar"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("new rc file mode = %o, want 600", got)
	}
}

func TestWriteBlock_LeavesExistingFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "rc")
	if err := os.WriteFile(path, []byte("existing line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteBlock(path, Bash, []Var{{"CLAUDE_DIR", "bar"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("existing rc file mode = %o, want unchanged 644", got)
	}
}
