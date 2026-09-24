//go:build windows

package service

import (
	"fmt"
	"os"
	"os/exec"
)

// GenerateTaskArgs returns the `schtasks /create` argument list that
// registers cfg to run at logon.
func GenerateTaskArgs(cfg Config) []string {
	cmd := ""
	for _, v := range cfg.Env {
		cmd += fmt.Sprintf("set %s=%s&& ", v.Name, v.Value)
	}
	cmd += fmt.Sprintf(`"%s"`, cfg.Command)
	for _, a := range cfg.Args {
		cmd += fmt.Sprintf(` "%s"`, a)
	}

	return []string{
		"/create", "/f",
		"/tn", cfg.Label,
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
