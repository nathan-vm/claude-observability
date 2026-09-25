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
- This describes the process landing with `feat/trunk-based-release`;
  until it merges, neither `pr-title.yml` nor that `release.yml` exist
  on this branch or `main` — `release.yml` here is tag-triggered only,
  with no changelog or PR-title enforcement yet.

## Worktree + docker isolation

- Non-trivial work happens in an isolated worktree:
  `scripts/worktree-add.sh <tipo> <descrição-kebab-case>` creates
  `.worktrees/<tipo>-<descrição>` on branch `<tipo>/<descrição>` from
  `origin/main`, and already generates that worktree's own
  `docker-worktree.local` (isolated ports + `COMPOSE_PROJECT_NAME` — see
  `scripts/docker-worktree-env.sh`) so its docker-compose stack never
  collides with the main checkout's or another worktree's.
- Bring up a worktree's stack with:
  `docker compose --env-file docker-worktree.local up -d --build`
- The **collector** is a host process (reads local transcripts, runs the
  logged-in `claude` CLI) — it never runs in docker-compose, isolated or
  not.
- See `docs/superpowers/specs/2026-09-24-claude-multiagent-worktree-design.md`
  for the full design.

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
