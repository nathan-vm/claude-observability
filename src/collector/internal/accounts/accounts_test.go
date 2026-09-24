package accounts

import (
	"path/filepath"
	"testing"
)

func TestResolveConfigDirs_DefaultsToHomeClaudeWhenUnset(t *testing.T) {
	got := ResolveConfigDirs("/home/nathan", "", "")
	// Unlike the other cases below, an unset CLAUDE_DIR takes the
	// filepath.Join branch in ResolveConfigDirs, so the expected value
	// must go through filepath.Join too — on Windows that's a
	// backslash-joined path, not the forward-slash string the other
	// (pure string-replace) tests can hardcode.
	want := filepath.Join("/home/nathan", ".claude")
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %v, want [%s]", got, want)
	}
}

func TestResolveConfigDirs_ExpandsHomeVarInClaudeDir(t *testing.T) {
	got := ResolveConfigDirs("/home/nathan", "${HOME}/.claude-work", "")
	if len(got) != 1 || got[0] != "/home/nathan/.claude-work" {
		t.Errorf("got %v", got)
	}
}

func TestResolveConfigDirs_ExpandsBareDollarHomeInExtraDirs(t *testing.T) {
	got := ResolveConfigDirs("/home/nathan", "/home/nathan/.claude", "$HOME/.claude-work:$HOME/.claude-personal")
	want := []string{"/home/nathan/.claude", "/home/nathan/.claude-work", "/home/nathan/.claude-personal"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveConfigDirs_PrimaryFirstAndDeduped(t *testing.T) {
	got := ResolveConfigDirs("/home/nathan", "/home/nathan/.claude", "/home/nathan/.claude: :/home/nathan/.claude-work")
	want := []string{"/home/nathan/.claude", "/home/nathan/.claude-work"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
