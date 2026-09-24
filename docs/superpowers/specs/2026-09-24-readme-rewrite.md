# README rewrite — spec

**Requested by:** Nathan, 2026-09-24 (via `/superpowers:writing-plans` arguments)

## Problem

The current `README.md` (834 lines) was written as a personal engineering
journal: exact measured numbers, "measured here," war stories about bugs
that were fixed, and deep rationale for every LogQL query. That was the
right way to write it while this was a one-person local tool. The project
is now open source, and a new reader needs to understand in a few minutes
*what this is, why each piece exists, what runs on their machine vs.
centrally, and how to get it running* — not read a post-mortem.

## Goals

1. **Professional, intuitive, concise.** A newcomer to the repo should be
   able to understand the architecture and get the stack running without
   reading the source.
2. **Explain the necessity of each component.** Not just "what" (OTel
   Collector, Loki, Grafana, dash-generator, collector, wizard) but "why
   this one exists and what breaks without it."
3. **Client vs. server split, explicit.** The repo runs everything
   locally today, but the architecture is designed for a real deployment
   where users only ever send OTLP and a central server does the rest.
   Every component needs an explicit "runs where" label so a reader
   doesn't confuse "runs in Docker on my laptop" with "will run in Docker
   on my laptop forever."
4. **FAQ for extra content.** The deep design rationale, edge cases, and
   "why not X" reasoning that made the old README valuable but long moves
   to a Q&A section, so the main body stays a straight line: what is
   this → why do I need each piece → how do I run it → what do I do when
   something's wrong.

## Non-goals

- Not adding a `LICENSE` or `CONTRIBUTING.md` — out of scope for this
  pass (flagged separately to the user; not part of this plan).
- Not changing any code, config, or dashboard behavior — README content
  only. Every command, path, port, and variable name in the new README
  must match what the source actually does today.
- Not preserving every fact from the old README. Exact measured numbers,
  one-off bug post-mortems, and step-by-step derivations that belong in
  code comments or the existing design specs
  (`docs/superpowers/specs/2026-09-23-*.md`) are cut, not migrated.

## Structure decision

Old README (834 lines, 1 file, chronological/stream-of-consciousness) →
new README (~230 lines), section order chosen so each section answers the
question a reader would have *next*:

| # | Section | Question it answers |
|---|---|---|
| 1 | Title + "Why this exists" | What is this, and why would I want it? |
| 2 | Architecture (diagram) | How does data get from Claude Code to a dashboard? |
| 3 | Components table (client/server column) | What does each piece do, and where does it run? |
| 4 | Quick start | How do I get this running? |
| 5 | Services table | What ports/URLs exist once it's running? |
| 6 | Configuration | How do I customize it (accounts, limits, knobs)? |
| 7 | Dashboards | What will I actually see? |
| 8 | Managing the stack | How do I stop/restart/check on it? |
| 9 | Troubleshooting | What do I do when something looks wrong? |
| 10 | Adding other tools later | Is this extensible? |
| 11 | FAQ | Everything else — design rationale, edge cases |

Content from the old README not carried into the main body (exact
percentages, specific bug stories, LogQL internals, panel-by-panel click
mechanics) is either cut entirely (superseded detail, e.g. the dropped
`usage-meter.mjs`) or condensed into one FAQ answer each, keeping only the
conclusion and the one-sentence reason, not the derivation.

## Acceptance criteria

- Every command, port, file path, env var name, and binary name in the
  new README is verified against the actual source (`docker-compose.yaml`,
  `src/*/cmd/*/main.go`, `.github/workflows/*.yml`) — not carried over
  from the old README on faith.
- Every internal anchor link (`#faq`, `#adding-other-tools-later`, …)
  resolves to a real heading.
- No section requires the reader to already know the codebase.
