package service

import (
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
)

// ErrNotOwned is returned by MigrateLegacy when a service exists under the
// legacy name but runs an executable this checkout doesn't own (typically a
// fork installed side by side). Nothing was changed.
var ErrNotOwned = errors.New("legacy service belongs to a different install")

// ErrUnverified is returned by MigrateLegacy when a service may exist under the
// legacy name but couldn't be inspected well enough to tell whose it is
// (unparseable definition, missing executable, failing query). Nothing was
// changed.
var ErrUnverified = errors.New("could not verify who owns the legacy service")

// MigrateOptions describes the legacy service to retire.
type MigrateOptions struct {
	LegacyLabel   string   // old service name on this OS
	OwnedCommands []string // absolute executable paths that count as "this checkout's collector"
}

type runner func(name string, args ...string) ([]byte, error)

func execRunner(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// resolvePath cleans p and resolves symlinks in its longest existing prefix,
// so a path whose final components don't exist still compares correctly.
func resolvePath(p string) string {
	p = filepath.Clean(p)
	tail := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

func pathsEqual(a, b string) bool {
	a, b = resolvePath(a), resolvePath(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func isOwned(command string, owned []string) bool {
	if command == "" {
		return false
	}
	for _, o := range owned {
		if pathsEqual(command, o) {
			return true
		}
	}
	return false
}

// plistProgramArguments returns the ProgramArguments strings of an XML plist.
func plistProgramArguments(data []byte) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		inKey, wantArray, inArray, inString bool
		text                                strings.Builder
		args                                []string
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case t.Name.Local == "key" && !inArray:
				inKey, wantArray = true, false
				text.Reset()
			case t.Name.Local == "array" && wantArray:
				inArray, wantArray = true, false
			case t.Name.Local == "string" && inArray:
				inString = true
				text.Reset()
			default:
				wantArray = false
			}
		case xml.CharData:
			if inKey || inString {
				text.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "key":
				if inKey && strings.TrimSpace(text.String()) == "ProgramArguments" {
					wantArray = true
				}
				inKey = false
			case "string":
				if inString {
					args = append(args, text.String())
				}
				inString = false
			case "array":
				if inArray {
					return args, nil
				}
			}
		}
	}
	return nil, errors.New("no ProgramArguments array in plist")
}

// unitExecStart returns the executable of the first ExecStart= line of a
// systemd unit.
func unitExecStart(data []byte) (string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, "@-:+!")
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return "", false
		}
		return fields[0], true
	}
	return "", false
}

func decodeText(data []byte) string {
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe {
		u := make([]uint16, 0, len(data)/2)
		for i := 2; i+1 < len(data); i += 2 {
			u = append(u, uint16(data[i])|uint16(data[i+1])<<8)
		}
		return string(utf16.Decode(u))
	}
	return string(data)
}

// taskOwned reports whether a `schtasks /query /xml` document runs one of the
// owned executables, matched as the quoted path the installer writes into the
// task's command line.
func taskOwned(data []byte, owned []string) bool {
	text := strings.TrimPrefix(decodeText(data), "\xef\xbb\xbf")
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	var cmdline strings.Builder
	capturing := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			capturing = t.Name.Local == "Command" || t.Name.Local == "Arguments"
			if capturing {
				cmdline.WriteByte(' ')
			}
		case xml.CharData:
			if capturing {
				cmdline.Write(t)
			}
		case xml.EndElement:
			capturing = false
		}
	}
	haystack := strings.ToLower(cmdline.String())
	for _, o := range owned {
		if strings.Contains(haystack, `"`+strings.ToLower(filepath.Clean(o))+`"`) {
			return true
		}
	}
	return false
}

func migrateLaunchAgent(plistPath, uid string, opts MigrateOptions, run runner) (bool, error) {
	data, err := os.ReadFile(plistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	args, err := plistProgramArguments(data)
	if err != nil {
		return false, fmt.Errorf("%w: %s: %v", ErrUnverified, plistPath, err)
	}
	if len(args) == 0 {
		return false, fmt.Errorf("%w: %s: empty ProgramArguments", ErrUnverified, plistPath)
	}
	if !isOwned(args[0], opts.OwnedCommands) {
		return false, ErrNotOwned
	}
	run("launchctl", "bootout", "gui/"+uid, plistPath)
	if err := os.Remove(plistPath); err != nil {
		return false, err
	}
	return true, nil
}

func migrateSystemdUnit(unitPath string, opts MigrateOptions, run runner) (bool, error) {
	data, err := os.ReadFile(unitPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	command, ok := unitExecStart(data)
	if !ok {
		return false, fmt.Errorf("%w: %s: no ExecStart", ErrUnverified, unitPath)
	}
	if !isOwned(command, opts.OwnedCommands) {
		return false, ErrNotOwned
	}
	run("systemctl", "--user", "disable", "--now", filepath.Base(unitPath))
	if err := os.Remove(unitPath); err != nil {
		return false, err
	}
	run("systemctl", "--user", "daemon-reload")
	return true, nil
}

// scheduledTaskExists lists every task rather than querying the legacy one
// by name, so "absent" doesn't depend on schtasks' localized error text.
func scheduledTaskExists(name string, run runner) (bool, error) {
	out, err := run("schtasks", "/query", "/fo", "CSV", "/nh")
	if err != nil {
		return false, err
	}
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(decodeText(out), "\xef\xbb\xbf")))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			continue
		}
		if len(rec) > 0 && strings.EqualFold(strings.TrimPrefix(rec[0], `\`), name) {
			return true, nil
		}
	}
}

func migrateScheduledTask(opts MigrateOptions, run runner) (bool, error) {
	exists, err := scheduledTaskExists(opts.LegacyLabel, run)
	if err != nil {
		return false, fmt.Errorf("%w: listing scheduled tasks: %v", ErrUnverified, err)
	}
	if !exists {
		return false, nil
	}
	out, err := run("schtasks", "/query", "/tn", opts.LegacyLabel, "/xml")
	if err != nil {
		return false, fmt.Errorf("%w: querying %s: %v", ErrUnverified, opts.LegacyLabel, err)
	}
	if !taskOwned(out, opts.OwnedCommands) {
		return false, ErrNotOwned
	}
	if out, err := run("schtasks", "/delete", "/tn", opts.LegacyLabel, "/f"); err != nil {
		return false, fmt.Errorf("schtasks /delete: %w: %s", err, out)
	}
	return true, nil
}
