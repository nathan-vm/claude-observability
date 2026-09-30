# Per-account "Since last reset" time-picker option

## Problem

Each account's generated Grafana dashboard (`grafana/dashboards/accounts/<slug>.json`)
has a fixed `timepicker.quick_ranges` entry, `"Current week (from Mon 9am)"`
(`now/w+9h+24h` → `now`), as a stand-in for "the account's current Claude
usage week." It's a convention, not the account's real weekly boundary —
until now there was no way to know that boundary from telemetry.

`/usage` actually reports it: `src/collector/internal/usagetruth/usagetruth.go`
already parses a line like `"Current week (all models): 42% used · resets
Sep 24 at 7:59pm (America/Sao_Paulo)"` into `Usage.WeekReset` (free text,
e.g. `"resets Sep 24 at 7:59pm (America/Sao_Paulo)"`). Anthropic's weekly
window is a fixed 7-day cycle, so `WeekReset` is always the *next* reset —
subtracting 7 days gives the start of the current week, i.e. "last reset."
Verified against live data from both of the user's accounts on 2026-09-30
(`nathan.v.marcelino@gmail.com`: next reset 2026-10-01T19:59 -03:00 → last
reset 2026-09-24T22:59:00Z; `nathan.marcelino@zallpy.com`: next reset
2026-10-03T14:00 -03:00 → last reset 2026-09-26T17:00:00Z).

Goal: give each account dashboard a quick-range option, always named the
same thing, whose value tracks the account's real last-reset moment —
replaced in place on every regeneration, never duplicated, and reset
timing can differ per account (confirmed: each account already gets its
own independently-generated JSON file, so this is a non-issue).

## Design

**Where the reset moment gets computed:** in `dashboardgen`, from a new
numeric structured-metadata field `week_reset_unix_ms` that `collector`
adds to the usage-truth line it already publishes every poll — *not* a
new collector-side "did the week change" detector, and not a new Loki
stream/event.

Why: `collector`'s existing `PublishUsageTruth` → `publish()`
(`usagetruth.go:216`) already pushes one line per account on every poll
(`POLL_SECONDS`, default 60s) to `{service_name="claude-code-usage-truth"}`,
with `session_pct`/`week_pct` in `Metadata` and the free-text reset
clauses in the line body (deliberately — see the comment at
`usagetruth.go:223`: the wording drifts wall-clock to wall-clock, e.g.
"7:59pm" vs "8pm" for the same instant, so it can't be a label without
Loki structured-metadata cardinality fanning out per wording). Parsing
`WeekReset` into an absolute UTC timestamp and adding it as *one more
numeric* metadata key sidesteps that wording-jitter problem entirely:
`dashboardgen` reads it with `last_over_time(...)`, which just wants the
latest value regardless of how many near-identical polls happened in
between. No new collector state, no new push call, no event-detection
logic to get wrong — it rides the existing per-poll line.

Alternative considered and rejected: have `collector` detect "a new
reset happened" and emit a one-off event. Rejected because `last_over_time`
already gives `dashboardgen` "the latest known value" for free, so
detecting *transitions* buys nothing and adds real failure modes (missed
transitions if collector is down at the exact reset moment, an extra
Loki write path to test).

**Collector side** (`src/collector/internal/usagetruth/usagetruth.go`):

1. Add `parseWeekReset(text string, now time.Time) (time.Time, bool)`
   parsing the `"<Mon> <D> at <H>[:MM](am|pm) (<IANA tz>)"` shape (e.g.
   `"Sep 24 at 7:59pm"` or `"Oct 3 at 2pm"` — minutes are optional, as
   seen in live data) into a UTC `time.Time`, using `time.LoadLocation`
   on the parenthesized IANA zone name. Year isn't in the text, so infer
   `now.Year()`; if the parsed date lands more than ~2 days in the past
   relative to `now`, roll forward to `now.Year()+1` (handles the
   December→January boundary, where "resets Jan 2" parsed in late
   December must mean next year). Returns `ok=false` on any parse
   failure or unknown zone — callers must not publish a zero/garbage
   value.
2. In `publish()` (`usagetruth.go:216`), call
   `parseWeekReset(usage.WeekReset, time.Now())`; when it succeeds, add
   `"week_reset_unix_ms": strconv.FormatInt(t.UnixMilli(), 10)` to the
   existing `Metadata` map. When `usage.WeekReset == ""` (the 0%-used
   case, no reset clause) or parsing fails, omit the key entirely rather
   than writing 0 or a stale value — `dashboardgen` must treat "field
   absent" as "no known reset for this account yet."
3. Update the doc comment at `usagetruth.go:223` (currently says *only*
   `session_pct`/`week_pct` are numeric-metadata-safe) to mention the new
   field and why it's safe: it's a monotonically-changing absolute
   timestamp, not free text, so no wording-fanout risk.

**dash-generator side** (`src/dash-generator/internal/dashboardgen/dashboardgen.go`):

1. New function, alongside `skillOwners`/`rateCutlines`:
   ```go
   func lastWeekReset(lokiURL, email string) (time.Time, bool)
   ```
   Runs an instant query (same shape as `rateCutlines`'s smoothed-rate
   query, `lokiclient.Query`, not `QueryRange` — we only want the latest
   value, not a series):
   ```
   sum(last_over_time({service_name="claude-code-usage-truth"} | user_email =~ `<escaped-email>` | unwrap week_reset_unix_ms [7d]) by (user_email))
   ```
   A 7-day lookback is correct by construction — a real weekly reset
   recurs at least once in any 7-day window, once collector has ever
   published it. On Loki error, empty result, or a value that fails
   `strconv.ParseFloat`, return `(time.Time{}, false)` — never inject a
   stale/fabricated entry.
2. In `scopeAccount` (`dashboardgen.go:396`), after the existing
   `replaceVariable`/`filter` calls and before `assertNoPlaceholders`:
   look up `dashboard["timepicker"].(map[string]interface{})["quick_ranges"].([]interface{})`,
   and:
   - if `lastWeekReset` succeeded: build
     `{"display": "Since last reset (from /usage)", "from": t.UTC().Format("2006-01-02T15:04:05.000Z"), "to": "now"}`
     and set it as the **first** element of `quick_ranges`, dropping any
     existing entry with that same `display` value first. Because
     `scopeAccount` always deep-clones fresh from `template` on every
     run (confirmed: `json.Marshal`/`Unmarshal` of the template, not an
     incremental patch of the prior output — `dashboardgen.go:399-406`),
     "always the same entry, updated not duplicated" falls out for free:
     every regeneration starts from the template's fixed 6 entries and
     prepends at most one fresh "Since last reset" entry.
   - if it failed (no data for this account yet): leave `quick_ranges`
     as the template's unmodified 6 entries — no placeholder, no stale
     value.
3. `scopeAccount`'s signature gains nothing new — `lastWeekReset` only
   needs `lokiURL` and `email`, both already in scope via the existing
   `GenerateDashboards` → `scopeAccount` call site
   (`dashboardgen.go:581`). Call it from inside `scopeAccount` itself
   rather than threading another parameter through `GenerateDashboards`.
4. `grafana/templates/claude-code.json` is **not** touched for the new
   entry itself — it's generator-injected per account, not template-fixed
   — but this is the right moment to delete the two
   `nathan-*`-named files I hand-edited and then had overwritten
   (`grafana/dashboards/accounts/*.json` are gitignored generator
   output, never committed — confirmed via `.gitignore`), so they don't
   carry stale manual edits into testing.

**Freshness (documented, not solved):** end-to-end staleness bound is
collector's poll cadence (default 60s) plus dash-generator's own
regeneration cadence (`DASHBOARD_INTERVAL_SECONDS`, default 600s) ≈ ≤11
minutes typical. That's an inherent property of "a file regenerated
periodically," not a bug to chase here.

**Module boundary:** no shared code needed between the two independent
`go.mod`s — `collector` only *produces* a UTC instant as epoch-ms
metadata; `dashboardgen` only *consumes* and formats it. Nothing to
factor out.

## Files to change

- `src/collector/internal/usagetruth/usagetruth.go` — `parseWeekReset`,
  wire it into `publish()`, update the metadata-safety doc comment.
- `src/collector/internal/usagetruth/usagetruth_test.go` — update
  `TestPublish_PutsResetTextInLineBodyNotMetadata` (currently asserts
  *exactly* 2 metadata keys; needs a fixture where `week_reset_unix_ms`
  is present as a 3rd, plus a case proving it's absent when
  `WeekReset == ""`); add `TestParseWeekReset_StandardFormat`,
  `TestParseWeekReset_NoMinutes` (the `"2pm"` shape), `TestParseWeekReset_YearRollover`
  (parse "Jan 2" when `now` is late December), `TestParseWeekReset_UnknownFormatReturnsFalse`.
- `src/dash-generator/internal/dashboardgen/dashboardgen.go` —
  `lastWeekReset`, the `quick_ranges` injection in `scopeAccount`.
- `src/dash-generator/internal/dashboardgen/dashboardgen_test.go` —
  tests for `lastWeekReset` (success, no-data, unparseable-value) and for
  `scopeAccount` producing the expected `quick_ranges` entry, staying
  first and not duplicating across two consecutive calls with different
  reset values (the "always the same option, updated in place"
  requirement).
- `grafana/dashboards/accounts/nathan-v-marcelino-gmail-com.json`,
  `grafana/dashboards/accounts/nathan-marcelino-zallpy-com.json` — revert
  the hand-edit (delete the manually-added `quick_ranges` entry; it's
  gitignored generated output anyway and will be regenerated correctly
  once this plan ships).

## Verification

- `go build ./... && go test ./... && go vet ./...` in both
  `src/collector` and `src/dash-generator`.
- Manual check against the **dev** Loki stack only (per this repo's own
  rule: `dash-generator` has no read-only mode, never point it at the
  real 47100 stack) — seed dev Loki by reading a recent slice of real
  `claude-code-usage-truth` lines from the real Loki (47100, read-only)
  and pushing them into dev Loki, then run
  `dash-generator --once` against the dev stack and inspect the
  generated `grafana/dashboards/accounts/<slug>.json`: confirm
  `quick_ranges[0].display == "Since last reset (from /usage)"`, and that
  its `from` matches `week_reset_unix_ms`'s latest seeded value minus
  nothing (it's already the *last* reset, not the next one — the minus-7-days
  step happens in collector's own publish, not at read time... **no** —
  re-check during implementation: confirm whether `week_reset_unix_ms`
  should store the *next* reset as parsed, or the *last* reset
  (next − 7d). Parsing directly gives the *next* reset; the dashboard
  wants the *last* one. Decide explicitly in implementation whether the
  7-day subtraction happens in `collector` (store `week_reset_unix_ms` as
  already-the-last-reset) or in `dashboardgen` (store the raw parsed
  *next* reset, subtract 7 days when building the `quick_ranges` entry).
  Recommend doing the subtraction in `dashboardgen` at injection time,
  not in `collector`: `collector`'s value should be an honest 1:1
  transcription of what `/usage` said ("this is when it resets"), and
  "last reset = next − 7d" is a display-time interpretation specific to
  this one panel, not a fact about the account.
- Run twice in a row against the same seeded dev data and confirm
  `quick_ranges` still has exactly one "Since last reset" entry (no
  duplication) with the same value (idempotent).

## CLAUDE.md conventions honored

- No hand-edited version anywhere (n/a here — no release-facing change).
- Structured-metadata rule from CLAUDE.md's Loki section: only the new
  numeric `week_reset_unix_ms` goes in `Metadata`; the free-text
  `WeekReset`/`SessionReset` stay in the line body, unchanged.
- `sum(... by (user_email))` grouping on every new query, matching the
  existing `week_pct` pattern, so structured metadata doesn't fan a
  series out per distinct value.
- Work happens in an isolated worktree per this repo's normal flow
  (created separately, after this plan is approved).
