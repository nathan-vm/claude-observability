# Trunk-Based Release Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every push to `main` automatically computes the next semantic version from Conventional Commit history, tags it, updates `CHANGELOG.md`, and publishes the existing cross-platform binaries as a GitHub Release — no manual tag push.

**Architecture:** A new `compute-version` job runs first on every push to `main` (bash only, no Node/semantic-release), reading commit subjects since the last `v*` tag to decide the bump. If there's a releasable change, it updates `CHANGELOG.md`, commits, tags, and pushes — then the existing per-platform build jobs (now version-aware via `-ldflags`) and the final GitHub Release job run only when a release actually happened. A new `pr-title.yml` workflow enforces Conventional Commit PR titles so `main`'s squash-merge commits are always parseable.

**Tech Stack:** GitHub Actions, bash, Go 1.25 `-ldflags`.

**Spec:** `docs/superpowers/specs/2026-09-24-trunk-based-release-design.md`

## Global Constraints

- No `VERSION` file or any in-repo version manifest — git tags are the sole source of truth.
- No Docker Hub or other registry publishing — distribution stays GitHub Release binaries only.
- `main` has no branch protection (confirmed via `gh api repos/nathan-vm/claude-observability/branches/main/protection` → 404) — the default `GITHUB_TOKEN` can push commits and tags directly, no PAT needed.
- Merge strategy is squash-only (confirmed via `gh repo view --json squashMergeAllowed,mergeCommitAllowed,rebaseMergeAllowed` → only `squashMergeAllowed: true`) — every commit reaching `main` has exactly one subject line, which is the PR title.
- Tag format: `v<major>.<minor>.<patch>` (e.g. `v0.1.0`), no `v` prefix in job outputs (matches existing artifact naming, e.g. `claude-observability-wizard-linux-amd64`).
- Bump classification precedence: `!` or scope-breaking prefix → MAJOR; `feat` → MINOR; `fix`/`perf`/`refactor` → PATCH; anything else → no release.
- First-ever release (no `v*` tag exists yet) is `v0.1.0`, unconditionally, regardless of the triggering commit's own type.
- Changelog format: [Keep a Changelog](https://keepachangelog.com/), newest section first, grouped as `### Changed` (breaking), `### Added` (`feat`), `### Fixed` (`fix`/`perf`/`refactor`).
- Non-release builds (local `go build`, `test.yml`) report version `"dev"` — only `release.yml` passes `-ldflags "-X main.version=..."`.
- The changelog commit's message always ends in `[skip ci]`, and `release.yml`'s `compute-version` job is guarded by `if: "!contains(github.event.head_commit.message, '[skip ci]')"` so that commit's own push doesn't re-trigger a release.

---

### Task 1: `--version` flag for collector

**Files:**
- Modify: `src/collector/cmd/collector/main.go`

**Interfaces:**
- Produces: package-level `var version = "dev"` in `package main`, checked via the existing `hasArg` helper (already defined at the bottom of this file).

- [ ] **Step 1: Add the version var and the early-exit check**

In `src/collector/cmd/collector/main.go`, add `var version = "dev"` right after the `import` block (before `func main()`), and change `func main()` to check for `--version` before calling `run()`:

```go
var version = "dev"

func main() {
	if hasArg("--version") {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

This replaces the existing `func main()` body (lines 15-20 today). `hasArg` is already defined later in the same file — no new import needed.

- [ ] **Step 2: Verify the default ("dev") build reports it**

Run: `cd src/collector && go build -o /tmp/collector-dev ./cmd/collector && /tmp/collector-dev --version`
Expected: prints `dev`

- [ ] **Step 3: Verify an ldflag-injected version reports it**

Run: `cd src/collector && go build -ldflags "-X main.version=1.2.3" -o /tmp/collector-versioned ./cmd/collector && /tmp/collector-versioned --version`
Expected: prints `1.2.3`

- [ ] **Step 4: Run the module's existing tests to confirm nothing broke**

Run: `cd src/collector && go test ./...`
Expected: all packages pass (unchanged — this task doesn't touch any tested package, only `main.go`)

- [ ] **Step 5: Clean up build artifacts and commit**

```bash
rm -f /tmp/collector-dev /tmp/collector-versioned
git add src/collector/cmd/collector/main.go
git commit -m "feat(collector): add --version flag"
```

---

### Task 2: `--version` flag for dash-generator

**Files:**
- Modify: `src/dash-generator/cmd/dash-generator/main.go`

**Interfaces:**
- Produces: package-level `var version = "dev"` in `package main`, checked via the existing `hasArg` helper (already defined at the bottom of this file).

- [ ] **Step 1: Add the version var and the early-exit check**

In `src/dash-generator/cmd/dash-generator/main.go`, add `var version = "dev"` right after the `import` block, and change `func main()`:

```go
var version = "dev"

func main() {
	if hasArg("--version") {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

`hasArg` is already defined later in the same file — no new import needed.

- [ ] **Step 2: Verify the default ("dev") build reports it**

Run: `cd src/dash-generator && go build -o /tmp/dash-generator-dev ./cmd/dash-generator && /tmp/dash-generator-dev --version`
Expected: prints `dev`

- [ ] **Step 3: Verify an ldflag-injected version reports it**

Run: `cd src/dash-generator && go build -ldflags "-X main.version=1.2.3" -o /tmp/dash-generator-versioned ./cmd/dash-generator && /tmp/dash-generator-versioned --version`
Expected: prints `1.2.3`

- [ ] **Step 4: Run the module's existing tests to confirm nothing broke**

Run: `cd src/dash-generator && go test ./...`
Expected: all packages pass

- [ ] **Step 5: Clean up build artifacts and commit**

```bash
rm -f /tmp/dash-generator-dev /tmp/dash-generator-versioned
git add src/dash-generator/cmd/dash-generator/main.go
git commit -m "feat(dash-generator): add --version flag"
```

---

### Task 3: `--version` flag for wizard

**Files:**
- Modify: `src/wizard/cmd/wizard/main.go`

**Interfaces:**
- Produces: package-level `var version = "dev"` in `package main`, plus a local `hasArg` helper (wizard's `main.go` has no such helper today, unlike collector/dash-generator).

- [ ] **Step 1: Add the version var, a hasArg helper, and the early-exit check**

In `src/wizard/cmd/wizard/main.go`, add `var version = "dev"` right after the `import` block, change `func main()`, and add a `hasArg` helper at the end of the file (mirroring the one already in collector/dash-generator's `main.go`):

```go
var version = "dev"

func main() {
	if hasArg("--version") {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

And at the end of the file:

```go
func hasArg(name string) bool {
	for _, a := range os.Args[1:] {
		if a == name {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Verify the default ("dev") build reports it**

Run: `cd src/wizard && go build -o /tmp/wizard-dev ./cmd/wizard && /tmp/wizard-dev --version`
Expected: prints `dev`

- [ ] **Step 3: Verify an ldflag-injected version reports it**

Run: `cd src/wizard && go build -ldflags "-X main.version=1.2.3" -o /tmp/wizard-versioned ./cmd/wizard && /tmp/wizard-versioned --version`
Expected: prints `1.2.3`

- [ ] **Step 4: Run the module's existing tests to confirm nothing broke**

Run: `cd src/wizard && go test ./...`
Expected: all packages pass

- [ ] **Step 5: Clean up build artifacts and commit**

```bash
rm -f /tmp/wizard-dev /tmp/wizard-versioned
git add src/wizard/cmd/wizard/main.go
git commit -m "feat(wizard): add --version flag"
```

---

### Task 4: `compute-next-version.sh` — version + changelog computation

**Files:**
- Create: `.github/scripts/compute-next-version.sh`

**Interfaces:**
- Produces: a standalone, executable bash script, callable as `compute-next-version.sh <changelog-fragment-output-path>` from a git working tree. Prints exactly two lines to stdout: `version=X.Y.Z` and `bumped=true|false`. When `bumped=true`, also writes the Keep-a-Changelog section for that version to `<changelog-fragment-output-path>`.
- Consumes: nothing from earlier tasks — reads `git tag`/`git describe`/`git log` directly.

- [ ] **Step 1: Write the script**

Create `.github/scripts/compute-next-version.sh`:

```bash
#!/usr/bin/env bash
# Computes the next semver from Conventional Commit subjects since the last
# v* tag (bump precedence: breaking > feat > fix/perf/refactor > none), and
# — if there is a release — writes its Keep a Changelog section to the path
# given as $1.
#
# Usage: compute-next-version.sh <changelog-fragment-path>
# Stdout: exactly "version=X.Y.Z" then "bumped=true|false".
set -euo pipefail

changelog_path="$1"

bumped=false
version=""
subjects=""

if ! git tag -l 'v*' | grep -q .; then
  # First release ever: no baseline tag to diff against.
  version="0.1.0"
  bumped=true
  subjects=$(git log --format="%s" --first-parent)
else
  last_tag=$(git describe --tags --abbrev=0 --match 'v*')
  subjects=$(git log "${last_tag}..HEAD" --format="%s" --first-parent)

  increment=""
  if [ -n "$subjects" ] && echo "$subjects" | grep -qE '^.+(\(.+\))?!:'; then
    increment=major
  elif [ -n "$subjects" ] && echo "$subjects" | grep -qE '^feat(\(.+\))?:'; then
    increment=minor
  elif [ -n "$subjects" ] && echo "$subjects" | grep -qE '^(fix|perf|refactor)(\(.+\))?:'; then
    increment=patch
  fi

  if [ -z "$increment" ]; then
    version="${last_tag#v}"
    bumped=false
  else
    IFS='.' read -r major minor patch <<< "${last_tag#v}"
    case "$increment" in
      major) major=$((major + 1)); minor=0; patch=0 ;;
      minor) minor=$((minor + 1)); patch=0 ;;
      patch) patch=$((patch + 1)) ;;
    esac
    version="${major}.${minor}.${patch}"
    bumped=true
  fi
fi

echo "version=${version}"
echo "bumped=${bumped}"

if [ "$bumped" = true ]; then
  {
    echo "## [${version}] - $(date -u +%Y-%m-%d)"
    echo
    changed=$(echo "$subjects" | grep -E '^.+(\(.+\))?!:' || true)
    added=$(echo "$subjects" | grep -E '^feat(\(.+\))?:' || true)
    fixed=$(echo "$subjects" | grep -E '^(fix|perf|refactor)(\(.+\))?:' || true)
    if [ -n "$changed" ]; then
      echo "### Changed"
      echo "$changed" | sed 's/^/- /'
      echo
    fi
    if [ -n "$added" ]; then
      echo "### Added"
      echo "$added" | sed 's/^/- /'
      echo
    fi
    if [ -n "$fixed" ]; then
      echo "### Fixed"
      echo "$fixed" | sed 's/^/- /'
      echo
    fi
  } > "$changelog_path"
fi
```

- [ ] **Step 2: Make it executable**

Run: `chmod +x .github/scripts/compute-next-version.sh`

- [ ] **Step 3: Validate against four constructed histories in a scratch repo**

Run this whole block (creates a throwaway repo under `/tmp`, exercises all four cases, prints each result):

```bash
set -e
scratch=$(mktemp -d)
git -C "$scratch" init -q -b main
git -C "$scratch" config user.email "test@example.com"
git -C "$scratch" config user.name "Test"
script="$(pwd)/.github/scripts/compute-next-version.sh"

# Case A: no tags yet, one arbitrary commit -> bootstrap v0.1.0
git -C "$scratch" commit -q --allow-empty -m "chore: init"
echo "=== Case A (bootstrap) ==="
(cd "$scratch" && "$script" /tmp/changelog-a.md); cat /tmp/changelog-a.md
git -C "$scratch" tag v0.1.0

# Case B: fix: since v0.1.0 -> patch bump, no MAJOR/MINOR
git -C "$scratch" commit -q --allow-empty -m "fix: correct off-by-one in dashboard interval"
echo "=== Case B (patch) ==="
(cd "$scratch" && "$script" /tmp/changelog-b.md); cat /tmp/changelog-b.md
git -C "$scratch" tag v0.1.1

# Case C: feat: since v0.1.1 -> minor bump
git -C "$scratch" commit -q --allow-empty -m "feat: add per-account rate panel"
echo "=== Case C (minor) ==="
(cd "$scratch" && "$script" /tmp/changelog-c.md); cat /tmp/changelog-c.md
git -C "$scratch" tag v0.2.0

# Case D: feat!: since v0.2.0 -> major bump
git -C "$scratch" commit -q --allow-empty -m "feat!: drop legacy account-limits format"
echo "=== Case D (major) ==="
(cd "$scratch" && "$script" /tmp/changelog-d.md); cat /tmp/changelog-d.md
git -C "$scratch" tag v1.0.0

# Case E: only chore: since v1.0.0 -> no release
git -C "$scratch" commit -q --allow-empty -m "chore: tidy comments"
echo "=== Case E (no release) ==="
(cd "$scratch" && "$script" /tmp/changelog-e.md 2>&1 || true)
[ -f /tmp/changelog-e.md ] && echo "UNEXPECTED: changelog file written" || echo "OK: no changelog file written"

rm -rf "$scratch" /tmp/changelog-*.md
```

Expected:
- Case A: `version=0.1.0`, `bumped=true`, changelog fragment is just `## [0.1.0] - <date>` with no `###` subsections — the only commit (`chore: init`) doesn't match `feat`/`fix`/`perf`/`refactor`/breaking, so nothing groups under any of them. This is expected, not a bug.
- Case B: `version=0.1.2`, `bumped=true`, changelog has `### Fixed` with the `fix:` subject.
- Case C: `version=0.2.0`, `bumped=true`, changelog has `### Added` with the `feat:` subject.
- Case D: `version=1.0.0`, `bumped=true`, changelog has `### Changed` with the `feat!:` subject.
- Case E: `version=1.0.0`, `bumped=false`, and `OK: no changelog file written`.

If any case doesn't match, fix the script (most likely culprit: the grep regexes or the `IFS='.' read` version-splitting) before continuing.

- [ ] **Step 4: Commit**

```bash
git add .github/scripts/compute-next-version.sh
git commit -m "feat(ci): add compute-next-version.sh for trunk-based releases"
```

---

### Task 5: Initial `CHANGELOG.md`

**Files:**
- Create: `CHANGELOG.md`

- [ ] **Step 1: Create the file**

Create `CHANGELOG.md` with exactly this content (this exact two-line header is what `release.yml`'s changelog-splice step assumes — see Task 7):

```markdown
# Changelog

```

(Note: the file is the literal string `# Changelog\n\n` — a header line, then one blank line, then EOF.)

- [ ] **Step 2: Verify the header shape the splice step will rely on**

Run: `head -n 2 CHANGELOG.md | cat -A | tail -1`
Expected: the second line is empty (shows just `$`), confirming line 2 is blank — Task 7's `tail -n +3 CHANGELOG.md` depends on exactly this.

- [ ] **Step 3: Commit**

```bash
git add CHANGELOG.md
git commit -m "docs: add CHANGELOG.md"
```

---

### Task 6: `pr-title.yml` — Conventional Commit PR title lint

**Files:**
- Create: `.github/workflows/pr-title.yml`

- [ ] **Step 1: Write the workflow**

Create `.github/workflows/pr-title.yml`:

```yaml
name: PR Title

on:
  pull_request:
    branches: [main]
    types: [opened, edited, synchronize, reopened]

jobs:
  title-check:
    name: Conventional Commit Title
    runs-on: ubuntu-latest
    steps:
      - uses: amannn/action-semantic-pull-request@v5
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        with:
          types: |
            feat
            fix
            docs
            style
            refactor
            perf
            test
            build
            ci
            chore
            revert
          requireScope: false
```

- [ ] **Step 2: Validate YAML syntax**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/pr-title.yml'))" && echo OK`
Expected: `OK`

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/pr-title.yml
git commit -m "ci: lint PR titles as Conventional Commits"
```

---

### Task 7: Rewrite `release.yml` for trunk-based, auto-versioned releases

**Files:**
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `.github/scripts/compute-next-version.sh` (Task 4), `CHANGELOG.md` (Task 5), the three binaries' `-X main.version=...` ldflag support (Tasks 1-3).
- Produces: job outputs `compute-version.outputs.version`, `compute-version.outputs.bumped`, `compute-version.outputs.changelog`, consumed by the `wizard`/`dash-generator`/`collector`/`release` jobs in this same file.

- [ ] **Step 1: Replace the whole file**

Replace the entire contents of `.github/workflows/release.yml` with:

```yaml
name: release
on:
  push:
    branches: [main]

permissions:
  contents: write

concurrency:
  group: release-main
  cancel-in-progress: false

jobs:
  compute-version:
    if: "!contains(github.event.head_commit.message, '[skip ci]')"
    runs-on: ubuntu-latest
    outputs:
      version: ${{ steps.compute.outputs.version }}
      bumped: ${{ steps.compute.outputs.bumped }}
      changelog: ${{ steps.compute.outputs.changelog }}
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Configure git
        run: |
          git config user.name "github-actions[bot]"
          git config user.email "github-actions[bot]@users.noreply.github.com"

      - name: Compute next version, update changelog, tag
        id: compute
        run: |
          output="$(.github/scripts/compute-next-version.sh /tmp/changelog-fragment.md)"
          echo "$output" >> "$GITHUB_OUTPUT"
          version=$(echo "$output" | grep '^version=' | cut -d= -f2)
          bumped=$(echo "$output" | grep '^bumped=' | cut -d= -f2)

          if [ "$bumped" = "true" ]; then
            {
              echo "# Changelog"
              echo
              cat /tmp/changelog-fragment.md
              tail -n +3 CHANGELOG.md
            } > /tmp/CHANGELOG.new
            mv /tmp/CHANGELOG.new CHANGELOG.md

            git add CHANGELOG.md
            git commit -m "docs(changelog): v${version} [skip ci]"
            git tag -a "v${version}" -m "Release v${version}"
            git push origin main --follow-tags

            echo "changelog<<CHANGELOG_EOF" >> "$GITHUB_OUTPUT"
            cat /tmp/changelog-fragment.md >> "$GITHUB_OUTPUT"
            echo "CHANGELOG_EOF" >> "$GITHUB_OUTPUT"
          fi

  wizard:
    needs: compute-version
    if: needs.compute-version.outputs.bumped == 'true'
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - goos: darwin
            goarch: amd64
          - goos: darwin
            goarch: arm64
          - goos: linux
            goarch: amd64
          - goos: linux
            goarch: arm64
          - goos: windows
            goarch: amd64
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25'
      - name: build
        working-directory: src/wizard
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CGO_ENABLED: '0'
        run: |
          ext=""
          if [ "${{ matrix.goos }}" = "windows" ]; then ext=".exe"; fi
          go build -ldflags "-X main.version=${{ needs.compute-version.outputs.version }}" -o "claude-observability-wizard-${{ matrix.goos }}-${{ matrix.goarch }}${ext}" ./cmd/wizard
      - uses: actions/upload-artifact@v4
        with:
          name: binaries-wizard-${{ matrix.goos }}-${{ matrix.goarch }}
          path: src/wizard/claude-observability-wizard-*

  dash-generator:
    needs: compute-version
    if: needs.compute-version.outputs.bumped == 'true'
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - goos: darwin
            goarch: amd64
          - goos: darwin
            goarch: arm64
          - goos: linux
            goarch: amd64
          - goos: linux
            goarch: arm64
          - goos: windows
            goarch: amd64
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25'
      - name: build
        working-directory: src/dash-generator
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CGO_ENABLED: '0'
        run: |
          ext=""
          if [ "${{ matrix.goos }}" = "windows" ]; then ext=".exe"; fi
          go build -ldflags "-X main.version=${{ needs.compute-version.outputs.version }}" -o "claude-observability-dash-generator-${{ matrix.goos }}-${{ matrix.goarch }}${ext}" ./cmd/dash-generator
      - uses: actions/upload-artifact@v4
        with:
          name: binaries-dash-generator-${{ matrix.goos }}-${{ matrix.goarch }}
          path: src/dash-generator/claude-observability-dash-generator-*

  collector:
    needs: compute-version
    if: needs.compute-version.outputs.bumped == 'true'
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - goos: darwin
            goarch: amd64
          - goos: darwin
            goarch: arm64
          - goos: linux
            goarch: amd64
          - goos: linux
            goarch: arm64
          - goos: windows
            goarch: amd64
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25'
      - name: build
        working-directory: src/collector
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CGO_ENABLED: '0'
        run: |
          ext=""
          if [ "${{ matrix.goos }}" = "windows" ]; then ext=".exe"; fi
          go build -ldflags "-X main.version=${{ needs.compute-version.outputs.version }}" -o "claude-observability-collector-${{ matrix.goos }}-${{ matrix.goarch }}${ext}" ./cmd/collector
      - uses: actions/upload-artifact@v4
        with:
          name: binaries-collector-${{ matrix.goos }}-${{ matrix.goarch }}
          path: src/collector/claude-observability-collector-*

  release:
    needs: [compute-version, wizard, dash-generator, collector]
    if: needs.compute-version.outputs.bumped == 'true'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v4
        with:
          path: dist
          merge-multiple: true
      - uses: softprops/action-gh-release@v2
        with:
          tag_name: v${{ needs.compute-version.outputs.version }}
          body: ${{ needs.compute-version.outputs.changelog }}
          files: dist/*
```

- [ ] **Step 2: Validate YAML syntax**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))" && echo OK`
Expected: `OK`

- [ ] **Step 3: Diff-review against the spec's Design section**

Re-read `docs/superpowers/specs/2026-09-24-trunk-based-release-design.md`'s "Design" section side by side with the new `release.yml` and confirm every piece is present: trigger on `push: branches: [main]`, `[skip ci]` guard, `concurrency` block, `compute-version` job producing `version`/`bumped`/`changelog`, all three build jobs gated on `bumped == 'true'` and passing the ldflag, and the `release` job using `tag_name`/`body` from `compute-version`'s outputs.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: make release.yml trunk-based — auto version, changelog, tag on every push to main"
```

---

## Post-implementation note (not a task — informational)

The next ordinary `feat:`/`fix:`/etc. commit that reaches `main` after this plan is merged **will** publish a real `v0.1.0` GitHub Release automatically (see spec's "Edge cases" — this was confirmed as an accepted consequence, not a bug). No task in this plan should attempt to prevent or dry-run that; it's the intended first real exercise of the pipeline.
