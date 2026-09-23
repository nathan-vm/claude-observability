//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const unitName = "claude-observability-collector.service"

func unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", unitName), nil
}

// GenerateUnit renders the systemd --user unit file for cfg.
func GenerateUnit(cfg Config) string {
	var envLines strings.Builder
	for _, v := range cfg.Env {
		fmt.Fprintf(&envLines, "Environment=%s=%s\n", v.Name, v.Value)
	}
	pathEnv := filepath.Dir(cfg.NodeBin) + ":" + filepath.Dir(cfg.ClaudeBin) + ":/usr/local/bin:/usr/bin:/bin"

	return fmt.Sprintf(`[Unit]
Description=claude-observability collector

[Service]
Type=simple
WorkingDirectory=%s
Environment=PATH=%s
%sExecStart=%s %s
Restart=always
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, filepath.Join(cfg.RepoRoot, "collector"), pathEnv, envLines.String(), cfg.NodeBin, cfg.CollectorScript(),
		filepath.Join(cfg.LogDir, "collector.log"), filepath.Join(cfg.LogDir, "collector.err.log"))
}

// Install writes the unit file and enables + starts it via systemctl --user.
func Install(cfg Config) error {
	path, err := unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(GenerateUnit(cfg)), 0o644); err != nil {
		return err
	}

	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, out)
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable: %w: %s", err, out)
	}
	return nil
}
