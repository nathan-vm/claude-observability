#!/usr/bin/env bash
# Exercises the prod-Loki write guard. Kept in a file because the guard
# (correctly) blocks any Bash command whose own text contains the patterns.
H="${1:-$(dirname "$0")/block-prod-loki-writes.sh}"
pass=0; fail=0
t() { # $1 = expected ALLOW|DENY, $2 = label, $3 = command string
  out=$(printf '%s' "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":$(printf '%s' "$3" | jq -Rs .)}}" | bash "$H")
  got=ALLOW; [ -n "$out" ] && got=DENY
  if [ "$got" = "$1" ]; then printf '  ok   %-34s %s\n' "$2" "$got"; pass=$((pass+1));
  else printf '  FAIL %-34s expected %s got %s\n' "$2" "$1" "$got"; fail=$((fail+1)); fi
}

echo "-- reads and dev-stack traffic must be ALLOWED --"
t ALLOW 'prod read via -G'        'curl -s -G http://127.0.0.1:47100/loki/api/v1/query --data-urlencode query=x'
t ALLOW 'prod read labels'        'curl -s http://127.0.0.1:47100/loki/api/v1/labels'
t ALLOW 'prod read query_range'   'curl -s "http://127.0.0.1:47100/loki/api/v1/query_range?start=1&end=2"'
t ALLOW 'unrelated command'       'go test ./...'
t ALLOW 'dev-stack dash-generator' 'LOKI_URL=http://127.0.0.1:40100 ./dash-generator --once'
t ALLOW 'dev-stack POST'          'curl -X POST http://127.0.0.1:40100/api/v1/foo -d x'
t ALLOW 'worktree-port collector' 'LOKI_URL=http://127.0.0.1:58800 go run ./cmd/collector'

echo "-- writes to prod must be DENIED --"
t DENY 'prod push endpoint'       'curl -X POST http://127.0.0.1:47100/loki/api/v1/push -d @x'
t DENY 'prod delete endpoint'     'curl -XDELETE "http://localhost:47100/loki/api/v1/delete?query=x"'
t DENY 'prod PUT'                 'curl -X PUT http://127.0.0.1:47100/x'
t DENY 'prod PATCH'               'curl --request PATCH http://127.0.0.1:47100/x'
t DENY 'prod body without -G'     'curl http://127.0.0.1:47100/x --data-raw y'
t DENY 'prod -d without -G'       'curl http://127.0.0.1:47100/x -d y'
t DENY 'prod dash-generator'      'LOKI_URL=http://127.0.0.1:47100 ./dash-generator --once'
t DENY 'prod dash-gen ipv6'       'LOKI_URL=http://[::1]:47100 ./dash-generator'
t DENY 'prod collector via go run' 'LOKI_URL=http://127.0.0.1:47100 go run ./cmd/collector'
t DENY 'prod collector binary'    'LOKI_URL=http://localhost:47100 ./.bin/claude-observability-collector'
t DENY 'push endpoint any port'   'curl -X POST http://example.com/loki/api/v1/push -d @x'

echo
echo "pass=$pass fail=$fail"
[ "$fail" = "0" ]
