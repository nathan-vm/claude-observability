//go:build darwin

package service

import (
	"strings"
	"testing"

	"claude-observability-setup/internal/envwriter"
)

func TestGeneratePlist_ContainsLabelAndEnv(t *testing.T) {
	cfg := Config{
		Label:      "com.agents-observability.dash-generator",
		Command:    "/usr/local/bin/dash-generator",
		WorkingDir: "/repo",
		Env:        []envwriter.Var{{Name: "CLAUDE_DIR", Value: "/home/.claude"}},
		LogDir:     "/repo/.state",
	}
	out := GeneratePlist(cfg)

	for _, want := range []string{
		"com.agents-observability.dash-generator",
		"/usr/local/bin/dash-generator",
		"<key>CLAUDE_DIR</key><string>/home/.claude</string>",
		"/repo/.state/dash-generator.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestGeneratePlist_EscapesXML(t *testing.T) {
	cfg := Config{
		Label: "com.agents-observability.x", Command: "/bin/x", WorkingDir: "/repo", LogDir: "/repo/.state",
		Env: []envwriter.Var{{Name: "FOO", Value: "a & b < c"}},
	}
	out := GeneratePlist(cfg)
	if !strings.Contains(out, "a &amp; b &lt; c") {
		t.Errorf("plist did not escape XML special chars: %s", out)
	}
}

func TestGeneratePlist_IncludesArgs(t *testing.T) {
	cfg := Config{
		Label: "com.agents-observability.x", Command: "/bin/x", Args: []string{"--once"}, WorkingDir: "/repo", LogDir: "/repo/.state",
	}
	out := GeneratePlist(cfg)
	if !strings.Contains(out, "<string>--once</string>") {
		t.Errorf("plist missing arg: %s", out)
	}
}
