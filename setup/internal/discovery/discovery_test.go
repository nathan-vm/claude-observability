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
