//go:build windows

package service

import (
	"strings"
	"testing"

	"claude-observability-setup/internal/envwriter"
)

func TestGenerateTaskArgs_ContainsTaskNameAndCommand(t *testing.T) {
	cfg := Config{
		RepoRoot: `C:\repo`, NodeBin: `C:\node\node.exe`, ClaudeBin: `C:\node\claude.exe`, LogDir: `C:\repo\.state`,
		Env: []envwriter.Var{{Name: "CLAUDE_DIR", Value: `C:\Users\me\.claude`}},
	}
	args := GenerateTaskArgs(cfg)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"ClaudeObservabilityCollector",
		"onlogon",
		`set CLAUDE_DIR=C:\Users\me\.claude`,
		`C:\node\node.exe`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q, got: %s", want, joined)
		}
	}
}
