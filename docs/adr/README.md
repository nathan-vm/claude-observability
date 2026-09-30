# Architecture Decision Records

One file per significant technical decision — a dependency choice, a
cross-module contract, a data-model tradeoff — that's worth recording
*because* it won't be obvious from the code later. Not for routine
implementation choices; those belong in the plan/spec for the change,
or nowhere at all if they're not worth writing down.

Naming: `NNNN-short-title.md`, numbered sequentially starting at `0001`.

Minimal shape:

```markdown
# NNNN. Title

Status: proposed | accepted | superseded by NNNN

## Context
What forced this decision.

## Decision
What was decided.

## Consequences
What this makes easier or harder going forward.
```
