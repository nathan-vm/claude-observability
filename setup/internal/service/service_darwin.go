//go:build darwin

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const label = "com.agents-observability.collector"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// GeneratePlist renders the launchd LaunchAgent plist for cfg.
func GeneratePlist(cfg Config) string {
	var envXML strings.Builder
	for _, v := range cfg.Env {
		fmt.Fprintf(&envXML, "    <key>%s</key><string>%s</string>\n", v.Name, xmlEscape(v.Value))
	}
	pathEnv := filepath.Dir(cfg.NodeBin) + ":" + filepath.Dir(cfg.ClaudeBin) + ":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>%s</string>
  </array>
  <key>WorkingDirectory</key><string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>%s</string>
%s  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
`, label, cfg.NodeBin, cfg.CollectorScript(), filepath.Join(cfg.RepoRoot, "collector"),
		pathEnv, envXML.String(),
		filepath.Join(cfg.LogDir, "collector.log"), filepath.Join(cfg.LogDir, "collector.err.log"))
}

// Install writes the plist and (re)loads it via launchctl.
func Install(cfg Config) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(GeneratePlist(cfg)), 0o644); err != nil {
		return err
	}

	uid := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", uid, path).Run() // ignore error: fine if not loaded yet
	if out, err := exec.Command("launchctl", "bootstrap", uid, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
	}
	if out, err := exec.Command("launchctl", "enable", uid+"/"+label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl enable: %w: %s", err, out)
	}
	return nil
}
