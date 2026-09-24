// Package service installs an executable as a background process: a
// launchd LaunchAgent on macOS, a systemd --user unit on Linux, and a
// logon-triggered Scheduled Task on Windows.
package service

import "claude-observability-wizard/internal/envwriter"

// Config holds everything an OS-specific installer needs to register a
// program as a background service. Generic over what it runs — the same
// package installs the wizard-built dash-generator binary today and the
// future collector binary later, with no per-program logic in here.
type Config struct {
	Label      string // service identifier: reverse-DNS on macOS, unit/task name elsewhere
	Command    string // absolute path to the executable to run
	Args       []string
	WorkingDir string
	Env        []envwriter.Var
	LogDir     string // absolute path for stdout/stderr logs
}
