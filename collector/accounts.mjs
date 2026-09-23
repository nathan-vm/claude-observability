// Resolves every Claude Code config directory this machine monitors.
//
// There used to be two consumers of this list, each with its own logic: the
// transcript-exporter container walked a single mounted root (/transcripts)
// that had every account's "projects/" bind-mounted as a subfolder of it, and
// dashboard-generator never needed the list at all. Now that both the
// transcript scan and the /usage capture run as one host process instead of
// separate containers, there is no shared root to mount things under — so
// this resolves the real host paths directly.
//
// The source of truth is two plain environment variables, set by
// bin/agent-setup alongside the OTel telemetry vars in your shell rc (see
// "Enabling telemetry in every session" in README.md) — not a file, so there
// is nothing here to read from disk:
//   CLAUDE_DIR                      the primary account's config directory
//   CLAUDE_OBSERVABILITY_EXTRA_DIRS colon-separated extra ones (like $PATH)
import { homedir } from 'node:os';
import { join } from 'node:path';

function expandHome(value) {
  return value.replaceAll('${HOME}', homedir()).replaceAll('$HOME', homedir());
}

export function resolveConfigDirs() {
  const primary = expandHome(process.env.CLAUDE_DIR || join(homedir(), '.claude'));
  const extras = (process.env.CLAUDE_OBSERVABILITY_EXTRA_DIRS || '')
    .split(':')
    .map((dir) => dir.trim())
    .filter(Boolean)
    .map(expandHome);
  return [...new Set([primary, ...extras])];
}
