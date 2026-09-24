# Claude Observability

Local stack for tracking **coding-assistant CLI sessions** — how much of your
limit each session burns and how it is used. Today it ingests Claude Code; other
tools plug in later through their own adapters. Split out on its own from a
general dev-setup project once this stack outgrew being one piece of it — this
repo's only concern is getting telemetry flowing somewhere and turning it into
dashboards; today "somewhere" is always localhost, this repo's own stack.

Path: Claude Code → OTel Collector → Loki (logs) → Grafana. In parallel, two
self-contained Go binaries do the rest — see "The collector service" below
for why the split isn't arbitrary: the **collector** is a host process (not a
container) that reads the local transcripts and runs the `claude` CLI to
publish what OTel alone doesn't give you — the real MCP and skill names, and
the real `/usage` numbers. **dash-generator** publishes the consumption-rate
series and generates each account's dashboard, and runs in Docker, alongside
Loki and Grafana — it stands in for what would, centrally, be "the server":
a real deployment has every user's machine sending OTel only, with dashboard
generation happening once, centrally, not per laptop.

## Services

| Service         | URL / port                                         | Role |
|------------------|----------------------------------------------------|------|
| OTel Collector   | `localhost:47317` (gRPC), `localhost:47318` (HTTP) | single OTLP ingest endpoint |
| Grafana          | http://localhost:47300                             | dashboards (anonymous Viewer, `admin`/`admin` to edit) |
| Loki             | http://localhost:47100                             | logs and events, 90d retention |
| dash-generator   | — (Docker, internal only)                          | rate meter, per-account dashboard generation — stands in for "the server" |
| collector        | — (host process, not Docker)                       | real tool/skill names, `/usage` ground truth |

## Getting started

Bring the stack up yourself first — the wizard never does this for you. This
also builds and starts dash-generator (Docker, standing in for "the server"):

```sh
docker compose up -d               # otel-collector + loki + grafana + dash-generator
```

Then download the `claude-observability-wizard` and
`claude-observability-collector` binaries for your OS/arch from this repo's
[Releases page](../../releases) (or build them yourself: `cd src/wizard && go
build -o claude-observability-wizard ./cmd/wizard`, `cd src/collector && go
build -o claude-observability-collector ./cmd/collector` — put both in the
same directory), and run the wizard from the repo root:

```sh
./claude-observability-wizard
```

It **discovers on its own** which Claude Code config directories you have,
identifies the account behind each one, asks which ones you want to monitor,
asks for the OTel endpoint and an optional token (prefilled with the local
defaults — edit them if you're running docker on different ports), checks
that the endpoint is actually reachable before writing anything, enables
telemetry in the right shell/OS, and offers to install the collector as a
background service. No Node, Python, or other runtime needed to run either
binary — just the OS itself (Windows included).

### More than one account

It is common to keep accounts split by context (`~/.claude-personal`,
`~/.claude-work`), each with its own config directory. The first one is
`CLAUDE_DIR`; the rest go in `CLAUDE_OBSERVABILITY_EXTRA_DIRS`, colon-separated
like `$PATH` — `src/collector/internal/accounts` reads both directly, as plain
environment variables:

```sh
export CLAUDE_DIR="$HOME/.claude-personal"
export CLAUDE_OBSERVABILITY_EXTRA_DIRS="$HOME/.claude-work"
```

The wizard asks which directories to monitor and writes both into your shell
rc (see "Enabling telemetry in every session" — the same step, same file), and
also puts the accounts you did NOT pick on the `ignore` list in
`account-limits.json` — otherwise they would get an empty dashboard as soon as
they showed up in Loki.

Scanning only one directory is a **silent failure**: nothing breaks, the panels
just show a fraction of your usage. Here that hid 331 calls from a single MCP
server — the panel read 4 thousand tokens where the real number was 268 thousand.

## Enabling telemetry in every session

The wizard does this for you: bash/zsh rc, fish config, or on Windows,
persistent user environment variables (`setx`) — whichever matches your OS,
picked automatically. It writes the OTel telemetry vars
(`OTEL_EXPORTER_OTLP_ENDPOINT`, and `OTEL_EXPORTER_OTLP_HEADERS` if you gave
it a token) and the collector config (`CLAUDE_DIR`,
`CLAUDE_OBSERVABILITY_EXTRA_DIRS`, `EXPORTER_STREAM`) into the same block, one
place to look. Re-running the binary is a no-op if that block is already
present — edit it by hand to change values.

Open a new terminal (or reload your shell config). The first data point
arrives within a minute of the first prompt (that is the export interval; see
"Resource usage").

There is no `.env` file. Every knob covered on this page is a plain
environment variable, unset ones falling back to sensible built-in defaults
(documented inline where each is read):

| Var | Read by | Default |
|---|---|---|
| `CLAUDE_DIR`, `CLAUDE_OBSERVABILITY_EXTRA_DIRS` | collector | first discovered account dir |
| `EXPORTER_STREAM` | collector (shell rc), dash-generator (docker-compose.yaml) | `claude-code-exporter-1` |
| `LOKI_URL` | collector (shell rc), dash-generator (docker-compose.yaml) | `http://localhost:47100` / `http://loki:3100` in-container |
| `POLL_SECONDS` | collector, dash-generator (rate loop) | `60` |
| `DEDUP_DAYS`, `BATCH_SIZE`, `ORPHAN_AFTER_MS`, `EMAIL_LOOKBACK_HOURS`, `STATE_FILE` | collector | see `src/collector/cmd/collector/main.go` |
| `RATE_HALFLIFE`, `RATE_BACKFILL_DAYS`, `DASHBOARD_INTERVAL_SECONDS` | dash-generator | `20m`, `14`, `600` |

The wizard's shell-rc block only carries `CLAUDE_DIR`,
`CLAUDE_OBSERVABILITY_EXTRA_DIRS` and `EXPORTER_STREAM` — the ones it actually
asks about, and only for the **collector**. The rest are power-user knobs:
for the collector, set them in your shell before running it by hand, or add
them to the installed service file directly (see "The collector service"
below) if you want the background service to pick one up, since
`launchd`/`systemd`/Task Scheduler do not source your shell rc on their own —
only the vars the wizard explicitly baked in at install time are there. For
dash-generator, set them as `environment:` entries on its docker-compose.yaml
service instead and `docker compose up -d dash-generator` to apply.

## One dashboard per account — and nothing else

There is no "all accounts" dashboard. Each account has its own MCP servers,
plugins and configuration, and the numbers do not add up into anything useful.

`src/dash-generator` (the `dash-generator` Docker service) runs its
dashboard-generation loop on its own `DASHBOARD_INTERVAL_SECONDS` cadence
(default 10 minutes — see "The collector service"). The first time a new
account sends data, its dashboard shows up as **"Claude Code — <email>"**.

The single source is `grafana/templates/claude-code.json`. It deliberately sits
**outside** `grafana/dashboards/`: that is the provisioned directory, and a
template there would become one more dashboard with an empty account filter. The
generator reads the template and swaps what is account-specific:

- pins the account filter to one email;
- injects **that account's hourly P75 and outlier fence** as the rate cutlines;
- injects **that account's limit references** (see below);
- fills the MCP server and skill owner filters with what the account actually used.

Trigger a pass outside its normal 10-minute cadence:
`docker compose exec dash-generator claude-observability-dash-generator --once`.
Accounts that disappear from the data have their file removed on the next
run. The generated files contain emails, are specific to this machine, and
are gitignored.

## The limit windows

Anthropic enforces two windows, and **neither is a rolling window**:

- **5h block**: it opens on your first message and expires 5h later. The next
  block only opens on your next message. A `sum_over_time[5h]` would add the
  tail of one block to the head of the next — a different measurement
  entirely.
- **Week**: it resets on a fixed day and hour that is **per account**, not a
  global default (verified on two real accounts here: one resets Thursday
  evening, the other Saturday afternoon). A `sum_over_time[7d]` would drag in
  last week's consumption, and a single global guess at the reset day would
  silently measure the wrong 7-day window.

An earlier version of this stack tried to derive both windows from raw OTel
activity (finding the block boundary by scanning for the gap where the
previous block expired, guessing the weekly reset day). That derivation —
`usage-meter.mjs` — was dropped: it was a second, driftable measurement of
something Anthropic already answers directly and authoritatively. The
**collector**'s `usage-truth` step (see below) runs `/usage` via the `claude`
CLI itself, per account, every poll, and publishes its two top-line
percentages straight into Loki — no window math, no reset-day guessing, no
drift. The Overview gauges (both the main "% used" pair and the "/usage says"
pair — they read the same stream now) are a direct read of that value.

### The limit references

**Anthropic does not expose the limit through any telemetry**, and it varies by
plan. The references live in `grafana/account-limits.json`, per account, in new
tokens. That file is **not versioned** (it contains emails, same rule as the
generated dashboards) — copy the template the first time:

```sh
cp grafana/account-limits.example.json grafana/account-limits.json
```

This file is **purely user-set** — there is no more auto-calibration (that
died with `usage-meter.mjs`, its only data source). Accounts with no entry
fall back to the hardcoded default in `dashboardgen.go`
(`Block5h: 1_750_000, Week: 21_500_000`). To calibrate by hand: run `/usage`
on the account, note both percentages, compare them with what the collector
last published for that account (`{service_name="claude-code-usage-truth"} |
user_email = \`<email>\``) — which will already match, since it is the exact
same `/usage` call — and set `limit = observed_tokens / (usage_percentage /
100)` from whatever raw consumption number you're calibrating against.

### Why "new tokens"

The windows count **input + output + cache creation**. Cache reads are excluded:
they are ~97% of the raw volume and are not new spend, just the drag of context
you already paid for. Including them would pin the gauges at 100% permanently.

## The three data sources

### OTel — what Claude Code sends on its own

The event that matters is `claude_code.api_request`: **one line per API request**,
carrying `cost_usd`, the four token types, `model`, `effort`, `speed`,
`duration_ms`, `query_source`, `skill_name`, `session_id`, `prompt_id` and
`request_id`. Every panel reads from it.

It is exact per request, unlike the OTel metrics Claude Code also emits, which are
per-session counters that go stale ~5min after a session ends. The Collector still
accepts them (nothing breaks if a client sends them) but nothing stores or reads
them — every panel, and the account list itself, reads Loki (see `discoverAccounts`
in `src/dash-generator/internal/dashboardgen/dashboardgen.go`).

### The collector — what OTel redacts

Claude Code **redacts the names of locally configured MCP servers across all of
its OTel telemetry**: `mcp_server_name` becomes `custom` in the metrics, and since
2.1.x `tool_name` in the `tool_result` log becomes `mcp_tool`. Only
`mcp_server_scope` survives. Measured on this machine: **~85% of MCP tokens fell
into a single anonymous bucket**. It is not a bug and there is no env var to turn
it off — it is intentional privacy redaction, documented at
https://code.claude.com/docs/en/monitoring-usage.

The same redaction applies to **plugin skills**, which become `third-party` in
`api_request`'s `skill_name`.

The local transcripts keep both real names. The collector's `transcriptscan`
package (`src/collector/internal/transcriptscan`) scans every configured
Claude Code directory (see "More than one account") **recursively** and
publishes to `{service_name="$EXPORTER_STREAM"}`, with a `kind` label
separating `tools` (one line per `tool_use` block) from `skills` (one line per
skill-tagged request). The stream name is **versioned** — see "Rescan and
reimport" below.

#### How tokens are attributed to a tool

A tool's cost is **what the model paid to read its result**: the input side
(`input_tokens + cache_creation_input_tokens`) of the **next** assistant message
on the same track. With several tools called in parallel, that total is split
proportionally to each result's size.

`cache_read` is left out **on purpose**: what matters is the marginal cost of that
call, not its drag on later turns. That is why the exporter's number is much
smaller than the raw token counters Claude Code emits for the same server — those
count `cache_read` too, so they measure a different thing, and only the exporter's
number answers "what did this tool cost me".

Subagent tracks (`isSidechain`) are tracked separately from the main thread,
otherwise a subagent's first message would settle the attribution of a tool called
on the track above.

**Bash calls are broken down by the actual command run** (`git`, `ls`, `glab`,
…), not left as one opaque `Bash` bucket. This machine has the external `rtk`
tool wired into a `PreToolUse` hook that rewrites recognized commands before
they execute — `git status` becomes `rtk git status`, and some are remapped
entirely (`cat file` → `rtk read file`) — so the exporter strips a leading
`rtk` token before naming the command, otherwise almost every Bash call would
show up as "rtk" instead of what it actually ran. A compound line
(`git status && ls -la`) attributes the call's full tokens to **every**
sub-command, not a split between them — that is a deliberate over-count in
exchange for not hiding either command; see `ExtractBashCommands` in
`src/collector/internal/shelltok` and `Settle` in
`src/collector/internal/transcriptscan`. These rows join the same
"Tokens by tool" table MCP calls use, under a synthetic `bash` server bucket.

#### About the skill numbers

The skills panel reads the exporter's stream, but the **values come from OTel** —
the exporter republishes the `api_request` attribution, changing only the name, to
undo the plugin-skill redaction. That is what makes the skills table reconcile
exactly with the weekly breakdown: `dip-code-review` reads 2,840,009 on both
sides, and so on.

The opposite was tried first and was wrong: a skill can be activated
**proactively**, with no `Skill` tool call, and the transcript does not cover every
request (subagents and rotated sessions are missing). Transcript-based attribution
was off by −87% on one skill and −100% on two others. The transcript now only says
**which** plugin skill ran in each session, and the name is only restored when a
session used exactly one — with two, there is no way to tell which is which.

The MCP panel keeps its own transcript-based attribution (the marginal cost of
reading a result), which is a different measurement with no OTel equivalent.

#### Deduplication

Resuming a session makes Claude Code rewrite the entire history into a new
transcript: same `request_id`, same `tool_use_id`, different `session_id`. Without
filtering, every resume counts everything again. The key is `tool_use_id`, and the
map is seeded from Loki itself when empty — which also makes losing the state
volume harmless (it re-reads everything, recognises what is already there, and
writes zero duplicates).

#### What has to be scanned

Two traps in scanning the transcripts, both found after the numbers looked too
low:

- **More than one config directory.** Accounts split by context use different
  directories (`~/.claude-personal`, `~/.claude-work`). Scanning only one loses
  everything from the other, with no error at all.
- **Subagents live one level deeper.** Their transcripts are in
  `<session>/subagents/*.jsonl`. A single-level scan ignores them — that was 134
  files on one account alone.

Hence the recursive scan of every directory `src/collector/internal/accounts`
resolves (see "More than one account").

#### How each call's account is discovered

Transcripts do not record the account. There are two attempts, in order, and the
`account_source` field on each line records which one won:

1. **`otel`** — by `session_id`, querying the `api_request` event in Loki. This is
   the exact path.
2. **`project`** — transcripts go further back than the telemetry, and sessions
   older than this stack have no OTel event at all. For those, the directory's
   owner decides: if every already-attributed session of a project belongs to the
   same account, the orphans there belong to it too. A project with two accounts
   is left alone — the inference only acts when there is no ambiguity. The map is
   built from Loki history **and from the batch being imported**, otherwise a
   from-scratch import would never infer anything. Measured here: 1,231 of 1,492
   orphaned records recovered, 0 ambiguous.

What is left without an account (projects that never had a session with telemetry)
exists in Loki but does not show up in the per-account dashboards.

#### Rescan and reimport

Loki is append-only, and the collector stores an offset at the end of each
transcript — a restart re-reads nothing. Two operations cover this.

**Rescan** (`--rescan`): zeroes the offsets and re-reads everything while keeping
the dedup map. Use it to generate a **new** record type out of existing history
without rewriting anything:

```sh
cd src/collector && go run ./cmd/collector --rescan --once
```

**Reimport**: to redo the whole derivation (say, after changing how accounts or
tokens are attributed), bump the stream generation — the single source both
the collector and dash-generator read. Edit `EXPORTER_STREAM` in your shell rc
and re-run the wizard to reinstall the collector service with the new value
(see "The collector service"); edit it in docker-compose.yaml's
`dash-generator` service and `docker compose up -d dash-generator` to apply
it there. Then drop the state:

```sh
rm -rf .state                                          # collector's state
docker compose down -v dash-generator                  # dash-generator's state
docker compose up -d dash-generator
```

The old generation is orphaned and ages out with the 90-day retention.

> **Why not use Loki's delete API.** The temptation is to delete the stream and
> reimport under the same name. It does not work, and it fails silently: a delete
> request marks a time window and Loki starts **filtering it at query time** — the
> stream looks empty, but anything reimported with historical timestamps lands
> inside that window and is born invisible. Worse, a request that has already been
> processed **cannot be removed** (`deletion of request which is in process or
> already processed is not allowed`), so that window stays blind forever on that
> stream. Hence the generation in the name.

### usage-truth — the real numbers, straight from `/usage`

`/usage` is the account's own authoritative answer — immune to the three ways
a derivation could drift from what Anthropic's server actually enforces (a
stale calibration, a wrong week-boundary guess, or Claude Code usage from
another client/machine that never reached this stack's telemetry at all — see
"The limit windows" above for why an earlier, derived version of this existed
and was dropped). The collector's `internal/usagetruth` package runs `/usage`
directly, per account, every pass:

```sh
CLAUDE_CONFIG_DIR=<account's config dir> claude -p "/usage" --output-format json --no-session-persistence
```

This costs nothing to poll: `/usage` is answered locally by the CLI, never
sent to the model — confirmed `total_cost_usd: 0`, all token counts `0`,
~300ms. `CLAUDE_CONFIG_DIR` picks the account, the same directories the
transcript scan already reads (see "More than one account"), so there is no
separate account list to maintain.

It publishes one line per account to `{service_name="claude-code-usage-truth"}`:
`session_pct`, `week_pct`, and the raw `session_reset_text`/`week_reset_text`
Anthropic reports — e.g. `resets Sep 24 at 8pm (America/Sao_Paulo)`. Two
Overview panels (below) show these numbers next to the derived gauges, for a
direct comparison with no mental unit conversion.

One caveat baked into `/usage` itself, not this script: the "What's
contributing to your limits usage" breakdown further down its output is
explicitly scoped by Anthropic to *"local sessions on this machine — does not
include other devices or claude.ai"*. Only the two top-line percentages and
reset times — the ones this script actually uses — are the server-side,
account-wide numbers that actually throttle you; the breakdown is not, and
this script does not read it.

That is the whole loop now — no calibration write-back, no week-boundary
guess to correct. The two Overview gauges and the two "/usage says" panels
below read this exact same published value; see "The limit windows" above.

## The panels

Fifteen panels in five sections, two of them collapsed by default. Reading top to
bottom, they answer: *how much can I still spend* → *where is it going* → *is my
rate high right now*.

Two filters at the top, **MCP server** and **Skill owner**, both set to "All" by
default. They only affect the granular tables; the rest of the dashboard ignores
them.

### Overview

| Panel | What it decides |
|-------|-----------------|
| **5h block limit — % used** | How much of the current block is gone, straight from `/usage` (see "usage-truth" above). If no block is open it reads zero — it does not carry over from the last one. |
| **Weekly limit — % used** | How much of the weekly limit is gone since the reset, same source. |
| **5h — /usage says** | The same number again, lower on the page next to the other `/usage`-sourced panels — kept as a second read-out rather than removed. |
| **Weekly — /usage says** | Same, for the weekly window. |
| **Cache reuse** | Share of input that came from cache you already paid for. |
| **Tokens by model** (donut) | Where the consumption went. |

The gauges show **% used**, not % remaining, on purpose: it is the same reading as
`/usage`, so you can check one against the other without flipping it in your head.
The value is clamped at 100% — going over the limit is not "more than 100% of the
limit", it is simply blown. LogQL has no `clamp_max`, so the clamp comes from
`(vector(100) < x) or x`.

The donut is in **tokens, not dollars**: on a fixed-price subscription what runs
out is the limit, and the dollar figure decides nothing.

#### Section "Weekly breakdown" (collapsed by default)

One table with all of the week's consumption, broken down **from the widest scope
to the narrowest**: `Model | Effort | Source | Skill | % of weekly limit`. It is
the most granular cut of the overview — it answers *why* the limit is being
consumed — which is why it sits right below it, but collapsed: it is a lookup, not
daily reading.

It covers **all** consumption, not just what ran inside a skill: requests with no
skill show an empty Skill cell. That is why the footer total reconciles with the
weekly gauge — verified here: 30.37% in the table against 30.37% on the gauge.
This holds as long as the time range is the current week (the default); on another
range the sum becomes that period's.

There is no tool column: the event that accounts for 100% of consumption does not
carry which tool was used. That cut lives in the "Tokens by tool" table, with
its own attribution.

**The Skill column here is OTel's, redacted.** Unlike the "Tokens by skill" table,
which uses the real name from the transcript, a plugin skill appears here as
`third-party`. The reason is the reconciliation: only OTel accounts for 100% of
consumption, and mixing the two sources would break the sum. To find out which
skill is behind `third-party`, the Skills and tools table answers.

### Skills and tools

| Panel | What it decides |
|-------|-----------------|
| **Tokens by skill** (table) | Which skill consumes most, with its real name. |
| **Tokens by MCP server** (table) | Which server consumes most. |
| **Tokens by source** (donut) | Main thread, subagents, or auxiliary calls. |

All three are clickable, and each opens its own table in the collapsed
**"Breakdown"** section below:

| Click | Opens | Showing |
|---|---|---|
| a server name | **Tokens by tool** | what each specific call of that server (or Bash command) cost |
| a skill name | **Tokens by skill, model and effort** | which model and effort that skill ran on |
| a donut slice | **Tokens by model and effort** | the models behind that origin |

Same pattern as the weekly breakdown — the granular cut stays collapsed, right
below the panels it details.

#### How a click opens a collapsed section

A link sets the filter variable and adds `viewPanel=panel-<id>`, which opens that
one table full screen, already filtered; "Back to dashboard" returns. It has to
work that way because **a row's collapsed state does not exist in the URL** — it
lives in the dashboard JSON, so no link can expand a section. `viewPanel` does
resolve a panel that sits inside a collapsed row (verified on this Grafana), which
is what makes the drill-down possible at all.

The two drill-down variables, `skill` and `source`, are **textbox** variables, not
the `custom` dropdowns used for `server` and `owner`. A skill name can contain a
colon (`superpowers:brainstorming`) and Grafana re-parses a custom variable's
`query` on that character, truncating the value — the same trap that broke the
owner filter before.

`source` is not a label in the data: the donut separates main / subagent /
auxiliary with regexes over `query_source`. The table turns that same rule into a
real label with `label_format`, so the clicked origin can be filtered by a
variable. Its totals were checked against the donut's three queries and match to
the token: main 5,714,385, subagent 2,959,681, auxiliary 1,327,210.

**The skill drill-down reads the exporter's stream, not OTel** — the same source as
the "Tokens by skill" table above it, with the un-redacted names. Clicking a row
there always finds data here; against OTel a plugin skill would be `third-party`
and the click would land on nothing. Verified per skill: `code-review` 1,075,077 in
both, `superpowers:brainstorming` 63,290 in both.

The tables carry a bar inside the cell and come sorted highest first. Tables
rather than bar charts for two practical reasons: they sort natively, and they
give the name full width — in a bar chart the names were cut off after the first
few characters.

The top filters act here: **Skill owner** narrows to one plugin (e.g.
`superpowers`) or to local skills; **MCP server** narrows both the server summary
table and the tool table — picking a server collapses the summary to a single row,
which is the expected effect of clicking its name.

The source donut groups `query_source`, which arrives detailed in Loki
(`repl_main_thread`, `agent:builtin:general-purpose`, `agent_summary`, …):
`repl_main_thread` → **main**, `agent:*` → **subagent**, everything else →
**auxiliary**.

### Consumption rate

Two line charts in tokens/hour. This panel is a **speedometer**, not a budget
meter: it answers "am I fast or slow right now" so you can judge whether that
speed is warranted — a session firing many tools and MCP calls, or an agent that
quietly spawned 30 subagents and started accelerating on its own. It says nothing
about the limit; the gauges above do that.

The curve is an **exponentially weighted moving average** with a 20-minute
half-life (`RATE_HALFLIFE`), computed by dash-generator's `internal/ratemeter`
package and published into Loki, because LogQL has no EWMA.

It used to be `sum_over_time(tokens[15m]) * 4` computed directly in the panel.
That is a boxcar, and on real data it behaved badly enough to make the panel
useless:

| | boxcar | EWMA 20m |
|---|---|---|
| jitter between consecutive points | 451,185 tokens/h | **44,296** |
| peak ÷ P75 cutline | 9.9× | **6.0×** |
| when a session stops | 30× drop in one 5-min step | ~50-minute glide |

The jitter is why crossing the line meant nothing: the chart was a field of
needles, and the peaks towered so far above the cutline that the line sat squashed
at the floor. And the cliff misrepresented reality — stopping does not mean you
were instantly slow, it means you decelerated.

The half-life is part of the **stream labels** on purpose. Loki cannot replace
derived data, so changing it would otherwise mix two different maths in one
series; as a label, a new value simply starts a fresh series and the old one ages
out with retention.

The rate-meter backfills the series on its first run (14 days by default,
`RATE_BACKFILL_DAYS`). The history is not lost — this Loki accepts old samples on
purpose.

### The two cutlines

Both are computed by the `dashboard-generator` over **that account's** last 7
days, as quantiles of the very same smoothed curve the panel draws:

| Line | What it is | What it means |
|---|---|---|
| green | P75 | Above it you are in the busiest quarter of your own normal. |
| orange | Q3 + 1.5×IQR (Tukey's inner fence) | Not "busy" any more — out of pattern. |
| red | Q3 + 3×IQR (Tukey's outer fence) | The textbook "far out" point. Worth looking at what ran there. |

**The curve itself changes colour with the band it is in**: white below P75 (your
usual pace), green, orange, red.

The colouring is not a gradient. The panel draws the same curve four times: a
white base, then one series per band filtered in LogQL to the samples above
that band's cutline (`sum(...) > 986651`). A comparison in LogQL drops the
samples that fail it, so with `spanNulls: false` each overlay renders only the
stretch that actually crossed, in one flat colour, and the cutline the query
filters on is the same number the dashed line is drawn at. The overlays are line
only, with no points of their own -- Grafana already draws a point under the
cursor on the base curve, which is the only moment one is useful. The cost is
that a crossing lasting a single 5-minute bucket has no segment to draw and so
goes uncoloured: 3 of the 27 crossings in a measured week, with the white curve
still showing the spike. The overlays are hidden from the legend and the
tooltip, being the same curve recut.

There are three cutlines rather than two because with a single fence the top band
ran from the fence all the way to the maximum — a 3.3x span on real data, so a
mild peak and an extreme one were painted the same colour and the top band stopped
meaning anything. Tukey defines both fences, so the second one is not an invented
threshold: 1.5×IQR is an outlier, 3×IQR is "far out".

Measured over 7 days, on two accounts:

| | white | green | orange | red | top band span |
|---|---|---|---|---|---|
| personal | 90.3% | 7.0% | 1.5% | 1.2% | 2.3× (was 3.3×) |
| work | 92.1% | 6.5% | 1.2% | 0.2% | 1.0× (was 3.3×) |

Most of a week is idle or coasting down, and the cutlines are quantiles of
*working* time, so the colours only light up while you are actually going.

The input/output panel keeps fixed per-series colours instead (blue and purple),
because there colour has to tell the two series apart.

They are computed **only over buckets that actually contained requests**. An EWMA
never quite reaches zero, so after a busy stretch it leaves a long tail of small
positive values — measured here, 76% of the "non-zero" points were tail rather
than work. Taking quantiles over that collapses the line (P75 fell from 560k to
274k and the peaks went to 12× above it). Masking by real activity also matches
what the line is supposed to mean: *faster than I usually run **while working***.

The values vary a lot between accounts — 555k/1.01M on one, 1.49M/3.25M on another
— which is why they are per account rather than constants.

The input/output panel has cutlines **of its own per series**, computed only over
that series. Input and output differ by an order of magnitude, so using the
total's cutline there would compare different things.

The cutlines must be quantiles of the curve actually drawn — that has burned
this dashboard before: an earlier version computed them over 1h windows while
the graph drew 15min ones, which put the line at 782k under a curve whose real
P75 was 1.09M — a line that lies. (An older version of this stack also had to
keep `RATE_HALFLIFE` in sync across two separate scripts that both read it;
dash-generator now reads it once and passes the same value to both the rate
publisher and the dashboard generator, so that class of bug can't recur.)

### Time range

The default is **`now/w+9h+24h` → `now`** (current week, from Monday 9am). The
picker also offers *Current day* (`now/d+9h`), *5 hours*, *24 hours*, *7 days* and
*30 days*.

The week rather than the day because **skills and MCP calls are sparse**: on a day
with no MCP use, or no skill run, those panels opened empty — and a dashboard that
opens empty is worth nothing. The week keeps the "right now" framing and still has
something to show.

The limit gauges **ignore this picker**: their windows are Anthropic's, not the
analysis window.

## Adding other tools later

- **GitHub Copilot** — no local telemetry. Poll the Copilot Metrics API with a
  small scraper exposing `/metrics`.
- **OpenAI Codex CLI** — no native OTEL. Parse its session JSONL.
- Give every adapter a `tool="…"` label.

## The collector service

`src/collector` (transcript scanning + usage-truth, every `POLL_SECONDS`,
default 60s) is the one piece of this stack that runs as a host background
service, not a container. It needs two things a container doesn't have: the
host's own logged-in `claude` CLI (for `usage-truth`), and the real Claude
Code config directories at their real host paths (for the transcript scan) —
no container mount replaces either. (dash-generator has no such requirement —
it only talks to Loki — so it runs in Docker; see "Services" above.)

**Configuration is plain environment variables, not a file.** The wizard
installs it as a native background service — `launchd` on macOS, `systemd
--user` on Linux, a Scheduled Task on Windows — and bakes the vars it already
collected (`CLAUDE_DIR`, `CLAUDE_OBSERVABILITY_EXTRA_DIRS`, `EXPORTER_STREAM`,
and the directory of `claude` on the wizard's own `PATH`) directly into the
service definition at install time, since none of the three service managers
source your shell rc on their own. See "Enabling telemetry in every session"
above for the full knob list and how to add one to an already-installed
service by hand.

Re-run the wizard to reinstall (overwrites the existing service definition
with fresh values — e.g. after editing your shell rc, moving the `claude-
observability-collector` binary, or `claude` itself moving — a Homebrew/nvm
upgrade, say). To manage the service directly:

| | macOS (`launchd`) | Linux (`systemd --user`) | Windows (Task Scheduler) |
|---|---|---|---|
| label/name | `com.claude-observability.collector` | `claude-observability-collector.service` | `ClaudeObservabilityCollector` |
| status | `launchctl list \| grep claude-observability` | `systemctl --user status <name>` | `schtasks /query /tn <name>` |
| stop | `launchctl bootout gui/$UID <plist path>` | `systemctl --user stop <name>` | `schtasks /end /tn <name>` |
| uninstall | stop, then `rm ~/Library/LaunchAgents/<label>.plist` | `systemctl --user disable --now <name>` then `rm ~/.config/systemd/user/<name>` | `schtasks /delete /tn <name> /f` |

Logs land in `.state/claude-observability-collector.log` / `.err.log`. By
hand, for testing:

```sh
cd src/collector
go run ./cmd/collector --once      # one pass, then exit
go run ./cmd/collector --dry-run   # writes nothing to Loki — reads only
```

dash-generator's equivalents, since it runs in Docker: `docker compose logs
-f dash-generator`; trigger a pass by hand with `docker compose exec
dash-generator claude-observability-dash-generator --once` (`--dry-run` works
the same way).

## Stop / reset

```sh
docker compose down                          # stop grafana/loki/otel-collector/dash-generator, keep the data
docker compose down -v                       # same, and wipe it
```

Stop or uninstall the collector service using the table above.

## Known traps

**A published series needs a lookback window, not `$__interval`.** The rate series
is published every 5 minutes. Querying it with `last_over_time(... [$__interval])`
looks back only as far as the graph's own step, which on a wide panel is 30s — so
19 steps out of 20 find nothing. Grafana then declares the frame's interval as 30s,
sees points 300s apart, and inserts nulls between them; with `spanNulls: false` and
`showPoints: never`, the entire line disappears while the tooltip still reports
values. The fix is a fixed `[10m]` window (always covers at least two published
points) plus `interval: 5m` on the panel so Grafana does not over-sample a series
that only has 5-minute resolution.

**A transparent base threshold makes the line invisible.** Grafana's default field
color mode is `thresholds`, so the line takes the colour of whichever band its
value falls in — which the rate panel relies on. But the base step must be a real
colour. It used to be `transparent` (so the threshold band would not tint the
chart), which drew every value below the first cutline as nothing at all. The panel
looked blank while its tooltip still showed values, and it only surfaced once the
curve was smoothed: the old spiky one crossed the cutline constantly, so coloured
fragments stayed visible.

**Colouring a line by threshold band is not what `color.mode: thresholds` does.**
Two modes both look right and are both wrong. With `gradientMode: "opacity"`
Grafana resolves the field colour **once for the whole series**, so the rate
curve came out uniformly green no matter how many peaks crossed the cutlines.
With `gradientMode: "scheme"` it colours per point, but as a *gradient*: it
interpolates between the threshold colours instead of stepping at them, so the
curve turns into a rainbow and a point still well below the red cutline is
already drawn reddish. Neither mode paints "the part of the line above the
line". Overlaying one filtered series per band does, which is what this panel
now uses, at the cost of one extra query per band.

**`allowUiUpdates` must be `false`.** With `true`, the first time a dashboard is
touched through the UI, Grafana unlinks it from provisioning (`meta.provisioned`
goes `false`) and serves the database copy forever — the file changes and nothing
happens. Since these dashboards are regenerated on a schedule, the database copy
must never win. To check what is actually being served (not just what is on disk):

```sh
curl -s -u admin:admin http://localhost:47300/api/dashboards/uid/cc-<slug> \
  | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d["meta"]["provisioned"], d["dashboard"]["version"])'
```

**None of the three service managers source your shell rc.** The collector's
`CLAUDE_DIR` and friends, and `claude` needing to be on `PATH` for
`usage-truth`, only exist because your shell rc sets them up. The wizard
avoids the old workaround of re-invoking your shell at install time
(brittle — see the git history of this file for what that used to cost) by
baking the vars it already collected directly into the service definition
(the plist's `EnvironmentVariables`, the systemd unit's `Environment=` lines,
or the Scheduled Task's `set VAR=value&&` prefix) at install time. Re-run the
wizard after editing your shell rc or moving a binary — neither is watched
for changes afterward.

**An `instant` query breaks series names.** Loki returns `numeric-multi` frames
for instant queries; Grafana merges those frames into one and renames the fields
to `Value #A`, discarding the name from `legendFormat`. Every panel uses `range`
except the weekly breakdown table — there, `range` would render one row per graph
step instead of one per combination.

## Resource usage

The stack runs all day on a developer machine, so what matters is not peak CPU —
it is **how often things wake up**. Every periodic task keeps the CPU from going
idle, and that is battery.

The intervals are tuned for the data to be useful, not instantaneous:

| Component | Interval | Why |
|---|---|---|
| Claude Code → OTel Collector | 60s (metrics), 30s (logs) | runs in EVERY session; was 10s/5s |
| `collector` poll loop (host) | 60s | transcript scan, usage-truth — both cheap |
| `dash-generator` rate loop (Docker) | 60s | EWMA rate publish — cheap |
| `dash-generator` dashboard loop (Docker) | 600s | spawns 7–30 day Loki queries per account; does not need to be fresher |
| Grafana re-provision | 300s | re-parses every dashboard on disk |
| Loki compactor | 600s | the image's default |
| Dashboard auto-refresh | 300s | each refresh fires ~14 Loki queries |

The collector's poll loop is 60s, not the 120s an earlier version used — a
deliberate choice to keep transcripts and the `/usage` ground truth fresh.
The dashboard loop stayed at 600s on purpose rather than following it down:
each pass spawns several 7-to-30-day Loki queries per account for numbers
(P75, outlier fences, MCP/skill lists) that do not meaningfully change minute
to minute — collapsing it to 60s too would be 10x the Loki load for no fresher
information. One fewer periodic task since the pre-Go version, too: Prometheus
(and its 60s scrape) was dropped entirely — it only ever existed to list
accounts, which is now a Loki query (`discoverAccounts` in
`src/dash-generator/internal/dashboardgen/dashboardgen.go`). Grafana also has
unified alerting (which keeps a scheduler running even with zero rules),
version checks and analytics turned off.

At rest the stack sits around **700 MiB** with CPU near zero. If you need fresher
data occasionally, raise the refresh in the Grafana tab rather than lowering these
intervals again.

## Notes

- Host ports: 47300 (Grafana), 47317/47318 (OTLP), 47100 (Loki). All published
  on `127.0.0.1`, not `0.0.0.0` — Grafana runs anonymous and the Loki API has no
  authentication, so binding them to every interface would expose both to anyone
  on the same network.
- There is no Prometheus in this stack. It used to exist only to list which
  accounts had sent telemetry (via `label_values(user_email)`, cheap because
  `resource_to_telemetry_conversion` turned Claude Code's resource attributes
  into real Prometheus labels); that list now comes from a Loki query instead
  (`discoverAccounts` in `src/dash-generator/internal/dashboardgen/dashboardgen.go`),
  following the same pattern the rate meter already used to find every account
  with telemetry. No panel ever read from Prometheus.
- `config/loki-config.yaml` deviates from the image's default in four places, all
  commented in the file: it accepts old samples (backfill), removes the query
  window cap, enables 90d retention, and raises `ingester.max_chunk_age` — without
  that last one Loki rejects every historical backfill with HTTP 400 as soon as the
  stream has a recent line. `config/otel-collector-config.yaml` sits next to it —
  both moved out of the repo root into `config/` to keep it to code and docs.
