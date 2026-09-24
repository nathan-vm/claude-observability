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
	// ExtraPathDirs, if set, is prepended to the service's PATH (darwin and
	// linux only) ahead of the standard OS bin dirs — for a program the
	// service needs on PATH that doesn't live in one of those (e.g. `claude`
	// installed via Homebrew on Apple Silicon, or nvm).
	ExtraPathDirs string
}
