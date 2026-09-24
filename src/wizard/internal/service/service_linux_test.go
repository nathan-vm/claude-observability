//go:build linux

package service

import (
	"strings"
	"testing"

	"claude-observability-wizard/internal/envwriter"
)

func TestGenerateUnit_ContainsExecAndEnv(t *testing.T) {
	cfg := Config{
		Label: "dash-generator", Command: "/usr/local/bin/dash-generator", Args: []string{"--once"},
		WorkingDir: "/repo", LogDir: "/repo/.state",
		Env: []envwriter.Var{{Name: "CLAUDE_DIR", Value: "/home/.claude"}},
	}
	out := GenerateUnit(cfg)

	for _, want := range []string{
		"ExecStart=/usr/local/bin/dash-generator --once",
		"Environment=CLAUDE_DIR=/home/.claude",
		"WantedBy=default.target",
		"StandardOutput=append:/repo/.state/dash-generator.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit missing %q, got:\n%s", want, out)
		}
	}
}
