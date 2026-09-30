# Collector — Design

Date: 2026-09-23
Status: approved-pending-review

## Context

Third and last piece of the Node-free collector architecture (see
`2026-09-23-dash-generator-design.md` for the wizard/collector/
dash-generator split and why). This spec covers **collector**: the
per-user, local-only binary that ports `transcript-scan.mjs` (1013 lines)
and a simplified `usage-truth.mjs`. Both need things only the user's own
machine has — the local transcript files, and a `claude` CLI logged into
the user's own account — so unlike dash-generator, this cannot run
anywhere but each monitored machine.

`transcript-scan.mjs` is the highest-risk file in this whole rewrite to
port inaccurately: its own comments document real bugs found the hard way
(331 missing MCP calls from not walking one directory level deeper for
subagent transcripts; a rate cutline that read 782k when the real P75 was
1.09M from a halflife mismatch). This spec is deliberately exhaustive
about exact behavior — every constant, regex, and edge case below was
read directly from the source, not summarized from memory.

**What's explicitly dropped, decided already in the dash-generator
spec:** `usage-meter.mjs` (LogQL block-boundary heuristics) is gone
entirely. That kills `usage-truth.mjs`'s calibration math too (
`parseResetWeekday`, `meterTokens`, the block_5h/week back-computation
into `account-limits.json`) — its only data source was the stream
`usage-meter.mjs` published. The Go `usagetruth` package below is
therefore much smaller than the original file: just the `/usage` capture
and publish, no calibration.

## Goals

- Byte-for-byte equivalent behavior to `transcript-scan.mjs` for
  everything that produces the tools/skills Loki streams: token
  attribution, MCP/skill/bash-command name resolution, email resolution,
  dedup, batching/retry. This is a port, not a redesign — see "Preserved
  exactly" under each section.
- No Node, no other runtime dependency beyond the `claude` CLI itself
  (which `usagetruth` spawns — that's a capability requirement, not a
  language one; Go's `os/exec` spawns it exactly like Node's
  `child_process.execFile` did).
- Same cross-platform build/release story as `setup/` and
  `dash-generator/`.

## Non-goals

- Rate metering, dashboard generation (dash-generator's job, already
  shipped).
- `/usage`-based calibration of `account-limits.json` (dead, see above).
- Changing the tools/skills Loki stream schema — dash-generator's
  `dashboardgen` package already queries these streams by their existing
  field names; changing them here would break that without touching it.

## Module layout

The existing Node implementation has been renamed to `collector-old/`
(already done, ahead of this plan) — `collector/` is free, so the Go
module is built directly there rather than through a temporary name and a
later cutover rename. `collector-old/` stays until this binary is
verified working end-to-end against a real account, then gets deleted as
the implementation plan's last task.

```
collector/
  go.mod
  cmd/collector/main.go
  internal/accounts/accounts.go            (port of accounts.mjs)
  internal/accounts/accounts_test.go
  internal/shelltok/shelltok.go            (port of tokenizeShell/extractBashCommands)
  internal/shelltok/shelltok_test.go
  internal/state/state.go                  (persisted JSON state)
  internal/state/state_test.go
  internal/transcriptscan/transcriptscan.go (the port of transcript-scan.mjs)
  internal/transcriptscan/transcriptscan_test.go
  internal/lokiclient/lokiclient.go        (copy of dash-generator's — see below)
  internal/lokiclient/lokiclient_test.go
  internal/usagetruth/usagetruth.go        (simplified port of usage-truth.mjs)
  internal/usagetruth/usagetruth_test.go
.github/workflows/collector-release.yml
```

`lokiclient` is intentionally duplicated from `dash-generator/internal/
lokiclient` rather than shared via a third module — same reasoning as
keeping `collector` and `dash-generator` separate modules in the
first place: these are independently deployed binaries, and a shared
internal module between them is coupling neither needs. It's ~40 lines;
duplication is cheaper than the coordination cost of a shared module.

## `internal/accounts` — port of `accounts.mjs`

```go
// ResolveConfigDirs returns every Claude Code config directory to scan:
// CLAUDE_DIR (default "$HOME/.claude") plus the colon-separated
// CLAUDE_OBSERVABILITY_EXTRA_DIRS, each with "${HOME}"/"$HOME" expanded,
// deduplicated, primary first.
func ResolveConfigDirs(homeDir string, claudeDirEnv, extraDirsEnv string) []string
```
Preserved exactly: dedup via insertion-order set (primary always first
even if it also appears in the extra-dirs list), both `${HOME}` and
`$HOME` literal-replaced (not a general env-var expansion).

## `internal/shelltok` — port of the Bash command extractor (JS lines 210-349)

```go
// ExtractBashCommands names every recognizable sub-command in a Bash
// tool's `input.command` string ("git status && ls -la" -> ["git","ls"]).
func ExtractBashCommands(command string) []string
```

Preserved exactly:
- **Tokenizer** splits on `&&`, `||`, `;`, `|`, and bare newlines. Quotes
  (`'`, `"`, `` ` ``) and parens make their contents atomic (no splitting
  inside — critical for `$(...)` surviving as one token).
- **Heredocs** (`<<EOF ... EOF`, `<<-`, quoted or bare delimiter) are
  atomic: find the exact byte offset where the body ends (a line matching
  the delimiter, optionally indented if `<<-`); falls through as ordinary
  text if it isn't really a heredoc or the closing line never appears
  (safe fallback for truncated logging — never throws).
- A trailing `\` immediately before `\n` is a line continuation: both
  characters are dropped, no segment break, no token.
- Per segment: skip leading env-var assignments (`^[A-Za-z_][A-Za-z0-9_]*=`)
  and bare `\` continuation artifacts, then take the first remaining token
  as the candidate command name.
- **`rtk` unwrap**: if the candidate name is exactly `"rtk"` (a
  PreToolUse hook on this machine that rewrites commands, e.g. `git
  status` → `rtk git status`) and there's a next token, use the *next*
  token instead.
- `looksLikeCommandName`: rejects Bash reserved words (`if then else elif
  fi for while until do done case esac function select in time`) and
  anything not matching `^[A-Za-z0-9_][A-Za-z0-9_.\/@:-]*$`.

## `internal/state` — persisted JSON state

```go
type Group struct {
    SessionID   string
    RequestID   string
    Model       string
    Effort      string
    GitBranch   string
    Project     string
    IsSidechain bool
    AtMs        int64
    Calls       []Call
}
type Call struct {
    ToolUseID    string
    ToolName     string
    ResultBytes  int
    Timestamp    string
    BashCommands []string
    ToolSource   string
    MCPServer    string
    MCPTool      string
}
type FileState struct {
    Offset          int64  `json:"offset"`
    PendingMain     *Group `json:"pendingMain,omitempty"`
    PendingSub      *Group `json:"pendingSub,omitempty"`
    ActiveSkillMain string `json:"activeSkillMain"`
    ActiveSkillSub  string `json:"activeSkillSub"`
}
type PluginSkillRequest struct {
    Skill string `json:"skill"`
    TsMs  int64  `json:"ts"`
}
type State struct {
    Version              int                            `json:"version"`
    Files                map[string]*FileState          `json:"files"`
    SessionEmail         map[string]string              `json:"sessionEmail"`
    Seen                 map[string]int64               `json:"seen"`
    PluginSkills         map[string][]string             `json:"pluginSkills"`
    PluginSkillByRequest map[string]PluginSkillRequest   `json:"pluginSkillByRequest"`
}

func Load(path string) (*State, error) // missing/unreadable/wrong version -> zero State (logged, not erred)
func Save(path string, st *State) error // write-temp-then-rename
```

Deliberately **not** carrying over `otelSkills` (confirmed dead in the JS —
declared in `emptyState()`, never read or written anywhere else) or
`ratePublished` (now owned solely by dash-generator's own state file).
`version` stays `2` for continuity of intent, though this is a fresh file
(`.state/collector-state.json`, not the JS's `.state/
collector-state.json`) so no real migration happens — Go's `Load` just
needs to produce a valid zero-value `State` on ENOENT, matching the JS's
"missing file -> `emptyState()`" behavior.

## `internal/lokiclient`

**Correction from this spec's first draft**, found by reading
`transcript-scan.mjs` line-by-line rather than paraphrasing: this is
**not** identical to `dash-generator/internal/lokiclient`. Every Loki call
in `transcript-scan.mjs` (`resolveEmails`, `seedSeenFromLoki`,
`projectOwners`, `rebuildSkillRecords`) queries raw **log lines** — a bare
`{selector} | filters` with no `sum(...)`/`count_over_time(...)`
aggregation — which returns Loki's `"streams"` result type (`stream:
{labels}` + `values: [[timestampNs, line], ...]`), not the `"vector"`/
`"matrix"` metric shape `dash-generator`'s client parses (whose JSON key
is `metric`, not `stream`). The two are structurally close (both are
labels + timestamped pairs) but the JSON field name differs and would
silently deserialize empty if reused as-is. Timestamps are also
nanosecond Unix integers here (matching exactly what every call site
below computes), not the unix-*seconds* `dash-generator` uses.

```go
type StreamResult struct {
    Labels map[string]string
    Values [][2]string // [timestamp_ns_as_string, line_text]
}

// QueryRange runs a log-query range request. start/end are nanosecond
// Unix timestamps. limit <= 0 omits the "limit" param; direction == ""
// omits "direction".
func QueryRange(lokiURL, query string, startNs, endNs int64, limit int, direction string) ([]StreamResult, error)

// Push posts to /loki/api/v1/push — identical shape/behavior to
// dash-generator's (same Stream/StreamValue types), duplicated rather
// than shared per this repo's independently-deployed-binaries convention.
type Stream struct {
    Labels map[string]string
    Values []StreamValue
}
type StreamValue struct {
    TimestampNs, Line string
    Metadata           map[string]string
}
func Push(lokiURL string, streams []Stream) error
```

## `internal/transcriptscan` — the port of `transcript-scan.mjs`

### Constants (env-var overridable, same names/defaults as the JS)

```go
const (
    DefaultBatchSize      = 2000
    DefaultOrphanAfterMs  = 15 * 60 * 1000
    DefaultDedupDays      = 90
    DefaultEmailLookbackH = 720 // 30 days
)
```
`WEEK_START_DAY`/`WEEK_START_HOUR`/`TZ_OFFSET_HOURS`/`RATE_HALFLIFE`/
`RATE_HALFLIFE_S`/`RATE_BACKFILL_DAYS` are **not** ported into this
package — the fork's analysis confirmed they're pure passthrough exports
in the JS, read internally only for `RATE_HALFLIFE`'s format validation
(`^(\d+)([smh])$`), which is dash-generator's concern now, not
transcript-scan's. Nothing in this package's own logic uses any of them.

### Transcript discovery

```go
// FindTranscripts walks every configDir + "/projects" recursively
// (depth <= 6, matching the JS) collecting every *.jsonl file, including
// "<session>/subagents/*.jsonl" one level deeper than session files —
// missing this cost 331 real MCP calls once; do not regress it.
func FindTranscripts(configDirs []string) ([]string, error)
```

### Per-file scan: byte-offset tracking (JS lines 450-465)

```go
// ProcessTranscript reads path starting at fileState.Offset, line by
// line, and returns newly-settled records plus the updated FileState.
// A line's byte length is only folded into the consumed count once the
// NEXT line is seen (proving the previous one ended in "\n") — this
// avoids counting a not-yet-newline-terminated final line (common: Claude
// Code writes concurrently) as consumed, which would silently skip the
// first byte written next. If the file shrank since fileState.Offset
// (truncated/replaced), everything resets: offset=0, both pending groups
// and active-skill trackers cleared.
func ProcessTranscript(path string, fileState state.FileState) ([]Record, state.FileState, error)
```

### Token attribution (JS `settle`/`attribute`/`blockSize`)

Preserved exactly:
- On an `assistant` entry with `usage` present AND a pending group already
  open for that track: settle the **previous** pending group using THIS
  message's `usage.input_tokens + usage.cache_creation_input_tokens`
  (`cache_read` tokens explicitly excluded). Then, if this same assistant
  message has its own `tool_use` blocks, open a NEW pending group.
- On a `user` entry (tool results) while a pending group exists: for each
  `tool_result` block, match by `tool_use_id`, set `call.ResultBytes =
  blockSize(block)`.
- `blockSize`: string content → its length; array content → sum of
  (string parts' length, or `.text` length for `{type:"text"}` parts, or
  the JSON-encoded length of anything else); anything else → JSON-encoded
  length of `content` (or of `""` if nil).
- `settle(group, inputTokens)`: `total = sum(call.ResultBytes)`. Per call,
  `tokensAttributed = round(total>0 ? inputTokens*call.ResultBytes/total :
  inputTokens/len(group.Calls))` — proportional split by result size, or
  even split if every result was empty.
- A `Bash` call with recognized sub-commands fans out into **one record
  per sub-command**, each reusing the SAME `tokens_attributed`/
  `result_bytes` (not split further), `mcp_server="bash"`,
  `mcp_tool=<commandName>`, dedup key `"bash:<toolUseId>:<index>"`. A
  non-Bash (or Bash-with-no-recognized-subcommand) call yields exactly one
  record via `parseToolName`.
- `timestampNs = strconv.FormatInt(parsedMs, 10) + "000000"` (ms→ns by
  string-appending 6 zeros, exactly matching the JS's precision-faking).

### Orphan handling (JS `runPass`, after all files processed)

```go
// Any group still pending (main or sub) whose Group.AtMs is more than
// orphanAfterMs in the past gets force-settled with inputTokens=0 and
// cleared — a session that ended mid-turn must not hold its tool calls
// forever waiting for a reply that will never come.
```

### MCP tool name resolution (JS lines 200-208, `parseToolName`)

```go
// ParseToolName splits only on the FIRST "__" after the "mcp__" prefix
// (the tool name itself may legitimately contain "__").
//   "mcp__github__search_code" -> source=mcp, server=github, tool=search_code
//   "mcp__github"              -> source=mcp, server=github, tool=""
//   "Bash"                     -> source=builtin, server="", tool=""
func ParseToolName(name string) (source, server, tool string)
```

### Skill name resolution (session-level + per-request)

Preserved exactly, two parallel structures:
- **Session-level** (`state.PluginSkills[sessionID]`, a set in memory,
  persisted as a sorted/deduped slice): every time a processed `assistant`
  entry's active-skill tracker holds a name containing `:` (i.e. looks
  like `owner:skillname`, a plugin skill), add it to the session's set.
  Fallback when a session used exactly ONE plugin skill (unambiguous).
- **Per-request** (`state.PluginSkillByRequest[requestID]`): records
  exactly which skill was active at the moment of THIS specific request —
  needed because 2+ plugin skills in one session makes the session-level
  set ambiguous.
- The active-skill tracker itself updates on any `tool_use` block named
  `"Skill"`, reading the real name from `block.input.skill` — the ONLY
  place the real name is available (Claude Code redacts it to
  `"third-party"` on OTel's own `api_request.skill_name` for any plugin
  skill). Sticky until the next `Skill` call, mirroring Claude Code's own
  OTel stickiness.
- **`RebuildSkillRecords`** (called once per pass, not per file — re-reads
  OTel's own `api_request` events from Loki, NOT transcript data, because
  OTel's per-skill token counts are the source of truth; transcript-based
  attribution measured 87-100% off on some skills since subagents/rotated
  sessions never appear in transcripts). For each OTel record with
  `skill_name != ""`:
  ```
  candidates = state.PluginSkills[sessionID]
  real = state.PluginSkillByRequest[requestID].Skill, or
         candidates[0] if len(candidates) == 1, else ""
  skill = real if meta.SkillName == "third-party" && real != "" else meta.SkillName
  owner = strings.SplitN(skill, ":", 2)[0] if strings.Contains(skill, ":") else "local"
  ```

### Email/account resolution (3-tier)

1. **Skill records**: `user_email` comes directly from the OTel label
   already present (`account_source: "otel"`) — skipped in the tiers
   below.
2. **Tool records, session→OTel** (`ResolveEmails`): for every distinct
   `session_id` among fresh records not cached in `state.SessionEmail`,
   query Loki: `` {service_name="claude-code"} | session_id = `<id>` |
   user_email != `` `` over the last `EMAIL_LOOKBACK_HOURS` (720h),
   `limit=1, direction=backward`. Result (or `""` if none — cached either
   way, so a session with no OTel data is never re-queried) cached
   forever in `state.SessionEmail[sessionID]`.
3. **Project inference** (`ProjectOwners`, only runs if something's still
   unresolved): builds a `project -> email` map from BOTH existing Loki
   history (`{service_name=<STREAM>, kind="tools"}` over the last
   `DEDUP_DAYS`) AND the current batch (critical for a from-scratch
   import where Loki history is empty) — a project maps to an email only
   if EVERY observed pairing used the same single email (ambiguous
   projects, 2+ distinct emails, left unmapped). Applied records get
   `account_source: "project"` (distinguishable from `"otel"`).

### Dedup (`DropAlreadySeen`)

Preserved exactly:
- Prune any `state.Seen[id]` older than `now - dedupDays*24h` at the start
  of every call.
- Dedup key: `record.DedupKey` if set (Bash-fanout, skill records), else
  `record.Meta["tool_use_id"]`. Neither present → always passes through.
- Dropped if already in `state.Seen` OR already claimed earlier in the
  SAME batch (a local map, since `state.Seen` isn't mutated until commit
  time, after a successful push).
- Fresh keys staged separately; only merged into `state.Seen` after Loki
  accepts the push. A record later found in the push's `skipped` list
  (see below) has its staged entry removed before commit — never marked
  seen, stays eligible for retry.
- Purpose: `claude --resume` rewrites the ENTIRE history into a new
  transcript file with identical `request_id`/`tool_use_id`/timestamps
  but a different `session_id` — without this, every resume re-counts the
  original session.

### Batched Loki push (`PushToLoki`)

Preserved exactly:
- All records sorted by `timestampNs` (exact integer comparison — Go's
  `int64`/`big.Int` as appropriate, matching the JS's BigInt compare;
  nanosecond timestamps fit in `int64` until year 2262, `int64` is
  sufficient) before pushing — Loki rejects too-far-out-of-order writes
  within a stream, and initial backfill interleaves many files in time.
- Chunked by `BatchSize` (2000). Within a chunk, records grouped by their
  exact stream-labels (tool vs. skill streams can't share one push).
- Each chunk POST retries up to 5 times, linear backoff `2000ms *
  attempt`, to tolerate Loki's cold-start "empty ring" error.
- A `400` containing `"too far behind"` (case-insensitive) is NOT
  retried — that chunk can never succeed later either. Those records go
  to `skipped`, not `pushed`, so dedup doesn't lock them out once the
  blocking window ages out.
- `429` (rate-limited — seen during full 90-day reimports outrunning
  per-user ingest limits) gets the SAME retry/backoff as a 5xx.
- Any other 4xx aborts that chunk immediately (returns an error).
- A fixed 300ms pace gap follows every successful non-final chunk push.

### What gets published (unchanged stream schema — dash-generator
already queries these exact names/fields)

**Tools stream** — labels `{service_name: <EXPORTER_STREAM>, kind:
"tools"}`. Line text = tool name. Metadata fields: `session_id,
request_id, tool_use_id, tool_name, tool_source, model, effort,
query_source, git_branch, project, result_bytes, tokens_attributed,
mcp_server, mcp_tool, user_email, account_source`.

**Skills stream** — labels `{service_name: <EXPORTER_STREAM>, kind:
"skills"}`. Line text = resolved skill name. Metadata fields: `skill,
skill_owner, session_id, request_id, model, effort, query_source,
project (always ""), tokens, user_email, account_source (always "otel")`.

`EXPORTER_STREAM` stays versioned for the same reason as today: Loki's
delete API can't truly remove already-flushed data, so re-importing
history means a new stream name.

### Entry points

```go
// InitState loads state (or starts fresh), honors --rescan (zeros
// state.Files only — keeps dedup/plugin-skill maps, a non-destructive
// re-read), and seeds state.Seen from Loki history when it's empty
// (fresh state / lost volume) by querying day-by-day back DedupDays,
// continuing past a single failed day rather than aborting the whole seed.
func InitState(cfg Config) (*state.State, error)

// RunPass does one full pass: scan every configured directory's new
// transcript lines, settle/orphan groups, rebuild skill records from
// OTel, dedup, resolve emails, push to Loki, persist state. Returns the
// count of records actually pushed.
func RunPass(cfg Config, st *state.State) (int, error)
```

## `internal/usagetruth` — simplified port of `usage-truth.mjs`

No calibration (dropped, see Context). Just capture and publish.

```go
type Usage struct {
    SessionPct   int
    SessionReset string
    WeekPct      int
    WeekReset    string
}

// AccountEmail runs `claude auth status` with CLAUDE_CONFIG_DIR=configDir
// and returns the logged-in email, or "" if not logged in.
func AccountEmail(configDir string) (string, error)

// FetchUsage runs `claude -p /usage --output-format json
// --no-session-persistence` and parses the two top-line percentages/reset
// clauses Anthropic always leads with:
//   Current session: 13% used · resets Sep 22 at 8:39pm (America/Sao_Paulo)
//   Current week (all models): 42% used · resets Sep 24 at 7:59pm (...)
// The "resets..." clause is absent at 0% used (no window open). Only
// these two top-line numbers are used — the rest of /usage's output is
// explicitly scoped by Anthropic to "local sessions on this machine" and
// was never used here either.
func FetchUsage(configDir string) (Usage, error)

// PublishUsageTruth runs AccountEmail+FetchUsage for every configured
// account and pushes one line per logged-in account to Loki on
// service_name="claude-code-usage-truth", labeled by user_email, fields
// session_pct/session_reset_text/week_pct/week_reset_text. A per-account
// failure (not logged in, claude timeout, publish failure) is logged and
// skipped — one account's failure must not stop the others.
func PublishUsageTruth(cfg Config, configDirs []string, log func(string, ...any)) error
```

Preserved exactly: the two regexes (`Current session:\s*(\d+)%\s*used(?:\s*·\s*(.+))?` and the week equivalent with `Current week[^:]*:`), the `claude` invocation args and the 30s timeout, `CLAUDE_CONFIG_DIR` as the account selector (same directories `accounts.ResolveConfigDirs` already reads transcripts from — no separate account list).

Dropped entirely (data source gone with `usage-meter`): `offsetHoursFor`,
`parseResetWeekday`, `meterTokens`, `loadLimits`/`writeLimits`, the whole
`account-limits.json` calibration write-back.

## Timestamped `/usage` history + reset-boundary visualization

Two related requirements added after this spec's first draft: (1) be able
to check "what % was I at yesterday" in the dashboard, and (2) when
panning a time range that straddles a reset (e.g. viewing 3pm-6pm and the
account's week reset at 5pm), see the OLD window's real values before 5pm
and the NEW window's real values from 5pm on — not one value smeared
across the whole range.

Both fall out of the design already above, with no new collector logic
needed — worth stating explicitly since it wasn't called out as a
deliberate property before:

- `PublishUsageTruth` runs once per `cmd/collector/main.go`'s
  `POLL_SECONDS` loop (default 60s) for as long as the collector runs —
  not once at startup. Every run pushes a NEW Loki line (own timestamp,
  Loki's native push semantics), it never overwrites a previous one.
- Because each line carries the real percentage AND its real "resets at"
  text as it stood at that exact poll, the raw history in
  `claude-code-usage-truth` already IS a correct time series: a query
  over 3pm-6pm returns whatever `session_pct`/`week_pct` values were
  actually true at each polled moment in that range — naturally low/
  rising before a 5pm reset, then dropping to whatever the new window's
  real value is from the first poll after 5pm on. There's no "current
  value smeared backward" to correct, because nothing ever computes a
  single current value and stamps it across a range — it's raw samples,
  each with the timestamp it was true at.
- What this DOES require, tracked as a dash-generator/template
  follow-up rather than a collector change: a time-series panel in
  `grafana/templates/claude-code.json` that plots `session_pct`/
  `week_pct` from `{service_name="claude-code-usage-truth"} | user_email
  =~ ...} | unwrap week_pct` (etc.) over the selected range, instead of
  (or alongside) an instant-value gauge. That's pure Grafana/LogQL
  panel-JSON work — no Go code in either `collector` or `dash-generator`
  needs to change for it, so it can be added to the dash-generator
  implementation plan as a template-only task once that plan is written.
- One accuracy caveat worth setting expectations on: the reset boundary
  in the graph will be accurate to within one `POLL_SECONDS` interval
  (60s default) of the real reset moment, not to the second — the data
  point immediately after the actual reset is whenever the next poll
  happens to land. Polling more often narrows this at the cost of more
  `claude -p /usage` invocations (a real cost since each spawns the CLI,
  unlike the free-to-poll-often Loki-only work in `dash-generator`).

## `cmd/collector/main.go`

One loop (unlike dash-generator's two — there's no dashboard cadence
here anymore):

```
POLL_SECONDS (default 60) -> transcriptscan.RunPass, then usagetruth.PublishUsageTruth
```

Flags: `--once`, `--dry-run` (transcript scan and usage-truth publish
nothing; state offsets still advance so a later real run doesn't
re-scan), `--rescan` (passed through to `InitState`).

Env vars: `CLAUDE_DIR`, `CLAUDE_OBSERVABILITY_EXTRA_DIRS`, `LOKI_URL`
(default `http://localhost:47100`), `EXPORTER_STREAM` (default
`claude-code-exporter-1`), `POLL_SECONDS`, `BATCH_SIZE`,
`ORPHAN_AFTER_MS`, `DEDUP_DAYS`, `EMAIL_LOOKBACK_HOURS`, `STATE_FILE`
(default `<repo root>/.state/collector-state.json`).

Requires being run from the repo root only for its default `STATE_FILE`
location — unlike the wizard/dash-generator, everything else it touches
(config dirs, Loki) comes from env vars, not repo-relative paths, so
`STATE_FILE` is the one value worth being able to override directly
rather than enforcing a repo-root check.

## Wizard changes (`setup/`)

The "Collector service" step (already repointed at `dash-generator` in
the previous spec) now offers to install `collector` too — as a SEPARATE
prompt ("Install the collector as a background service now?"), since a
user might legitimately want one without the other (e.g. a machine that
only hosts Loki/Grafana/dash-generator, with collectors running
elsewhere once the shared-collector project exists). Uses the same
generalized `service.Config`/`Install` already built for dash-generator —
no further service-package changes needed.

## Testing

- `accounts`: dedup, `${HOME}`/`$HOME` expansion, primary-first ordering.
- `shelltok`: table-driven cases covering `&&`/`||`/`;`/`|`/newline
  splitting, quote/paren atomicity, `$(...)` survival, heredoc (plain,
  `<<-` indented, quoted delimiter, unterminated-falls-through), line
  continuation, env-var-assignment skipping, the `rtk` unwrap, reserved
  words rejected, the name-pattern allowlist boundary cases.
- `state`: load/save round-trip, missing-file-is-zero-value, `--rescan`
  zeroing only `Files`.
- `transcriptscan`: this is the bulk of the test surface —
  - `ProcessTranscript` against fixture `.jsonl` files: single tool call,
    parallel tool calls (proportional split verified against hand
    computed numbers), all-empty-results (even split), a Bash call with
    multiple sub-commands (fan-out + shared token count), byte-offset
    resume across two passes on a growing file, the not-yet-newline-
    terminated final line NOT being double-counted, a shrunk/truncated
    file resetting offset+pending+activeSkill.
  - Orphan handling: a pending group older than `orphanAfterMs` gets
    force-settled with zero tokens.
  - `ParseToolName` table-driven, covering the "first __ only" case.
  - Skill resolution: session-level single-candidate fallback,
    per-request disambiguation with 2+ candidates, the "third-party"
    override condition, the owner-from-`:` split.
  - `DropAlreadySeen`: cross-batch dedup, same-batch dedup, pruning by
    age, a `skipped` record's staged key being removed before commit.
  - `PushToLoki` against an `httptest.Server`: chunk splitting at
    `BatchSize`, the 5-retry linear backoff on a simulated cold-start
    error, `"too far behind"` NOT retried and landing in `skipped`, `429`
    retried like a 5xx, another 4xx aborting immediately, the 300ms pace
    gap between chunks (via a fake clock or by asserting call ordering
    rather than real sleeps in the test).
  - Email resolution tiers: OTel-resolved cached correctly (including the
    "no match -> cached as empty, never re-queried" case), project
    inference's ambiguous-project exclusion.
- `usagetruth`: `FetchUsage`'s regex parsing against real-shaped sample
  output (including the 0%-used/no-reset-clause case); `claude` itself
  mocked via a fake executable on `PATH` in the test (a small script
  asserting the exact args/env it was called with and echoing canned
  JSON), not by mocking `os/exec` internals.
- CI: same `go test ./...` matrix pattern as the other two modules.

## Open items (not resolved here)

- The dashboard template's usage gauges reading `usage-truth`'s
  percentages directly instead of the old token math (flagged in the
  dash-generator spec too).
- The final cutover: deleting `collector-old/` and updating
  `bin/install-service.sh`/README references that still point at it —
  planned as the implementation plan's last task, done only once this
  binary is verified against a real account's real transcripts.
- The `/usage` history + Grafana reset-boundary visualization requested
  after this spec was first drafted — see "Timestamped /usage history"
  below; the collector side is already covered by the existing
  `usagetruth` design, the template panel is dash-generator's side.
