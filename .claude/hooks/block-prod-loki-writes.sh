#!/usr/bin/env bash
# PreToolUse/Bash guard: refuse commands that would WRITE to the "prod" stack's
# Loki (docker-compose.yaml, port 47100). Reads stay allowed — verifying a query
# against real data is legitimate and was never the problem.
#
# Why this exists: a QA run pointed `dash-generator --once` at 127.0.0.1:47100
# with a fresh state file and backfilled 8120 rate points into the live
# instance. docker-compose.dev.yaml + a worktree's docker-worktree.local already
# provide an isolated stack (40xxx defaults, per-worktree ports) that is free to
# write to — that is where anything with a publish path belongs.
#
# Matching is on the PORT, not the host, so 127.0.0.1:47100, localhost:47100 and
# [::1]:47100 are all covered.
#
# LIMITS — this is a guardrail, not a sandbox. It cannot see through a URL built
# from a variable (LOKI_URL="$SOMEWHERE") or a port passed indirectly. It raises
# the cost of the mistake; it does not make the write impossible. For a hard
# boundary, put Loki behind a proxy that only passes GET, or bind it somewhere
# the agent has no route to.
set -uo pipefail

PROD_LOKI_PORT="47100"

payload="$(cat)"
cmd="$(printf '%s' "$payload" | jq -r '.tool_input.command // empty' 2>/dev/null)"
[ -z "$cmd" ] && exit 0

deny() {
  jq -n --arg reason "$1" '{
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      permissionDecisionReason: $reason
    }
  }'
  exit 0
}

# 1. Loki's write/delete endpoints are never acceptable to call by hand,
#    whatever port they are on.
if printf '%s' "$cmd" | grep -Eq '/loki/api/v1/(push|delete)'; then
  deny "Blocked: /loki/api/v1/push and /loki/api/v1/delete are write endpoints. If you are testing, bring up the worktree's isolated stack (docker compose -f docker-compose.dev.yaml --env-file docker-worktree.local up -d) and target its Loki instead. See issue #11."
fi

# Everything below only concerns the prod port.
printf '%s' "$cmd" | grep -q "$PROD_LOKI_PORT" || exit 0

# 2. An explicit mutating HTTP method aimed at prod.
if printf '%s' "$cmd" | grep -Eiq -- '(-X|--request)[[:space:]]+[\"'"'"']?(POST|PUT|DELETE|PATCH)'; then
  deny "Blocked: mutating HTTP method against the prod stack on port $PROD_LOKI_PORT. Reads (GET) are fine; writes belong on the isolated dev stack (docker-compose.dev.yaml). See issue #11."
fi

# 3. curl sending a body to prod without -G/--get. curl defaults to POST when
#    given data, so `--data-urlencode` WITHOUT -G is a write; with -G it is the
#    normal way to pass a LogQL query and stays allowed.
if printf '%s' "$cmd" | grep -Eq -- '(--data|--data-raw|--data-binary|--data-urlencode|--form|[[:space:]]-d[[:space:]])' \
   && ! printf '%s' "$cmd" | grep -Eq -- '([[:space:]]-G[[:space:]]|--get)'; then
  deny "Blocked: this sends a request body to port $PROD_LOKI_PORT without -G/--get, which curl turns into a POST. Add -G for a read query, or point it at the isolated dev stack for a write. See issue #11."
fi

# 4. Running a binary that publishes, with LOKI_URL pointed at prod. Both
#    collector (PublishUsageTruth) and dash-generator (ratemeter.PublishRate)
#    write to Loki as a side effect of running at all.
if printf '%s' "$cmd" | grep -Eq "LOKI_URL=[^[:space:]]*$PROD_LOKI_PORT" \
   && printf '%s' "$cmd" | grep -Eq '(dash-generator|claude-observability-collector|/collector|go run[^|;]*cmd/(collector|dash-generator))'; then
  deny "Blocked: running collector/dash-generator with LOKI_URL on port $PROD_LOKI_PORT writes to the live instance — dash-generator's --once also runs a rate-publish pass, and --dry-run skips dashboard generation entirely, so there is no read-only invocation. Use the worktree's dev stack. See issue #11."
fi

exit 0
