#!/usr/bin/env node
// The single collection service for this stack. Docker only runs the always-on
// pieces now (otel-collector, loki, grafana) — everything that has to poll or
// run the `claude` CLI runs here instead, as one host process, because it
// needs things a container doesn't have: the local Claude Code config
// directories at their real paths, and (for usage-truth) the host's own
// `claude` binary and its logged-in session. See bin/install-service.sh for
// running this as a launchd service, and README.md for why launchd rather
// than systemd (this machine is macOS).
//
// Configuration is plain environment variables — CLAUDE_DIR,
// CLAUDE_OBSERVABILITY_EXTRA_DIRS, EXPORTER_STREAM, RATE_HALFLIFE,
// POLL_SECONDS, DASHBOARD_INTERVAL_SECONDS, WEEK_START_DAY/HOUR,
// TZ_OFFSET_HOURS — the same ones bin/agent-setup writes into your shell rc
// alongside the OTel telemetry vars (see README's "Enabling telemetry in
// every session"). There is no .env file: install-service.sh resolves these
// once at install time (sourcing your shell's rc) and bakes them into the
// LaunchAgent's own environment, since launchd does not inherit your shell.
//
// Two cadences in one process, not one:
//   - every POLL_SECONDS (default 60s): transcript scan, rate-meter,
//     usage-meter, usage-truth — all cheap, all worth keeping fresh.
//   - every DASHBOARD_INTERVAL_SECONDS (default 600s): dashboard
//     regeneration. Left slower on purpose — it spawns 7-30 day Loki queries
//     per account for P75/cutlines, which do not meaningfully change minute
//     to minute. Collapsing this to 60s too would just be 10x the Loki load
//     for no fresher information; see README's "Resource usage".
//
// Usage:
//   node collector.mjs            # runs both loops until killed
//   node collector.mjs --once     # one pass of each loop, then exit
//   node collector.mjs --dry-run  # transcript-scan/rate/usage-meter write
//                                 # nothing; dashboard-gen and usage-truth's
//                                 # Loki publish are skipped too

import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import {
  initState, runPass, saveState, log,
  LOKI_URL, ONCE, DRY_RUN,
  WEEK_START_DAY, WEEK_START_HOUR, TZ_OFFSET_HOURS,
  RATE_HALFLIFE, RATE_HALFLIFE_S, RATE_BACKFILL_DAYS,
} from './transcript-scan.mjs';
import { publishRate } from './rate-meter.mjs';
import { publishUsage } from './usage-meter.mjs';
import { publishUsageTruth } from './usage-truth.mjs';
import { generateDashboards } from './dashboard-generator.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const OBS_ROOT = join(HERE, '..');

const POLL_SECONDS = Number(process.env.POLL_SECONDS || 60);
const DASHBOARD_INTERVAL_SECONDS = Number(process.env.DASHBOARD_INTERVAL_SECONDS || 600);
const LIMITS_PATH = join(OBS_ROOT, 'grafana', 'account-limits.json');

// Per-account week_start_day/hour, written by usage-truth.mjs from the real
// reset text /usage reports. Missing file or missing entries just mean
// "no override yet" — usage-meter.mjs falls back to WEEK_START_DAY/HOUR.
async function loadWeekOverrides() {
  try {
    const limits = JSON.parse(await readFile(LIMITS_PATH, 'utf8'));
    const overrides = {};
    for (const [email, entry] of Object.entries(limits.accounts ?? {})) {
      if (entry.week_start_day !== undefined && entry.week_start_hour !== undefined) {
        overrides[email] = { weekStartDay: entry.week_start_day, weekStartHour: entry.week_start_hour };
      }
    }
    return overrides;
  } catch (error) {
    if (error.code === 'ENOENT') return {};
    log(`week-start overrides unreadable, using the global default: ${error.message}`);
    return {};
  }
}

async function collectionPass(state) {
  try {
    const count = await runPass(state);
    if (count) log(`${count} tool call(s) exported`);
  } catch (error) {
    log(`transcript scan failed: ${error.message}`);
  }
  // rate-meter and usage-meter measure what OTel already recorded — they
  // must republish even on a pass with nothing new to export.
  try {
    await publishRate({
      dryRun: DRY_RUN, lokiUrl: LOKI_URL, halfLifeS: RATE_HALFLIFE_S,
      halfLifeLabel: RATE_HALFLIFE, backfillDays: RATE_BACKFILL_DAYS, state, log,
    });
    if (!DRY_RUN) await saveState(state);
  } catch (error) {
    log(`rate meter failed: ${error.message}`);
  }
  try {
    await publishUsage({
      dryRun: DRY_RUN, lokiUrl: LOKI_URL, weekStartDay: WEEK_START_DAY,
      weekStartHour: WEEK_START_HOUR, tzOffsetHours: TZ_OFFSET_HOURS,
      weekStartOverrides: await loadWeekOverrides(), log,
    });
  } catch (error) {
    log(`usage meter failed: ${error.message}`);
  }
  try {
    const calibrated = await publishUsageTruth({
      lokiUrl: LOKI_URL, limitsPath: LIMITS_PATH,
      tzOffsetHours: TZ_OFFSET_HOURS, log, dryRun: DRY_RUN,
    });
    if (calibrated) log(`usage-truth: recalibrated ${calibrated} account(s) in account-limits.json`);
  } catch (error) {
    log(`usage-truth failed: ${error.message}`);
  }
}

async function dashboardPass() {
  if (DRY_RUN) return;
  try {
    await generateDashboards();
  } catch (error) {
    log(`dashboard generation failed: ${error.message}`);
  }
}

async function loop(intervalSeconds, run) {
  for (;;) {
    await run();
    if (ONCE) return;
    await new Promise((resolve) => setTimeout(resolve, intervalSeconds * 1000));
  }
}

async function main() {
  const state = await initState();
  await Promise.all([
    loop(POLL_SECONDS, () => collectionPass(state)),
    loop(DASHBOARD_INTERVAL_SECONDS, dashboardPass),
  ]);
}

main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
