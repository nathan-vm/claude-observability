// Package transcriptscan ports transcript-scan.mjs: exports Claude Code
// tool calls from local transcripts into Loki, attributing tokens and
// recovering the real MCP/skill/bash-command names OTel redacts.
package transcriptscan

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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

type pushPayload struct {
	Streams []pushStreamEntry `json:"streams"`
}
type pushStreamEntry struct {
	Stream map[string]string `json:"stream"`
	Values []pushValueEntry  `json:"values"`
}
type pushValueEntry [3]interface{}

// PushToLoki sorts records by timestamp (Loki rejects too-far-out-of-order
// writes within a stream, and the initial backfill interleaves many files
// in time), chunks by batchSize, and pushes each chunk — grouped by its
// exact stream labels, since tool and skill records can't share one push.
// Each chunk retries up to 5 times with linear backoff to tolerate Loki's
// cold-start "empty ring" error. A 400 containing "too far behind" is NOT
// retried (that chunk can never succeed later either) — those records go
// to skipped, not pushed, so the caller's dedup doesn't lock them out once
// the blocking window ages out. A 429 gets the same retry/backoff as a
// 5xx (seen during full reimports outrunning per-user ingest limits). Any
// other 4xx aborts immediately. A fixed 300ms pace gap follows every
// successful non-final chunk.
func PushToLoki(lokiURL string, batchSize int, defaultLabels map[string]string, records []Record, log func(string, ...any)) (pushed, skipped []Record, err error) {
	if len(records) == 0 {
		return nil, nil, nil
	}
	ordered := append([]Record{}, records...)
	sort.Slice(ordered, func(i, j int) bool {
		a, _ := strconv.ParseInt(ordered[i].TimestampNs, 10, 64)
		b, _ := strconv.ParseInt(ordered[j].TimestampNs, 10, 64)
		return a < b
	})

	pushURL, urlErr := url.Parse(lokiURL)
	if urlErr != nil {
		return nil, nil, fmt.Errorf("invalid loki url %q: %w", lokiURL, urlErr)
	}
	pushURL.Path = "/loki/api/v1/push"

	for i := 0; i < len(ordered); i += batchSize {
		end := i + batchSize
		if end > len(ordered) {
			end = len(ordered)
		}
		chunk := ordered[i:end]

		byLabelsKey := map[string]*pushStreamEntry{}
		var order []string
		for _, r := range chunk {
			labels := r.StreamLabels
			if labels == nil {
				labels = defaultLabels
			}
			key := labelsKey(labels)
			entry, ok := byLabelsKey[key]
			if !ok {
				entry = &pushStreamEntry{Stream: labels}
				byLabelsKey[key] = entry
				order = append(order, key)
			}
			entry.Values = append(entry.Values, pushValueEntry{r.TimestampNs, r.Line, r.Meta})
		}
		payload := pushPayload{}
		for _, key := range order {
			payload.Streams = append(payload.Streams, *byLabelsKey[key])
		}
		body, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return nil, nil, marshalErr
		}

		var lastErr error
		tooOld := false
		for attempt := 0; attempt < 5; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(2000*attempt) * time.Millisecond)
			}
			resp, reqErr := http.Post(pushURL.String(), "application/json", bytes.NewReader(body))
			if reqErr != nil {
				lastErr = fmt.Errorf("push failed: %v", reqErr)
				continue
			}
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				lastErr = nil
				break
			}
			lastErr = fmt.Errorf("push failed %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
			if resp.StatusCode == 400 && strings.Contains(strings.ToLower(string(respBody)), "too far behind") {
				tooOld = true
				break
			}
			if resp.StatusCode != 429 && resp.StatusCode >= 400 && resp.StatusCode < 500 {
				break
			}
		}

		if !tooOld && lastErr == nil && end < len(ordered) {
			time.Sleep(300 * time.Millisecond)
		}
		if tooOld {
			log("%d record(s) rejected as too old by Loki, skipped: %v", len(chunk), lastErr)
			skipped = append(skipped, chunk...)
			continue
		}
		if lastErr != nil {
			return pushed, skipped, lastErr
		}
		pushed = append(pushed, chunk...)
	}
	return pushed, skipped, nil
}

func labelsKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(';')
	}
	return b.String()
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Config bundles everything RunPass/InitState need. ConfigDirs must
// already have "/projects" appended by the caller.
type Config struct {
	ConfigDirs         []string
	LokiURL            string
	ExporterStream     string
	BatchSize          int
	OrphanAfterMs      int64
	DedupDays          int
	EmailLookbackHours int
	DryRun             bool
	Rescan             bool
	StatePath          string
	Log                func(string, ...any)
}

func (cfg Config) defaultLabels() map[string]string {
	return map[string]string{"service_name": cfg.ExporterStream, "kind": "tools"}
}

// SeedSeenFromLoki rebuilds seen from what's already in Loki — runs when
// the map is empty (an upgrade, a lost state volume, or a deleted file).
// Scanned one day at a time to stay under Loki's per-query line limit,
// continuing past a single failed day rather than aborting the whole seed.
func SeedSeenFromLoki(lokiURL, exporterStream string, dedupDays int, seen map[string]int64, log func(string, ...any)) {
	const step = 24 * 3600 * 1_000_000_000 // 1 day, ns
	total := 0
	windowTotalNs := int64(dedupDays) * 24 * 3600 * 1_000_000_000
	nowNs := time.Now().UnixNano()
	query := fmt.Sprintf(`{service_name="%s"}`, exporterStream)
	for offset := int64(0); offset < windowTotalNs; offset += step {
		end := nowNs - offset
		start := end - step
		results, err := lokiclient.QueryRange(lokiURL, query, start, end, 5000, "backward")
		if err != nil {
			log("seed window failed (%v); continuing to the next", err)
			continue
		}
		for _, r := range results {
			id := r.Labels["tool_use_id"]
			if id == "" {
				continue
			}
			for _, v := range r.Values {
				ns, _ := strconv.ParseInt(v[0], 10, 64)
				seen[id] = ns / 1e6
				total++
			}
		}
	}
	if total > 0 {
		log("dedup seeded with %d call(s) already in Loki", len(seen))
	}
}

// InitState loads state (or starts fresh), honors --rescan (zeros
// state.Files only — keeps the dedup/plugin-skill maps, a non-destructive
// re-read), and seeds state.Seen from Loki when it's empty.
func InitState(cfg Config) (*state.State, error) {
	cfg.Log("transcripts: %s", strings.Join(cfg.ConfigDirs, ", "))
	if cfg.DryRun {
		cfg.Log("loki: %s  (dry-run)", cfg.LokiURL)
	} else {
		cfg.Log("loki: %s", cfg.LokiURL)
	}
	st, err := state.Load(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	if cfg.Rescan {
		cfg.Log("RESCAN: zeroing offsets, keeping the dedup map")
		st.Files = map[string]*state.FileState{}
	}
	if len(st.Files) == 0 {
		cfg.Log("first run: importing the full transcript history")
	}
	if len(st.Seen) == 0 {
		SeedSeenFromLoki(cfg.LokiURL, cfg.ExporterStream, cfg.DedupDays, st.Seen, cfg.Log)
	}
	return st, nil
}

// RunPass does one full pass: scan every configured directory's new
// transcript lines, settle/orphan groups, rebuild skill records from
// OTel, dedup, resolve emails, push to Loki, persist state. Returns the
// count of records actually pushed.
func RunPass(cfg Config, st *state.State) (int, error) {
	firstPass := len(st.Files) == 0
	files, err := FindTranscripts(cfg.ConfigDirs, cfg.Log)
	if err != nil {
		return 0, err
	}

	type update struct {
		fs state.FileState
	}
	updates := map[string]update{}

	pluginSkills := map[string]map[string]bool{}
	for sessionID, names := range st.PluginSkills {
		set := map[string]bool{}
		for _, n := range names {
			set[n] = true
		}
		pluginSkills[sessionID] = set
	}
	pluginSkillByRequest := map[string]state.PluginSkillRequest{}
	cutoffMs := time.Now().AddDate(0, 0, -cfg.DedupDays).UnixMilli()
	for reqID, info := range st.PluginSkillByRequest {
		if info.TsMs >= cutoffMs {
			pluginSkillByRequest[reqID] = info
		}
	}

	var collected []Record
	for _, path := range files {
		fs := state.FileState{}
		if existing, ok := st.Files[path]; ok && existing != nil {
			fs = *existing
		}
		records, newFs, err := ProcessTranscript(path, fs, pluginSkills, pluginSkillByRequest)
		if err != nil {
			cfg.Log("failed reading %s: %v", filepath.Base(path), err)
			continue
		}

		for _, track := range []string{"main", "sub"} {
			var group **state.Group
			if track == "main" {
				group = &newFs.PendingMain
			} else {
				group = &newFs.PendingSub
			}
			if *group != nil && time.Now().UnixMilli()-(*group).AtMs > cfg.OrphanAfterMs {
				records = append(records, Settle(**group, 0)...)
				*group = nil
			}
		}

		collected = append(collected, records...)
		updates[path] = update{fs: newFs}
	}

	skillRecords, err := RebuildSkillRecords(cfg.LokiURL, cfg.ExporterStream, cfg.DedupDays, pluginSkills, pluginSkillByRequest, firstPass, cfg.Log)
	if err != nil {
		return 0, err
	}
	collected = append(collected, skillRecords...)

	fresh, novos := DropAlreadySeen(st.Seen, cfg.DedupDays, collected)
	if skipped := len(collected) - len(fresh); skipped > 0 {
		cfg.Log("%d call(s) skipped: already exported (resumed session)", skipped)
	}

	commit := func() {
		for path, u := range updates {
			fs := u.fs
			st.Files[path] = &fs
		}
		for id, ts := range novos {
			st.Seen[id] = ts
		}
		for sessionID, set := range pluginSkills {
			names := make([]string, 0, len(set))
			for n := range set {
				names = append(names, n)
			}
			st.PluginSkills[sessionID] = names
		}
		st.PluginSkillByRequest = pluginSkillByRequest
	}

	if len(fresh) == 0 {
		if len(collected) > 0 {
			commit()
			if !cfg.DryRun {
				if err := state.Save(cfg.StatePath, st); err != nil {
					return 0, err
				}
			}
		}
		return 0, nil
	}
	collected = fresh

	sessionIDs := map[string]bool{}
	for _, r := range collected {
		if id := r.Meta["session_id"]; id != "" {
			sessionIDs[id] = true
		}
	}
	ids := make([]string, 0, len(sessionIDs))
	for id := range sessionIDs {
		ids = append(ids, id)
	}
	ResolveEmails(cfg.LokiURL, cfg.EmailLookbackHours, st.SessionEmail, ids, cfg.Log)
	for i := range collected {
		if collected[i].Meta["user_email"] != "" {
			continue
		}
		email := st.SessionEmail[collected[i].Meta["session_id"]]
		collected[i].Meta["user_email"] = email
		if email != "" {
			collected[i].Meta["account_source"] = "otel"
		} else {
			collected[i].Meta["account_source"] = ""
		}
	}

	needsProject := false
	for _, r := range collected {
		if r.Meta["user_email"] == "" {
			needsProject = true
			break
		}
	}
	if needsProject {
		owners, err := ProjectOwners(cfg.LokiURL, cfg.ExporterStream, cfg.DedupDays, collected)
		if err != nil {
			return 0, err
		}
		for i := range collected {
			if collected[i].Meta["user_email"] != "" {
				continue
			}
			if owner, ok := owners[collected[i].Meta["project"]]; ok {
				collected[i].Meta["user_email"] = owner
				collected[i].Meta["account_source"] = "project"
			}
		}
	}

	if cfg.DryRun {
		commit()
		return 0, nil
	}

	pushedRecords, rejected, err := PushToLoki(cfg.LokiURL, cfg.BatchSize, cfg.defaultLabels(), collected, cfg.Log)
	if err != nil {
		return 0, err
	}
	for _, r := range rejected {
		id := r.DedupKey
		if id == "" {
			id = r.Meta["tool_use_id"]
		}
		delete(novos, id)
	}
	commit()
	if err := state.Save(cfg.StatePath, st); err != nil {
		return 0, err
	}
	return len(pushedRecords), nil
}
