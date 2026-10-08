// Package envwriter writes the telemetry + collector configuration into a
// shell's startup file (bash/zsh/fish) or, on Windows, into persistent user
// environment variables.
package envwriter

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// Marker is written as the first line of the block and identifies it on a
// re-run. It is namespaced so a fork installed on the same machine, which
// keeps writing LegacyMarker, never collides with this install's block.
const Marker = "# nathan-vm/claude-observability: telemetry + collector config"

// LegacyMarker is the marker earlier releases wrote. It is only read, and
// consumed, during the one-time migration in WriteBlock; RemoveBlock never
// matches it.
const LegacyMarker = "# claude-observability: telemetry + collector config"

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

// WriteResult describes what WriteBlock did.
type WriteResult struct {
	Changed        bool // file content actually changed
	Replaced       bool // a previous block of this install was removed and rewritten
	MigratedLegacy bool // a LegacyMarker block was consumed this call
}

var managedAssignment = regexp.MustCompile(`^(?:export ([A-Za-z_][A-Za-z0-9_]*)=|set -gx ([A-Za-z_][A-Za-z0-9_]*) )`)

// managedNames are the variables the wizard writes into the block, besides
// every OTEL_* variable. A block only absorbs assignments of these, so a
// foreign line directly after it (export PATH=...) is never treated as part of
// it. cmd/wizard's tests assert every var it builds is covered, so this list
// can't silently drift from what Render is given.
var managedNames = []string{
	"CLAUDE_CODE_ENABLE_TELEMETRY",
	"CLAUDE_DIR",
	"CLAUDE_OBSERVABILITY_EXTRA_DIRS",
	"EXPORTER_STREAM",
	"LOKI_URL",
}

// IsManagedName reports whether name is a variable the wizard writes into the
// block.
func IsManagedName(name string) bool {
	if strings.HasPrefix(name, "OTEL_") {
		return true
	}
	for _, n := range managedNames {
		if n == name {
			return true
		}
	}
	return false
}

func isManagedLine(line string) bool {
	m := managedAssignment.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	return IsManagedName(m[1] + m[2])
}

// splitLines splits text into lines that keep their terminators, so joining
// them back reproduces text byte for byte (CRLF and a missing final newline
// included).
func splitLines(text string) []string {
	var lines []string
	for text != "" {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:i+1])
		text = text[i+1:]
	}
	return lines
}

func lineContent(line string) string {
	return strings.TrimRight(line, "\r\n")
}

type span struct{ start, end int }

// findBlocks returns the line ranges of every block opened by marker. A
// marker line matches only when the whole line equals it (trailing
// whitespace ignored), so a comment that merely quotes it is not a block. A
// block is the marker line plus the contiguous assignments of managed
// variables (export NAME=... or set -gx NAME ..., see IsManagedName) after it,
// in either shell flavour; it ends at the first blank line, comment, or other
// line, or at EOF. Known limit: a hand-added assignment of a managed variable
// directly adjacent to the block is treated as part of it.
func findBlocks(lines []string, marker string) []span {
	var spans []span
	for i := 0; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\r\n") != marker {
			continue
		}
		end := i + 1
		for end < len(lines) && isManagedLine(lineContent(lines[end])) {
			end++
		}
		spans = append(spans, span{i, end})
		i = end - 1
	}
	return spans
}

func removeSpans(lines []string, spans []span) []string {
	var out []string
	prev := 0
	for _, sp := range spans {
		out = append(out, lines[prev:sp.start]...)
		prev = sp.end
	}
	return append(out, lines[prev:]...)
}

// WriteBlock makes Render(shell, vars) the last block of path: every existing
// Marker block is removed and the new one is appended at the end of the file,
// so it wins over any earlier assignment in bash, zsh and fish. Everything
// outside the removed block is left byte-identical. When no Marker block
// existed but a LegacyMarker block does, the first legacy block is consumed
// (one-time migration); if a Marker block existed, legacy blocks belong to
// another install and are never touched. Creates the file (and its parent
// directory) if missing, and does not write when the content is unchanged.
func WriteBlock(path string, shell Shell, vars []Var) (WriteResult, error) {
	var res WriteResult
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return res, err
	}
	fileExists := err == nil

	lines := splitLines(string(existing))
	newSpans := findBlocks(lines, Marker)
	res.Replaced = len(newSpans) > 0
	lines = removeSpans(lines, newSpans)
	if !res.Replaced {
		if legacy := findBlocks(lines, LegacyMarker); len(legacy) > 0 {
			lines = removeSpans(lines, legacy[:1])
			res.MigratedLegacy = true
		}
	}

	rest := strings.Join(lines, "")
	if rest != "" && !strings.HasSuffix(rest, "\n") {
		rest += "\n"
	}
	out := rest + Render(shell, vars)

	if fileExists && out == string(existing) {
		res.Replaced = false
		return res, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return WriteResult{}, err
	}
	if err := writeFileAtomic(path, []byte(out)); err != nil {
		return WriteResult{}, err
	}
	res.Changed = true
	return res, nil
}

// RemoveBlock deletes every Marker block from path, leaving the rest of the
// file byte-identical. It never matches LegacyMarker, so a fork's block is
// never removed. A missing file is a no-op.
func RemoveBlock(path string) (removed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	lines := splitLines(string(data))
	spans := findBlocks(lines, Marker)
	if len(spans) == 0 {
		return false, nil
	}
	if err := writeFileAtomic(path, []byte(strings.Join(removeSpans(lines, spans), ""))); err != nil {
		return false, err
	}
	return true, nil
}

// PendingMigration reports whether path holds a LegacyMarker block and no
// Marker block, i.e. whether the next WriteBlock will migrate.
func PendingMigration(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	lines := splitLines(string(data))
	return len(findBlocks(lines, Marker)) == 0 && len(findBlocks(lines, LegacyMarker)) > 0, nil
}

// writeFileAtomic replaces path's content via a temp file in the same
// directory and a rename, following a symlinked path (dotfile managers
// symlink rc files) and keeping the existing file's mode. A new file is 0600.
func writeFileAtomic(path string, data []byte) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".envwriter-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return err
	}
	return nil
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

// ExistingVar reports the value assigned to name inside path's Marker block,
// if any; the last assignment in the block wins, and assignments outside the
// block (e.g. a fork's block) are ignored. When path has no Marker block but
// has a LegacyMarker block, the first legacy block is read instead, which is
// how the first run after upgrading learns an existing value before
// WriteBlock migrates it. A Marker block that never assigned name does not
// fall back to legacy. A bash value is returned unquoted (the inverse of the
// %q Render applies). ok is false, with err nil, when path doesn't exist,
// has no block, or the block never assigned name. Any other read error is
// returned as err.
func ExistingVar(path string, shell Shell, name string) (value string, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	lines := splitLines(string(data))
	spans := findBlocks(lines, Marker)
	if len(spans) == 0 {
		spans = findBlocks(lines, LegacyMarker)
		if len(spans) > 1 {
			spans = spans[:1]
		}
	}

	var pattern *regexp.Regexp
	switch shell {
	case Fish:
		pattern = regexp.MustCompile(`^set -gx ` + regexp.QuoteMeta(name) + ` (.+)$`)
	default:
		pattern = regexp.MustCompile(`^export ` + regexp.QuoteMeta(name) + `="(.*)"$`)
	}
	for _, sp := range spans {
		for _, line := range lines[sp.start+1 : sp.end] {
			if m := pattern.FindStringSubmatch(lineContent(line)); m != nil {
				value, ok = m[1], true
				if shell != Fish {
					if unq, err := strconv.Unquote(`"` + m[1] + `"`); err == nil {
						value = unq
					}
				}
			}
		}
	}
	return value, ok, nil
}
