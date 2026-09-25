# Claude Code multi-agent orchestration + isolated worktrees — Design

Date: 2026-09-24
Status: approved-pending-review

## Context

This repo has no `.claude/` configuration yet (the directory exists and is
empty) and no `CLAUDE.md`. This spec ports two things from reference repos
explored in brainstorming (2026-09-24):

- The **planner → developer → code-reviewer → qa** subagent pipeline from
  `subforge-mcp`, each agent pinned to a model/effort envelope in its own
  `.claude/agents/*.md` file, entered through a single `orchestrator`
  skill.
- The **isolated worktree + isolated docker-compose stack** pattern from
  `delivery-intelligent-platform`: one `.worktrees/<slug>` per task, with
  a generated per-worktree env file so multiple stacks can run
  concurrently without port/volume/container collisions.

Unlike both reference repos, `claude-observability` has no external task
backlog (tasks come from the conversation directly) and is a 3-module Go
monorepo with a single `docker-compose.yaml`, not a JS/pnpm monorepo — the
designs below are adapted accordingly. This repo also already has its own
Superpowers-driven planning history (`docs/superpowers/specs/` and
`docs/superpowers/plans/` from the collector/dash-generator/wizard work),
so the `planner` subagent uses that same workflow rather than inventing a
separate one.

## Goals

- One command creates an isolated worktree + isolated docker-compose
  environment for a task, so multiple in-flight changes never collide.
- A single `orchestrator` skill entry point runs request → plan →
  implement → review → (conditional) QA, entirely within this session,
  using fresh subagents with no shared context between the review/QA
  steps and the development step.
- The `planner` subagent produces its plan the way this repo already
  does — via Superpowers (`brainstorming` + `writing-plans`), landing as
  a real `docs/superpowers/plans/*.md` file — not an ad hoc breakdown.
- Superpowers is available to subagents without depending on the user's
  personal, machine-local `~/.claude-personal/settings.json` — declared
  in this project's own `.claude/settings.json` instead.
- Model/effort is pinned per agent (Sonnet, effort high — confirmed with
  the owner) with escalation beyond that requiring an explicit ask.

## Non-goals

- No external backlog integration (ClickUp/GitHub Issues) — tasks come
  from the conversation.
- No change to the trunk-based release pipeline (`2026-09-24-trunk-based-
  release-design.md` — separate spec, already approved).
- Branch protection / required-check configuration on GitHub — out of
  scope here too, same reasoning as the release spec.

## Design

### Worktree creation: `scripts/worktree-add.sh <tipo> <descrição-kebab-case>`

No ticket code (no external backlog), unlike DIP's `<CÓDIGO> <tipo>
<descrição>`. Validates `<tipo>` against the same Conventional Commit
types `pr-title.yml` (release spec) enforces: `feat fix docs style
refactor perf test build ci chore revert`. Validates
`<descrição-kebab-case>` is lowercase-kebab. Then:

```bash
mkdir -p .worktrees
git fetch origin main --quiet
git worktree add .worktrees/<tipo>-<descrição> -b <tipo>/<descrição> origin/main
(cd .worktrees/<tipo>-<descrição> && ../../scripts/docker-worktree-env.sh)
```

Refuses (matching DIP's script) if the target path or branch already
exists.

**`.worktrees/` is added to `.gitignore`** (not `.git/info/exclude`) —
the repo owner wants it versioned as a real ignore rule so it's visible
to anyone who clones the repo, not just a personal local convenience.
This is a deliberate deviation from the default global convention for
this project.

### Docker-compose isolation: `scripts/docker-worktree-env.sh`

`docker-compose.yaml` today hardcodes host ports (`47317`, `47318`,
`47300`, `47100`) and a fixed top-level `name: claude-observability`.
Change: parameterize the four host-side port mappings
(`${OTEL_GRPC_PORT:-47317}`, `${OTEL_HTTP_PORT:-47318}`,
`${GRAFANA_PORT:-47300}`, `${LOKI_PORT:-47100}}` — container-internal
ports unchanged). `COMPOSE_PROJECT_NAME` as an env var already takes
precedence over the file's `name:` field, so no compose-file change
needed there — named volumes inherit the project-name isolation
automatically.

`docker-worktree-env.sh` computes one deterministic offset from a hash of
the worktree directory name (same technique as DIP's script: `cksum` of
the sanitized name, modulo a max, retried against other worktrees'
already-generated files on collision). The difference from DIP: this
repo's four base ports are numerically close together (47100–47318, ~218
apart), so a flat 1-unit-per-offset scheme (DIP's approach, safe there
because its bands are thousands of ports apart) isn't safe here — a
small offset could make one port's band reach into a neighboring port's
band. Each port instead gets `base + offset * 300`: 300 divides none of
the pairwise gaps between the four base ports (1, 17, 18, 200, 217, 218),
which guarantees no two ports — across any two worktrees, any offsets —
ever land on the same number. Offset range 1–50 (main repo / no env file
= offset 0 = today's unmodified ports; worktrees get 1–50), keeping the
highest possible port at 47318 + 50×300 = 62318, safely under 65535.

Output: `docker-worktree.local` (`COMPOSE_PROJECT_NAME`, `OTEL_GRPC_PORT`,
`OTEL_HTTP_PORT`, `GRAFANA_PORT`, `LOKI_PORT`), consumed via `docker
compose --env-file docker-worktree.local up -d --build`. **Added to
`.gitignore`** — it's a generated, per-machine artifact, not a worktree
convenience file.

The **collector** is a host process by design (reads local transcripts
and runs the logged-in `claude` CLI — see README, "The collector service"
— it cannot run in a container). QA exercising collector or wizard
changes runs the binary directly on the host, pointed at the worktree's
isolated `OTEL_GRPC_PORT`/`OTEL_HTTP_PORT` rather than via compose.

### `CLAUDE.md` (new)

Repo currently has none. New root `CLAUDE.md` documents: the trunk-based
release conventions from the other spec (Conventional Commit PR titles,
squash-only merge), the `.worktrees/` + `scripts/worktree-add.sh`
convention, the per-worktree docker isolation, the `orchestrator` skill
as the entry point for non-trivial work, and the model/effort escalation
rule (Sonnet/high is the ceiling; going beyond needs to ask first). This
is what the `SessionStart` hook below points subagents and fresh sessions
at, and what `code-reviewer` checks changes against.

### `.claude/settings.json`

```json
{
  "permissions": {
    "allow": [
      "Bash(go build:*)", "Bash(go test:*)", "Bash(go vet:*)",
      "Bash(git status)", "Bash(git diff:*)", "Bash(git log:*)",
      "Bash(git worktree add:*)", "Bash(git worktree remove:*)",
      "Bash(git worktree list:*)"
    ]
  },
  "enabledPlugins": {
    "superpowers@superpowers-dev": true
  },
  "extraKnownMarketplaces": {
    "superpowers-dev": {
      "source": { "source": "github", "repo": "obra/superpowers" }
    }
  },
  "hooks": {
    "SessionStart": [
      { "hooks": [{ "type": "command", "command": "bash .claude/hooks/session-start.sh" }] }
    ]
  }
}
```

`enabledPlugins`/`extraKnownMarketplaces` here mirror the owner's personal
`~/.claude-personal/settings.json` entries but declared at the **project**
level — so Superpowers is available to any session or subagent working in
this repo (any machine, any contributor) rather than depending on the
owner's personal global config.

### `.claude/hooks/session-start.sh`

Same shape as `subforge-mcp`'s: emits `additionalContext` pointing a
fresh session at `CLAUDE.md` and the `orchestrator` skill for any
non-trivial change.

### `.claude/skills/orchestrator/SKILL.md`

1. **Plan.** Invoke `planner` (`Agent`, `subagent_type: "planner"`).
   Skip only for a genuinely trivial one-file change.
2. **Worktree.** The orchestrator itself runs
   `scripts/worktree-add.sh <tipo> <slug>` (not the developer subagent —
   this way the docker isolation env is already generated before
   `developer` starts, and the orchestrator, which already knows the
   task's type/description from the conversation, doesn't need to pass
   that back and forth).
3. **Develop.** Invoke `developer` with the worktree path and the
   planner's plan file path.
4. **Review.** Only once `developer` reports done — never mid-flight —
   invoke `code-reviewer` with just the worktree path (no development
   context).
5. **QA — conditional.** If the change is user/tool-facing (new Grafana
   panel, new wizard prompt, changed collector/dash-generator behavior),
   invoke `qa` with the worktree path after review passes.
6. **Integrate.** Report the plan, the diff, review findings and
   resolution, QA results (if run), back to the user.

Model/effort policy: all four subagents Sonnet, effort high (already at
the approved envelope in their own agent files). Escalating beyond that —
Opus, or higher effort — requires asking the user first, same rule as
both reference repos.

### `.claude/agents/planner.md`

Read-only (`Read, Grep, Glob, Skill`). Given a request, it invokes
`superpowers:brainstorming` (skipping straight to a short in-chat design
only for genuinely bounded changes, per that skill's own path
classification) and then `superpowers:writing-plans` to produce the
actual plan, saved as `docs/superpowers/plans/YYYY-MM-DD-<slug>.md` —
matching this repo's existing convention. Returns the plan file path to
the orchestrator; does not implement anything.

### `.claude/agents/developer.md`

Works inside the worktree the orchestrator already created (never touches
the caller's own tree). Implements the plan from `planner`, running `go
build ./...`, `go test ./...`, `go vet ./...` for every Go module it
touched (this repo has three independent `go.mod`s — only the modules
actually changed need re-running, but at least one always is). Follows
`CLAUDE.md`. Reports the worktree path, what changed, and any assumption
made.

### `.claude/agents/code-reviewer.md`

Fresh agent, no development context. Reviews the diff against `main`
inside the existing worktree, runs the same build/test/vet commands,
checks against `CLAUDE.md` conventions (Conventional Commit title, no
hand-edited version anywhere, worktree/docker isolation conventions
followed if the change touches them). Reports ranked findings; fixes
nothing itself.

### `.claude/agents/qa.md`

Only spawned for user/tool-facing changes. Brings up the isolated stack
(`docker compose --env-file docker-worktree.local up -d --build`) for
Grafana/dash-generator/otel-collector changes, or runs the collector/
wizard binary directly against the worktree's isolated OTLP ports for
host-process changes. Exercises the golden path, reports pass/fail with
exact commands and output — not an impression.

## Edge cases

- **Worktree path or branch already exists**: `worktree-add.sh` refuses
  and exits non-zero rather than overwriting (matches DIP).
- **Port collision despite the ×300 scheme**: only possible if two
  worktrees independently land on the exact same hash-derived offset;
  `docker-worktree-env.sh` checks existing `docker-worktree.local` files
  and retries with a different offset, same as DIP's script.
- **QA on a collector/wizard-only change**: no docker-compose stack
  needed for the process itself, but it still needs a Loki/otel-collector
  target to push to — QA brings up the isolated stack anyway and points
  the host binary at its isolated ports, rather than skipping isolation
  for host-process changes.
- **A truly trivial one-file change**: orchestrator may skip `planner`
  (and therefore skip worktree/review/QA machinery entirely) per its own
  step 1 — this is intentional, matching subforge-mcp's "skip only for a
  genuinely trivial change" rule, not a gap.

## Testing

`scripts/docker-worktree-env.sh`'s offset arithmetic is deterministic and
can be validated directly: create two-plus real worktrees via
`worktree-add.sh`, confirm the generated `docker-worktree.local` files
never share a port or `COMPOSE_PROJECT_NAME`, and confirm `docker compose
--env-file docker-worktree.local up -d` succeeds for two of them
simultaneously (`docker compose ps` on both shows all four services
healthy, no port-bind errors). The agent pipeline itself is validated by
actually running the `orchestrator` skill on one real, small task end to
end during implementation and confirming each stage (plan file created,
worktree created, code-reviewer catches at least one planted issue in a
dry run, QA report is concrete) behaves as designed.
