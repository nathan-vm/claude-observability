package main

import (
	"path/filepath"
	"regexp"
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
