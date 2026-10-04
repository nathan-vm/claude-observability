#!/usr/bin/env bash
# Adds, rotates, or removes one person's Caddy ingest credential in
# config/caddy-ingest-users.txt (see config/Caddyfile and
# docs/specs/2026-09-29-production-auth-gateway-design.md).
# One credential is valid on BOTH ingest routes (OTel Collector, collector
# -> Loki) — there is only ever one line per email.
#
# Usage:
#   scripts/manage-tokens.sh add <email>       # prints the plaintext token ONCE,
#                                              # on a line of the form TOKEN=<token>
#   scripts/manage-tokens.sh remove <email>
#
# CADDY_USERS_FILE overrides the target file (default:
# config/caddy-ingest-users.txt) — used by tests to avoid touching the real
# file.
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "Uso: $0 <add|remove> <email>" >&2
  exit 1
fi

action="$1"
email="$2"
file="${CADDY_USERS_FILE:-config/caddy-ingest-users.txt}"

email_re='^[^[:space:]#]+@[^[:space:]]+$'
if ! [[ "$email" =~ $email_re ]]; then
  echo "email inválido: '$email' (sem espaços nem '#', formato usuario@dominio)" >&2
  exit 1
fi

if [ ! -f "$file" ]; then
  echo "$file não existe — copie o .example antes: cp ${file}.example $file" >&2
  exit 1
fi

case "$action" in
  add)
    token="$(openssl rand -hex 24)"
    hash="$(printf '%s\n' "$token" | docker run --rm -i caddy:2 caddy hash-password)"
    tmp="$(mktemp "$(dirname "$file")/.manage-tokens.XXXXXX")"
    awk -v e="$email" '$1 != e' "$file" > "$tmp"
    printf '%s %s\n' "$email" "$hash" >> "$tmp"
    mv "$tmp" "$file"
    echo "Token gerado para $email (mostrado uma única vez, entregue por um canal seguro):"
    echo "TOKEN=$token"
    echo
    echo "Aplique no gateway rodando:"
    echo "  docker compose -f docker-compose.yaml -f docker-compose.server.yaml exec caddy caddy reload --config /etc/caddy/Caddyfile"
    ;;
  remove)
    if ! awk -v e="$email" '$1 == e { found=1 } END { exit !found }' "$file"; then
      echo "nenhuma credencial encontrada para $email em $file" >&2
      exit 1
    fi
    tmp="$(mktemp "$(dirname "$file")/.manage-tokens.XXXXXX")"
    awk -v e="$email" '$1 != e' "$file" > "$tmp"
    mv "$tmp" "$file"
    echo "credencial de $email removida. Aplique no gateway rodando:"
    echo "  docker compose -f docker-compose.yaml -f docker-compose.server.yaml exec caddy caddy reload --config /etc/caddy/Caddyfile"
    ;;
  *)
    echo "ação '$action' inválida — use 'add' ou 'remove'" >&2
    exit 1
    ;;
esac
