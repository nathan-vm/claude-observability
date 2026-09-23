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
