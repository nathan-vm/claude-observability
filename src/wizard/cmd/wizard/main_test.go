package main

import (
	"path/filepath"
	"regexp"
	"runtime"
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
