# Trunk-based versioning & release — Design

Date: 2026-09-24
Status: approved-pending-review

## Context

The repo has two workflows today: `test.yml` (matrix `go test` per module on
every push/PR) and `release.yml` (cross-compiles `wizard`, `collector`,
`dash-generator` for 5 platforms and publishes a GitHub Release, but only
when someone manually pushes a `v*` tag). There is no automatic versioning,
no changelog, and none of the three binaries knows its own version.

This spec ports the trunk-based release model from `obsidian-mcp`
(reference repo, Python/uv): every push to `main` decides for itself
whether it contains a releasable change, and if so computes the next
version from Conventional Commit history, tags it, and publishes — no
manual tag push. Unlike obsidian-mcp, this repo is pure Go with no
manifest file that carries a `version` field (no `pyproject.toml`
equivalent), and `main` currently has no branch protection, so the design
differs in two ways: the version lives only in git tags (no `VERSION`
file, no version-bump commit), and the changelog commit-back can use the
default `GITHUB_TOKEN` directly (obsidian-mcp needs a PAT specifically
because its branch *is* protected).

Decisions below were confirmed with the repo owner in brainstorming
(2026-09-24): automatic release on every push to `main`; version derived
from git tags only; PR title lint enforcing Conventional Commits (merge
strategy is already squash-only, confirmed via `gh repo view`); version
embedded into all three binaries via `-ldflags`; a generated
`CHANGELOG.md`.

## Goals

- Every push to `main` that contains a releasable commit (`feat`, `fix`,
  `perf`, `refactor`, or a breaking change) automatically gets the next
  semantic version, a GitHub Release with the existing cross-platform
  binaries, and a `CHANGELOG.md` entry — no manual step.
- Non-releasable pushes (`chore`, `docs`, `ci`, `style`, `test`, `build`
  only) publish nothing.
- PR titles are the sole source of commit subjects on `main` (squash-only
  merge already enforced repo-side) and are linted as Conventional
  Commits before merge is allowed.
- Each of the three binaries reports its own build version via
  `--version`.
- No dependency on Node/semantic-release — bash only, consistent with the
  rest of this repo's Go/shell tooling.

## Non-goals

- Docker Hub (or any container registry) publishing — distribution stays
  GitHub Release binaries only, as it is today.
- Enabling GitHub branch protection on `main` to make the new PR checks
  required. This is a repository *setting*, not a code change; flagged as
  a follow-up the owner can apply separately.
- A `VERSION` file or any other in-repo version manifest — the tag is the
  single source of truth.
- Multi-agent Claude Code configuration, `.worktrees/`, and per-worktree
  `docker-compose` isolation — covered by the separate
  `2026-09-24-claude-multiagent-worktree-design.md` spec.

## Design

### New workflow: `pr-title.yml`

Triggered on `pull_request` (`opened`, `edited`, `synchronize`,
`reopened`) against `main`. Single job using
`amannn/action-semantic-pull-request`, same `types` list as
obsidian-mcp's `pr.yml` (`feat fix docs style refactor perf test chore
build ci revert`), `requireScope: false`. This is the only new gate on
PRs; `test.yml` is untouched.

### `release.yml`: trigger change

`on.push.tags: ['v*']` becomes `on.push.branches: [main]`, guarded by
`if: "!contains(github.event.head_commit.message, '[skip ci]')"` (mirrors
obsidian-mcp — prevents the changelog commit described below from
re-triggering itself). Add:

```yaml
concurrency:
  group: release-main
  cancel-in-progress: false
```

so two rapid merges to `main` can't interleave tag/changelog writes.

### New first job: `compute-version`

Runs before the existing build jobs. Logic (bash, ported from
obsidian-mcp's `main.yml` bump step, with the manifest-rewrite parts
removed since there's no file to rewrite):

1. `git fetch --tags`. If no `v*` tag exists, this is the first release:
   `NEXT_VERSION=0.1.0`, `LAST_TAG` is unset (diff range is "all of
   history").
2. Otherwise `LAST_TAG=$(git describe --tags --abbrev=0 --match 'v*')`,
   and the subject range is `git log ${LAST_TAG}..HEAD --format=%s
   --first-parent`.
3. Classify by regex over those subjects, same precedence as
   obsidian-mcp: `^.+(\(.+\))?!:` → MAJOR, else `^feat(\(.+\))?:` →
   MINOR, else `^(fix|perf|refactor)(\(.+\))?:` → PATCH, else no
   releasable subject found → `bumped=false`, job stops here (no tag, no
   changelog, no downstream jobs run).
4. Compute `NEXT_VERSION` from `LAST_TAG` + increment (`0.1.0` bootstrap
   case skips this — it *is* the next version).
5. Generate the changelog section for this version (see below), commit
   `docs(changelog): v${NEXT_VERSION} [skip ci]` to `main` using the
   default `GITHUB_TOKEN` (no PAT needed — `main` is unprotected today,
   confirmed via `gh api repos/.../branches/main/protection` → 404), then
   create annotated tag `v${NEXT_VERSION}` on that new commit and push
   `main` + the tag together (`git push origin main --follow-tags`).
6. Job outputs: `version` (e.g. `0.4.0`, no `v` prefix, matching the
   existing binary artifact naming), `bumped` (`true`/`false`).

### Existing build jobs (`wizard`, `dash-generator`, `collector`)

Unchanged except: `needs: compute-version`, `if:
needs.compute-version.outputs.bumped == 'true'`, and the `go build`
step's args gain:

```
-ldflags "-X main.version=${{ needs.compute-version.outputs.version }}"
```

### Version embedding in binaries

Each of `src/wizard/cmd/wizard/main.go`,
`src/collector/cmd/collector/main.go`,
`src/dash-generator/cmd/dash-generator/main.go` gains a package-level
`var version = "dev"` and a `--version` flag/early check that prints it
and exits 0, following whichever flag-parsing convention that binary
already uses (implementation plan decides the exact mechanics per file).
`"dev"` is what every non-release build (local `go build`, `test.yml`)
reports, since only `release.yml` passes the ldflag.

### Changelog

Format: [Keep a Changelog](https://keepachangelog.com/), one `##
[X.Y.Z] - YYYY-MM-DD` section per release, entries grouped under
`### Added` (from `feat`), `### Fixed` (from `fix`/`perf`/`refactor`),
`### Changed` (breaking-change subjects), built directly from the same
commit subjects `compute-version` already classified — no second pass.
New sections are prepended after the `# Changelog` header (newest first).
The same section body is reused as the GitHub Release's `body` in the
final `release` job, so the two never drift apart.

### Final `release` job

Unchanged from today except `needs` grows to include `compute-version`
and its `if` gains the `bumped == 'true'` guard; `softprops/action-gh-release`
gets `tag_name: v${{ needs.compute-version.outputs.version }}` and `body`
set to the changelog section generated above.

## Edge cases

- **No releasable commits since last tag**: `compute-version` sets
  `bumped=false`; every downstream job (`if: ... == 'true'`) is skipped
  entirely — no tag, no changelog commit, no release. Matches
  obsidian-mcp.
- **First release ever**: no `v*` tag exists → bootstrap to `v0.1.0`
  unconditionally on the first qualifying push, regardless of what the
  triggering commit's own type is (there's no prior baseline to diff
  against). Practical consequence: the *next* ordinary `feat`/`fix` merge
  to `main` after this spec lands will publish a real `v0.1.0` GitHub
  Release — confirmed acceptable with the owner.
- **Rapid successive merges**: `concurrency.group: release-main` with
  `cancel-in-progress: false` queues the second run rather than cancelling
  it, so no release is silently dropped; it recomputes `LAST_TAG` fresh
  (now including the first run's tag) once it starts.
- **Changelog commit re-triggering the workflow**: the `[skip ci]` guard
  on the workflow's own `if` prevents this, same mechanism obsidian-mcp
  uses for its bump commit.
- **PR title not Conventional-Commit-shaped**: blocked at PR review time
  by `pr-title.yml`, so a non-conforming subject should never reach
  `main` in the first place; if one does anyway (e.g. a merge from before
  this spec), `compute-version`'s classifier just treats it as
  non-releasable rather than erroring.

## Testing

GitHub Actions logic can't be unit-tested in the traditional sense, so
validation is: extract the classification+bump arithmetic from
`compute-version` into a small standalone script
(`.github/scripts/compute-next-version.sh`) callable independently of the
workflow, and exercise it in a scratch git repo (or with fabricated `git
log` output) against four constructed histories — a `feat:` commit
(minor bump), a `fix:` commit (patch bump), a `feat!:`/`BREAKING CHANGE`
commit (major bump), and an all-`chore:`/`docs:` history (no bump) —
before merging. The implementation plan runs this validation explicitly
and records the four outputs.

The end-to-end path (tag creation, changelog commit, binary ldflags,
release publish) is only verifiable by actually merging to `main`, since
it needs real GitHub Actions permissions (`GITHUB_TOKEN` push) that don't
exist locally. The first real merge after this ships **is** the test —
flagged above as an accepted, visible consequence, not a hidden one.
