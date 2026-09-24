//go:build windows

package service

import (
	"strings"
	"testing"

	"claude-observability-wizard/internal/envwriter"
)

func TestGenerateTaskArgs_ContainsTaskNameAndCommand(t *testing.T) {
	cfg := Config{
		Label: "DashGenerator", Command: `C:\bin\dash-generator.exe`, WorkingDir: `C:\repo`, LogDir: `C:\repo\.state`,
		Env: []envwriter.Var{{Name: "CLAUDE_DIR", Value: `C:\Users\me\.claude`}},
	}
	args := GenerateTaskArgs(cfg)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"DashGenerator",
		"onlogon",
		`set CLAUDE_DIR=C:\Users\me\.claude`,
		`C:\bin\dash-generator.exe`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q, got: %s", want, joined)
		}
	}
}
