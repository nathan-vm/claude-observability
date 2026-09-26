# Claude Observability

**Self-hosted usage dashboards for Claude Code.** Track how much of your
rate limit each session burns, and where it goes — by model, skill, MCP
server, and tool — on your own machine, with your own Grafana.

Today it ingests **Claude Code** sessions. Other CLI coding assistants can
plug in later through their own adapters — see [Adding other tools
later](#adding-other-tools-later).

## Why this exists

Claude Code emits usage telemetry, but three things are missing from it
out of the box:

- **The actual limit.** Anthropic enforces a 5-hour and a weekly cap, but
  never tells you where the ceiling is through telemetry — only `/usage`
  does, and only as a percentage.
- **MCP server and skill names.** Claude Code redacts them from telemetry
  by design (privacy), so raw OTel data shows `custom` and `third-party`
  instead of which server or skill actually ran.
- **A per-account view.** If you split work across multiple Claude
  accounts (personal, work, …), nothing separates their usage for you.

This stack closes those three gaps: it reads your local session
transcripts (which still have the real names), asks `/usage` directly for
the real limit numbers, and generates one dashboard per account.

## Architecture

```
Claude Code                        collector (host service, every 60s)
    │  OTLP (metrics + logs)           │  reads transcripts,
    ▼                                  │  runs `claude /usage`
OTel Collector                         │
    │  push                            │  push — direct, skips OTel Collector
    └────────────────┬─────────────────┘
                      ▼
                    Loki
          (append-only store, 90d retention)
                      │  ▲
   live LogQL queries │  │ writes the computed
   on every dashboard  │  │ rate (EWMA) back in
   view/refresh        │  │
                      ▼  │
                 dash-generator
        • rate loop (60s): reads Loki → EWMA → writes to Loki
        • dashboard loop (600s): reads Loki → writes dashboard JSON
                      │
                      ▼  writes JSON files to grafana/dashboards/
                  Grafana
        (loads the JSON from disk, then queries
         Loki live for each panel's actual data)
```

Two separate paths write to Loki — they never touch each other: what
Claude Code sends over OTLP through the OTel Collector, and what the
**collector** pushes directly by reading your local session transcripts
and running `claude /usage` (the only way to recover what OTel redacts).
Loki itself only stores and indexes — **dash-generator** does the actual
work on the read side: it computes the smoothed rate series and writes it
back into Loki, and separately regenerates each account's dashboard
*definition* as a JSON file on disk. Grafana picks that file up
automatically, but the numbers you see aren't baked into it — every panel
holds a LogQL query, and Grafana asks Loki for fresh data each time you
open or refresh a dashboard.

## Components

Everything below runs locally today. The **runs where** column shows what
a real multi-user deployment looks like: every user's machine only ever
sends OTLP telemetry, and dashboard generation happens once, centrally.
Locally, there's no separate server yet, so `dash-generator`'s Docker
container plays that role.

| Component | Runs where | Why it exists |
|---|---|---|
| **OTel Collector** | Server (Docker) | Single ingest endpoint every Claude Code session sends telemetry to. |
| **Loki** | Server (Docker) | Stores both the OTel events and what the collector extracts from transcripts. |
| **Grafana** | Server (Docker) | Serves the generated per-account dashboards. |
| **dash-generator** | Server (Docker) — stands in for "the server" | Computes the consumption-rate series and generates each account's dashboard from Loki data. In a real deployment this runs once, centrally, not per laptop. |
| **collector** | Client (your machine, host process — not Docker) | Reads your local Claude Code session transcripts to recover the real MCP/skill names OTel redacts, and calls `claude /usage` for the real limit numbers. Needs your logged-in `claude` CLI and your real config directories — a container has access to neither. |
| **wizard** | Client (your machine, run once) | Detects your Claude accounts, turns on telemetry, and installs the collector as a background service. Never touches Docker. |

## Quick start

**1. Start the server-side stack.** The wizard doesn't do this for you:

```sh
docker compose up -d
```

This pulls the published `dash-generator` image and starts it alongside
`otel-collector`, `loki`, and `grafana`.

**2. Get the two client binaries.** Download the zip for your OS/arch
from this repo's [Releases page](../../releases) — each one bundles
`claude-observability-wizard` and `claude-observability-collector`
together — or build them yourself:

```sh
cd src/wizard && go build -o claude-observability-wizard ./cmd/wizard
cd src/collector && go build -o claude-observability-collector ./cmd/collector
```

Unzip (or place both built binaries) into the same directory.

**3. Run the wizard** from the repo root:

```sh
./claude-observability-wizard
```

It finds your Claude Code accounts, asks which ones to monitor and where
to send telemetry, verifies the endpoint is reachable before writing
anything, turns on telemetry for your shell/OS, and offers to install the
collector as a background service. No runtime dependency beyond the OS
itself — Windows included.

**4. Open Grafana** at http://localhost:47300. A dashboard named
**"Claude Code — \<email\>"** appears within a few minutes of your first
prompt in a new terminal.

## Services

| Service | Address | Purpose |
|---|---|---|
| OTel Collector | `localhost:47317` (gRPC), `localhost:47318` (HTTP) | OTLP ingest |
| Grafana | http://localhost:47300 | Dashboards (anonymous Viewer; `admin` / `admin` to edit) |
| Loki | http://localhost:47100 | Storage, 90-day retention |
| dash-generator | internal only | Rate metering + dashboard generation |
| collector | — (host service) | Transcript scan + `/usage` polling |

All ports are published on `127.0.0.1` only — see [Is my data sent
anywhere?](#faq)

## Configuration

There's no `.env` file — every knob is a plain environment variable, with
a sensible default.

| Variable | Used by | Default |
|---|---|---|
| `CLAUDE_DIR`, `CLAUDE_OBSERVABILITY_EXTRA_DIRS` | collector | first discovered account dir |
| `EXPORTER_STREAM` | collector, dash-generator | random ID set by the wizard at install |
| `LOKI_URL` | collector, dash-generator | `http://localhost:47100` (host) / `http://loki:3100` (in-container) |
| `POLL_SECONDS` | collector, dash-generator | `60` |
| `RATE_HALFLIFE`, `RATE_BACKFILL_DAYS`, `DASHBOARD_INTERVAL_SECONDS` | dash-generator | `20m`, `14`, `600` |
| `DEDUP_DAYS`, `BATCH_SIZE`, `ORPHAN_AFTER_MS`, `EMAIL_LOOKBACK_HOURS`, `STATE_FILE` | collector | see `src/collector/cmd/collector/main.go` |

The wizard writes `CLAUDE_DIR`, `CLAUDE_OBSERVABILITY_EXTRA_DIRS`, and
`EXPORTER_STREAM` into your shell rc (or Windows environment) for you.
Everything else is a power-user knob: set it in your shell before running
the collector by hand, or edit `docker-compose.yaml` for dash-generator's
settings.

### Multiple accounts

Common when work is split by context (`~/.claude-personal`,
`~/.claude-work`). The wizard asks which directories to monitor and each
gets its own dashboard. Accounts you don't pick go on the `ignore` list in
`grafana/account-limits.json`, so they don't show up as empty dashboards
the moment they send telemetry.

## Dashboards

One dashboard per account, generated automatically by dash-generator every
`DASHBOARD_INTERVAL_SECONDS` (10 minutes by default) from the shared
template at `grafana/templates/claude-code.json`. There's no combined
"all accounts" view — see [the FAQ](#faq) for why.

Each dashboard has five sections, read top to bottom as: *how much can I
still spend* → *where is it going* → *is my rate high right now*:

| Section | Answers |
|---|---|
| **Overview** | % of the 5-hour and weekly limit used, straight from `/usage` |
| **Weekly breakdown** | The week's consumption by model, effort, source, and skill, each row's share of your weekly limit scaled from /usage's real percentage |
| **Skills and tools** | Which skill or MCP server consumes the most, with drill-downs |
| **Consumption rate** | Tokens/hour right now, against your own historical pace |
| **Time range picker** | Defaults to the current week — skill/MCP activity is sparse, so a single day is often empty |

## Managing the stack

```sh
docker compose down       # stop, keep data
docker compose down -v    # stop and wipe all data — irreversible
```

The collector runs as a native background service (installed by the
wizard), not a container:

| | macOS (`launchd`) | Linux (`systemd --user`) | Windows (Task Scheduler) |
|---|---|---|---|
| status | `launchctl list \| grep claude-observability` | `systemctl --user status claude-observability-collector` | `schtasks /query /tn ClaudeObservabilityCollector` |
| stop | `launchctl bootout gui/$UID <plist path>` | `systemctl --user stop claude-observability-collector` | `schtasks /end /tn ClaudeObservabilityCollector` |
| uninstall | stop, then remove the `.plist` | `systemctl --user disable --now claude-observability-collector` then remove the unit file | `schtasks /delete /tn ClaudeObservabilityCollector /f` |

Re-run the wizard any time to reinstall the service with fresh values —
e.g. after moving a binary or editing your shell rc.

Logs: `.state/claude-observability-collector.log` (collector),
`docker compose logs -f dash-generator`.

## Troubleshooting

- **A dashboard doesn't reflect edits to the template.** Don't edit
  dashboards through the Grafana UI — the first UI edit unlinks a
  dashboard from provisioning and Grafana serves that stale copy forever.
  Edit `grafana/templates/claude-code.json` and let dash-generator
  regenerate it.
- **Only scanning one account directory.** This fails silently — no
  error, just a dashboard showing a fraction of real usage. Check
  `CLAUDE_OBSERVABILITY_EXTRA_DIRS` is set if you use more than one
  account.
- **Wizard says the OTel endpoint is unreachable.** Run
  `docker compose up -d` first — the wizard never starts the stack for
  you, and it refuses to write config against an endpoint it can't reach.

## Adding other tools later

- **GitHub Copilot** — no local telemetry; poll the Copilot Metrics API.
- **OpenAI Codex CLI** — no native OTel; parse its session JSONL.
- New adapters should tag their data with `tool="…"` so it can sit next to
  Claude Code's without conflicting.

## FAQ

**Why does dash-generator run in Docker instead of on my machine, like the collector?**
Because in a real multi-user deployment it has to run once, centrally,
reading everyone's Loki data and writing everyone's dashboards — not
duplicated per laptop. Locally there's no separate server yet, so its
Docker container plays that role, alongside Loki and Grafana.

**Why does the collector need to run on my machine instead of in Docker?**
It needs two things no container has: your logged-in `claude` CLI (to run
`/usage`), and your real Claude Code config directories at their real host
paths (to read transcripts).

**Why do MCP server and skill names show up as `custom` / `third-party`?**
Claude Code intentionally redacts locally configured MCP server and
plugin skill names from telemetry, for privacy — documented behavior, not
a bug, and there's no environment variable to disable it. The collector
recovers the real names from your local transcripts, which Claude Code
doesn't redact.

**Why is there no "all accounts" dashboard?**
Each account has its own MCP servers, plugins, and limit. Summed numbers
across accounts wouldn't answer a real question, so the stack deliberately
doesn't generate one.

**How is the Weekly breakdown's percentage column computed, if there's no configured limit anymore?**
Each row's tokens (input + output + cache creation, over the selected time
range) are weighted by that row's share of the account's trailing 7-day
token total, then scaled by the real `week_pct` that `/usage` reports. Set
the range to 7 days and the rows sum to exactly `week_pct`. There's nothing
to calibrate by hand — see the panel's own description in Grafana for the
proportionality assumption it makes.

**Why doesn't the weekly window just use a rolling 7-day sum?**
Each account's reset day and hour is personal, not a global default —
different across accounts in practice. A rolling 7-day sum would silently
measure the wrong window. The collector sidesteps the problem entirely by
asking Anthropic's own `/usage` endpoint instead of deriving the window.

**Does polling `/usage` cost tokens or money?**
No — it's answered locally by the CLI and never reaches the model.

**Why exclude cache reads from the consumption numbers?**
Cache reads are the reused cost of context you already paid for, not new
spend, and they dominate raw token volume on a typical session. Including
them would pin every gauge near 100% permanently and tell you nothing
about actual limit consumption.

**Why is the rate chart an exponentially weighted average instead of a simple moving average?**
A boxcar average (a fixed lookback window, recomputed each step) was
tried first and was unusable in practice: far noisier between points, and
it dropped to zero the instant a session stopped instead of reflecting
that the session was winding down. See
`src/dash-generator/internal/ratemeter`.

**Can I reprocess my history after changing how something is attributed?**
Two options: `--rescan` re-reads every transcript while keeping the
existing dedup and account cache — use it when transcripts have new data
the last pass hadn't reached yet. To fully redo the attribution logic
itself, start a fresh `EXPORTER_STREAM` (see
`src/wizard/internal/streamid`) and drop local state, then let both
services rebuild from scratch.

**Is my data sent anywhere?**
No. Every service publishes only on `127.0.0.1`. Grafana runs with
anonymous Viewer access and Loki has no authentication at all — safe on
localhost, not safe on a shared network, which is why nothing is exposed
beyond loopback.

**What does this cost to run all day?**
Around 700 MiB at rest, near-zero CPU. Poll intervals are tuned for
freshness where it matters (60s for the collector and the rate loop) and
left slower where it doesn't (600s for full dashboard regeneration, which
runs multi-day Loki queries per account).
