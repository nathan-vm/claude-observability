//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func unitPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := label
	if !strings.HasSuffix(name, ".service") {
		name += ".service"
	}
	return filepath.Join(home, ".config", "systemd", "user", name), nil
}

// GenerateUnit renders the systemd --user unit file for cfg.
func GenerateUnit(cfg Config) string {
	var envLines strings.Builder
	for _, v := range cfg.Env {
		fmt.Fprintf(&envLines, "Environment=%s=%s\n", v.Name, v.Value)
	}
	pathEnv := filepath.Dir(cfg.Command)
	if cfg.ExtraPathDirs != "" {
		pathEnv += ":" + cfg.ExtraPathDirs
	}
	pathEnv += ":/usr/local/bin:/usr/bin:/bin"
	execLine := cfg.Command
	for _, a := range cfg.Args {
		execLine += " " + a
	}
	base := strings.TrimSuffix(filepath.Base(cfg.Command), filepath.Ext(cfg.Command))

	return fmt.Sprintf(`[Unit]
Description=%s

[Service]
Type=simple
WorkingDirectory=%s
Environment=PATH=%s
%sExecStart=%s
Restart=always
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, cfg.Label, cfg.WorkingDir, pathEnv, envLines.String(), execLine,
		filepath.Join(cfg.LogDir, base+".log"), filepath.Join(cfg.LogDir, base+".err.log"))
}

// Install writes the unit file and enables + starts it via systemctl --user.
func Install(cfg Config) error {
	path, err := unitPath(cfg.Label)
	if err != nil {
		return err
	}
	unitName := filepath.Base(path)
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
