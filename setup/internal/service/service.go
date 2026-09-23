// Package service installs the collector (collector/collector.mjs) as a
// background process: a launchd LaunchAgent on macOS, a systemd --user unit
// on Linux, and a logon-triggered Scheduled Task on Windows.
package service

import (
	"path/filepath"

	"claude-observability-setup/internal/envwriter"
)

// Config holds everything an OS-specific installer needs to register the
// collector as a background service.
type Config struct {
	RepoRoot  string          // absolute path to the claude-observability checkout
	NodeBin   string          // absolute path to `node`, from exec.LookPath("node")
	ClaudeBin string          // absolute path to `claude`, from exec.LookPath("claude")
	Env       []envwriter.Var // collector env vars to bake in (CLAUDE_DIR, EXPORTER_STREAM, ...)
	LogDir    string          // absolute path for stdout/stderr logs
}

// CollectorScript returns the absolute path to collector/collector.mjs
// under cfg.RepoRoot.
func (c Config) CollectorScript() string {
	return filepath.Join(c.RepoRoot, "collector", "collector.mjs")
}
