//go:build windows

package service

import (
	"fmt"
	"os"
	"os/exec"
)

const taskName = "ClaudeObservabilityCollector"

// GenerateTaskArgs returns the `schtasks /create` argument list that
// registers the collector to run at logon. Env vars are passed as part of
// the command line (`set NAME=VALUE&& ...`) since schtasks has no native
// per-task environment block. Paths are wrapped in plain double quotes, not
// Go's %q (which would escape the backslashes in a Windows path and break
// cmd.exe's own quoting).
func GenerateTaskArgs(cfg Config) []string {
	cmd := ""
	for _, v := range cfg.Env {
		cmd += fmt.Sprintf("set %s=%s&& ", v.Name, v.Value)
	}
	cmd += fmt.Sprintf(`"%s" "%s"`, cfg.NodeBin, cfg.CollectorScript())

	return []string{
		"/create", "/f",
		"/tn", taskName,
		"/sc", "onlogon",
		"/tr", "cmd /c " + cmd,
	}
}

// Install registers the Scheduled Task via schtasks.
func Install(cfg Config) error {
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return err
	}
	out, err := exec.Command("schtasks", GenerateTaskArgs(cfg)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /create: %w: %s", err, out)
	}
	return nil
}
