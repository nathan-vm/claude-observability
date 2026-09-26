# claude-observability — Claude Code conventions

Self-hosted Claude Code usage dashboards (Go monorepo: `src/wizard`,
`src/collector`, `src/dash-generator`). See `README.md` for what the
project does; this file is about how to work on it with Claude Code.

## Release conventions (trunk-based)

- Every commit reaching `main` is a squash-merge commit — its subject is
  the PR title, and `.github/workflows/pr-title.yml` enforces it's a
  Conventional Commit (`feat fix docs style refactor perf test build ci
  chore revert`).
- Every push to `main` with a releasable commit (`feat`/`fix`/`perf`/
  `refactor`/breaking) auto-publishes: `.github/workflows/release.yml`
  computes the next version from git tags (no `VERSION` file — the tag
  *is* the version), updates `CHANGELOG.md`, tags, and publishes a
  GitHub Release with the three binaries.
- Never hand-edit a version anywhere — there's nowhere to edit; it's
  derived from `v*` git tags at release time.
- See `docs/superpowers/specs/2026-09-24-trunk-based-release-design.md`
  for the full design.

## Worktree + docker isolation

- Non-trivial work happens in an isolated worktree:
  `scripts/worktree-add.sh <tipo> <descrição-kebab-case>` creates
  `.worktrees/<tipo>-<descrição>` on branch `<tipo>/<descrição>` from
  `origin/main`, and already generates that worktree's own
  `docker-worktree.local` (isolated ports + `COMPOSE_PROJECT_NAME` — see
  `scripts/docker-worktree-env.sh`) so its docker-compose stack never
  collides with the main checkout's or another worktree's.
- Bring up a worktree's stack with:
  `docker compose -f docker-compose.dev.yaml --env-file docker-worktree.local up -d --build`
- The **collector** is a host process (reads local transcripts, runs the
  logged-in `claude` CLI) — it never runs in docker-compose, isolated or
  not.
- See `docs/superpowers/specs/2026-09-24-claude-multiagent-worktree-design.md`
  for the full design.

## Testing against Loki

There are two stacks on purpose. `docker-compose.yaml` is the real one
(47xxx ports, the data you actually care about). `docker-compose.dev.yaml`
is a standalone throwaway — 40xxx defaults, its own `name:` and volumes,
and a `dash-generator` built from local source instead of the published
image. A worktree's generated `docker-worktree.local` shifts the dev ports
again so several worktrees can run at once.

- **The 47xxx stack is read-only for testing.** Query it freely —
  `curl -G .../loki/api/v1/query`, `query_range`, `labels`, `series`.
  Reading real data is how you verify a LogQL change is actually correct,
  and it is safe. Anything that *writes* belongs on the dev stack.
- **`dash-generator` has no read-only mode.** `--once` runs a rate-publish
  pass (`ratemeter.PublishRate`, which pushes EWMA points into Loki)
  *before* generating dashboards, and `--dry-run` skips dashboard
  generation entirely — so no flag combination generates without writing.
  Never point it at 47100. With a fresh `STATE_FILE` it also backfills
  `RATE_BACKFILL_DAYS` (14) of history, which is how ~8k stray rate points
  once landed in the real Loki.
- **`collector` writes too**: `PublishUsageTruth` pushes to Loki. To
  exercise the `/usage` probe without writing anything, call
  `usagetruth.FetchUsage` / `AccountEmail` directly from a test — they only
  shell out to `claude` and parse the result.
- **A fresh dev Loki generates zero dashboards.** `GenerateDashboards`
  derives its account list from Loki itself (`discoverAccounts`), so an
  empty instance produces nothing to inspect. Seed it first: read a slice
  out of the real Loki with `query_range` and push that into the dev one.
  Reading from real + writing to dev is the safe combination.
- **Verify a dashboard query from the generated JSON, not from the
  template.** Extract the panel's `expr` out of
  `grafana/dashboards/accounts/*.json`, substitute `$account` and
  `$__range`, and run it — retyping the query by hand cannot catch a
  JSON-escaping slip, and no Go test covers the template's LogQL at all.
  Assert an invariant you can state out loud: e.g. the weekly-breakdown
  rows must sum to exactly the account's own `week_pct` when `$__range` is
  `7d`.
- **Structured metadata is part of query-time series identity.** `unwrap`
  promotes it to result labels, so a bare
  `sum(last_over_time({...} | unwrap week_pct [30m]))` fans out into one
  series per distinct metadata combination and double-counts. Group
  explicitly — `sum(last_over_time(...) by (user_email))`. Only put numeric
  unwrap targets in metadata; free text belongs in the log line.
- Tear the dev stack down with
  `docker compose -f docker-compose.dev.yaml --env-file docker-worktree.local down -v`.
  `-v` is safe and wanted there — which is exactly why it must never be
  typed against `docker-compose.yaml`, where it wipes 90 days of metrics
  with no confirmation.

## Multi-agent workflow

For any non-trivial feature, fix, or refactor, invoke the `orchestrator`
skill instead of implementing directly. It runs: `planner` (produces a
real plan via Superpowers `brainstorming`/`writing-plans`, saved to
`docs/superpowers/plans/`) → creates the isolated worktree → `developer`
(implements, `go build`/`go test`/`go vet`) → `code-reviewer` (fresh
context, diff-only) → `qa` (conditional, only for user/tool-facing
changes).

All four subagents are pinned to Sonnet, effort high. Escalating beyond
that (Opus, or higher effort) is never an agent's call alone — ask the
user first.

This repo's `.claude/settings.json` auto-enables the `superpowers-dev`
marketplace (`github.com/obra/superpowers`) for any session opened
here, tracking its default branch with no version pin — worth knowing
for supply-chain awareness.

## Go module layout

Three independent `go.mod`s under `src/`: `wizard`, `collector`,
`dash-generator`. Only rebuild/retest the module(s) actually touched.
No `golangci-lint` config exists yet — `go vet` plus `go test` is the
current bar; match this repo's existing style rather than introducing a
new linter unasked.
