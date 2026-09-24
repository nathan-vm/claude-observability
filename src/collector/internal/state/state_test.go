package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_MissingFileReturnsZeroValue(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != 2 {
		t.Errorf("Version = %d, want 2", st.Version)
	}
	if st.Files == nil || st.SessionEmail == nil || st.Seen == nil ||
		st.PluginSkills == nil || st.PluginSkillByRequest == nil {
		t.Error("all maps must be non-nil on a fresh state")
	}
}

func TestSaveThenLoad_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := &State{
		Version: 2,
		Files: map[string]*FileState{
			"/t.jsonl": {
				Offset: 42,
				PendingMain: &Group{
					SessionID: "s1", RequestID: "r1", Model: "claude", Effort: "high",
					GitBranch: "main", Project: "repo", IsSidechain: false, AtMs: 1700000000000,
					Calls: []Call{{ToolUseID: "tu1", ToolName: "Bash", ResultBytes: 10, Timestamp: "2026-01-01T00:00:00Z", BashCommands: []string{"git"}, ToolSource: "builtin"}},
				},
				ActiveSkillMain: "owner:skill",
			},
		},
		SessionEmail:         map[string]string{"s1": "a@example.com"},
		Seen:                 map[string]int64{"tu1": 1700000000000},
		PluginSkills:         map[string][]string{"s1": {"owner:skill"}},
		PluginSkillByRequest: map[string]PluginSkillRequest{"r1": {Skill: "owner:skill", TsMs: 1700000000000}},
	}
	if err := Save(path, st); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	fs := loaded.Files["/t.jsonl"]
	if fs == nil || fs.Offset != 42 || fs.PendingMain == nil || fs.PendingMain.SessionID != "s1" {
		t.Fatalf("loaded file state = %+v", fs)
	}
	if fs.PendingMain.Calls[0].ToolUseID != "tu1" || len(fs.PendingMain.Calls[0].BashCommands) != 1 {
		t.Fatalf("loaded call = %+v", fs.PendingMain.Calls[0])
	}
	if loaded.SessionEmail["s1"] != "a@example.com" {
		t.Errorf("SessionEmail = %v", loaded.SessionEmail)
	}
	if loaded.PluginSkillByRequest["r1"].Skill != "owner:skill" {
		t.Errorf("PluginSkillByRequest = %v", loaded.PluginSkillByRequest)
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "state.json")
	if err := Save(path, &State{Version: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
