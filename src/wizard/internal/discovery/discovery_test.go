package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func mkAccountDir(t *testing.T, home, name, email string) {
	t.Helper()
	dir := filepath.Join(home, name)
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if email != "" {
		content := `{"oauthAccount":{"emailAddress":"` + email + `"}}`
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFind_OrdersDefaultFirst(t *testing.T) {
	home := t.TempDir()
	mkAccountDir(t, home, ".claude-work", "work@example.com")
	mkAccountDir(t, home, ".claude", "personal@example.com")
	mkAccountDir(t, home, ".claude-aaa", "aaa@example.com")

	accounts, err := Find(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 {
		t.Fatalf("got %d accounts, want 3", len(accounts))
	}
	want := []string{".claude", ".claude-aaa", ".claude-work"}
	for i, w := range want {
		if got := filepath.Base(accounts[i].Dir); got != w {
			t.Errorf("accounts[%d] = %s, want %s", i, got, w)
		}
	}
}

func TestFind_ReadsEmail(t *testing.T) {
	home := t.TempDir()
	mkAccountDir(t, home, ".claude", "personal@example.com")

	accounts, err := Find(home)
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].Email != "personal@example.com" {
		t.Errorf("Email = %q, want personal@example.com", accounts[0].Email)
	}
}

func TestFind_SkipsDirsWithoutProjects_AndHandlesMalformedJSON(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude-broken")
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	accounts, err := Find(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1 (only .claude-broken has projects/)", len(accounts))
	}
	if accounts[0].Email != "" {
		t.Errorf("Email = %q, want empty for malformed json", accounts[0].Email)
	}
}

func TestFind_NoAccountsReturnsEmptySlice(t *testing.T) {
	home := t.TempDir()
	accounts, err := Find(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Errorf("got %d accounts, want 0", len(accounts))
	}
}

func TestWithConfigDir_AddsOutOfHomeDir(t *testing.T) {
	home := t.TempDir()
	mkAccountDir(t, home, ".claude", "a@example.com")
	other := t.TempDir()
	mkAccountDir(t, other, "cfg", "b@example.com")
	dir := filepath.Join(other, "cfg")

	accounts, _ := Find(home)
	got := WithConfigDir(accounts, dir)
	if len(got) != 2 || got[1].Dir != dir || got[1].Email != "b@example.com" {
		t.Errorf("got %+v", got)
	}
}

func TestWithConfigDir_DedupesSymlinkAndTrailingSlash(t *testing.T) {
	home := t.TempDir()
	mkAccountDir(t, home, ".claude", "a@example.com")
	accounts, _ := Find(home)
	real := filepath.Join(home, ".claude")

	if got := WithConfigDir(accounts, real+string(filepath.Separator)); len(got) != 1 {
		t.Errorf("trailing slash duplicated: %+v", got)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := WithConfigDir(accounts, link); len(got) != 1 {
		t.Errorf("symlink duplicated: %+v", got)
	}
}

func TestWithConfigDir_IgnoresInvalidDirs(t *testing.T) {
	base := []Account{{Dir: "/x"}}
	noProjects := t.TempDir()
	for name, dir := range map[string]string{
		"empty":       "",
		"nonexistent": filepath.Join(t.TempDir(), "nope"),
		"no projects": noProjects,
	} {
		if got := WithConfigDir(base, dir); len(got) != 1 {
			t.Errorf("%s: got %+v, want unchanged", name, got)
		}
	}
}

func TestWithConfigDir_RelativeDirMadeAbsoluteAndDeduped(t *testing.T) {
	home := t.TempDir()
	mkAccountDir(t, home, ".claude", "a@example.com")
	mkAccountDir(t, home, "cfg", "b@example.com")
	accounts, _ := Find(home)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	got := WithConfigDir(accounts, "cfg")
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if !filepath.IsAbs(got[1].Dir) {
		t.Errorf("Dir = %q, want absolute", got[1].Dir)
	}
	if got[1].Email != "b@example.com" {
		t.Errorf("Email = %q", got[1].Email)
	}
	if again := WithConfigDir(accounts, ".claude"); len(again) != 1 {
		t.Errorf("relative duplicate added: %+v", again)
	}
}
