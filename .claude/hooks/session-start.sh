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
