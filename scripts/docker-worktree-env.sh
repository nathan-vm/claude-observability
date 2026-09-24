#!/usr/bin/env bash
# Generates COMPOSE_PROJECT_NAME + isolated host ports for the worktree this
# is run from, so `docker compose --env-file docker-worktree.local up -d`
# can run in several worktrees at once without colliding.
#
# The four base ports (47100/47300/47317/47318) are only ~218 apart, so a
# flat "add the offset" scheme (safe when ports are thousands apart) isn't
# safe here — a small offset could push one port's band into a neighboring
# port's. Each port instead gets base + (offset * 300): 300 divides none of
# the pairwise gaps between the four bases (1, 17, 18, 200, 217, 218), so no
# combination of offsets, across any two worktrees, can ever collide.
#
# Usage: run from inside the worktree, before `docker compose up`:
#   scripts/docker-worktree-env.sh
#   docker compose --env-file docker-worktree.local up -d --build
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
worktree_name="$(basename "$repo_root")"

project_name="$(echo "$worktree_name" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9' '-' | sed 's/-\+/-/g; s/^-//; s/-$//')"

# A name that sanitizes to empty (e.g. all-punctuation) would collide
# silently at offset 0 with an un-isolated instance — fall back to a
# hash-derived name instead.
if [ -z "$project_name" ]; then
  project_name="wt-$(echo -n "$repo_root" | cksum | cut -d' ' -f1)"
fi

max_offset=50
offset=$(( $(echo -n "$project_name" | cksum | cut -d' ' -f1) % max_offset + 1 ))

other_worktrees="$(git worktree list --porcelain | awk '/^worktree /{print substr($0, 10)}')"

for _ in $(seq 1 "$max_offset"); do
  collision=0
  candidate_grpc_port=$((47317 + offset * 300))
  while IFS= read -r wt; do
    [ -n "$wt" ] || continue
    [ "$wt" = "$repo_root" ] && continue
    other_env="$wt/docker-worktree.local"
    [ -f "$other_env" ] || continue
    other_grpc_port="$(grep '^OTEL_GRPC_PORT=' "$other_env" 2>/dev/null | cut -d= -f2)"
    if [ "$candidate_grpc_port" = "$other_grpc_port" ]; then
      collision=1
      break
    fi
  done <<EOF
$other_worktrees
EOF
  [ "$collision" = 0 ] && break
  offset=$(( offset % max_offset + 1 ))
done

loki_port=$((47100 + offset * 300))
grafana_port=$((47300 + offset * 300))
otel_grpc_port=$((47317 + offset * 300))
otel_http_port=$((47318 + offset * 300))

out_file="$repo_root/docker-worktree.local"

cat >"$out_file" <<EOF
COMPOSE_PROJECT_NAME=$project_name
OTEL_GRPC_PORT=$otel_grpc_port
OTEL_HTTP_PORT=$otel_http_port
GRAFANA_PORT=$grafana_port
LOKI_PORT=$loki_port
EOF

cat <<EOF
worktree:      $worktree_name
project:       $project_name
otel grpc:     $otel_grpc_port
otel http:     $otel_http_port
grafana:       $grafana_port
loki:          $loki_port

arquivo gerado: $out_file
rode: docker compose --env-file docker-worktree.local up -d --build
EOF
