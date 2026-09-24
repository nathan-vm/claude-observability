#!/usr/bin/env bash
# Creates an isolated worktree at .worktrees/<tipo>-<descrição>, on branch
# <tipo>/<descrição> from origin/main, and already generates its isolated
# docker-compose env (scripts/docker-worktree-env.sh) — one command instead
# of three manual steps.
#
# Usage:
#   scripts/worktree-add.sh <tipo> <descrição-kebab-case>
#   scripts/worktree-add.sh feat grafana-per-account-panel
#   # -> .worktrees/feat-grafana-per-account-panel, branch feat/grafana-per-account-panel
#
# <tipo> matches the Conventional Commit types pr-title.yml enforces.
set -euo pipefail

tipos_validos="feat fix docs style refactor perf test build ci chore revert"

if [ $# -ne 2 ]; then
  echo "Uso: $0 <tipo> <descrição-kebab-case>" >&2
  echo "Ex.:  $0 feat grafana-per-account-panel" >&2
  echo "Tipos válidos: $tipos_validos" >&2
  exit 1
fi

tipo="$1"
descricao="$2"

if ! grep -qw "$tipo" <<<"$tipos_validos"; then
  echo "tipo '$tipo' não é um dos aceitos por pr-title.yml: $tipos_validos" >&2
  exit 1
fi

if ! [[ "$descricao" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]]; then
  echo "descrição deve ser kebab-case (recebido: '$descricao')." >&2
  exit 1
fi

repo_root="$(git rev-parse --show-toplevel)"
slug="${tipo}-${descricao}"
worktree_path="$repo_root/.worktrees/$slug"
branch="$tipo/$descricao"

if [ -e "$worktree_path" ]; then
  echo "Já existe algo em $worktree_path — aborta (remova antes com 'git worktree remove' se for engano)." >&2
  exit 1
fi

if git -C "$repo_root" rev-parse --verify --quiet "$branch" >/dev/null; then
  echo "A branch '$branch' já existe — aborta." >&2
  exit 1
fi

mkdir -p "$repo_root/.worktrees"
git -C "$repo_root" fetch origin main --quiet
git -C "$repo_root" worktree add "$worktree_path" -b "$branch" origin/main

(cd "$worktree_path" && "$repo_root/scripts/docker-worktree-env.sh")

echo
echo "worktree:  $worktree_path"
echo "branch:    $branch"
