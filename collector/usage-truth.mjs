// Captures the REAL numbers behind the 5h/7d gauges by running `/usage`
// itself, per account, and republishing them into Loki — plus using them to
// keep grafana/account-limits.json's calibration current automatically.
//
// WHY THIS EXISTS
// usage-meter.mjs derives the two gauges from raw OTel activity: it finds the
// block boundary by scanning for the 5h idle gap, and turns "% used" into
// tokens against a manually calibrated limit in account-limits.json. Both
// steps can drift from what Anthropic's server actually enforces — a stale
// calibration, a block boundary the heuristic got wrong, or Claude Code usage
// from another device/client that never reached this machine's telemetry at
// all. `/usage` is the account's own authoritative answer, so it is the one
// source that can catch all three.
//
// `/usage` costs nothing to poll: it is answered locally by the CLI, never
// sent to the model (confirmed: $0, 0 tokens, ~300ms). CLAUDE_CONFIG_DIR
// selects which account it answers for, the same directories transcript-scan
// already reads transcripts from (see accounts.mjs) — so no separate account
// list to maintain.
//
// WHAT IT PUBLISHES
// One line per account on service_name="claude-code-usage-truth": the real
// session/week percentages and Anthropic's own "resets at" text for each —
// useful on its own as a check on usage-meter's block-boundary math, not only
// as a calibration input.
//
// THEN: for a percentage that came back > 0, it reads that account's current
// block_tokens/week_tokens off the claude-code-usage stream (the same values
// the gauges show) and rewrites account-limits.json's accounts[email] entry
// with limit = meter_tokens / (usage_percentage / 100) — the exact formula
// the README used to ask you to run by hand.

import { execFile } from 'node:child_process';
import { readFile, writeFile, rename } from 'node:fs/promises';
import { promisify } from 'node:util';
import { resolveConfigDirs } from './accounts.mjs';

const execFileAsync = promisify(execFile);
const CLAUDE_TIMEOUT_MS = 30_000;

function escapeRegex(value) {
  return value.replace(/[.+*?()|[\]{}\\^$`]/g, '\\$&');
}

// The UTC offset of an IANA zone at a given instant, in hours (e.g. -3 for
// "America/Sao_Paulo"). Used to convert /usage's reset text — which is always
// in the ACCOUNT's own reporting timezone, not necessarily the one
// TZ_OFFSET_HOURS was set for — into a real instant.
function offsetHoursFor(tzName, atMs) {
  const parts = new Intl.DateTimeFormat('en-US', { timeZone: tzName, timeZoneName: 'shortOffset' })
    .formatToParts(atMs);
  const raw = parts.find((part) => part.type === 'timeZoneName')?.value ?? '';
  const match = /GMT([+-]\d{1,2})(?::(\d{2}))?/.exec(raw);
  if (!match) return null;
  const sign = match[1].startsWith('-') ? -1 : 1;
  return sign * (Math.abs(Number(match[1])) + Number(match[2] || 0) / 60);
}

const MONTHS = ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'];

// Turns "Sep 24 at 8pm (America/Sao_Paulo)" into the weekday/hour
// usage-meter.mjs's currentWeekStart should reset the week on — expressed
// under THIS process's TZ_OFFSET_HOURS, the same one currentWeekStart shifts
// by, regardless of what timezone the account itself reports in.
//
// This is the fix for a real bug found running this against real accounts:
// the code default (Monday 00:00) matched neither account here — one resets
// Thursday evening, the other Saturday afternoon — so the weekly gauge (and
// any calibration built on it) was measuring the wrong 7-day window.
function parseResetWeekday(resetText, referenceMs, tzOffsetHours) {
  const match = /(\w{3})\w*\s+(\d{1,2})\s+at\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(([^)]+)\)/i.exec(resetText);
  if (!match) return null;
  const monthIdx = MONTHS.indexOf(match[1].toLowerCase());
  if (monthIdx < 0) return null;
  const day = Number(match[2]);
  let hour24 = Number(match[3]) % 12;
  if (/pm/i.test(match[5])) hour24 += 12;
  const minute = Number(match[4] || 0);
  const tzName = match[6];

  const accountOffset = offsetHoursFor(tzName, referenceMs);
  if (accountOffset === null) return null;

  const reference = new Date(referenceMs);
  const year = reference.getUTCFullYear();
  let trueUtcMs = Date.UTC(year, monthIdx, day, hour24, minute) - accountOffset * 3600 * 1000;
  // The reset is always within days of "now", never months — if the current-year
  // guess lands well in the past, it actually wrapped into next year (Dec -> Jan).
  if (trueUtcMs < referenceMs - 24 * 3600 * 1000) {
    trueUtcMs = Date.UTC(year + 1, monthIdx, day, hour24, minute) - accountOffset * 3600 * 1000;
  }

  const localShifted = new Date(trueUtcMs + tzOffsetHours * 3600 * 1000);
  return { weekStartDay: localShifted.getUTCDay(), weekStartHour: localShifted.getUTCHours() };
}

async function runClaude(configDir, args) {
  const { stdout } = await execFileAsync('claude', args, {
    env: { ...process.env, CLAUDE_CONFIG_DIR: configDir },
    timeout: CLAUDE_TIMEOUT_MS,
    maxBuffer: 4 * 1024 * 1024,
  });
  return stdout;
}

async function accountEmail(configDir) {
  const stdout = await runClaude(configDir, ['auth', 'status']);
  const status = JSON.parse(stdout);
  return status.loggedIn ? status.email : null;
}

// Parses the two lines /usage always leads with, e.g.:
//   Current session: 13% used · resets Sep 22 at 8:39pm (America/Sao_Paulo)
//   Current week (all models): 42% used · resets Sep 24 at 7:59pm (...)
// The "resets..." clause is absent when the window reads 0% — no block open
// carries no expiry to report, same as usage-meter.mjs's own block_active.
// The rest of /usage's output ("What's contributing...") is explicitly
// scoped by Anthropic to "local sessions on this machine" and is not used
// here — only these two top-line numbers are the server-side, account-wide
// figures that actually throttle you.
function parseUsage(text) {
  const session = /Current session:\s*(\d+)%\s*used(?:\s*·\s*(.+))?/i.exec(text);
  const week = /Current week[^:]*:\s*(\d+)%\s*used(?:\s*·\s*(.+))?/i.exec(text);
  if (!session || !week) throw new Error(`/usage output not in the expected shape: ${text.slice(0, 200)}`);
  return {
    sessionPct: Number(session[1]),
    sessionReset: session[2]?.trim() ?? '',
    weekPct: Number(week[1]),
    weekReset: week[2]?.trim() ?? '',
  };
}

async function fetchUsage(configDir) {
  const stdout = await runClaude(configDir,
    ['-p', '/usage', '--output-format', 'json', '--no-session-persistence']);
  const result = JSON.parse(stdout);
  if (result.is_error) throw new Error(`claude -p "/usage" failed: ${result.result ?? 'unknown error'}`);
  return parseUsage(result.result ?? '');
}

async function lokiInstant(lokiUrl, query) {
  const url = new URL('/loki/api/v1/query', lokiUrl);
  url.searchParams.set('query', query);
  url.searchParams.set('time', String(Math.round(Date.now() / 1000)));
  const response = await fetch(url);
  if (!response.ok) throw new Error(`Loki ${response.status}: ${(await response.text()).slice(0, 160)}`);
  const body = await response.json();
  if (body.status !== 'success') throw new Error(`Loki status=${body.status}`);
  return Number(body.data?.result?.[0]?.value?.[1] ?? 0);
}

// Same values the gauges themselves read (see the templates' block_tokens /
// week_tokens queries) — needed to back-compute a limit from a percentage.
async function meterTokens(lokiUrl, email, field) {
  const pattern = escapeRegex(email);
  const query = `sum(last_over_time({service_name="claude-code-usage"} `
    + `| user_email =~ \`${pattern}\` | unwrap ${field} [30m]))`;
  return lokiInstant(lokiUrl, query);
}

async function publishTruth(lokiUrl, email, usage, dryRun) {
  if (dryRun) return;
  const nowMs = Date.now();
  const payload = {
    streams: [{
      stream: { service_name: 'claude-code-usage-truth', user_email: email },
      values: [[`${nowMs}000000`, 'usage-truth', {
        session_pct: String(usage.sessionPct),
        session_reset_text: usage.sessionReset,
        week_pct: String(usage.weekPct),
        week_reset_text: usage.weekReset,
      }]],
    }],
  };
  const response = await fetch(new URL('/loki/api/v1/push', lokiUrl), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
  if (!response.ok) {
    throw new Error(`usage-truth did not publish: ${response.status} ${(await response.text()).slice(0, 160)}`);
  }
}

async function loadLimits(limitsPath) {
  try {
    return JSON.parse(await readFile(limitsPath, 'utf8'));
  } catch (error) {
    if (error.code === 'ENOENT') return { default: { block_5h: 1_750_000, week: 21_500_000 }, accounts: {}, ignore: [] };
    // Any other error (bad JSON, permissions) must abort the write, not
    // silently revert calibration and the ignore list to nothing.
    throw new Error(`account-limits.json unreadable (${error.message}); leaving it untouched`);
  }
}

async function writeLimits(limitsPath, limits) {
  const tmp = `${limitsPath}.tmp-${process.pid}`;
  await writeFile(tmp, `${JSON.stringify(limits, null, 2)}\n`);
  await rename(tmp, limitsPath);
}

export async function publishUsageTruth({
  lokiUrl, limitsPath, tzOffsetHours, log, dryRun = false,
}) {
  const configDirs = resolveConfigDirs();
  let calibrated = 0;
  let limits = null;

  for (const configDir of configDirs) {
    let email;
    try {
      email = await accountEmail(configDir);
      if (!email) {
        log(`usage-truth: ${configDir} is not logged in, skipping`);
        continue;
      }
    } catch (error) {
      log(`usage-truth: auth status for ${configDir} failed: ${error.message}`);
      continue;
    }

    let usage;
    try {
      usage = await fetchUsage(configDir);
    } catch (error) {
      log(`usage-truth: /usage for ${email} failed: ${error.message}`);
      continue;
    }

    try {
      await publishTruth(lokiUrl, email, usage, dryRun);
    } catch (error) {
      log(`usage-truth: publish for ${email} failed: ${error.message}`);
    }

    if (dryRun) continue;
    try {
      const [blockTokens, weekTokens] = await Promise.all([
        usage.sessionPct > 0 ? meterTokens(lokiUrl, email, 'block_tokens') : Promise.resolve(0),
        usage.weekPct > 0 ? meterTokens(lokiUrl, email, 'week_tokens') : Promise.resolve(0),
      ]);
      const block5h = usage.sessionPct > 0 && blockTokens > 0
        ? Math.round(blockTokens / (usage.sessionPct / 100)) : null;
      const week = usage.weekPct > 0 && weekTokens > 0
        ? Math.round(weekTokens / (usage.weekPct / 100)) : null;
      const weekStart = usage.weekReset ? parseResetWeekday(usage.weekReset, Date.now(), tzOffsetHours) : null;
      if (block5h === null && week === null && !weekStart) continue;

      limits ??= await loadLimits(limitsPath);
      const existing = limits.accounts?.[email] ?? {};
      limits.accounts ??= {};
      limits.accounts[email] = {
        ...existing,
        ...(block5h !== null ? { block_5h: block5h } : {}),
        ...(week !== null ? { week } : {}),
        // The real per-account week boundary, used by usage-meter.mjs instead
        // of the single global WEEK_START_DAY/HOUR guess — see the comment on
        // parseResetWeekday above for why this exists.
        ...(weekStart ? { week_start_day: weekStart.weekStartDay, week_start_hour: weekStart.weekStartHour } : {}),
        _calibrated: `${new Date().toISOString()} auto from /usage = `
          + `${usage.sessionPct}% (block), ${usage.weekPct}% (week)`,
      };
      calibrated += 1;
    } catch (error) {
      log(`usage-truth: calibration for ${email} failed: ${error.message}`);
    }
  }

  if (limits && calibrated) await writeLimits(limitsPath, limits);
  return calibrated;
}
