package transcriptscan

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestResolveEmails_ResolvesAndCachesEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.Contains(q, "s1") {
			w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
				{"stream":{"user_email":"a@example.com"},"values":[["1","x"]]}
			]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	sessionEmail := map[string]string{}
	ResolveEmails(srv.URL, 720, sessionEmail, []string{"s1", "s2"}, t.Logf)
	if sessionEmail["s1"] != "a@example.com" {
		t.Errorf("s1 = %q", sessionEmail["s1"])
	}
	if _, ok := sessionEmail["s2"]; !ok {
		t.Error("s2 must be cached as empty, not left unresolved (so it's never re-queried)")
	}
	if sessionEmail["s2"] != "" {
		t.Errorf("s2 = %q, want empty", sessionEmail["s2"])
	}
}

func TestResolveEmails_SkipsAlreadyCachedSessions(t *testing.T) {
	queried := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queried = true
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	sessionEmail := map[string]string{"s1": "cached@example.com"}
	ResolveEmails(srv.URL, 720, sessionEmail, []string{"s1"}, t.Logf)
	if queried {
		t.Error("must not re-query an already-cached session")
	}
}

func TestProjectOwners_OnlyMapsUnambiguousProjects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"project":"solo","user_email":"a@example.com"},"values":[["1","x"]]},
			{"stream":{"project":"shared","user_email":"a@example.com"},"values":[["1","x"]]},
			{"stream":{"project":"shared","user_email":"b@example.com"},"values":[["1","x"]]}
		]}}`))
	}))
	defer srv.Close()

	owners, err := ProjectOwners(srv.URL, "s", 90, nil)
	if err != nil {
		t.Fatal(err)
	}
	if owners["solo"] != "a@example.com" {
		t.Errorf("solo = %q", owners["solo"])
	}
	if _, ok := owners["shared"]; ok {
		t.Error("a project with 2+ distinct emails must be left unmapped")
	}
}

func TestProjectOwners_IncludesCurrentBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	batch := []Record{{Meta: map[string]string{"project": "fresh", "user_email": "a@example.com"}}}
	owners, err := ProjectOwners(srv.URL, "s", 90, batch)
	if err != nil {
		t.Fatal(err)
	}
	if owners["fresh"] != "a@example.com" {
		t.Errorf("owners = %v, want the batch-only project mapped", owners)
	}
}

func TestDropAlreadySeen_FiltersCrossBatchAndSameBatchDuplicates(t *testing.T) {
	seen := map[string]int64{"already": time.Now().UnixMilli()}
	records := []Record{
		{Meta: map[string]string{"tool_use_id": "already"}},
		{Meta: map[string]string{"tool_use_id": "fresh1"}},
		{Meta: map[string]string{"tool_use_id": "fresh1"}}, // duplicate within this same batch
	}
	fresh, novos := DropAlreadySeen(seen, 90, records)
	if len(fresh) != 1 {
		t.Fatalf("got %d fresh, want 1", len(fresh))
	}
	if _, ok := novos["fresh1"]; !ok {
		t.Error("novos must stage the newly-seen key")
	}
}

func TestDropAlreadySeen_PrunesEntriesOlderThanDedupDays(t *testing.T) {
	oldMs := time.Now().AddDate(0, 0, -100).UnixMilli()
	seen := map[string]int64{"stale": oldMs}
	DropAlreadySeen(seen, 90, nil)
	if _, ok := seen["stale"]; ok {
		t.Error("entries older than dedupDays must be pruned")
	}
}

func TestDropAlreadySeen_RecordWithNoIDAlwaysPasses(t *testing.T) {
	fresh, _ := DropAlreadySeen(map[string]int64{}, 90, []Record{{Meta: map[string]string{}}})
	if len(fresh) != 1 {
		t.Error("a record with neither dedupKey nor tool_use_id must always pass through")
	}
}

func TestRebuildSkillRecords_PerRequestOverridesThirdParty(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// fullHistory=false makes RebuildSkillRecords loop 2 day-windows;
		// a real Loki would only return this event within its true
		// window, but this fixture doesn't filter by start/end, so it
		// only answers the FIRST call to avoid an artificial duplicate.
		if atomic.AddInt32(&calls, 1) > 1 {
			w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"session_id":"s1","request_id":"r1","skill_name":"third-party","model":"claude","input_tokens":"10","output_tokens":"20","cache_creation_tokens":"0"},"values":[["1700000000000000000","x"]]}
		]}}`))
	}))
	defer srv.Close()

	pluginSkills := map[string]map[string]bool{"s1": {"superpowers:brainstorming": true, "other:thing": true}}
	pluginSkillByRequest := map[string]state.PluginSkillRequest{"r1": {Skill: "superpowers:brainstorming", TsMs: 1}}

	records, err := RebuildSkillRecords(srv.URL, "claude-code-exporter-1", 90, pluginSkills, pluginSkillByRequest, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records", len(records))
	}
	if records[0].Line != "superpowers:brainstorming" {
		t.Errorf("skill = %q, want the per-request resolved name (ambiguous session-level set correctly bypassed)", records[0].Line)
	}
	if records[0].Meta["skill_owner"] != "superpowers" {
		t.Errorf("owner = %q", records[0].Meta["skill_owner"])
	}
	if records[0].Meta["tokens"] != "30" {
		t.Errorf("tokens = %q, want 30 (10+20+0)", records[0].Meta["tokens"])
	}
	if records[0].DedupKey != "skill:r1" {
		t.Errorf("dedupKey = %q", records[0].DedupKey)
	}
}

func TestRebuildSkillRecords_SessionLevelFallbackWhenUnambiguous(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// See the comment in TestRebuildSkillRecords_PerRequestOverridesThirdParty:
		// fullHistory=false loops 2 day-windows against this non-windowed fixture.
		if atomic.AddInt32(&calls, 1) > 1 {
			w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"session_id":"s1","request_id":"r-unseen","skill_name":"third-party"},"values":[["1700000000000000000","x"]]}
		]}}`))
	}))
	defer srv.Close()

	pluginSkills := map[string]map[string]bool{"s1": {"owner:only-one": true}}
	records, err := RebuildSkillRecords(srv.URL, "s", 90, pluginSkills, map[string]state.PluginSkillRequest{}, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Line != "owner:only-one" {
		t.Fatalf("got %+v, want session-level unambiguous fallback", records)
	}
}

func TestRebuildSkillRecords_NonThirdPartyUsesOTelNameAsIs(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// See the comment in TestRebuildSkillRecords_PerRequestOverridesThirdParty:
		// fullHistory=false loops 2 day-windows against this non-windowed fixture.
		if atomic.AddInt32(&calls, 1) > 1 {
			w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"session_id":"s1","request_id":"r1","skill_name":"local-skill"},"values":[["1700000000000000000","x"]]}
		]}}`))
	}))
	defer srv.Close()

	records, err := RebuildSkillRecords(srv.URL, "s", 90, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{}, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Line != "local-skill" || records[0].Meta["skill_owner"] != "local" {
		t.Fatalf("got %+v", records)
	}
}

func TestRebuildSkillRecords_SplitsSaturatedWindow(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		start := r.URL.Query().Get("start")
		end := r.URL.Query().Get("end")
		startNs, _ := strconv.ParseInt(start, 10, 64)
		endNs, _ := strconv.ParseInt(end, 10, 64)
		// The first call spans a full day (way over 60s) — return exactly
		// 5000 lines to force a split. Recursive halves return few lines.
		if endNs-startNs > 60_000*1e6 {
			values := make([]string, 5000)
			for i := range values {
				values[i] = fmt.Sprintf(`["%d","x"]`, startNs+int64(i))
			}
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[
				{"stream":{"session_id":"s1","request_id":"r-full","skill_name":"local"},"values":[%s]}
			]}}`, strings.Join(values, ","))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"session_id":"s1","request_id":"r-half","skill_name":"local"},"values":[["` + start + `","x"]]}
		]}}`))
	}))
	defer srv.Close()

	_, err := RebuildSkillRecords(srv.URL, "s", 1, map[string]map[string]bool{}, map[string]state.PluginSkillRequest{}, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if calls < 3 {
		t.Errorf("got %d Loki calls, want the saturated window to have split into at least 2 sub-windows (3+ total calls)", calls)
	}
}

func TestPushToLoki_SplitsIntoBatchSizeChunks(t *testing.T) {
	var pushCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pushCalls, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	records := make([]Record, 5)
	for i := range records {
		records[i] = Record{TimestampNs: strconv.Itoa(1700000000000000000 + i), Line: "x", Meta: map[string]string{"tool_use_id": strconv.Itoa(i)}}
	}
	pushed, skipped, err := PushToLoki(srv.URL, 2, map[string]string{"service_name": "s", "kind": "tools"}, records, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 5 || len(skipped) != 0 {
		t.Fatalf("pushed=%d skipped=%d", len(pushed), len(skipped))
	}
	if pushCalls != 3 { // ceil(5/2)
		t.Errorf("push calls = %d, want 3", pushCalls)
	}
}

func TestPushToLoki_TooFarBehindGoesToSkippedNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("entry too far behind"))
	}))
	defer srv.Close()

	records := []Record{{TimestampNs: "1700000000000000000", Line: "x", Meta: map[string]string{"tool_use_id": "a"}}}
	pushed, skipped, err := PushToLoki(srv.URL, 100, map[string]string{"service_name": "s"}, records, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || len(pushed) != 0 {
		t.Fatalf("pushed=%d skipped=%d", len(pushed), len(skipped))
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (not retried)", attempts)
	}
}

func TestPushToLoki_OtherFourHundredAbortsImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("nope"))
	}))
	defer srv.Close()

	records := []Record{{TimestampNs: "1700000000000000000", Line: "x", Meta: map[string]string{"tool_use_id": "a"}}}
	_, _, err := PushToLoki(srv.URL, 100, map[string]string{"service_name": "s"}, records, t.Logf)
	if err == nil {
		t.Error("want error for a non-retryable, non-too-old 4xx")
	}
}

func TestPushToLoki_RetriesOnServerErrorThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	records := []Record{{TimestampNs: "1700000000000000000", Line: "x", Meta: map[string]string{"tool_use_id": "a"}}}
	pushed, _, err := PushToLoki(srv.URL, 100, map[string]string{"service_name": "s"}, records, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 1 {
		t.Fatalf("pushed=%d, want 1 after the retry succeeded", len(pushed))
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want a retry to have happened", attempts)
	}
}

func TestPushToLoki_RetriesOn429LikeServerError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	records := []Record{{TimestampNs: "1700000000000000000", Line: "x", Meta: map[string]string{"tool_use_id": "a"}}}
	pushed, _, err := PushToLoki(srv.URL, 100, map[string]string{"service_name": "s"}, records, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 1 {
		t.Errorf("pushed=%d, want 1 (429 retried like a 5xx, not aborted)", len(pushed))
	}
}

func TestPushToLoki_GroupsByExplicitStreamLabelsWhenSet(t *testing.T) {
	var receivedStreams int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		streams, _ := body["streams"].([]interface{})
		receivedStreams = len(streams)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	records := []Record{
		{TimestampNs: "1700000000000000000", Line: "x", Meta: map[string]string{"tool_use_id": "a"}}, // default (tools) labels
		{TimestampNs: "1700000000000000001", Line: "y", DedupKey: "skill:r1",
			StreamLabels: map[string]string{"service_name": "s", "kind": "skills"}, Meta: map[string]string{}},
	}
	_, _, err := PushToLoki(srv.URL, 100, map[string]string{"service_name": "s", "kind": "tools"}, records, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if receivedStreams != 2 {
		t.Errorf("got %d streams in the push payload, want 2 (tools and skills grouped separately)", receivedStreams)
	}
}

func TestPushToLoki_EmptyRecordsIsNoop(t *testing.T) {
	pushed, skipped, err := PushToLoki("http://unused.invalid", 100, nil, nil, t.Logf)
	if err != nil || len(pushed) != 0 || len(skipped) != 0 {
		t.Errorf("pushed=%v skipped=%v err=%v", pushed, skipped, err)
	}
}

func TestSeedSeenFromLoki_PopulatesFromToolUseIDLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"tool_use_id":"tu1"},"values":[["1700000000000000000","x"]]}
		]}}`))
	}))
	defer srv.Close()

	seen := map[string]int64{}
	SeedSeenFromLoki(srv.URL, "claude-code-exporter-1", 1, seen, t.Logf)
	if seen["tu1"] == 0 {
		t.Errorf("seen = %v, want tu1 populated", seen)
	}
}

func TestSeedSeenFromLoki_ContinuesPastAFailedDay(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"tool_use_id":"tu2"},"values":[["1700000000000000000","x"]]}
		]}}`))
	}))
	defer srv.Close()

	seen := map[string]int64{}
	SeedSeenFromLoki(srv.URL, "s", 2, seen, t.Logf)
	if seen["tu2"] == 0 {
		t.Error("a failed day must not stop the remaining days from seeding")
	}
}

func TestRunPass_EndToEndSettlesAndPushes(t *testing.T) {
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects")
	must(t, os.MkdirAll(projectsDir, 0o755))
	writeJSONL(t, filepath.Join(projectsDir, "s.jsonl"), []string{
		`{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":"2026-01-01T00:00:00Z","message":{"model":"claude","content":[{"type":"tool_use","id":"tu1","name":"Read"}]}}`,
		`{"type":"assistant","sessionId":"s1","requestId":"r2","timestamp":"2026-01-01T00:01:00Z","message":{"model":"claude","usage":{"input_tokens":50},"content":[]}}`,
	})

	var pushed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/push" {
			pushed = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	statePath := filepath.Join(root, "state.json")
	cfg := Config{
		ConfigDirs: []string{projectsDir}, LokiURL: srv.URL, ExporterStream: "claude-code-exporter-1",
		BatchSize: 2000, OrphanAfterMs: 15 * 60 * 1000, DedupDays: 90, EmailLookbackHours: 720,
		StatePath: statePath, Log: t.Logf,
	}
	st, err := state.Load(statePath)
	must(t, err)

	n, err := RunPass(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pushed count = %d, want 1", n)
	}
	if !pushed {
		t.Error("Loki push endpoint was never called")
	}
	if len(st.Files) != 1 {
		t.Errorf("state.Files = %v, want the transcript's offset recorded", st.Files)
	}
}

func TestRunPass_OrphanGroupSettlesWithZeroTokensAfterTimeout(t *testing.T) {
	root := t.TempDir()
	projectsDir := filepath.Join(root, "projects")
	must(t, os.MkdirAll(projectsDir, 0o755))
	oldTimestamp := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	writeJSONL(t, filepath.Join(projectsDir, "s.jsonl"), []string{
		fmt.Sprintf(`{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":%q,"message":{"model":"claude","content":[{"type":"tool_use","id":"tu1","name":"Read"}]}}`, oldTimestamp),
	})

	var pushedTokens string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/push" {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			streams := body["streams"].([]interface{})
			s0 := streams[0].(map[string]interface{})
			values := s0["values"].([]interface{})
			v0 := values[0].([]interface{})
			meta := v0[2].(map[string]interface{})
			pushedTokens = meta["tokens_attributed"].(string)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	cfg := Config{
		ConfigDirs: []string{projectsDir}, LokiURL: srv.URL, ExporterStream: "s",
		BatchSize: 2000, OrphanAfterMs: 1000, DedupDays: 90, EmailLookbackHours: 720, // 1s orphan timeout, easily tripped by the 1h-old fixture
		StatePath: filepath.Join(root, "state.json"), Log: t.Logf,
	}
	st, err := state.Load(cfg.StatePath)
	must(t, err)
	if _, err := RunPass(cfg, st); err != nil {
		t.Fatal(err)
	}
	if pushedTokens != "0" {
		t.Errorf("orphaned group's tokens_attributed = %q, want \"0\"", pushedTokens)
	}
}

func TestInitState_RescanZeroesFilesButKeepsDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	seeded := &state.State{
		Version: 2,
		Files:   map[string]*state.FileState{"/old.jsonl": {Offset: 999}},
		Seen:    map[string]int64{"tu1": 1},
	}
	must(t, state.Save(path, seeded))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	st, err := InitState(Config{StatePath: path, LokiURL: srv.URL, ExporterStream: "s", DedupDays: 90, Rescan: true, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Files) != 0 {
		t.Errorf("Files = %v, want zeroed by --rescan", st.Files)
	}
	if st.Seen["tu1"] != 1 {
		t.Error("--rescan must keep the dedup map")
	}
}

func TestInitState_SeedsWhenSeenIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var seedQueried bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seedQueried = true
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	_, err := InitState(Config{StatePath: path, LokiURL: srv.URL, ExporterStream: "s", DedupDays: 1, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if !seedQueried {
		t.Error("a fresh state (empty Seen) must trigger seedSeenFromLoki")
	}
}
