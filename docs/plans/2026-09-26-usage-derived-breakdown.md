# Weekly Breakdown /usage-Derived Percentage — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the "Weekly breakdown" table's percentage column derive from Anthropic's real `/usage` percentage (`week_pct`, published by the collector's `usagetruth` package into the `claude-code-usage-truth` Loki stream) instead of the hand-set `limit_tokens_week` template variable, delete the now-redundant duplicate stat panels, and remove all the limit-configuration plumbing that formula made necessary.

**Architecture:** `row_pct = row_tokens($__range, by dims) * week_pct / total_tokens([7d])` — the row-level numerator keeps respecting the dashboard's time range picker; the two scaling factors are pinned to a fixed trailing-7-day window independent of the picker, combined via LogQL vector matching (`on() group_left()`) so a single unlabeled series broadcasts across every grouped row. The query lives in `grafana/templates/claude-code.json` (dash-generator only substitutes placeholders, it never parses LogQL); the limit-configuration code being deleted lives in `src/dash-generator/internal/dashboardgen`.

**Tech Stack:** Go 1.22 (`src/dash-generator`'s own `go.mod`, standard library only), Grafana dashboard JSON (schemaVersion 41), LogQL.

---

## ⚠️ THE QUERY IS ALREADY VERIFIED — USE IT VERBATIM

The orchestrator ran this against the live Loki at `http://127.0.0.1:47100` before handing the plan over. **Two bugs in the first draft were found and corrected.** Do not "simplify" the expression back toward either broken form.

### Bug A: no `100 *` prefix

The old formula was `100 * tokens / limit` — the `100 *` converted a fraction to a percentage. The new formula multiplies by `week_pct`, which is **already** 0–100. Keeping `100 *` inflates every row 100×. Measured: rows reading `270` and `441` percent.

### Bug B: `week_pct` needs `by (user_email)` INSIDE `last_over_time`

`usagetruth.publish` writes `week_reset_text` as **structured metadata**, and that text drifts as wall-clock wording changes:

```
week_reset_text='resets Sep 26 at 1:59pm (America/Sao_Paulo)'  week_pct=84
week_reset_text='resets Sep 26 at 2pm (America/Sao_Paulo)'     week_pct=84
```

Structured metadata participates in Loki series identity, so each wording forks a **new series**. A bare `sum(last_over_time(... unwrap week_pct [30m]))` therefore adds 84 + 84 = **168**. The multiplier is not a constant — it equals however many distinct reset-text wordings fall in the window.

The fix is `sum(last_over_time(...) by (user_email))`, which matches what the existing gauges (panels 1/2) already do and is why they were never affected. Verified: returns exactly one unlabeled series, value `84`.

### Verified invariants

| numerator window | denominator | SUM of all rows | rows |
|---|---|---|---|
| `[7d]` | `[7d]` | **84.0000** = `week_pct` exactly | 41 |
| `[24h]` | `[7d]` | 12.93 (well under 84) | 15 |

The first proves correctness; the second proves the user-chosen "day's contribution toward the week" semantics.

`* on() group_left()` and `/ on() group_left()` are confirmed supported by this Loki version.

---

## Global Constraints

- Module touched: `src/dash-generator` only. Do not touch `src/collector`. `src/wizard`'s `internal/limits` needs **zero changes** (verified schema-agnostic — generic `map[string]interface{}`, only touches `"ignore"`/`"accounts"`); prove that in Task 6 rather than assuming it.
- Verification bar: `cd src/dash-generator && go build ./... && go vet ./... && go test ./...`. No new linter.
- `grafana/dashboards/accounts/*.json` are generated and gitignored — never hand-edit them. Only `grafana/templates/claude-code.json` is edited.
- Never edit or delete the real, gitignored `grafana/account-limits.json`. `loadLimits` must keep working against an existing file that still has old `block_5h`/`week` fields — it already does, since it only reads `"ignore"`.
- Do not bring up any docker-compose stack. Read-only `curl` against the already-running Loki at `127.0.0.1:47100` is fine.

---

## File Structure

```
grafana/templates/claude-code.json          (MODIFY: remove limit_tokens_5h/limit_tokens_week vars,
                                              delete stat panels id 15/16, reflow+retitle gauges 1/2,
                                              rewrite panel id 10's query + max + description)
grafana/account-limits.example.json         (MODIFY: drop "default"/"accounts", rewrite "_comment")
src/dash-generator/internal/dashboardgen/
  dashboardgen.go                            (MODIFY: remove AccountLimits + limitsFor; drop the
                                               limits param from scopeAccount and the injection loop;
                                               update GenerateDashboards and loadLimits comments)
  dashboardgen_test.go                       (MODIFY: remove TestLimitsFor_*, update TestScopeAccount_*)
README.md                                    (MODIFY: delete "### Setting your limit", replace the
                                               FAQ entry, touch up the Weekly breakdown table row)
```

No new files.

---

### Task 1: `grafana/templates/claude-code.json`

- [ ] **Step 1: Remove the two dead template variables**

Delete both objects from `templating.list` (they sit between the `"owner"` and `"skill"` entries): the one with `"name": "limit_tokens_5h"` and the one with `"name": "limit_tokens_week"`.

- [ ] **Step 2: Delete the duplicate stat panels and reflow the OVERVIEW row**

Delete panel objects `"id": 15` (`5h — /usage says`) and `"id": 16` (`Weekly — /usage says`) entirely.

Retitle panel id 1 to `"5h block — % used (from /usage)"` and panel id 2 to `"Weekly — % used (from /usage)"`. Leave their `targets`/`expr` exactly as they are — those queries are correct and already read `/usage`.

Reflow the row to fill the freed space (4 panels across 24 columns):
- panel 1 `gridPos` → `{"x": 0, "y": 1, "w": 6, "h": 6}`
- panel 2 `gridPos` → `{"x": 6, "y": 1, "w": 6, "h": 6}`
- panel 3 `gridPos` → `{"x": 12, "y": 1, "w": 6, "h": 6}`
- panel 4 `gridPos` → `{"x": 18, "y": 1, "w": 6, "h": 6}`

Change nothing else about panels 3 and 4.

Set panel 1's `description` to:

```
The real number from Anthropic's own /usage command (src/collector/internal/usagetruth), not something derived from OTel token counts.
```

Set panel 2's `description` to:

```
The real number from Anthropic's own /usage command (src/collector/internal/usagetruth). The Weekly breakdown table below scales its own token-derived shares by this same percentage — see that panel's description.
```

- [ ] **Step 3: Rewrite panel id 10's ("Weekly breakdown") query**

Replace panel id 10's `expr` with exactly this (as a single-line JSON string, with `\"` for the double quotes and literal backticks preserved). This is the verified expression — note there is **no** `100 *` prefix, and `week_pct` uses an inner `by (user_email)`:

```
label_replace(label_replace((sum by (skill_name, model, effort, query_source)(sum_over_time({service_name="claude-code"} | event_name = `api_request` | user_email =~ `$account` | unwrap input_tokens [$__range])) + sum by (skill_name, model, effort, query_source)(sum_over_time({service_name="claude-code"} | event_name = `api_request` | user_email =~ `$account` | unwrap output_tokens [$__range])) + sum by (skill_name, model, effort, query_source)(sum_over_time({service_name="claude-code"} | event_name = `api_request` | user_email =~ `$account` | unwrap cache_creation_tokens [$__range]))) * on() group_left() (sum(last_over_time({service_name="claude-code-usage-truth"} | user_email =~ `$account` | unwrap week_pct [30m]) by (user_email))) / on() group_left() (sum(sum_over_time({service_name="claude-code"} | event_name = `api_request` | user_email =~ `$account` | unwrap input_tokens [7d])) + sum(sum_over_time({service_name="claude-code"} | event_name = `api_request` | user_email =~ `$account` | unwrap output_tokens [7d])) + sum(sum_over_time({service_name="claude-code"} | event_name = `api_request` | user_email =~ `$account` | unwrap cache_creation_tokens [7d]))), "model", "$1", "model", "claude-(.+)"), "model", "$1", "model", "(.+)-[0-9]{8}$")
```

In panel id 10's `fieldConfig.overrides[0].properties`, change the `"max"` property's `"value"` from `10` to `100`.

Add a top-level `"description"` to panel id 10:

```
Each row's percentage is not a share of a configured limit — there isn't one anymore. It's this row's token share (input + output + cache creation, over the selected time range) of the account's trailing 7-day token total, scaled by the real week_pct that /usage reports. Set the range to 7 days and the rows sum to exactly week_pct; narrow it and they sum to that range's contribution toward the week. 'Trailing 7 days' is a fixed-window proxy for 'week-to-date': Anthropic doesn't expose the account's real weekly reset boundary through telemetry, and LogQL range durations must be literals. Two caveats: this assumes tokens count toward the weekly cap linearly across models and effort levels — if Anthropic weights them differently, per-model rows skew relative to their true share; and every row reads 0% early in a week, before /usage reports a nonzero week_pct.
```

- [ ] **Step 4: Validate and re-verify against live Loki**

```bash
python3 -c "import json; json.load(open('grafana/templates/claude-code.json'))" && echo "valid JSON"
```

Then extract panel 10's `expr`, substitute a real account email for `$account` and a literal duration for `$__range`, and confirm against Loki that with `[7d]` substituted for `$__range` the rows sum to that account's `week_pct`. Find a live account with:

```bash
curl -s -G 'http://127.0.0.1:47100/loki/api/v1/query' \
  --data-urlencode 'query=sum by (user_email)(last_over_time({service_name="claude-code-usage-truth"} | unwrap week_pct [30m]) by (user_email))'
```

This re-verification is cheap and catches a JSON-escaping slip that no Go test would.

- [ ] **Step 5: Commit**

```bash
git add grafana/templates/claude-code.json
git commit -m "$(cat <<'EOF'
fix: derive Weekly breakdown percentages from /usage instead of a configured limit

Each row's percentage is now its token share of a fixed trailing 7-day
total, scaled by the real week_pct /usage reports, combined via LogQL
vector matching instead of dividing by the hand-set limit_tokens_week
template variable. With the range set to 7 days the rows sum to exactly
week_pct; narrowing the range shows that range's contribution toward the
week.

Two subtleties the expression encodes deliberately: there is no 100x
prefix (week_pct is already a percentage), and week_pct is aggregated with
an inner `by (user_email)` because week_reset_text is structured metadata
whose wording drifts, forking a new Loki series that a bare sum() would
double-count.

Also deletes the two stat panels (id 15/16) whose LogQL was byte-identical
to the gauges beside them, reflows the OVERVIEW row across the freed
space, and retitles the gauges to make clear the numbers come from /usage.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: `dashboardgen.go` — remove `AccountLimits`/`limitsFor` and the injection

- [ ] **Step 1: Delete the `AccountLimits` type and its doc comment** (~lines 369-374).

- [ ] **Step 2: Narrow `loadLimits`'s doc comment and error text.** Add this doc comment and change both error strings from `"...would revert limits and the ignore list"` to `"...would revert the ignore list"`:

```go
// loadLimits reads account-limits.json and returns it as a generic map.
// The only field GenerateDashboards still reads is "ignore" — anything else
// in the file (e.g. leftover default/accounts/block_5h/week fields from
// before per-account limits were removed) is opaque here and passed through
// unread, so an existing real file with the old shape keeps working without
// migration.
```

- [ ] **Step 3: Delete `limitsFor`** (~lines 391-412).

- [ ] **Step 4: Update `scopeAccount`'s signature and doc comment** — drop the `limits AccountLimits` parameter, giving:

```go
func scopeAccount(template map[string]interface{}, email string, cutlines AllCutlines,
	servers []string, owners []Option, exporterStream, rateHalfLife string,
) (map[string]interface{}, error) {
```

and remove the phrase `sets the limit-reference textboxes,` from its doc comment.

- [ ] **Step 5: Delete the limit-variable injection loop** inside `scopeAccount` — the `for _, nv := range []struct{ name string; value int64 }{{"limit_tokens_5h", limits.Block5h}, {"limit_tokens_week", limits.Week}}` block. Check whether `strconv` is still used elsewhere in the file; if not, drop the import.

- [ ] **Step 6: Update `GenerateDashboards`' account loop** — delete the `accountLimits := limitsFor(limits, email)` line, drop `accountLimits` from the `scopeAccount` call, and drop `, limits %s/5h and %s/week` plus its two `commaFormat(...)` args from the `log(...)` call. The `limits` variable from `loadLimits` stays — it's still used for `limits["ignore"]`.

- [ ] **Step 7: Build and vet.** Expect failures *only* inside `dashboardgen_test.go` at this point (Task 3 fixes them); confirm no non-test errors remain.

- [ ] **Step 8: Commit**

```bash
git add src/dash-generator/internal/dashboardgen/dashboardgen.go
git commit -m "$(cat <<'EOF'
refactor: remove AccountLimits/limitsFor and the limit-variable injection

No dashboard panel references limit_tokens_5h or limit_tokens_week anymore
(see the prior commit) — this was their only producer. loadLimits and
account-limits.json survive purely for the "ignore" list, which
src/wizard's internal/limits package still writes and this package still
reads, unchanged.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `dashboardgen_test.go`

- [ ] **Step 1:** Delete `TestLimitsFor_FallsBackThroughDefaultThenAccount` and `TestLimitsFor_HardcodedFallbackWhenNoDefaultEither` in full. Keep `TestLoadLimits_MissingFileReturnsEmptyDoc` exactly as-is.

- [ ] **Step 2:** In `TestScopeAccount_ReplacesAllPlaceholders`, delete the `limits := AccountLimits{...}` line and remove `limits,` from the `scopeAccount(...)` call.

- [ ] **Step 3:** In `TestScopeAccount_OriginalTemplateUntouched`, remove `AccountLimits{},` from the `scopeAccount(...)` call.

- [ ] **Step 4:** `cd src/dash-generator && go build ./... && go vet ./... && go test ./... -v` — all PASS. `TestGenerateDashboards_*` should pass unmodified.

- [ ] **Step 5: Commit**

```bash
git add src/dash-generator/internal/dashboardgen/dashboardgen_test.go
git commit -m "$(cat <<'EOF'
test: update dashboardgen tests for the removed AccountLimits/limitsFor

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: `grafana/account-limits.example.json`

- [ ] **Step 1: Replace the file's full contents**

```json
{
  "_comment": [
    "TEMPLATE. Copy to account-limits.json and adjust — the real file is not",
    "versioned because it contains email addresses (same rule as the .gitignore",
    "entry for grafana/dashboards/accounts/).",
    "",
    "'ignore': accounts that should not get a dashboard. Useful for a",
    "deactivated or third-party account that still shows up in old Loki data",
    "until retention expires.",
    "",
    "There used to be per-account token-limit fields here ('default' and",
    "'accounts', with block_5h/week). Removed: the Weekly breakdown table's",
    "percentages are derived from /usage's own week_pct now, so there is",
    "nothing left to calibrate by hand. An account-limits.json created before",
    "this change can still carry those fields — dash-generator ignores them",
    "and reads only 'ignore'."
  ],
  "ignore": []
}
```

- [ ] **Step 2:** `python3 -c "import json; json.load(open('grafana/account-limits.example.json'))" && echo "valid JSON"`

- [ ] **Step 3:** `cd src/wizard && go test ./internal/limits/... -v` — all PASS unchanged (these tests write their own inline fixtures; this is a sanity confirmation).

- [ ] **Step 4: Commit**

```bash
git add grafana/account-limits.example.json
git commit -m "$(cat <<'EOF'
docs: drop the token-limit fields from account-limits.example.json

Weekly breakdown percentages come from /usage's real week_pct now, so
there is nothing left to calibrate in this file — it's the ignore list.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: `README.md`

- [ ] **Step 1:** Delete the `### Setting your limit` section in full (it sits between `### Multiple accounts` and `## Dashboards`, and includes the `cp grafana/account-limits.example.json ...` snippet and the "how to calibrate the number" line). Leave the file's normal one-blank-line spacing between sections.

- [ ] **Step 2:** In the dashboard-sections table, replace the Weekly breakdown row with:

```
| **Weekly breakdown** | The week's consumption by model, effort, source, and skill, each row's share of your weekly limit scaled from /usage's real percentage |
```

- [ ] **Step 3:** Replace the `**How do I set my token limit?**` FAQ entry (the one describing `limit = observed_tokens / (usage_percentage / 100)`) with:

```
**How is the Weekly breakdown's percentage column computed, if there's no configured limit anymore?**
Each row's tokens (input + output + cache creation, over the selected time
range) are weighted by that row's share of the account's trailing 7-day
token total, then scaled by the real `week_pct` that `/usage` reports. Set
the range to 7 days and the rows sum to exactly `week_pct`. There's nothing
to calibrate by hand — see the panel's own description in Grafana for the
proportionality assumption it makes.
```

- [ ] **Step 4:** Grep the README for any other mention of `account-limits.json`, `calibrat`, or token limits, and fix anything now inaccurate. Report what you found.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "$(cat <<'EOF'
docs: rewrite the token-limit docs for the /usage-derived Weekly breakdown

Removes the manual "set your limit, calibrate it against /usage" flow —
percentages are computed automatically from /usage's own week_pct now.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Final integration verification

- [ ] **Step 1:** `cd src/dash-generator && go build ./... && go vet ./... && go test ./... -v` — all PASS.
- [ ] **Step 2:** `cd src/wizard && go build ./... && go vet ./... && go test ./...` — all PASS (zero files changed there; this proves it).
- [ ] **Step 3:** `python3 -c "import json; json.load(open('grafana/templates/claude-code.json')); json.load(open('grafana/account-limits.example.json'))" && echo "both valid"`
- [ ] **Step 4:** `grep -rn "limit_tokens_5h\|limit_tokens_week\|AccountLimits\|limitsFor" --include="*.go" --include="*.json" --include="*.md" src/dash-generator grafana README.md` — expect **no output**.

---

## Out of scope — a separate bug this work uncovered

`usagetruth.publish` writes `week_reset_text`/`session_reset_text` as Loki **structured metadata**, which participates in series identity. Because the text is free-form wall-clock wording ("resets Sep 26 at 1:59pm" → "at 2pm"), each wording forks a new series for the same account. Consequences: any naive `sum()` over the usage-truth stream double-counts (see Bug B above), and series cardinality grows with every wording change for as long as retention holds.

This plan works around it correctly with `by (user_email)`. **Do not fix it here** — it's a `src/collector` data-model change, tracked separately.
