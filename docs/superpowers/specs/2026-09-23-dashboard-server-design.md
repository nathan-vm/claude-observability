# Dashboard Server (Go) — Design

Date: 2026-09-23
Status: approved-pending-review

## Context

Mid-implementation of the local setup wizard, it became clear the intended
end state is **no Node.js dependency anywhere** — not just in the setup
wizard (already Go), but in the collector too. The collector as it exists
today (`collector/*.mjs`, six files) becomes three Go binaries instead:

1. **wizard** — already built (`setup/`). Configures Claude Code telemetry,
   installs background services.
2. **collector** — per-user, local-only. Ports `transcript-scan.mjs` (real
   MCP/skill/bash-command attribution from local transcripts, since OTel
   redacts these) and `usage-truth.mjs` (spawns the real `claude` CLI for
   `/usage` ground truth). Needs the filesystem and a logged-in `claude`
   CLI, so it cannot run anywhere but the user's own machine. **Not covered
   by this spec** — `transcript-scan.mjs` is 1013 lines with several
   intricate subsystems (shell tokenizer, proportional token attribution,
   3-tier email resolution, session/per-request skill tracking, dedup with
   resume-safety, batched Loki push with specific retry rules). It gets its
   own spec once this one ships.
3. **dashboard-server** — **this spec**. Ports `rate-meter.mjs` and
   `dashboard-generator.mjs`. Both have zero dependency on transcripts or
   the `claude` CLI — pure Loki-HTTP-in, Loki/file-out — so they can run
   anywhere Loki is reachable: the same local machine in a local setup
   today, or a genuinely separate shared server once the external-Grafana
   project exists. This is what "the server creates dashboards, not the
   user's local setup" means concretely: dashboard generation becomes an
   architecturally separate process from the per-user collector, even
   though in local mode it happens to run on the same physical machine.

`usage-meter.mjs` is **dropped entirely** (not ported) — its LogQL-based 5h
block-boundary heuristic and manual-calibration-against-a-guessed-limit
approach is being replaced by relying solely on `/usage`'s own ground
truth, which is `usage-truth.mjs`'s job in the future collector spec. This
also kills `usage-truth.mjs`'s calibration math (`parseResetWeekday`,
`meterTokens`, the block_5h/week-limit back-computation) since its only
data source was the stream `usage-meter.mjs` published — but that's the
future collector spec's concern, noted here only because it explains why
`account-limits.json`'s `block_5h`/`week` fields become purely
user-set/manual going forward (no automatic calibration), which affects
how this spec's `dashboard-generator` port reads them (unchanged read
path, just no longer auto-updated by anything).

**Known follow-up, explicitly out of scope here:** the dashboard template's
usage gauges currently read token math derived from `usage-meter`'s
stream. With `usage-meter` gone, those panels will eventually need to read
`usage-truth`'s direct percentages instead. That's a `grafana/templates/`
edit, decided alongside the collector spec, not touched by this one.

## Goals

- No Node dependency: a self-contained Go binary, same cross-platform
  build/release story as the wizard (`setup/`'s `go.mod`, GitHub Actions
  release workflow — this binary gets its own module and its own release
  job, same pattern).
- Byte-for-byte equivalent LogQL queries, EWMA math, quantile/cutline
  math, and dashboard JSON mutation as today's `rate-meter.mjs` +
  `dashboard-generator.mjs` — this is a port, not a redesign.
- The wizard's "install a background service" step installs this instead
  of the old Node collector, for now (see "Wizard changes" below) — until
  the collector spec ships, transcript-scan/usage-truth simply aren't
  running for a wizard-driven local setup. Flagged explicitly, not silent.

## Non-goals

- Porting `transcript-scan.mjs` or `usage-truth.mjs` (future spec).
- Redesigning the Grafana dashboard template.
- Any change to `usage-meter.mjs`'s fate beyond "it's dropped" (already
  decided; nothing here reconsiders it).

## Module layout

New top-level directory `dashboard-server/` (own `go.mod`, own release
binary), mirroring `setup/`'s structure:

```
dashboard-server/
  go.mod
  cmd/dashboard-server/main.go
  internal/lokiclient/lokiclient.go       (Query, QueryRange, Push)
  internal/lokiclient/lokiclient_test.go  (httptest.Server-backed)
  internal/ratemeter/ratemeter.go         (port of rate-meter.mjs)
  internal/ratemeter/ratemeter_test.go
  internal/dashboardgen/dashboardgen.go   (port of dashboard-generator.mjs)
  internal/dashboardgen/dashboardgen_test.go
  internal/state/state.go                 (small JSON state: ratePublished)
  internal/state/state_test.go
.github/workflows/dashboard-server-release.yml
```

## `internal/lokiclient` — shared HTTP client

Both ported files talk to Loki the same two ways today (instant query,
range query, push); consolidating into one small client avoids duplicating
HTTP/JSON plumbing across `ratemeter` and `dashboardgen`.

```go
type Series struct {
    Metric map[string]string
    Values [][2]string // [timestamp_unix_seconds_as_string, value_as_string]
}

// Query runs an instant query (Loki's /loki/api/v1/query) at atUnix.
func Query(lokiURL, query string, atUnix int64) ([]Series, error)

// QueryRange runs a range query (/loki/api/v1/query_range).
func QueryRange(lokiURL, query string, startUnix, endUnix, stepSeconds int64) ([]Series, error)

// Stream is one Loki push stream: labels plus (timestamp_ns, line, metadata) triples.
type Stream struct {
    Labels map[string]string
    Values []StreamValue
}
type StreamValue struct {
    TimestampNs string
    Line        string
    Metadata    map[string]string
}

// Push posts to /loki/api/v1/push.
func Push(lokiURL string, streams []Stream) error
```

`Query`'s instant-query result normalizes Loki's `{metric, value: [ts,val]}`
shape into the same `Series{Values: [][2]string}` shape `QueryRange` uses
(with a single element), so callers that only care about "the one current
value" and callers walking a time series share one type.

## `internal/ratemeter` — port of `rate-meter.mjs`

Preserves exactly: the EWMA math (`alpha = 1 - exp(-ln2 * bucketS /
halfLifeS)`), the 300s bucket size, the three token fields summed
(`input_tokens`, `output_tokens`, `cache_creation_tokens`), the day-by-day
`query_range` stitching (Loki's per-query point cap), the warm-up window
(`max(6h, halfLifeS*8)`) before the last-published timestamp, and the
per-`halflife:email` dedup key in state so a restart never re-publishes/
duplicates a point.

```go
type PublishConfig struct {
    LokiURL      string
    HalfLifeS    int64
    HalfLifeLabel string // e.g. "20m" — goes into the stream label AND must
                          // match dashboard-generator's own RATE_HALFLIFE
    BackfillDays int
    DryRun       bool
}

// PublishRate computes and pushes the smoothed rate for every account with
// recent activity, using and updating state.RatePublished for dedup.
// Returns the number of points published (or, under DryRun, the number
// that WOULD have been).
func PublishRate(cfg PublishConfig, st *state.State, log func(string, ...any)) (int, error)
```

Internal helpers mirror the JS 1:1: `apiSelector(emailPattern) string`,
`escapeRegex(value) string`, `rateBuckets(...)`, `rateBucketsOver(...)`,
`ewma(points []Point, halfLifeS int64) []Point`.

## `internal/dashboardgen` — port of `dashboard-generator.mjs`

The dashboard JSON is manipulated as `map[string]interface{}` (Go's
`encoding/json` into a generic tree), mirroring the JS's approach of
reading the template as a plain object and mutating fields — deliberately
not modeling the Grafana schema as typed Go structs, since the generator
must round-trip *every* field in the template untouched except the
specific ones it's documented to change (typed structs would silently
drop unknown fields on marshal).

```go
type Config struct {
    LokiURL        string
    ExporterStream string
    RateHalfLife   string // e.g. "20m" — must match ratemeter's HalfLifeLabel
    GrafanaDir     string // .../grafana — templates/, account-limits.json, dashboards/accounts/
}

// GenerateDashboards discovers every account with data, skips ones on the
// ignore list, computes rate cutlines (P75/outlier/extreme via Tukey
// fences over the account's last 7 days, busy-bucket-only per the JS's
// "76% of non-zero points were tail" comment), pulls MCP servers + skill
// owners actually used, scopes the template per account, and writes/
// removes files under GrafanaDir/dashboards/accounts/ so the directory
// exactly matches accounts that currently have data.
func GenerateDashboards(cfg Config) error
```

Internal helpers mirror the JS 1:1: `slug`, `escapeRegex`, `discoverAccounts`,
`rateCutlines` (with the `CUTLINE_FALLBACK` constant and the `values.length
< 20` threshold), `skillOwners`, `mcpServers`, `loadLimits`/`limitsFor`,
`allPanels` (recursive panel walk including collapsed-row panels),
`assertNoPlaceholders` (fails loudly rather than writing a dashboard with
an unreplaced `__PLACEHOLDER__`), `applyCutlines`, `scopeAccount`.

## `internal/state` — small persisted state

```go
type State struct {
    RatePublished map[string]int64 `json:"ratePublished"` // "halflife:email" -> last published unix ts
}

func Load(path string) (*State, error) // missing file -> zero State, no error
func Save(path string, st *State) error // write-temp-then-rename, same as the JS
```

Default path: `<repo root>/.state/dashboard-server-state.json` — a new
file, separate from the JS collector's `.state/collector-state.json` (no
shared fields, no reason to collide).

## `cmd/dashboard-server/main.go`

Two independent loops, matching `collector.mjs`'s existing cadence split
(rate publishing is cheap and worth keeping fresh; dashboard regeneration
spawns several 7-30-day Loki queries per account, so it stays slower):

```
POLL_SECONDS (default 60)             -> ratemeter.PublishRate
DASHBOARD_INTERVAL_SECONDS (default 600) -> dashboardgen.GenerateDashboards
```

Flags: `--once` (one pass of each loop, then exit — matches
`node collector/dashboard-generator.mjs`'s existing by-hand usage),
`--dry-run` (rate publish computes but doesn't push; dashboard generation
is skipped entirely, matching `collector.mjs`'s current `dashboardPass`
under `DRY_RUN`).

Env vars (all optional, same names/defaults as today where they overlap):
`LOKI_URL` (default `http://localhost:47100`), `EXPORTER_STREAM` (default
`claude-code-exporter-1`), `RATE_HALFLIFE` (default `20m`, validated with
the same `^(\d+)([smh])$` pattern), `RATE_BACKFILL_DAYS` (default 14),
`POLL_SECONDS`, `DASHBOARD_INTERVAL_SECONDS`.

Like the wizard, requires being run from the `claude-observability` repo
root (same `docker-compose.yaml`-presence check) — it needs
`grafana/templates/claude-code.json`, `grafana/account-limits.json`, and
writes into `grafana/dashboards/accounts/`, none of which it bundles.

## Wizard changes (`setup/`)

Two changes to already-built code:

1. **Generalize `internal/service.Config`** from its current
   collector.mjs-specific shape (`NodeBin`, `ClaudeBin`,
   `CollectorScript()`) to a generic one any binary can use:
   ```go
   type Config struct {
       Label      string          // service identifier (e.g. "com.agents-observability.dashboard-server")
       Command    string          // absolute path to the executable to run
       Args       []string
       WorkingDir string
       Env        []envwriter.Var
       LogDir     string
   }
   ```
   `GeneratePlist`/`GenerateUnit`/`GenerateTaskArgs` change to use
   `cfg.Command`/`cfg.Args` instead of the hardcoded node-plus-script
   shape. This is required regardless of dashboard-server, since the
   future collector binary needs the exact same generic installer.

2. **`cmd/setup/main.go`'s "Collector service" step now installs
   `dashboard-server` instead of the old Node collector.** It no longer
   does `exec.LookPath("node")`/`exec.LookPath("claude")` — it looks for
   the `dashboard-server` binary (built alongside `claude-observability-
   setup` from the same release, or found via `exec.LookPath` if the user
   put it on PATH) and installs *that* as the background service. The
   prompt text changes from "Install the collector as a background service
   now?" to "Install the dashboard server as a background service now?"
   with a one-line note that transcript scanning / usage-truth aren't
   wired up yet (future spec).

## Testing

- `lokiclient`: `httptest.Server` returning fixed JSON for query/
  query_range/push; asserts request URLs/params built correctly and
  response parsing matches Loki's actual shape.
- `ratemeter`: `ewma` math tested directly (pure function, no I/O) against
  hand-computed values; `PublishRate` tested against an `httptest.Server`
  fixture covering: first-run backfill window sizing, warm-up window
  sizing, dedup (nothing before the last-published timestamp gets
  re-sent), and the multi-day stitching for a >1-day range.
- `dashboardgen`: `quantile`/cutline math tested directly against
  hand-picked datasets (including the `<20 samples -> fallback` branch);
  `scopeAccount` tested against a small fixture template asserting every
  documented substitution (`__EXPORTER_STREAM__`, `__HALFLIFE__`, the
  three `__CUT_*__` placeholders, the account/server/owner variables, the
  limit textboxes, the `__DASHBOARD__` drill-down links) with
  `assertNoPlaceholders` verified to fail on an intentionally-broken
  fixture; `allPanels` tested against a fixture with a collapsed row to
  confirm nested panels are found; `GenerateDashboards` end-to-end against
  an `httptest.Server` fixture plus a temp `GrafanaDir`, asserting the
  output directory exactly matches the fixture's accounts (including a
  stale file getting removed).
- `state`: load/save round-trip, missing-file-is-zero-value.
- CI: same `go test ./...` matrix pattern as `setup/`'s workflow, pointed
  at `dashboard-server/`.

## Open items for the future collector spec (not resolved here)

- `transcript-scan.mjs` port: state machine, shell tokenizer, token
  attribution, email resolution, skill resolution, dedup, batched Loki
  push with retry/backoff.
- `usage-truth.mjs` port, simplified: no calibration math (its data source
  died with `usage-meter`), just `claude auth status` + `claude -p
  /usage`, parse, publish `session_pct`/`week_pct`/reset text to Loki.
- The dashboard template's usage gauges need to read `usage-truth`'s
  direct percentages instead of `usage-meter`'s token math.
- Whether the wizard's "Collector service" step should offer *both*
  dashboard-server and the future collector, or fold them into one
  prompt.
