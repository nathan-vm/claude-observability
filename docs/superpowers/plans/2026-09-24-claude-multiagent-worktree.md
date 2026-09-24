# Claude Code Multi-Agent + Isolated Worktree Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One command creates an isolated git worktree with its own isolated docker-compose stack (no port/volume/container collisions with the main checkout or other worktrees), and a Claude Code `orchestrator` skill runs request → plan → implement → review → (conditional) QA through fresh, pinned-model subagents.

**Architecture:** `scripts/worktree-add.sh` creates the worktree and calls `scripts/docker-worktree-env.sh`, which derives a per-worktree `COMPOSE_PROJECT_NAME` and four host ports from a hash of the worktree name (offset × 300, chosen so it can never collide across the four numerically-close base ports). `docker-compose.yaml`'s host ports become env-var-driven so the generated `docker-worktree.local` can override them. `.claude/` gets a `planner` → `developer` → `code-reviewer` → `qa` pipeline behind one `orchestrator` skill, with `planner` using this repo's existing Superpowers workflow (`brainstorming` + `writing-plans`) to produce its plan.

**Tech Stack:** bash, Docker Compose, Claude Code `.claude/` config (settings, hooks, agents, skills).

**Spec:** `docs/superpowers/specs/2026-09-24-claude-multiagent-worktree-design.md`

## Global Constraints

- `.worktrees/` is already in `.gitignore` (versioned ignore rule, not `.git/info/exclude` — confirmed present, see Task 1) — this was a deliberate deviation from the global default convention, made explicit in the spec.
- `docker-worktree.local` (the generated per-worktree env file) also goes in `.gitignore` — it's a generated, per-machine artifact.
- No external task backlog integration (no ClickUp/GitHub Issues) — the `orchestrator` skill takes its task from the conversation.
- All four subagents (`planner`, `developer`, `code-reviewer`, `qa`) are pinned to `model: sonnet`, `effort: high` in their own agent files — escalating beyond that (Opus, or higher effort) requires asking the user first.
- The four host ports in `docker-compose.yaml` today are `47317` (OTLP gRPC), `47318` (OTLP HTTP), `47300` (Grafana), `47100` (Loki) — these stay the *defaults* (offset 0 / no env file = today's unmodified behavior for the main checkout).
- Port offset formula: `base + (offset × 300)`, offset range 1–50. 300 divides none of the pairwise gaps between the four base ports (1, 17, 18, 200, 217, 218), so no combination of offsets across any two worktrees can ever produce a collision. Max possible port: `47318 + 50×300 = 62318` (safely under 65535).
- The **collector** binary is a host process by design (reads local transcripts, runs the logged-in `claude` CLI) — it never runs inside docker-compose, in a worktree or otherwise.
- Conventional Commit types accepted for `<tipo>` in `worktree-add.sh` match `pr-title.yml` from the trunk-based-release plan: `feat fix docs style refactor perf test build ci chore revert`.

---

### Task 1: Parameterize `docker-compose.yaml` host ports + gitignore

**Files:**
- Modify: `docker-compose.yaml`
- Modify: `.gitignore`

- [ ] **Step 1: Confirm `.worktrees/` is already ignored**

Run: `grep -n '.worktrees/' .gitignore`
Expected: one match (already present — no change needed for that line).

- [ ] **Step 2: Add `docker-worktree.local` to `.gitignore`**

Add this block to `.gitignore` (place it near the existing `.worktrees/` entry, under the same "Git worktrees" comment or its own short one):

```
# Generated per-worktree docker-compose env (ports/project name) — see
# scripts/docker-worktree-env.sh. Machine-specific, never committed.
docker-worktree.local
```

- [ ] **Step 3: Parameterize the four host ports in `docker-compose.yaml`**

In `docker-compose.yaml`, change these four lines (container-side ports and the `127.0.0.1` bind stay exactly as-is — only the host-side port number becomes a variable):

```yaml
      - "127.0.0.1:47317:4317"   # OTLP gRPC  (uncommon host port)
      - "127.0.0.1:47318:4318"   # OTLP HTTP  (uncommon host port)
```
becomes
```yaml
      - "127.0.0.1:${OTEL_GRPC_PORT:-47317}:4317"   # OTLP gRPC  (uncommon host port)
      - "127.0.0.1:${OTEL_HTTP_PORT:-47318}:4318"   # OTLP HTTP  (uncommon host port)
```

```yaml
      - "127.0.0.1:47100:3100"
```
becomes
```yaml
      - "127.0.0.1:${LOKI_PORT:-47100}:3100"
```

```yaml
      - "127.0.0.1:47300:3000"
```
becomes
```yaml
      - "127.0.0.1:${GRAFANA_PORT:-47300}:3000"
```

- [ ] **Step 4: Verify defaults are unchanged with no env file**

Run: `docker compose config | grep -A1 "127.0.0.1"`
Expected: shows `127.0.0.1:47317:4317/tcp`, `127.0.0.1:47318:4318/tcp`, `127.0.0.1:47100:3100/tcp`, `127.0.0.1:47300:3000/tcp` — identical to before this change.

- [ ] **Step 5: Verify an override env file actually changes them**

```bash
cat > /tmp/test-docker-worktree.local <<'EOF'
COMPOSE_PROJECT_NAME=test-wt
OTEL_GRPC_PORT=47617
OTEL_HTTP_PORT=47618
GRAFANA_PORT=47600
LOKI_PORT=47400
EOF
docker compose --env-file /tmp/test-docker-worktree.local config | grep "127.0.0.1"
rm /tmp/test-docker-worktree.local
```
Expected: shows `47617`, `47618`, `47600`, `47400` instead of the defaults.

- [ ] **Step 6: Commit**

```bash
git add docker-compose.yaml .gitignore
git commit -m "feat: parameterize docker-compose host ports for per-worktree isolation"
```

---

### Task 2: `scripts/docker-worktree-env.sh`

**Files:**
- Create: `scripts/docker-worktree-env.sh`

**Interfaces:**
- Produces: `docker-worktree.local` in the current worktree's repo root, containing `COMPOSE_PROJECT_NAME`, `OTEL_GRPC_PORT`, `OTEL_HTTP_PORT`, `GRAFANA_PORT`, `LOKI_PORT`. Must be run with cwd inside the target worktree (uses `git rev-parse --show-toplevel`).

- [ ] **Step 1: Write the script**

Create `scripts/docker-worktree-env.sh`:

```bash
#!/usr/bin/env bash
# Generates COMPOSE_PROJECT_NAME + isolated host ports for the worktree this
# is run from, so `docker compose --env-file docker-worktree.local up -d`
# can run in several worktrees at once without colliding.
#
# The four base ports (47100/47300/47317/47318) are only ~218 apart, so a
# flat "add the offset" scheme (safe when ports are thousands apart) isn't
# safe here — a small offset could push one port's band into a neighboring
# port's. Each port instead gets base + (offset * 300): 300 divides none of
# the pairwise gaps between the four bases (1, 17, 18, 200, 217, 218), so no
# combination of offsets, across any two worktrees, can ever collide.
#
# Usage: run from inside the worktree, before `docker compose up`:
#   scripts/docker-worktree-env.sh
#   docker compose --env-file docker-worktree.local up -d --build
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
worktree_name="$(basename "$repo_root")"

project_name="$(echo "$worktree_name" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9' '-' | sed 's/-\+/-/g; s/^-//; s/-$//')"

# A name that sanitizes to empty (e.g. all-punctuation) would collide
# silently at offset 0 with an un-isolated instance — fall back to a
# hash-derived name instead.
if [ -z "$project_name" ]; then
  project_name="wt-$(echo -n "$repo_root" | cksum | cut -d' ' -f1)"
fi

max_offset=50
offset=$(( $(echo -n "$project_name" | cksum | cut -d' ' -f1) % max_offset + 1 ))

other_worktrees="$(git worktree list --porcelain | awk '/^worktree /{print substr($0, 10)}')"

for _ in $(seq 1 "$max_offset"); do
  collision=0
  candidate_grpc_port=$((47317 + offset * 300))
  while IFS= read -r wt; do
    [ -n "$wt" ] || continue
    [ "$wt" = "$repo_root" ] && continue
    other_env="$wt/docker-worktree.local"
    [ -f "$other_env" ] || continue
    other_grpc_port="$(grep '^OTEL_GRPC_PORT=' "$other_env" 2>/dev/null | cut -d= -f2)"
    if [ "$candidate_grpc_port" = "$other_grpc_port" ]; then
      collision=1
      break
    fi
  done <<EOF
$other_worktrees
EOF
  [ "$collision" = 0 ] && break
  offset=$(( offset % max_offset + 1 ))
done

loki_port=$((47100 + offset * 300))
grafana_port=$((47300 + offset * 300))
otel_grpc_port=$((47317 + offset * 300))
otel_http_port=$((47318 + offset * 300))

out_file="$repo_root/docker-worktree.local"

cat >"$out_file" <<EOF
COMPOSE_PROJECT_NAME=$project_name
OTEL_GRPC_PORT=$otel_grpc_port
OTEL_HTTP_PORT=$otel_http_port
GRAFANA_PORT=$grafana_port
LOKI_PORT=$loki_port
EOF

cat <<EOF
worktree:      $worktree_name
project:       $project_name
otel grpc:     $otel_grpc_port
otel http:     $otel_http_port
grafana:       $grafana_port
loki:          $loki_port

arquivo gerado: $out_file
rode: docker compose --env-file docker-worktree.local up -d --build
EOF
```

- [ ] **Step 2: Make it executable**

Run: `chmod +x scripts/docker-worktree-env.sh`

- [ ] **Step 3: Verify two different worktree names produce non-colliding output**

The script isn't committed yet at this point in the task, so `git worktree add ... HEAD` wouldn't check it out inside the new worktree — instead, create two real temp worktrees and run the *working-directory* copy of the script against each, with cwd set inside the worktree (the script finds its own repo root via `git rev-parse --show-toplevel`, so this exercises the exact same logic a committed copy would):

```bash
git worktree add /tmp/wt-test-alpha -b test/wt-alpha-tmp HEAD --quiet
git worktree add /tmp/wt-test-beta -b test/wt-beta-tmp HEAD --quiet

script_path="$(pwd)/scripts/docker-worktree-env.sh"
(cd /tmp/wt-test-alpha && bash "$script_path")
(cd /tmp/wt-test-beta && bash "$script_path")
diff <(grep -v COMPOSE_PROJECT_NAME /tmp/wt-test-alpha/docker-worktree.local) \
     <(grep -v COMPOSE_PROJECT_NAME /tmp/wt-test-beta/docker-worktree.local) || echo "OK: ports differ"
```

Expected: `OK: ports differ` (or the `diff` shows differing port lines) — the two worktrees, having different directory names, hash to different offsets and get different ports.

- [ ] **Step 4: Clean up the temporary worktrees**

```bash
git worktree remove /tmp/wt-test-alpha --force
git worktree remove /tmp/wt-test-beta --force
git branch -D test/wt-alpha-tmp test/wt-beta-tmp
```

- [ ] **Step 5: Commit**

```bash
git add scripts/docker-worktree-env.sh
git commit -m "feat: add per-worktree docker-compose port/project isolation script"
```

---

### Task 3: `scripts/worktree-add.sh`

**Files:**
- Create: `scripts/worktree-add.sh`

**Interfaces:**
- Consumes: `scripts/docker-worktree-env.sh` (Task 2).

- [ ] **Step 1: Write the script**

Create `scripts/worktree-add.sh`:

```bash
#!/usr/bin/env bash
# Creates an isolated worktree at .worktrees/<tipo>-<descrição>, on branch
# <tipo>/<descrição> from origin/main, and already generates its isolated
# docker-compose env (scripts/docker-worktree-env.sh) — one command instead
# of three manual steps.
#
# Usage:
#   scripts/worktree-add.sh <tipo> <descrição-kebab-case>
#   scripts/worktree-add.sh feat grafana-per-account-panel
#   # -> .worktrees/feat-grafana-per-account-panel, branch feat/grafana-per-account-panel
#
# <tipo> matches the Conventional Commit types pr-title.yml enforces.
set -euo pipefail

tipos_validos="feat fix docs style refactor perf test build ci chore revert"

if [ $# -ne 2 ]; then
  echo "Uso: $0 <tipo> <descrição-kebab-case>" >&2
  echo "Ex.:  $0 feat grafana-per-account-panel" >&2
  echo "Tipos válidos: $tipos_validos" >&2
  exit 1
fi

tipo="$1"
descricao="$2"

if ! grep -qw "$tipo" <<<"$tipos_validos"; then
  echo "tipo '$tipo' não é um dos aceitos por pr-title.yml: $tipos_validos" >&2
  exit 1
fi

if ! [[ "$descricao" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]]; then
  echo "descrição deve ser kebab-case (recebido: '$descricao')." >&2
  exit 1
fi

repo_root="$(git rev-parse --show-toplevel)"
slug="${tipo}-${descricao}"
worktree_path="$repo_root/.worktrees/$slug"
branch="$tipo/$descricao"

if [ -e "$worktree_path" ]; then
  echo "Já existe algo em $worktree_path — aborta (remova antes com 'git worktree remove' se for engano)." >&2
  exit 1
fi

if git -C "$repo_root" rev-parse --verify --quiet "$branch" >/dev/null; then
  echo "A branch '$branch' já existe — aborta." >&2
  exit 1
fi

mkdir -p "$repo_root/.worktrees"
git -C "$repo_root" fetch origin main --quiet
git -C "$repo_root" worktree add "$worktree_path" -b "$branch" origin/main

(cd "$worktree_path" && "$repo_root/scripts/docker-worktree-env.sh")

echo
echo "worktree:  $worktree_path"
echo "branch:    $branch"
```

- [ ] **Step 2: Make it executable**

Run: `chmod +x scripts/worktree-add.sh`

- [ ] **Step 3: Verify argument validation**

```bash
scripts/worktree-add.sh 2>&1 | grep -q "Uso:" && echo "OK: missing args rejected"
scripts/worktree-add.sh bogus my-slug 2>&1 | grep -q "não é um dos aceitos" && echo "OK: bad tipo rejected"
scripts/worktree-add.sh feat NotKebabCase 2>&1 | grep -q "kebab-case" && echo "OK: bad descricao rejected"
```
Expected: all three `OK:` lines print.

- [ ] **Step 4: Verify a real worktree gets created correctly**

(This requires `scripts/docker-worktree-env.sh` from Task 2 to already be committed, since `worktree-add.sh` runs it from the *main* repo root, which only has committed files — an uncommitted script wouldn't be found by `git worktree add ... origin/main` either way, but the invocation at the end reads `$repo_root/scripts/docker-worktree-env.sh` from the working tree, not from the new branch, so this works as long as Task 2 was committed first.)

```bash
scripts/worktree-add.sh test worktree-add-smoke-test
ls .worktrees/test-worktree-add-smoke-test/docker-worktree.local && echo "OK: env file generated"
git -C .worktrees/test-worktree-add-smoke-test branch --show-current
```
Expected: `OK: env file generated`, and the branch name prints as `test/worktree-add-smoke-test`.

- [ ] **Step 5: Clean up the smoke-test worktree**

```bash
git worktree remove .worktrees/test-worktree-add-smoke-test --force
git branch -D test/worktree-add-smoke-test
```

- [ ] **Step 6: Commit**

```bash
git add scripts/worktree-add.sh
git commit -m "feat: add worktree-add.sh for one-command isolated worktree creation"
```

---

### Task 4: `CLAUDE.md`

**Files:**
- Create: `CLAUDE.md`

- [ ] **Step 1: Write the file**

Create `CLAUDE.md`:

```markdown
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

## Go module layout

Three independent `go.mod`s under `src/`: `wizard`, `collector`,
`dash-generator`. Only rebuild/retest the module(s) actually touched.
No `golangci-lint` config exists yet — `go vet` plus `go test` is the
current bar; match this repo's existing style rather than introducing a
new linter unasked.
```

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: add CLAUDE.md"
```

---

### Task 5: `.claude/settings.json` + `.claude/hooks/session-start.sh`

**Files:**
- Create: `.claude/settings.json`
- Create: `.claude/hooks/session-start.sh`

- [ ] **Step 1: Write `.claude/settings.json`**

Create `.claude/settings.json`:

```json
{
  "permissions": {
    "allow": [
      "Bash(go build:*)",
      "Bash(go test:*)",
      "Bash(go vet:*)",
      "Bash(git status)",
      "Bash(git diff:*)",
      "Bash(git log:*)",
      "Bash(git worktree add:*)",
      "Bash(git worktree remove:*)",
      "Bash(git worktree list:*)"
    ]
  },
  "enabledPlugins": {
    "superpowers@superpowers-dev": true
  },
  "extraKnownMarketplaces": {
    "superpowers-dev": {
      "source": {
        "source": "github",
        "repo": "obra/superpowers"
      }
    }
  },
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash .claude/hooks/session-start.sh"
          }
        ]
      }
    ]
  }
}
```

- [ ] **Step 2: Validate JSON syntax**

Run: `python3 -c "import json; json.load(open('.claude/settings.json'))" && echo OK`
Expected: `OK`

- [ ] **Step 3: Write `.claude/hooks/session-start.sh`**

Create `.claude/hooks/session-start.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

cat <<'EOF'
{
  "hookSpecificOutput": {
    "hookEventName": "SessionStart",
    "additionalContext": "This repo (claude-observability) uses an agentic workflow documented in CLAUDE.md. For any non-trivial feature, fix, or refactor, invoke the orchestrator skill instead of implementing directly — it delegates to planner (which uses Superpowers brainstorming/writing-plans), creates an isolated worktree, developer, code-reviewer, and (conditionally) qa, each a pinned Sonnet/high-effort sub-agent. Escalating model or effort beyond that requires asking the user first."
  }
}
EOF
```

- [ ] **Step 4: Make the hook executable**

Run: `chmod +x .claude/hooks/session-start.sh`

- [ ] **Step 5: Verify the hook emits valid JSON**

Run: `bash .claude/hooks/session-start.sh | python3 -c "import json,sys; json.load(sys.stdin)" && echo OK`
Expected: `OK`

- [ ] **Step 6: Commit**

```bash
git add .claude/settings.json .claude/hooks/session-start.sh
git commit -m "feat(claude): add project settings and SessionStart hook"
```

---

### Task 6: `.claude/agents/planner.md`

**Files:**
- Create: `.claude/agents/planner.md`

- [ ] **Step 1: Write the agent file**

Create `.claude/agents/planner.md`:

```markdown
---
name: planner
description: Turns a feature/fix/refactor request into a real Superpowers plan by reading the actual code first. Read-only — never writes implementation code. Invoked by the orchestrator skill before dispatching work to the developer agent.
tools: Read, Grep, Glob, Skill
model: sonnet
effort: high
---

You are the planning subagent for claude-observability. You receive a request from the orchestrator and turn it into an executable plan — you do not implement anything yourself.

1. Read the relevant source under `src/wizard`, `src/collector`, `src/dash-generator`, and `grafana/`/`config/` as needed, to ground the plan in what actually exists, not assumptions.
2. Invoke the `superpowers:brainstorming` skill for the request — it classifies the work (spike/bounded/architectural) and gets a design approved before any plan is written. For a genuinely bounded, well-scoped change it may resolve with just a short in-chat design; that's expected, not a shortcut you're taking.
3. Once a design is approved (per that skill's own gate), invoke `superpowers:writing-plans` to produce the actual implementation plan, saved to `docs/superpowers/plans/YYYY-MM-DD-<slug>.md` — this repo's existing convention (see `docs/superpowers/plans/` for examples from the collector/dash-generator/wizard work).
4. Flag anything that blocks planning (ambiguous requirement, missing context) instead of guessing past it.
5. Note any conventions from `CLAUDE.md` that constrain the approach (trunk-based release rules, worktree/docker isolation, no hand-edited version).
6. Return the plan file's path to the orchestrator.

Do not write or edit implementation files. Do not run `Bash` beyond what `Read`/`Grep`/`Glob` need. Do not spawn other agents — return the plan to whoever invoked you.
```

- [ ] **Step 2: Validate the frontmatter is well-formed YAML**

Run: `python3 -c "
import re
text = open('.claude/agents/planner.md').read()
m = re.match(r'^---\n(.*?)\n---\n', text, re.DOTALL)
import yaml
yaml.safe_load(m.group(1))
print('OK')
"`
Expected: `OK`

- [ ] **Step 3: Commit**

```bash
git add .claude/agents/planner.md
git commit -m "feat(claude): add planner agent"
```

---

### Task 7: `.claude/agents/developer.md`

**Files:**
- Create: `.claude/agents/developer.md`

- [ ] **Step 1: Write the agent file**

Create `.claude/agents/developer.md`:

```markdown
---
name: developer
description: Implements one concrete, scoped coding task in claude-observability inside an isolated worktree the orchestrator already created — writes the code, runs build/test/vet for every Go module touched, and reports what changed. Does not review its own work for correctness beyond making checks pass.
tools: Read, Edit, Write, Bash, Grep, Glob
model: sonnet
effort: high
---

You are the developer subagent for claude-observability. You receive one concrete task — the plan produced by `planner`, and the path to a worktree the orchestrator already created via `scripts/worktree-add.sh` — and implement it end to end inside that worktree.

- Never touch the caller's own working tree — all edits, builds, and commits happen inside the given worktree path.
- Follow `CLAUDE.md`: never hand-edit a version anywhere (there's no version file — it's derived from git tags at release time), Conventional Commit messages, worktree/docker isolation conventions if the task touches them.
- This repo has three independent Go modules under `src/`: `wizard`, `collector`, `dash-generator`. For every module your changes touch, run `go build ./...`, `go test ./...`, and `go vet ./...` from that module's directory, and fix what they surface. Don't run these for modules you didn't touch.
- Write no unnecessary comments; don't add abstractions, error handling, or config the task didn't ask for.
- Report back: the worktree path, what changed and why, which module(s) you built/tested, and any assumption you had to make.

Do not spawn other agents or invoke other skills — reviewing your own diff is `code-reviewer`'s job, not yours.
```

- [ ] **Step 2: Validate the frontmatter is well-formed YAML**

Run: `python3 -c "
import re
text = open('.claude/agents/developer.md').read()
m = re.match(r'^---\n(.*?)\n---\n', text, re.DOTALL)
import yaml
yaml.safe_load(m.group(1))
print('OK')
"`
Expected: `OK`

- [ ] **Step 3: Commit**

```bash
git add .claude/agents/developer.md
git commit -m "feat(claude): add developer agent"
```

---

### Task 8: `.claude/agents/code-reviewer.md`

**Files:**
- Create: `.claude/agents/code-reviewer.md`

- [ ] **Step 1: Write the agent file**

Create `.claude/agents/code-reviewer.md`:

```markdown
---
name: code-reviewer
description: Reviews a worktree's diff against main for correctness, security, and simplification issues, with no knowledge of how the change was built or discussed. Only ever invoked after the developer subagent reports a change finished — never mid-development.
tools: Read, Grep, Glob, Bash
model: sonnet
effort: high
---

You are the code-review subagent for claude-observability. You are handed only a worktree path — never the developer's reasoning or conversation. Form your own opinion from the code, diff, tests, and commit history alone.

1. Use the worktree the developer already worked in — don't create a second one. Diff it against `origin/main`.
2. Read the diff, then the surrounding code the diff touches — not just the changed lines.
3. For every Go module the diff touches, run `go build ./...`, `go test ./...`, `go vet ./...` inside that module's directory.
4. Check against `CLAUDE.md`: Conventional Commit messages, no hand-edited version anywhere, worktree/docker isolation conventions followed if the change touches `scripts/worktree-add.sh` / `scripts/docker-worktree-env.sh` / `docker-compose.yaml`.
5. Report findings ranked by severity — correctness bugs first, then security, then reuse/simplification/efficiency. State the concrete failure scenario for each, not a vague concern.

Do not fix anything yourself; report only. Do not spawn other agents or invoke other skills.
```

- [ ] **Step 2: Validate the frontmatter is well-formed YAML**

Run: `python3 -c "
import re
text = open('.claude/agents/code-reviewer.md').read()
m = re.match(r'^---\n(.*?)\n---\n', text, re.DOTALL)
import yaml
yaml.safe_load(m.group(1))
print('OK')
"`
Expected: `OK`

- [ ] **Step 3: Commit**

```bash
git add .claude/agents/code-reviewer.md
git commit -m "feat(claude): add code-reviewer agent"
```

---

### Task 9: `.claude/agents/qa.md`

**Files:**
- Create: `.claude/agents/qa.md`

- [ ] **Step 1: Write the agent file**

Create `.claude/agents/qa.md`:

```markdown
---
name: qa
description: Runs smoke tests against a built change to confirm the golden path works at runtime, for changes that are user- or tool-facing enough to warrant it (new Grafana panel, new wizard prompt, changed collector/dash-generator behavior). Not needed for most changes — unit tests already cover pure logic.
tools: Read, Bash
model: sonnet
effort: high
---

You are the QA subagent for claude-observability. You're only spawned when a change is visible enough to warrant a runtime check beyond unit tests.

1. In the given worktree, bring up its isolated docker-compose stack: `docker compose --env-file docker-worktree.local up -d --build`. If it's not already generated, run `scripts/docker-worktree-env.sh` first.
2. For a Grafana/dash-generator/otel-collector change: exercise it against that isolated stack (e.g. check the generated dashboard JSON under `grafana/dashboards/`, query Loki on the worktree's isolated `LOKI_PORT`, load Grafana on its isolated `GRAFANA_PORT`).
3. For a collector- or wizard-only change: these are host processes, not containers — build the binary in the worktree and run it directly, pointed at the worktree's isolated `OTEL_GRPC_PORT`/`OTEL_HTTP_PORT`/`LOKI_PORT` (from `docker-worktree.local`) rather than the stack's container-internal addresses.
4. Exercise the golden path for the changed behavior plus one obvious edge case.
5. Report pass/fail per scenario with the exact command and output — not an impression.

If the change isn't smoke-testable this way (pure refactor, internal-only, docs/config), say so and stop rather than inventing a test. Do not spawn other agents or invoke other skills. Do not tear down the stack when done — leave it for the user's own review.
```

- [ ] **Step 2: Validate the frontmatter is well-formed YAML**

Run: `python3 -c "
import re
text = open('.claude/agents/qa.md').read()
m = re.match(r'^---\n(.*?)\n---\n', text, re.DOTALL)
import yaml
yaml.safe_load(m.group(1))
print('OK')
"`
Expected: `OK`

- [ ] **Step 3: Commit**

```bash
git add .claude/agents/qa.md
git commit -m "feat(claude): add qa agent"
```

---

### Task 10: `.claude/skills/orchestrator/SKILL.md`

**Files:**
- Create: `.claude/skills/orchestrator/SKILL.md`

**Interfaces:**
- Consumes: `scripts/worktree-add.sh` (Task 3), and the four agents from Tasks 6-9 (`planner`, `developer`, `code-reviewer`, `qa`), invoked via the `Agent` tool with matching `subagent_type` values.

- [ ] **Step 1: Write the skill**

Create `.claude/skills/orchestrator/SKILL.md`:

```markdown
---
name: orchestrator
description: Entry point for agentic work in claude-observability. Breaks a request into planning, an isolated worktree, development, review, and conditional QA instead of doing it all inline. Use for any non-trivial feature, fix, or refactor in this repo.
---

You are acting as the orchestrator for claude-observability. For anything beyond a genuinely trivial one-file change, delegate — don't write, review, or test code yourself. Your job is sequencing, worktree setup, and integrating results.

## Workflow

1. **Plan.** Invoke `planner` (Agent tool, `subagent_type: "planner"`) with the request. Skip only for a one-file, obviously-scoped change. `planner` returns a plan file path under `docs/superpowers/plans/`.
2. **Worktree.** Run `scripts/worktree-add.sh <tipo> <slug>` yourself (not the developer subagent) — `<tipo>` and `<slug>` come from the task (e.g. `feat`, `add-per-account-rate-panel`). This generates the worktree's isolated `docker-worktree.local` before `developer` starts.
3. **Develop.** Invoke `developer` (Agent tool, `subagent_type: "developer"`) with the worktree path from step 2 and the plan file path from step 1.
4. **Review.** Only once `developer`'s call has returned and reported the change finished — never while it's still in flight — invoke `code-reviewer` (Agent tool, `subagent_type: "code-reviewer"`) with just the worktree path. Nothing else from the developer's report or the planning conversation.
5. **QA — conditional.** If the change is user/tool-facing (new or changed Grafana panel, new wizard prompt, changed collector/dash-generator runtime behavior), invoke `qa` (Agent tool, `subagent_type: "qa"`) with the worktree path, after review passes. Skip for pure refactors, internal-only changes, or doc/config edits.
6. **Integrate.** Report the plan, the diff, the reviewer's findings and how you addressed them, and QA results (if run) back to the user. To fix a reviewer-flagged issue, send the developer subagent another scoped task, then re-review before calling it done.

## Model/effort policy

Each subagent's `.claude/agents/*.md` sets `model: sonnet`, `effort: high` — already the approved envelope. Going outside it — Opus, or effort above high — is never your call alone. Stop and ask the user first, e.g.: "this task needs a developer with opus — approve?" Only proceed on an explicit yes.

## Calling other skills

- Calls: `planner`, `developer`, `code-reviewer`, `qa` (via the `Agent` tool).
- Called by: the repo's `SessionStart` hook (see `.claude/settings.json`), or directly via `/orchestrator`.
```

- [ ] **Step 2: Validate the frontmatter is well-formed YAML**

Run: `python3 -c "
import re
text = open('.claude/skills/orchestrator/SKILL.md').read()
m = re.match(r'^---\n(.*?)\n---\n', text, re.DOTALL)
import yaml
yaml.safe_load(m.group(1))
print('OK')
"`
Expected: `OK`

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/orchestrator/SKILL.md
git commit -m "feat(claude): add orchestrator skill"
```

---

### Task 11: End-to-end validation of worktree + docker isolation

**Files:** none (validation only — no new files)

- [ ] **Step 1: Create two real worktrees**

```bash
scripts/worktree-add.sh test e2e-validation-alpha
scripts/worktree-add.sh test e2e-validation-beta
```
Expected: both succeed, each prints a `docker-worktree.local` summary with different ports.

- [ ] **Step 2: Confirm no port/project-name collision between them**

```bash
diff .worktrees/test-e2e-validation-alpha/docker-worktree.local \
     .worktrees/test-e2e-validation-beta/docker-worktree.local
```
Expected: every line differs (all 5 values — project name and 4 ports — are worktree-specific).

- [ ] **Step 3: Bring up one worktree's stack and confirm it's healthy**

```bash
cd .worktrees/test-e2e-validation-alpha
docker compose --env-file docker-worktree.local up -d
sleep 5
docker compose --env-file docker-worktree.local ps
cd -
```
Expected: `otel-collector`, `loki`, `grafana`, `dash-generator` all show `running`/`healthy` — no port-bind errors.

- [ ] **Step 4: Confirm the main checkout's own stack (if running) is unaffected**

```bash
docker compose ps
```
Expected: if the main stack was already up, it's still up on its original ports (`47317`/`47318`/`47300`/`47100`) — the worktree's stack used entirely different ports and a different `COMPOSE_PROJECT_NAME`, so `docker compose ps` here shows only the main project's containers.

- [ ] **Step 5: Tear down and clean up everything from this validation**

```bash
(cd .worktrees/test-e2e-validation-alpha && docker compose --env-file docker-worktree.local down -v)
git worktree remove .worktrees/test-e2e-validation-alpha --force
git worktree remove .worktrees/test-e2e-validation-beta --force
git branch -D test/e2e-validation-alpha test/e2e-validation-beta
```

- [ ] **Step 6: No commit for this task** — it's pure validation of Tasks 1-3; nothing new to commit. If any step failed, go back and fix the relevant earlier task's files instead, then re-run this task.

---

## Post-implementation note (not a task — informational)

Validating the full `orchestrator` pipeline (planner → worktree → developer → code-reviewer → qa) end-to-end requires actually invoking it on a real task after this plan is merged — that's a live multi-agent run, not a scriptable plan step. The natural first exercise is the next genuinely non-trivial change made to this repo: invoke `/orchestrator` (or the `orchestrator` skill directly) instead of working inline, and confirm each stage behaves as designed (a plan file appears under `docs/superpowers/plans/`, a worktree appears under `.worktrees/`, `code-reviewer` reports something concrete, `qa` runs only when warranted).
