// Package transcriptscan ports transcript-scan.mjs: exports Claude Code
// tool calls from local transcripts into Loki, attributing tokens and
// recovering the real MCP/skill/bash-command names OTel redacts.
package transcriptscan

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"claude-observability-collector/internal/lokiclient"
	"claude-observability-collector/internal/shelltok"
	"claude-observability-collector/internal/state"
)

// Record is one line ready to push to Loki — either a tool record
// (StreamLabels nil, caller applies the default tools-stream labels) or a
// skill record (StreamLabels set explicitly to the skills stream).
type Record struct {
	TimestampNs  string
	Line         string
	DedupKey     string
	StreamLabels map[string]string
	Meta         map[string]string
}

// FindTranscripts walks every configDir recursively (depth <= 6),
// collecting every *.jsonl file — including "<session>/subagents/*.jsonl"
// one level deeper than session files. A missing configDir is logged and
// skipped, not fatal (matches collector-old/transcript-scan.mjs:176-178).
func FindTranscripts(configDirs []string, log func(string, ...any)) ([]string, error) {
	var all []string
	for _, dir := range configDirs {
		found, err := findTranscriptsRec(dir, 0, log)
		if err != nil {
			return nil, err
		}
		all = append(all, found...)
	}
	return all, nil
}

func findTranscriptsRec(dir string, depth int, log func(string, ...any)) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if depth == 0 {
				log("transcript directory does not exist: %s", dir)
			}
			return nil, nil
		}
		return nil, err
	}
	var found []string
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if depth < 6 {
				sub, err := findTranscriptsRec(full, depth+1, log)
				if err != nil {
					return nil, err
				}
				found = append(found, sub...)
			}
		} else if strings.HasSuffix(e.Name(), ".jsonl") {
			found = append(found, full)
		}
	}
	return found, nil
}

// ParseToolName splits only on the FIRST "__" after "mcp__" (the tool
// name itself may legitimately contain "__").
func ParseToolName(name string) (source, server, tool string) {
	if !strings.HasPrefix(name, "mcp__") {
		return "builtin", "", ""
	}
	rest := name[len("mcp__"):]
	sep := strings.Index(rest, "__")
	if sep < 0 {
		return "mcp", rest, ""
	}
	return "mcp", rest[:sep], rest[sep+2:]
}

// BlockSize measures a tool_result content block the same way the JS
// does: string content by length, array content by summing string parts'
// length / {type:"text"} parts' text length / JSON-encoded length of
// anything else, and anything else by JSON-encoded length. Uses Go's
// byte length, not JS's UTF-16 code-unit length — see Global Constraints.
func BlockSize(block map[string]interface{}) int {
	content, ok := block["content"]
	if !ok || content == nil {
		encoded, _ := json.Marshal("")
		return len(encoded)
	}
	switch c := content.(type) {
	case string:
		return len(c)
	case []interface{}:
		sum := 0
		for _, part := range c {
			switch p := part.(type) {
			case string:
				sum += len(p)
			case map[string]interface{}:
				if p["type"] == "text" {
					if text, ok := p["text"].(string); ok {
						sum += len(text)
					}
					continue
				}
				encoded, _ := json.Marshal(p)
				sum += len(encoded)
			default:
				encoded, _ := json.Marshal(p)
				sum += len(encoded)
			}
		}
		return sum
	default:
		encoded, _ := json.Marshal(content)
		return len(encoded)
	}
}

func toTimestampNs(timestamp string) string {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "0000000000000"
	}
	return strconv.FormatInt(t.UnixMilli(), 10) + "000000"
}

func parseTimestampMs(timestamp string) int64 {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// attribute computes one call's share of the group's input tokens, and
// the meta fields every record for that call shares regardless of how
// many rows it fans out into (mcp_server/mcp_tool are added by the
// caller, per-row, since a Bash fanout gives each row a different one).
func attribute(call state.Call, group state.Group, inputTokens, total int64) map[string]string {
	var tokensAttributed int64
	if total > 0 {
		tokensAttributed = int64(math.Round(float64(inputTokens) * float64(call.ResultBytes) / float64(total)))
	} else if len(group.Calls) > 0 {
		tokensAttributed = int64(math.Round(float64(inputTokens) / float64(len(group.Calls))))
	}
	querySource := "main"
	if group.IsSidechain {
		querySource = "subagent"
	}
	return map[string]string{
		"session_id":        group.SessionID,
		"request_id":        group.RequestID,
		"tool_use_id":       call.ToolUseID,
		"tool_name":         call.ToolName,
		"tool_source":       call.ToolSource,
		"model":             group.Model,
		"effort":            group.Effort,
		"query_source":      querySource,
		"git_branch":        group.GitBranch,
		"project":           group.Project,
		"result_bytes":      strconv.Itoa(call.ResultBytes),
		"tokens_attributed": strconv.FormatInt(tokensAttributed, 10),
	}
}

// Settle attributes the group's input tokens to its tool calls,
// proportionally to each one's result size (or evenly if every result was
// empty). A Bash call with recognized sub-commands fans out into one row
// per sub-command, each reusing the SAME tokens_attributed/result_bytes —
// token cost belongs to the LLM turn, not a proportional split within it.
func Settle(group state.Group, inputTokens int64) []Record {
	var total int64
	for _, c := range group.Calls {
		total += int64(c.ResultBytes)
	}
	var out []Record
	for _, call := range group.Calls {
		timestampNs := toTimestampNs(call.Timestamp)
		meta := attribute(call, group, inputTokens, total)
		if len(call.BashCommands) == 0 {
			m := cloneMeta(meta)
			m["mcp_server"] = call.MCPServer
			m["mcp_tool"] = call.MCPTool
			out = append(out, Record{TimestampNs: timestampNs, Line: call.ToolName, Meta: m})
			continue
		}
		for i, name := range call.BashCommands {
			m := cloneMeta(meta)
			m["mcp_server"] = "bash"
			m["mcp_tool"] = name
			out = append(out, Record{
				TimestampNs: timestampNs, Line: call.ToolName,
				DedupKey: fmt.Sprintf("bash:%s:%d", call.ToolUseID, i),
				Meta:     m,
			})
		}
	}
	return out
}

func cloneMeta(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func asFloat(v interface{}) float64 {
	f, _ := v.(float64)
	return f
}

// ProcessTranscript reads path starting at fs.Offset, line by line, and
// returns newly-settled records plus the updated FileState. A line's byte
// length is only folded into the consumed count once the NEXT line is
// seen (proving the previous one ended in "\n") — Claude Code writes to
// these files concurrently, so the final line commonly has no trailing
// newline yet, and counting it as consumed would skip the first byte
// written next. If the file shrank since fs.Offset (truncated/replaced),
// everything resets.
func ProcessTranscript(path string, fs state.FileState, pluginSkills map[string]map[string]bool, pluginSkillByRequest map[string]state.PluginSkillRequest) ([]Record, state.FileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, state.FileState{}, err
	}
	size := info.Size()
	offset := fs.Offset
	pendingMain, pendingSub := fs.PendingMain, fs.PendingSub
	activeSkillMain, activeSkillSub := fs.ActiveSkillMain, fs.ActiveSkillSub

	if size < offset {
		offset = 0
		pendingMain, pendingSub = nil, nil
		activeSkillMain, activeSkillSub = "", ""
	}
	if size == offset {
		return nil, state.FileState{
			Offset: offset, PendingMain: pendingMain, PendingSub: pendingSub,
			ActiveSkillMain: activeSkillMain, ActiveSkillSub: activeSkillSub,
		}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, state.FileState{}, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, state.FileState{}, err
	}

	var records []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var consumed, pendente int64

	for scanner.Scan() {
		line := scanner.Text()
		consumed += pendente
		pendente = int64(len(line)) + 1

		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		message, _ := entry["message"].(map[string]interface{})
		if message == nil {
			continue
		}
		content, _ := message["content"].([]interface{})
		isSidechain, _ := entry["isSidechain"].(bool)
		track := "main"
		if isSidechain {
			track = "sub"
		}
		entryType, _ := entry["type"].(string)
		sessionID, _ := entry["sessionId"].(string)
		requestID, _ := entry["requestId"].(string)
		timestamp, _ := entry["timestamp"].(string)

		if entryType == "assistant" {
			usage, hasUsage := message["usage"].(map[string]interface{})
			activeSkill := activeSkillMain
			pending := pendingMain
			if track == "sub" {
				activeSkill = activeSkillSub
				pending = pendingSub
			}

			if sessionID != "" && strings.Contains(activeSkill, ":") {
				set := pluginSkills[sessionID]
				if set == nil {
					set = map[string]bool{}
					pluginSkills[sessionID] = set
				}
				set[activeSkill] = true
				if requestID != "" {
					ts := parseTimestampMs(timestamp)
					if ts == 0 {
						ts = time.Now().UnixMilli()
					}
					pluginSkillByRequest[requestID] = state.PluginSkillRequest{Skill: activeSkill, TsMs: ts}
				}
			}

			if hasUsage && pending != nil {
				inputTokens := int64(asFloat(usage["input_tokens"])) + int64(asFloat(usage["cache_creation_input_tokens"]))
				records = append(records, Settle(*pending, inputTokens)...)
				pending = nil
			}

			var calls []state.Call
			for _, blockRaw := range content {
				block, ok := blockRaw.(map[string]interface{})
				if !ok || block["type"] != "tool_use" {
					continue
				}
				name, _ := block["name"].(string)
				id, _ := block["id"].(string)
				source, server, tool := ParseToolName(name)
				var bashCommands []string
				if name == "Bash" {
					input, _ := block["input"].(map[string]interface{})
					cmd, _ := input["command"].(string)
					bashCommands = shelltok.ExtractBashCommands(cmd)
				}
				calls = append(calls, state.Call{
					ToolUseID: id, ToolName: name, ResultBytes: 0, Timestamp: timestamp,
					BashCommands: bashCommands, ToolSource: source, MCPServer: server, MCPTool: tool,
				})
			}
			for _, blockRaw := range content {
				block, ok := blockRaw.(map[string]interface{})
				if !ok || block["type"] != "tool_use" || block["name"] != "Skill" {
					continue
				}
				input, _ := block["input"].(map[string]interface{})
				if skill, ok := input["skill"].(string); ok && skill != "" {
					activeSkill = skill
				}
			}

			if len(calls) > 0 {
				project := ""
				if cwd, ok := entry["cwd"].(string); ok && cwd != "" {
					project = filepath.Base(cwd)
				}
				effort, _ := entry["perTurnEffort"].(string)
				if effort == "" {
					effort, _ = entry["effort"].(string)
				}
				gitBranch, _ := entry["gitBranch"].(string)
				model, _ := message["model"].(string)
				at := parseTimestampMs(timestamp)
				if at == 0 {
					at = time.Now().UnixMilli()
				}
				pending = &state.Group{
					SessionID: sessionID, RequestID: requestID, Model: model, Effort: effort,
					GitBranch: gitBranch, Project: project, IsSidechain: isSidechain, AtMs: at, Calls: calls,
				}
			}

			if track == "sub" {
				activeSkillSub, pendingSub = activeSkill, pending
			} else {
				activeSkillMain, pendingMain = activeSkill, pending
			}
		} else if entryType == "user" {
			pending := pendingMain
			if track == "sub" {
				pending = pendingSub
			}
			if pending != nil {
				for _, blockRaw := range content {
					block, ok := blockRaw.(map[string]interface{})
					if !ok || block["type"] != "tool_result" {
						continue
					}
					toolUseID, _ := block["tool_use_id"].(string)
					for i := range pending.Calls {
						if pending.Calls[i].ToolUseID == toolUseID {
							pending.Calls[i].ResultBytes = BlockSize(block)
							break
						}
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, state.FileState{}, err
	}

	return records, state.FileState{
		Offset: offset + consumed, PendingMain: pendingMain, PendingSub: pendingSub,
		ActiveSkillMain: activeSkillMain, ActiveSkillSub: activeSkillSub,
	}, nil
}

// ResolveEmails fills sessionEmail (in place) for every sessionID in
// sessionIDs not already cached, by querying the session's own OTel data
// for the most recent user_email it carries. A session with no match is
// cached as "" too — otherwise every pass re-queries sessions that never
// sent OTel telemetry at all.
func ResolveEmails(lokiURL string, emailLookbackHours int, sessionEmail map[string]string, sessionIDs []string, log func(string, ...any)) {
	var missing []string
	for _, id := range sessionIDs {
		if id == "" {
			continue
		}
		if _, ok := sessionEmail[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return
	}
	endNs := time.Now().UnixNano()
	startNs := endNs - int64(emailLookbackHours)*3600*1e9
	for _, sessionID := range missing {
		query := fmt.Sprintf("{service_name=\"claude-code\"} | session_id = `%s` | user_email != ``", sessionID)
		results, err := lokiclient.QueryRange(lokiURL, query, startNs, endNs, 1, "backward")
		if err != nil {
			short := sessionID
			if len(short) > 8 {
				short = short[:8]
			}
			log("could not resolve the account for session %s: %v", short, err)
			continue
		}
		email := ""
		if len(results) > 0 {
			email = results[0].Labels["user_email"]
		}
		sessionEmail[sessionID] = email
	}
}

// ProjectOwners builds a project->email map from both Loki history and
// the current batch (critical for a from-scratch import, where Loki
// history is empty). A project maps to an email only if every observed
// pairing used the same single email — ambiguous projects are left
// unmapped.
func ProjectOwners(lokiURL, exporterStream string, dedupDays int, batch []Record) (map[string]string, error) {
	endNs := time.Now().UnixNano()
	startNs := endNs - int64(dedupDays)*24*3600*1e9
	query := fmt.Sprintf(`{service_name="%s", kind="tools"}`, exporterStream)
	results, err := lokiclient.QueryRange(lokiURL, query, startNs, endNs, 5000, "backward")
	if err != nil {
		results = nil // fall back to batch-only, matching the JS's try/catch
	}

	seen := map[string]map[string]bool{}
	note := func(project, email string) {
		if project == "" || email == "" {
			return
		}
		if seen[project] == nil {
			seen[project] = map[string]bool{}
		}
		seen[project][email] = true
	}
	for _, r := range results {
		note(r.Labels["project"], r.Labels["user_email"])
	}
	for _, record := range batch {
		note(record.Meta["project"], record.Meta["user_email"])
	}

	owners := map[string]string{}
	for project, emails := range seen {
		if len(emails) == 1 {
			for email := range emails {
				owners[project] = email
			}
		}
	}
	return owners, nil
}

// DropAlreadySeen prunes seen entries older than dedupDays, then splits
// records into fresh (not previously exported, and not a duplicate
// within this same batch) and the staged dedup keys (novos) the caller
// commits to seen only after a successful push. A record with neither
// DedupKey nor a tool_use_id in Meta always passes through.
func DropAlreadySeen(seen map[string]int64, dedupDays int, records []Record) (fresh []Record, novos map[string]int64) {
	cutoff := time.Now().Add(-time.Duration(dedupDays) * 24 * time.Hour).UnixMilli()
	for id, ts := range seen {
		if ts < cutoff {
			delete(seen, id)
		}
	}
	novos = map[string]int64{}
	for _, record := range records {
		id := record.DedupKey
		if id == "" {
			id = record.Meta["tool_use_id"]
		}
		if id == "" {
			fresh = append(fresh, record)
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		if _, ok := novos[id]; ok {
			continue
		}
		ts, _ := strconv.ParseInt(record.TimestampNs, 10, 64)
		novos[id] = ts / 1e6
		fresh = append(fresh, record)
	}
	return fresh, novos
}

const skillFetchLimit = 5000

// fetchSkillWindow queries one time window for skill-tagged api_request
// events. Loki caps the response at skillFetchLimit and a saturated
// window comes back truncated in silence — when a window is at/over the
// limit, it's split in half and retried (depth-limited, and never below
// 60s wide, to bound the recursion).
func fetchSkillWindow(lokiURL string, startNs, endNs int64, depth int, log func(string, ...any)) []lokiclient.StreamResult {
	results, err := lokiclient.QueryRange(lokiURL,
		"{service_name=\"claude-code\"} | event_name = `api_request` | skill_name != ``",
		startNs, endNs, skillFetchLimit, "")
	if err != nil {
		log("OTel skills unavailable for that window: %v", err)
		return nil
	}
	total := 0
	for _, r := range results {
		total += len(r.Values)
	}
	if total >= skillFetchLimit && endNs-startNs > 60_000*1_000_000 && depth < 12 {
		mid := (startNs + endNs) / 2
		out := fetchSkillWindow(lokiURL, startNs, mid, depth+1, log)
		out = append(out, fetchSkillWindow(lokiURL, mid, endNs, depth+1, log)...)
		return out
	}
	return results
}

// RebuildSkillRecords re-reads OTel's own api_request events tagged with
// a skill (rather than the transcript) and republishes them with ONE
// adjustment: when OTel redacted the name to "third-party" (any plugin
// skill), the transcript-derived real name is substituted in, using the
// per-request resolution first and falling back to the session-level set
// only when it has exactly one candidate (unambiguous).
func RebuildSkillRecords(lokiURL, exporterStream string, dedupDays int, pluginSkills map[string]map[string]bool, pluginSkillByRequest map[string]state.PluginSkillRequest, fullHistory bool, log func(string, ...any)) ([]Record, error) {
	days := 2
	if fullHistory {
		days = dedupDays
	}
	const step = 24 * 3600 * 1_000_000_000 // 1 day, in nanoseconds
	nowNs := time.Now().UnixNano()

	var out []Record
	for offset := int64(0); offset < int64(days)*step; offset += step {
		end := nowNs - offset
		results := fetchSkillWindow(lokiURL, end-step, end, 0, log)
		for _, r := range results {
			sessionID := r.Labels["session_id"]
			requestID := r.Labels["request_id"]
			skillName := r.Labels["skill_name"]

			var real string
			if req, ok := pluginSkillByRequest[requestID]; ok {
				real = req.Skill
			} else if candidates := pluginSkills[sessionID]; len(candidates) == 1 {
				for s := range candidates {
					real = s
				}
			}
			skill := skillName
			if skillName == "third-party" && real != "" {
				skill = real
			}
			if skill == "" || requestID == "" {
				continue
			}
			owner := "local"
			if idx := strings.Index(skill, ":"); idx >= 0 {
				owner = skill[:idx]
			}

			inputT := r.Labels["input_tokens"]
			outputT := r.Labels["output_tokens"]
			cacheT := r.Labels["cache_creation_tokens"]
			tokens := parseIntOr0(inputT) + parseIntOr0(outputT) + parseIntOr0(cacheT)

			for _, v := range r.Values {
				out = append(out, Record{
					TimestampNs:  v[0],
					Line:         skill,
					DedupKey:     "skill:" + requestID,
					StreamLabels: map[string]string{"service_name": exporterStream, "kind": "skills"},
					Meta: map[string]string{
						"skill":          skill,
						"skill_owner":    owner,
						"session_id":     sessionID,
						"request_id":     requestID,
						"model":          r.Labels["model"],
						"effort":         r.Labels["effort"],
						"query_source":   r.Labels["query_source"],
						"project":        "",
						"tokens":         strconv.FormatInt(tokens, 10),
						"user_email":     r.Labels["user_email"],
						"account_source": "otel",
					},
				})
			}
		}
	}
	return out, nil
}

func parseIntOr0(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
