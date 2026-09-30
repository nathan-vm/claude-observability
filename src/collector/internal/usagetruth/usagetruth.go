// Package usagetruth captures the real numbers behind Anthropic's 5h/week
// limits by running `/usage` via the `claude` CLI itself, per account, and
// publishing them into Loki. Simplified port of usage-truth.mjs — no
// calibration (its data source died with usage-meter.mjs).
package usagetruth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"claude-observability-collector/internal/lokiclient"
)

const claudeTimeout = 30 * time.Second

// emptyMCPConfig is the literal file content passed to `claude` via
// --mcp-config, paired with --strict-mcp-config, so FetchUsage's headless
// `/usage` probe boots zero MCP servers instead of the user's full
// configured set (including Docker-based ones whose container survives a
// timed-out `claude` process — see runClaudeWithTimeout for the second half
// of that fix).
const emptyMCPConfig = `{"mcpServers":{}}`

var (
	mcpConfigMu  sync.Mutex
	mcpConfigDir string
)

// writeEmptyMCPConfigFile writes emptyMCPConfig to a fixed filename inside
// a private, mode-0700, randomly named directory (via os.MkdirTemp) and
// returns that path. The random directory name is what defeats a local
// user pre-creating a symlink at a predictable path — os.WriteFile follows
// symlinks — and the 0700 mode locks out other users entirely, without
// resorting to a platform-specific O_NOFOLLOW open; the directory itself
// is deliberately left behind on process exit (the collector has no
// shutdown hook to remove it, and one leaked directory per process start
// is a cost worth paying to keep the path unpredictable) — one directory
// per process lifetime is the accumulation this is meant to bound, not
// zero.
//
// Self-healing, not sync.Once-cached: this runs inside a `for { pass();
// sleep }` loop with no process restart, so a one-time cache would latch
// forever onto whatever it first saw. Concretely: (a) systemd-tmpfiles-clean
// (or equivalent) removes stale /tmp entries after ~10 days on several
// distros, which would leave every later call pointing at a missing
// directory, and claude would fail --mcp-config on every poll from then
// on; (b) a transient MkdirTemp/WriteFile failure would latch that error
// forever instead of retrying once the condition clears. Recreating the
// directory (and rewriting the 25-byte file) on every call that finds it
// gone costs nothing and never leaves a permanent failure mode.
//
// The staleness check uses os.Lstat, not os.Stat, and requires a real
// directory: os.Stat follows symlinks, and the directory's name is no
// longer secret once tmpfiles-clean has removed it once — a co-resident
// local user could plant a symlink at that exact path pointing wherever
// they want written, and a Stat-based check would resolve through it and
// os.WriteFile straight into their target. Lstat sees the symlink itself,
// fails the IsDir check, and forces a fresh MkdirTemp with a new,
// unguessed random name instead.
func writeEmptyMCPConfigFile() (string, error) {
	mcpConfigMu.Lock()
	defer mcpConfigMu.Unlock()

	if mcpConfigDir != "" {
		if fi, err := os.Lstat(mcpConfigDir); err != nil || !fi.IsDir() {
			// Gone underneath us (tmpfiles-clean or similar), or replaced
			// by something that isn't a real directory (e.g. a planted
			// symlink) — drop the stale reference and fall through to
			// recreate it below. RemoveAll's error is deliberately
			// discarded: if removal fails, this directory is simply
			// abandoned (one extra /tmp entry) and we proceed with a
			// fresh one — staying available matters more here than
			// tidying up, and the package has no logger to report it
			// through.
			_ = os.RemoveAll(mcpConfigDir)
			mcpConfigDir = ""
		}
	}
	if mcpConfigDir == "" {
		dir, err := os.MkdirTemp("", "claude-observability-mcp-config-*")
		if err != nil {
			return "", fmt.Errorf("creating empty MCP config dir: %w", err)
		}
		mcpConfigDir = dir
	}

	path := filepath.Join(mcpConfigDir, "empty-mcp-config.json")
	if err := os.WriteFile(path, []byte(emptyMCPConfig), 0o600); err != nil {
		return "", fmt.Errorf("writing empty MCP config: %w", err)
	}
	return path, nil
}

// Usage is one account's current session/week percentages and Anthropic's
// own "resets at" text for each (empty when the window reads 0% used —
// no open block carries no expiry to report).
type Usage struct {
	SessionPct   int
	SessionReset string
	WeekPct      int
	WeekReset    string
}

func runClaude(configDir string, args ...string) ([]byte, error) {
	return runClaudeWithTimeout(configDir, claudeTimeout, args...)
}

// runClaudeWithTimeout is runClaude with an injectable timeout — the seam
// TestRunClaudeWithTimeout_KillsGrandchildProcessGroup uses to exercise the
// process-group-kill path in well under a second instead of the real 30s
// claudeTimeout. Production code only ever reaches this through runClaude,
// which always passes the untouched claudeTimeout constant, so this
// refactor changes no production timeout behavior.
func runClaudeWithTimeout(configDir string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+configDir)

	// exec.CommandContext's default Cancel kills only the direct `claude`
	// child; a grandchild it spawned (e.g. a Docker-based MCP server via
	// `docker run`) reparents to PID 1 and keeps running. Starting claude
	// in its own process group and overriding Cancel to kill that whole
	// group closes that gap. WaitDelay bounds how long Wait() can be stuck
	// after Cancel runs, in case a grandchild still holds an inherited
	// stdout/stderr pipe fd open.
	setNewProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// AccountEmail runs `claude auth status` with CLAUDE_CONFIG_DIR=configDir
// and returns the logged-in email, or "" if not logged in.
func AccountEmail(configDir string) (string, error) {
	out, err := runClaude(configDir, "auth", "status")
	if err != nil {
		return "", err
	}
	var status struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", fmt.Errorf("auth status decode: %w", err)
	}
	if !status.LoggedIn {
		return "", nil
	}
	return status.Email, nil
}

var sessionRe = regexp.MustCompile(`(?i)Current session:\s*(\d+)%\s*used(?:\s*·\s*(.+))?`)
var weekRe = regexp.MustCompile(`(?i)Current week[^:]*:\s*(\d+)%\s*used(?:\s*·\s*(.+))?`)

// weekResetRe matches the "<Mon> <D> at <H>[:MM](am|pm) (<IANA tz>)" shape
// found anywhere inside Usage.WeekReset's free text (e.g. "resets Sep 24 at
// 7:59pm (America/Sao_Paulo)" or "resets Oct 3 at 2pm (America/Sao_Paulo)")
// — minutes are optional, as seen in live data.
var weekResetRe = regexp.MustCompile(`(?i)([A-Za-z]{3,9})\s+(\d{1,2})\s+at\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(([^)]+)\)`)

func titleCaseMonth(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// parseWeekReset parses Usage.WeekReset's free-text reset clause into an
// absolute UTC instant. The year isn't in the text, so it's inferred as
// now.Year(), rolling forward to now.Year()+1 when the parsed date would
// otherwise land more than ~2 days in now's past — the December->January
// boundary, where "resets Jan 2" parsed in late December must mean next
// year. Returns ok=false on any parse failure or unrecognized IANA zone;
// callers must not publish a zero/garbage value in that case.
func parseWeekReset(text string, now time.Time) (time.Time, bool) {
	m := weekResetRe.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	monthStr, dayStr, hourStr, minStr, ampm, tzName := m[1], m[2], m[3], m[4], m[5], m[6]

	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return time.Time{}, false
	}
	monthTime, err := time.Parse("Jan", titleCaseMonth(monthStr))
	if err != nil {
		return time.Time{}, false
	}
	day, err := strconv.Atoi(dayStr)
	if err != nil {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(hourStr)
	if err != nil {
		return time.Time{}, false
	}
	minute := 0
	if minStr != "" {
		minute, err = strconv.Atoi(minStr)
		if err != nil {
			return time.Time{}, false
		}
	}
	switch strings.ToLower(ampm) {
	case "pm":
		if hour != 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}

	candidate := time.Date(now.Year(), monthTime.Month(), day, hour, minute, 0, 0, loc)
	if candidate.Before(now.Add(-48 * time.Hour)) {
		candidate = time.Date(now.Year()+1, monthTime.Month(), day, hour, minute, 0, 0, loc)
	}
	return candidate.UTC(), true
}

func parseUsage(text string) (Usage, error) {
	session := sessionRe.FindStringSubmatch(text)
	week := weekRe.FindStringSubmatch(text)
	if session == nil || week == nil {
		short := text
		if len(short) > 200 {
			short = short[:200]
		}
		return Usage{}, fmt.Errorf("/usage output not in the expected shape: %s", short)
	}
	sessionPct, _ := strconv.Atoi(session[1])
	weekPct, _ := strconv.Atoi(week[1])
	return Usage{
		SessionPct: sessionPct, SessionReset: strings.TrimSpace(session[2]),
		WeekPct: weekPct, WeekReset: strings.TrimSpace(week[2]),
	}, nil
}

// FetchUsage runs `claude -p /usage --output-format json
// --no-session-persistence` and parses the two top-line percentages/reset
// clauses Anthropic always leads with. Only these two numbers are used —
// the rest of /usage's output is explicitly scoped by Anthropic to "local
// sessions on this machine".
func FetchUsage(configDir string) (Usage, error) {
	configPath, err := writeEmptyMCPConfigFile()
	if err != nil {
		return Usage{}, err
	}
	// Flag order matters: --mcp-config is variadic and greedily consumes
	// following bare words, so -p must come immediately after the path.
	out, err := runClaude(configDir,
		"--strict-mcp-config", "--mcp-config", configPath,
		"-p", "/usage", "--output-format", "json", "--no-session-persistence")
	if err != nil {
		return Usage{}, err
	}
	var result struct {
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return Usage{}, fmt.Errorf("claude -p /usage decode: %w", err)
	}
	if result.IsError {
		return Usage{}, fmt.Errorf(`claude -p "/usage" failed: %s`, result.Result)
	}
	return parseUsage(result.Result)
}

func publish(lokiURL, email string, usage Usage) error {
	now := time.Now()
	nowMs := now.UnixMilli()
	// The reset-text wording is free-form and drifts wall-clock to
	// wall-clock (e.g. "resets Sep 26 at 1:59pm" vs "resets Sep 26 at
	// 2pm" for the same underlying window), so it goes in the line body,
	// not Metadata — never JSON-decode-failing since it's our own
	// marshal.
	line, _ := json.Marshal(struct {
		SessionResetText string `json:"session_reset_text"`
		WeekResetText    string `json:"week_reset_text"`
	}{SessionResetText: usage.SessionReset, WeekResetText: usage.WeekReset})

	// Loki structured metadata is promoted to result-series labels by
	// `unwrap` at query time, so only the numeric fields the dashboards
	// actually unwrap belong here — a free-text field (like the
	// reset-text wording) fans one logical series into one result series
	// per distinct wording, silently multiplying sum()/last_over_time()
	// results across whatever wordings land in the query window.
	// week_reset_unix_ms is safe here specifically because it's a
	// monotonically-changing absolute timestamp derived from the text,
	// not the text itself — no wording-fanout risk, same as
	// session_pct/week_pct. Omitted entirely (not zero/stale) when
	// usage.WeekReset is empty (the 0%-used case) or fails to parse;
	// dashboardgen treats "field absent" as "no known reset yet."
	metadata := map[string]string{
		"session_pct": strconv.Itoa(usage.SessionPct),
		"week_pct":    strconv.Itoa(usage.WeekPct),
	}
	if t, ok := parseWeekReset(usage.WeekReset, now); ok {
		metadata["week_reset_unix_ms"] = strconv.FormatInt(t.UnixMilli(), 10)
	}

	return lokiclient.Push(lokiURL, []lokiclient.Stream{{
		Labels: map[string]string{"service_name": "claude-code-usage-truth", "user_email": email},
		Values: []lokiclient.StreamValue{{
			TimestampNs: strconv.FormatInt(nowMs, 10) + "000000",
			Line:        string(line),
			Metadata:    metadata,
		}},
	}})
}

// PublishUsageTruth runs AccountEmail+FetchUsage for every configDir and
// publishes one line per logged-in account. A per-account failure (not
// logged in, claude timeout, publish failure) is logged and skipped — one
// account's failure must not stop the others.
func PublishUsageTruth(lokiURL string, configDirs []string, log func(string, ...any)) error {
	for _, configDir := range configDirs {
		email, err := AccountEmail(configDir)
		if err != nil {
			log("usage-truth: auth status for %s failed: %v", configDir, err)
			continue
		}
		if email == "" {
			log("usage-truth: %s is not logged in, skipping", configDir)
			continue
		}
		usage, err := FetchUsage(configDir)
		if err != nil {
			log("usage-truth: /usage for %s failed: %v", email, err)
			continue
		}
		if err := publish(lokiURL, email, usage); err != nil {
			log("usage-truth: publish for %s failed: %v", email, err)
		}
	}
	return nil
}
