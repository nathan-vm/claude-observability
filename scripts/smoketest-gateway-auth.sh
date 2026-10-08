#!/usr/bin/env bash
# Proves the mechanism config/Caddyfile relies on — basic_auth + import of
# config/caddy-ingest-users.txt's format + zero-downtime revocation via
# `caddy reload` + the Loki ingest path allowlist — by running the real
# config/Caddyfile (LOKI_INGEST_DOMAIN on a plain-HTTP throwaway port, a stub
# standing in for Loki) with no real DNS/TLS/stack required. Does NOT prove gRPC-through-Caddy works (needs a
# real OTLP client and TLS — see the spec's own flagged gap and Task 8's
# manual verification note).
set -euo pipefail

workdir="$(mktemp -d)"
trap 'docker rm -f gateway-smoketest gateway-smoketest-loki >/dev/null 2>&1 || true; docker network rm gateway-smoketest-net >/dev/null 2>&1 || true; rm -rf "$workdir"' EXIT

users_file="$workdir/caddy-ingest-users.txt"
echo "# smoketest" > "$users_file"
cp config/Caddyfile "$workdir/Caddyfile"

CADDY_USERS_FILE="$users_file" scripts/manage-tokens.sh add alice@example.com > "$workdir/alice.out"
alice_token="$(sed -n 's/^TOKEN=//p' "$workdir/alice.out")"
CADDY_USERS_FILE="$users_file" scripts/manage-tokens.sh add bob@example.com > "$workdir/bob.out"
bob_token="$(sed -n 's/^TOKEN=//p' "$workdir/bob.out")"

# Mount the whole workdir (not the individual files) at /etc/caddy: a
# single-file bind mount can go stale when the host atomically replaces the
# source (mktemp+mv, as manage-tokens.sh does for revocation) after the
# container has started — mounting the containing directory instead lets
# the container see each rename immediately.
docker network create gateway-smoketest-net > /dev/null
docker run -d --name gateway-smoketest-loki --network gateway-smoketest-net \
  --network-alias loki caddy:2 caddy respond --listen :3100 "loki-ok" > /dev/null
docker run -d --name gateway-smoketest --network gateway-smoketest-net -p 18080:8080 \
  -e OTEL_DOMAIN=:8081 -e LOKI_INGEST_DOMAIN=:8080 -e GRAFANA_DOMAIN=:8082 \
  -v "$workdir:/etc/caddy:ro" \
  caddy:2 > /dev/null
sleep 2

code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

got="$(code http://localhost:18080/loki/api/v1/labels)"
[ "$got" = "401" ] && echo "no credentials -> 401: OK" || { echo "no credentials -> $got, want 401" >&2; exit 1; }

got="$(code -u "alice@example.com:$alice_token" http://localhost:18080/loki/api/v1/labels)"
[ "$got" = "200" ] && echo "alice's credential -> 200: OK" || { echo "alice's credential -> $got, want 200" >&2; exit 1; }

got="$(code -u "bob@example.com:$bob_token" http://localhost:18080/loki/api/v1/labels)"
[ "$got" = "200" ] && echo "bob's credential -> 200: OK" || { echo "bob's credential -> $got, want 200" >&2; exit 1; }

push=http://localhost:18080/loki/api/v1/push

got="$(code -u "alice@example.com:$alice_token" "$push")"
[ "$got" = "200" ] && echo "allowlisted path /loki/api/v1/push -> 200: OK" || { echo "allowlisted path -> $got, want 200" >&2; exit 1; }

got="$(code -u "alice@example.com:$alice_token" -X POST http://localhost:18080/loki/api/v1/delete)"
[ "$got" = "403" ] && echo "authenticated POST /loki/api/v1/delete -> 403: OK" || { echo "POST /loki/api/v1/delete -> $got, want 403" >&2; exit 1; }

got="$(code -u "alice@example.com:$alice_token" --path-as-is http://localhost:18080/loki/api/v1/push/../delete)"
[ "$got" = "403" ] && echo "path traversal out of the allowlist -> 403: OK" || { echo "path traversal -> $got, want 403" >&2; exit 1; }

got="$(code -X POST http://localhost:18080/loki/api/v1/delete)"
[ "$got" = "401" ] && echo "unauthenticated POST /loki/api/v1/delete -> 401: OK" || { echo "unauthenticated delete -> $got, want 401" >&2; exit 1; }

CADDY_USERS_FILE="$users_file" scripts/manage-tokens.sh remove alice@example.com > /dev/null
docker exec gateway-smoketest caddy reload --config /etc/caddy/Caddyfile > /dev/null

got="$(code -u "alice@example.com:$alice_token" http://localhost:18080/loki/api/v1/labels)"
[ "$got" = "401" ] && echo "alice revoked -> 401: OK" || { echo "alice revoked -> $got, want 401" >&2; exit 1; }

got="$(code -u "bob@example.com:$bob_token" http://localhost:18080/loki/api/v1/labels)"
[ "$got" = "200" ] && echo "bob still works after alice's revocation -> 200: OK" || { echo "bob after revocation -> $got, want 200" >&2; exit 1; }

echo "all gateway auth checks passed"
