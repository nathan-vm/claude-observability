//go:build linux

package service

import (
	"strings"
	"testing"

	"claude-observability-setup/internal/envwriter"
)

func TestGenerateUnit_ContainsExecAndEnv(t *testing.T) {
	cfg := Config{
		RepoRoot: "/repo", NodeBin: "/usr/bin/node", ClaudeBin: "/usr/bin/claude", LogDir: "/repo/.state",
		Env: []envwriter.Var{{Name: "CLAUDE_DIR", Value: "/home/.claude"}},
	}
	out := GenerateUnit(cfg)

	for _, want := range []string{
		"ExecStart=/usr/bin/node /repo/collector/collector.mjs",
		"Environment=CLAUDE_DIR=/home/.claude",
		"WantedBy=default.target",
		"StandardOutput=append:/repo/.state/collector.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit missing %q, got:\n%s", want, out)
		}
	}
}
