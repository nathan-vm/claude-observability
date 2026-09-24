//go:build darwin

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func plistPath(label string) (string, error) {
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
	var argsXML strings.Builder
	fmt.Fprintf(&argsXML, "    <string>%s</string>\n", cfg.Command)
	for _, a := range cfg.Args {
		fmt.Fprintf(&argsXML, "    <string>%s</string>\n", xmlEscape(a))
	}
	pathEnv := filepath.Dir(cfg.Command)
	if cfg.ExtraPathDirs != "" {
		pathEnv += ":" + cfg.ExtraPathDirs
	}
	pathEnv += ":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
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
`, cfg.Label, argsXML.String(), cfg.WorkingDir, pathEnv, envXML.String(),
		filepath.Join(cfg.LogDir, baseName(cfg.Command)+".log"), filepath.Join(cfg.LogDir, baseName(cfg.Command)+".err.log"))
}

func baseName(command string) string {
	return strings.TrimSuffix(filepath.Base(command), filepath.Ext(command))
}

// Install writes the plist and (re)loads it via launchctl.
func Install(cfg Config) error {
	path, err := plistPath(cfg.Label)
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
	exec.Command("launchctl", "bootout", uid, path).Run()
	if out, err := exec.Command("launchctl", "bootstrap", uid, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
	}
	if out, err := exec.Command("launchctl", "enable", uid+"/"+cfg.Label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl enable: %w: %s", err, out)
	}
	return nil
}
