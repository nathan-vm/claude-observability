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
