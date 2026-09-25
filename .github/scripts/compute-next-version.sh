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

  if [[ ! "${last_tag#v}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "error: last tag '${last_tag}' is not shaped like vMAJOR.MINOR.PATCH" >&2
    exit 1
  fi

  subjects=$(git log "${last_tag}..HEAD" --format="%s" --first-parent)

  increment=""
  if [ -n "$subjects" ] && echo "$subjects" | grep -qE '^[a-z]+(\(.+\))?!:'; then
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
    changed=$(echo "$subjects" | grep -E '^[a-z]+(\(.+\))?!:' || true)
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
