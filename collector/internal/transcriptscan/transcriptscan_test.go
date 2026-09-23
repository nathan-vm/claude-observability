package transcriptscan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"claude-observability-collector/internal/state"
)

func TestFindTranscripts_WalksDepthAndFindsSubagentFiles(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "projects", "proj1", "session1", "subagents"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "projects", "proj1", "session1.jsonl"), []byte("{}"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "projects", "proj1", "session1", "subagents", "sub1.jsonl"), []byte("{}"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "projects", "proj1", "notes.txt"), []byte("x"), 0o644))

	found, err := FindTranscripts([]string{filepath.Join(root, "projects")}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("got %d files, want 2 (session + subagent): %v", len(found), found)
	}
}

func TestFindTranscripts_MissingDirIsNonFatal(t *testing.T) {
	found, err := FindTranscripts([]string{filepath.Join(t.TempDir(), "nope")}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("got %v, want empty", found)
	}
}

func TestParseToolName(t *testing.T) {
	cases := []struct {
		name, wantSource, wantServer, wantTool string
	}{
		{"mcp__github__search_code", "mcp", "github", "search_code"},
		{"mcp__github", "mcp", "github", ""},
		{"Bash", "builtin", "", ""},
		{"", "builtin", "", ""},
	}
	for _, c := range cases {
		source, server, tool := ParseToolName(c.name)
		if source != c.wantSource || server != c.wantServer || tool != c.wantTool {
			t.Errorf("ParseToolName(%q) = %q,%q,%q want %q,%q,%q", c.name, source, server, tool, c.wantSource, c.wantServer, c.wantTool)
		}
	}
}

func TestParseToolName_SplitsOnlyOnFirstDoubleUnderscore(t *testing.T) {
	_, server, tool := ParseToolName("mcp__github__search__code")
	if server != "github" || tool != "search__code" {
		t.Errorf("server=%q tool=%q, want github, search__code", server, tool)
	}
}

func TestBlockSize_StringContent(t *testing.T) {
	block := map[string]interface{}{"content": "hello"}
	if got := BlockSize(block); got != 5 {
		t.Errorf("got %d, want 5", got)
	}
}

func TestBlockSize_ArrayContentMixedParts(t *testing.T) {
	block := map[string]interface{}{"content": []interface{}{
		"abc",
		map[string]interface{}{"type": "text", "text": "de"},
		map[string]interface{}{"type": "other", "foo": "bar"},
	}}
	other := map[string]interface{}{"type": "other", "foo": "bar"}
	encoded, _ := json.Marshal(other)
	want := 3 + 2 + len(encoded)
	if got := BlockSize(block); got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

func TestBlockSize_NilContentFallsBackToEmptyStringJSON(t *testing.T) {
	block := map[string]interface{}{}
	if got := BlockSize(block); got != 2 { // json.Marshal("") == `""`
		t.Errorf("got %d, want 2", got)
	}
}

func TestSettle_ProportionalSplitAcrossParallelCalls(t *testing.T) {
	group := state.Group{
		SessionID: "s1", RequestID: "r1", Model: "claude", IsSidechain: false,
		Calls: []state.Call{
			{ToolUseID: "a", ToolName: "Read", ResultBytes: 30, Timestamp: "2026-01-01T00:00:00Z", ToolSource: "builtin"},
			{ToolUseID: "b", ToolName: "Read", ResultBytes: 10, Timestamp: "2026-01-01T00:00:00Z", ToolSource: "builtin"},
		},
	}
	records := Settle(group, 400)
	if len(records) != 2 {
		t.Fatalf("got %d records", len(records))
	}
	byID := map[string]Record{}
	for _, r := range records {
		byID[r.Meta["tool_use_id"]] = r
	}
	if byID["a"].Meta["tokens_attributed"] != "300" {
		t.Errorf("call a tokens_attributed = %v", byID["a"].Meta["tokens_attributed"])
	}
	if byID["b"].Meta["tokens_attributed"] != "100" {
		t.Errorf("call b tokens_attributed = %v", byID["b"].Meta["tokens_attributed"])
	}
}

func TestSettle_EvenSplitWhenAllResultsEmpty(t *testing.T) {
	group := state.Group{
		SessionID: "s1", Calls: []state.Call{
			{ToolUseID: "a", ToolName: "Read", ResultBytes: 0, Timestamp: "2026-01-01T00:00:00Z"},
			{ToolUseID: "b", ToolName: "Read", ResultBytes: 0, Timestamp: "2026-01-01T00:00:00Z"},
		},
	}
	records := Settle(group, 100)
	for _, r := range records {
		if r.Meta["tokens_attributed"] != "50" {
			t.Errorf("tokens_attributed = %v, want 50 (even split)", r.Meta["tokens_attributed"])
		}
	}
}

func TestSettle_BashFanoutSharesTokensAndHasDistinctDedupKeys(t *testing.T) {
	group := state.Group{
		SessionID: "s1", Calls: []state.Call{
			{ToolUseID: "bash1", ToolName: "Bash", ResultBytes: 100, Timestamp: "2026-01-01T00:00:00Z",
				BashCommands: []string{"git", "ls"}, ToolSource: "builtin"},
		},
	}
	records := Settle(group, 200)
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (one per sub-command)", len(records))
	}
	if records[0].Meta["tokens_attributed"] != "200" || records[1].Meta["tokens_attributed"] != "200" {
		t.Error("each fanout row must reuse the full call's tokens_attributed, not split further")
	}
	if records[0].DedupKey != "bash:bash1:0" || records[1].DedupKey != "bash:bash1:1" {
		t.Errorf("dedup keys = %q, %q", records[0].DedupKey, records[1].DedupKey)
	}
	if records[0].Meta["mcp_server"] != "bash" || records[0].Meta["mcp_tool"] != "git" {
		t.Errorf("meta = %v", records[0].Meta)
	}
}

func writeJSONL(t *testing.T, path string, lines []string) {
	t.Helper()
	var content string
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestProcessTranscript_SettlesOnNextAssistantMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	writeJSONL(t, path, []string{
		`{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":"2026-01-01T00:00:00Z","message":{"model":"claude","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"git status"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}`,
		`{"type":"assistant","sessionId":"s1","requestId":"r2","timestamp":"2026-01-01T00:01:00Z","message":{"model":"claude","usage":{"input_tokens":100,"cache_creation_input_tokens":0},"content":[]}}`,
	})

	records, fs, err := ProcessTranscript(path, state.FileState{}, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1 (settled by the second assistant message)", len(records))
	}
	if records[0].Meta["tokens_attributed"] != "100" {
		t.Errorf("tokens_attributed = %v", records[0].Meta["tokens_attributed"])
	}
	if fs.PendingMain != nil {
		t.Error("pending group should be cleared after settling")
	}
}

func TestProcessTranscript_ResumesFromOffsetAcrossTwoPasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	line1 := `{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":"2026-01-01T00:00:00Z","message":{"model":"claude","usage":{"input_tokens":10},"content":[]}}`
	writeJSONL(t, path, []string{line1})

	// A line's bytes are only folded into the consumed offset once the
	// NEXT line proves it ended in "\n" — with only one line in the file,
	// pass 1 must NOT advance the offset yet, even though this line does
	// have a real trailing newline (the algorithm can't tell the
	// difference without a following line; see ProcessTranscript's doc
	// comment). Confirmed by running this against the implementation.
	_, fs1, err := ProcessTranscript(path, state.FileState{}, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if fs1.Offset != 0 {
		t.Fatalf("offset after pass 1 = %d, want 0 (line 1's bytes aren't proven consumed yet)", fs1.Offset)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	must(t, err)
	line2 := `{"type":"assistant","sessionId":"s1","requestId":"r2","timestamp":"2026-01-01T00:01:00Z","message":{"model":"claude","content":[{"type":"tool_use","id":"tu2","name":"Read"}]}}`
	f.WriteString(line2 + "\n")
	f.Close()

	// Pass 2 re-reads from offset 0 (line 1 is re-parsed — harmless here,
	// it had no tool_use and nothing was pending to settle), and NOW
	// line 1's bytes fold into the offset once line 2 is seen after it.
	// Line 2 itself is the new last line, so ITS bytes stay pending in
	// turn — this is the same held-back-last-line rule applying again,
	// one line later.
	records2, fs2, err := ProcessTranscript(path, fs1, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records2) != 0 {
		t.Fatalf("got %d records on pass 2, want 0 (tu2 not yet settled)", len(records2))
	}
	if fs2.Offset != int64(len(line1)+1) {
		t.Errorf("offset after pass 2 = %d, want %d (line 1 now proven consumed)", fs2.Offset, len(line1)+1)
	}
	if fs2.PendingMain == nil || fs2.PendingMain.RequestID != "r2" {
		t.Fatalf("pending after pass 2 = %+v", fs2.PendingMain)
	}
}

func TestProcessTranscript_ShrunkFileResetsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	writeJSONL(t, path, []string{`{"type":"assistant","sessionId":"s1","timestamp":"2026-01-01T00:00:00Z","message":{"model":"claude","content":[{"type":"tool_use","id":"tu1","name":"Read"}]}}`})
	info, _ := os.Stat(path)
	bigOffset := info.Size() + 1000

	// After the shrink reset, the file gets properly re-scanned from
	// offset 0 — its one real line DOES have an unsettled tool_use, so a
	// fresh pending group is legitimately (re)built. What must be true is
	// that it's the REAL group (session "s1"), not the stale leftover
	// ("stale") the caller passed in — nil would be wrong here too, since
	// this file genuinely has an open tool call once actually read.
	_, fs, err := ProcessTranscript(path, state.FileState{Offset: bigOffset, PendingMain: &state.Group{SessionID: "stale"}}, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if fs.PendingMain == nil || fs.PendingMain.SessionID != "s1" {
		t.Errorf("pending after shrink+rescan = %+v, want a fresh group from session s1, not the stale one", fs.PendingMain)
	}
}
