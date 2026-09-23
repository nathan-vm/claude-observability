package accounts

import "testing"

func TestResolveConfigDirs_DefaultsToHomeClaudeWhenUnset(t *testing.T) {
	got := ResolveConfigDirs("/home/nathan", "", "")
	if len(got) != 1 || got[0] != "/home/nathan/.claude" {
		t.Errorf("got %v", got)
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
