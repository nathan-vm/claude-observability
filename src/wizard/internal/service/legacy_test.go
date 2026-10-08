package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
)

type call struct{ name, args string }

type fakeRunner struct {
	calls []call
	out   []byte
	outs  map[string][]byte
	errs  map[string]error
}

func (f *fakeRunner) run(name string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, call{name, joined})
	if out, ok := f.outs[name+" "+joined]; ok {
		return out, f.errs[name+" "+joined]
	}
	return f.out, f.errs[name+" "+joined]
}

func (f *fakeRunner) seen() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, c.name+" "+c.args)
	}
	return out
}

func plistFixture(command string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.claude-observability.collector</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + command + `</string>
    <string>--once</string>
  </array>
  <key>WorkingDirectory</key><string>/repo</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>/usr/bin</string>
  </dict>
</dict>
</plist>
`
}

func TestPlistProgramArguments(t *testing.T) {
	got, err := plistProgramArguments([]byte(plistFixture("/repo/.bin/claude-observability-collector")))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/repo/.bin/claude-observability-collector", "--once"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"", "not xml at all", "bplist00\x00\x01\x02", "<plist><dict><key>Label</key><string>x</string></dict></plist>"} {
		if _, err := plistProgramArguments([]byte(bad)); err == nil {
			t.Errorf("plistProgramArguments(%q) = nil error, want error", bad)
		}
	}
}

func TestMigrateLaunchAgent(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	ownedBin := filepath.Join(repo, ".bin", "claude-observability-collector")
	otherBin := filepath.Join(t.TempDir(), "zallpy", "claude-observability", ".bin", "claude-observability-collector")
	installBin := filepath.Join(t.TempDir(), "elsewhere", "claude-observability-collector")
	opts := MigrateOptions{LegacyLabel: "com.claude-observability.collector", OwnedCommands: []string{installBin, ownedBin}}

	setup := func(t *testing.T, content string) string {
		path := filepath.Join(t.TempDir(), "legacy.plist")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("absent", func(t *testing.T) {
		f := &fakeRunner{}
		migrated, err := migrateLaunchAgent(filepath.Join(t.TempDir(), "none.plist"), "501", opts, f.run)
		if migrated || err != nil || len(f.calls) != 0 {
			t.Errorf("migrated=%v err=%v calls=%v", migrated, err, f.calls)
		}
	})

	for name, bin := range map[string]string{"repo bin": ownedBin, "install command": installBin} {
		t.Run("owned via "+name, func(t *testing.T) {
			path := setup(t, plistFixture(bin))
			f := &fakeRunner{}
			migrated, err := migrateLaunchAgent(path, "501", opts, f.run)
			if !migrated || err != nil {
				t.Fatalf("migrated=%v err=%v", migrated, err)
			}
			if want := []string{"launchctl bootout gui/501 " + path}; !reflect.DeepEqual(f.seen(), want) {
				t.Errorf("calls = %v, want %v", f.seen(), want)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Error("plist not removed")
			}
		})
	}

	t.Run("same binary name in a different checkout", func(t *testing.T) {
		content := plistFixture(otherBin)
		path := setup(t, content)
		f := &fakeRunner{}
		migrated, err := migrateLaunchAgent(path, "501", opts, f.run)
		if migrated || !errors.Is(err, ErrNotOwned) || len(f.calls) != 0 {
			t.Errorf("migrated=%v err=%v calls=%v", migrated, err, f.calls)
		}
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Error("foreign plist was modified")
		}
	})

	t.Run("unparseable", func(t *testing.T) {
		path := setup(t, "bplist00\x00garbage")
		f := &fakeRunner{}
		migrated, err := migrateLaunchAgent(path, "501", opts, f.run)
		if migrated || !errors.Is(err, ErrUnverified) || len(f.calls) != 0 {
			t.Errorf("migrated=%v err=%v calls=%v", migrated, err, f.calls)
		}
		if _, err := os.Stat(path); err != nil {
			t.Error("unparseable plist was removed")
		}
	})

	t.Run("bootout failure still removes", func(t *testing.T) {
		path := setup(t, plistFixture(ownedBin))
		f := &fakeRunner{errs: map[string]error{"launchctl bootout gui/501 " + path: errors.New("not loaded")}}
		migrated, err := migrateLaunchAgent(path, "501", opts, f.run)
		if !migrated || err != nil {
			t.Fatalf("migrated=%v err=%v", migrated, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("plist not removed")
		}
	})

	t.Run("remove failure is returned", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs unix permissions and a non-root user")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "legacy.plist")
		if err := os.WriteFile(path, []byte(plistFixture(ownedBin)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o700)
		f := &fakeRunner{}
		migrated, err := migrateLaunchAgent(path, "501", opts, f.run)
		if migrated || err == nil || errors.Is(err, ErrNotOwned) {
			t.Errorf("migrated=%v err=%v, want a remove error", migrated, err)
		}
	})
}

func TestUnitExecStart(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"[Service]\nExecStart=/a/b --x\nRestart=always\n", "/a/b", true},
		{"[Service]\nExecStart=-/a/b\n", "/a/b", true},
		{"[Service]\nRestart=always\n", "", false},
		{"ExecStart=\n", "", false},
	}
	for _, c := range cases {
		got, ok := unitExecStart([]byte(c.in))
		if got != c.want || ok != c.ok {
			t.Errorf("unitExecStart(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestMigrateSystemdUnit(t *testing.T) {
	repo := t.TempDir()
	ownedBin := filepath.Join(repo, ".bin", "claude-observability-collector")
	opts := MigrateOptions{LegacyLabel: "claude-observability-collector", OwnedCommands: []string{ownedBin}}
	unit := func(bin string) string {
		return "[Service]\nWorkingDirectory=/repo\nExecStart=" + bin + "\nRestart=always\n"
	}
	write := func(t *testing.T, content string) string {
		path := filepath.Join(t.TempDir(), "claude-observability-collector.service")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("owned", func(t *testing.T) {
		path := write(t, unit(ownedBin))
		f := &fakeRunner{}
		migrated, err := migrateSystemdUnit(path, opts, f.run)
		if !migrated || err != nil {
			t.Fatalf("migrated=%v err=%v", migrated, err)
		}
		want := []string{
			"systemctl --user disable --now claude-observability-collector.service",
			"systemctl --user daemon-reload",
		}
		if !reflect.DeepEqual(f.seen(), want) {
			t.Errorf("calls = %v, want %v", f.seen(), want)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("unit not removed")
		}
	})

	t.Run("not owned", func(t *testing.T) {
		path := write(t, unit("/other/zallpy/.bin/claude-observability-collector"))
		f := &fakeRunner{}
		migrated, err := migrateSystemdUnit(path, opts, f.run)
		if migrated || !errors.Is(err, ErrNotOwned) || len(f.calls) != 0 {
			t.Errorf("migrated=%v err=%v calls=%v", migrated, err, f.calls)
		}
		if _, err := os.Stat(path); err != nil {
			t.Error("foreign unit was removed")
		}
	})

	t.Run("no ExecStart", func(t *testing.T) {
		path := write(t, "[Service]\nRestart=always\n")
		f := &fakeRunner{}
		migrated, err := migrateSystemdUnit(path, opts, f.run)
		if migrated || !errors.Is(err, ErrUnverified) || len(f.calls) != 0 {
			t.Errorf("migrated=%v err=%v calls=%v", migrated, err, f.calls)
		}
		if _, err := os.Stat(path); err != nil {
			t.Error("unit was removed")
		}
	})

	t.Run("absent", func(t *testing.T) {
		f := &fakeRunner{}
		migrated, err := migrateSystemdUnit(filepath.Join(t.TempDir(), "none.service"), opts, f.run)
		if migrated || err != nil || len(f.calls) != 0 {
			t.Errorf("migrated=%v err=%v calls=%v", migrated, err, f.calls)
		}
	})
}

func taskXML(command, arguments string) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Actions Context="Author">
    <Exec>
      <Command>` + command + `</Command>
      <Arguments>` + arguments + `</Arguments>
    </Exec>
  </Actions>
</Task>`
}

func TestTaskOwned(t *testing.T) {
	owned := []string{`C:\Repo\.bin\claude-observability-collector.exe`}
	args := func(bin string) string { return `/c set A=1&amp;&amp; &quot;` + bin + `&quot;` }

	if !taskOwned([]byte(taskXML("cmd", args(`C:\Repo\.bin\claude-observability-collector.exe`))), owned) {
		t.Error("exact path should be owned")
	}
	if !taskOwned([]byte(taskXML("cmd", args(`c:\repo\.BIN\claude-observability-collector.exe`))), owned) {
		t.Error("path match should be case-insensitive")
	}
	if taskOwned([]byte(taskXML("cmd", args(`C:\Zallpy\.bin\claude-observability-collector.exe`))), owned) {
		t.Error("different checkout should not be owned")
	}
	if taskOwned([]byte(taskXML("cmd", args(`C:\Repo\.bin\claude-observability-collector.exe.bak`))), owned) {
		t.Error("longer path should not be owned")
	}

	u := utf16.Encode([]rune(taskXML("cmd", args(`C:\Repo\.bin\claude-observability-collector.exe`))))
	raw := []byte{0xff, 0xfe}
	for _, c := range u {
		raw = append(raw, byte(c), byte(c>>8))
	}
	if !taskOwned(raw, owned) {
		t.Error("UTF-16 output should be decoded")
	}
}

func TestMigrateScheduledTask(t *testing.T) {
	bin := `C:\Repo\.bin\claude-observability-collector.exe`
	opts := MigrateOptions{LegacyLabel: "ClaudeObservabilityCollector", OwnedCommands: []string{bin}}
	const (
		list  = "schtasks /query /fo CSV /nh"
		query = "schtasks /query /tn ClaudeObservabilityCollector /xml"
		del   = "schtasks /delete /tn ClaudeObservabilityCollector /f"
	)
	listing := []byte("\"\\Other\",\"N/A\",\"Ready\"\n\"\\ClaudeObservabilityCollector\",\"At log on\",\"Ready\"\n")
	xmlFor := func(b string) []byte { return []byte(taskXML("cmd", `/c &quot;`+b+`&quot;`)) }

	t.Run("owned", func(t *testing.T) {
		f := &fakeRunner{outs: map[string][]byte{list: listing, query: xmlFor(bin)}}
		migrated, err := migrateScheduledTask(opts, f.run)
		if !migrated || err != nil {
			t.Fatalf("migrated=%v err=%v", migrated, err)
		}
		if want := []string{list, query, del}; !reflect.DeepEqual(f.seen(), want) {
			t.Errorf("calls = %v, want %v", f.seen(), want)
		}
	})

	t.Run("different path", func(t *testing.T) {
		f := &fakeRunner{outs: map[string][]byte{list: listing, query: xmlFor(`C:\Zallpy\.bin\claude-observability-collector.exe`)}}
		migrated, err := migrateScheduledTask(opts, f.run)
		if migrated || !errors.Is(err, ErrNotOwned) || len(f.calls) != 2 {
			t.Errorf("migrated=%v err=%v calls=%v", migrated, err, f.seen())
		}
	})

	t.Run("task absent", func(t *testing.T) {
		f := &fakeRunner{outs: map[string][]byte{list: []byte("\"\\Other\",\"N/A\",\"Ready\"\n")}}
		migrated, err := migrateScheduledTask(opts, f.run)
		if migrated || err != nil || len(f.calls) != 1 {
			t.Errorf("migrated=%v err=%v calls=%v", migrated, err, f.seen())
		}
	})

	t.Run("listing fails", func(t *testing.T) {
		f := &fakeRunner{errs: map[string]error{list: errors.New("access denied")}}
		migrated, err := migrateScheduledTask(opts, f.run)
		if migrated || !errors.Is(err, ErrUnverified) {
			t.Errorf("migrated=%v err=%v", migrated, err)
		}
	})

	t.Run("query of existing task fails", func(t *testing.T) {
		f := &fakeRunner{outs: map[string][]byte{list: listing}, errs: map[string]error{query: errors.New("exit 1")}}
		migrated, err := migrateScheduledTask(opts, f.run)
		if migrated || !errors.Is(err, ErrUnverified) || len(f.calls) != 2 {
			t.Errorf("migrated=%v err=%v calls=%v", migrated, err, f.seen())
		}
	})

	t.Run("delete failure", func(t *testing.T) {
		f := &fakeRunner{outs: map[string][]byte{list: listing, query: xmlFor(bin)}, errs: map[string]error{del: errors.New("denied")}}
		migrated, err := migrateScheduledTask(opts, f.run)
		if migrated || err == nil || errors.Is(err, ErrNotOwned) || errors.Is(err, ErrUnverified) {
			t.Errorf("migrated=%v err=%v, want a delete error", migrated, err)
		}
	})
}

func TestPathsEqual(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a", "bin")
	if !pathsEqual(a, filepath.Join(dir, "a", ".", "bin")) {
		t.Error("cleaning should make these equal")
	}
	if !pathsEqual(a, a+string(filepath.Separator)) {
		t.Error("trailing separator should be ignored")
	}
	if pathsEqual(a, filepath.Join(dir, "b", "bin")) {
		t.Error("different paths reported equal")
	}

	if runtime.GOOS == "windows" {
		return
	}
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("cannot create symlink:", err)
	}
	if !pathsEqual(filepath.Join(real, "bin"), filepath.Join(link, "bin")) {
		t.Error("symlinked directory should resolve to the same path")
	}
}
