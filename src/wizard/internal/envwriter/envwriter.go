// Package envwriter writes the telemetry + collector configuration into a
// shell's startup file (bash/zsh/fish) or, on Windows, into persistent user
// environment variables.
package envwriter

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Shell selects the rc-file syntax Render produces.
type Shell int

const (
	Bash Shell = iota
	Fish
)

// Var is one NAME=VALUE pair. Order is preserved in Render's output.
type Var struct {
	Name  string
	Value string
}

// Marker is written as the first line of the block and used to detect an
// already-configured file on a re-run.
const Marker = "# claude-observability: telemetry + collector config"

// Render formats vars as an rc-file block, prefixed by Marker.
func Render(shell Shell, vars []Var) string {
	var b strings.Builder
	b.WriteString(Marker)
	b.WriteString("\n")
	for _, v := range vars {
		switch shell {
		case Fish:
			fmt.Fprintf(&b, "set -gx %s %s\n", v.Name, v.Value)
		default:
			fmt.Fprintf(&b, "export %s=%q\n", v.Name, v.Value)
		}
	}
	return b.String()
}

// WriteBlock appends Render(shell, vars) to path unless Marker is already
// present in it. Creates the file (and its parent directory) if missing.
// wrote is false when the file already had the marker (no-op).
func WriteBlock(path string, shell Shell, vars []Var) (wrote bool, err error) {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if strings.Contains(string(existing), Marker) {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()

	block := Render(shell, vars)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		block = "\n" + block
	}
	if _, err := f.WriteString(block); err != nil {
		return false, err
	}
	return true, nil
}

// WriteWindows sets each var as a persistent user environment variable by
// calling run(name, value) for each — real callers pass a function that
// shells out to `setx`; tests pass a fake. setx overwrites rather than
// appending, so this is naturally idempotent and needs no marker.
func WriteWindows(vars []Var, run func(name, value string) error) error {
	for _, v := range vars {
		if err := run(v.Name, v.Value); err != nil {
			return fmt.Errorf("setting %s: %w", v.Name, err)
		}
	}
	return nil
}

// ExistingVar reports the value already assigned to name inside path's
// Marker block, if any. ok is false — with err nil — when: path doesn't
// exist yet (nothing configured); path exists but has no Marker block yet
// (nothing configured); or the Marker block exists but never assigned name
// (e.g. an accounts-less first run wrote the telemetry vars but had no
// collector vars to write yet). Any other read error is returned as err.
func ExistingVar(path string, shell Shell, name string) (value string, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if !strings.Contains(string(data), Marker) {
		return "", false, nil
	}

	var pattern *regexp.Regexp
	switch shell {
	case Fish:
		pattern = regexp.MustCompile(`(?m)^set -gx ` + regexp.QuoteMeta(name) + ` (.+)$`)
	default:
		pattern = regexp.MustCompile(`(?m)^export ` + regexp.QuoteMeta(name) + `="(.*)"$`)
	}
	m := pattern.FindStringSubmatch(string(data))
	if m == nil {
		return "", false, nil
	}
	return m[1], true, nil
}
