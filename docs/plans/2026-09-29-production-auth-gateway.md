# Production Auth Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put a single authenticated TLS gateway (Caddy) in front of a real-server deployment of otel-collector/Loki/Grafana, with per-person revocable credentials on both ingest paths and individual Grafana accounts — additive to, and without changing, today's unauthenticated local single-machine setup.

**Architecture:** Caddy is the only service exposed on `0.0.0.0`, added via a `docker-compose.server.yaml` overlay. It terminates TLS and checks HTTP Basic Auth (one shared credentials file, one credential per person, valid on both ingest routes) in front of the OTel Collector's gRPC receiver and Loki's push/query API; Grafana gets Google OAuth instead. otel-collector, Loki, and dash-generator stay exactly as auth-blind internally as they are today. The wizard's existing OTel-token prompt switches from an (unvalidated) Bearer header to a Basic Auth header Caddy actually checks, and gains a twin prompt for the `collector` process's own Loki credentials — which need no client code changes, since Go's `net/http` applies Basic Auth automatically from a URL's userinfo.

**Tech Stack:** Go 1.22 (wizard module `claude-observability-wizard`), Caddy 2 (≥2.8.0, for the `basic_auth` directive name), Docker Compose overlay files, bash (repo operator tooling, matching `scripts/worktree-add.sh`'s existing convention).

**Spec:** `docs/superpowers/specs/2026-09-29-production-auth-gateway-design.md`

## Global Constraints

- TLS on every publicly reachable endpoint (Caddy's automatic HTTPS via Let's Encrypt — no manual cert handling).
- One credential per person, valid on **both** ingest routes (OTel Collector, `collector`→Loki) — not two separate credentials.
- Individually revocable: edit the credentials file, `caddy reload` — no restart, no other credential affected.
- **All credential checking happens in Caddy only.** otel-collector and Loki are never given their own auth config; the internal `otel-collector → loki` write inside the docker network stays unauthenticated, unchanged.
- Zero behavior change to today's local setup: plain `docker compose up -d` (no `-f docker-compose.server.yaml`) must build, come up, and behave exactly as before.
- `src/collector`'s Go code needs **no changes** — `PushToLoki` and `lokiclient.QueryRange`/`lokiclient.Push` already build requests via `net/http`'s `Client.Do`, which applies HTTP Basic Auth automatically from `url.Parse(lokiURL).User` when no `Authorization` header is already set. Only `LOKI_URL`'s value changes (userinfo embedded), never the collector's code.
- Caddyfile directive is `basic_auth` (renamed from `basicauth` in Caddy 2.8.0) — pin `caddy:2` (current 2.x is well past 2.8.0) and confirm via `caddy validate`, not by assumption.
- gRPC backends need `reverse_proxy ... { transport http { versions h2c } }` — without it Caddy defaults to HTTP/1.1 to the backend, which gRPC cannot use.
- Secrets (the real `config/caddy-ingest-users.txt`, `.env`) are never committed — each gets a `.example` template alongside it, matching this repo's existing `.gitignore` convention ("Cada entrada tem um modelo versionado ao lado").

## Review Focus

- Credential values containing characters special to the OTLP header-list format or URL userinfo (comma, `=`, `:`, `@`, space) must round-trip intact, not truncate or corrupt the auth header/URL — Tasks 1 and 3.
- A wizard run with no ingest email/token entered (purely local stack) must add no `Authorization` header and leave `LOKI_URL` exactly as typed — must not silently emit a broken empty-credential header — Tasks 2 and 3.
- Re-running `scripts/manage-tokens.sh add` for an email that already has a line must replace it (token rotation), not create a duplicate, ambiguous entry — Task 6.
- Revoking a credential (remove line + reload) must reject that exact credential on the very next request while a different, still-valid credential keeps working untouched — Task 7.
- A missing or malformed `config/caddy-ingest-users.txt` must make Caddy fail loudly (`caddy validate`/container start error), never silently serve the ingest routes with no auth — Task 4.

---

## Task 1: Wizard — OTLP header percent-encoding + Basic Auth value helper

**Files:**
- Modify: `src/wizard/cmd/wizard/main.go`
- Test: `src/wizard/cmd/wizard/main_test.go`

**Interfaces:**
- Produces: `percentEncodeHeaderValue(s string) string`, `basicAuthHeaderValue(username, password string) string` — both used by Task 2.

- [ ] **Step 1: Write the failing tests**

Add to `src/wizard/cmd/wizard/main_test.go` (add `"encoding/base64"` and `"net/url"` to its import block alongside the existing `"path/filepath"`, `"regexp"`, `"runtime"`, `"testing"`, and `"claude-observability-wizard/internal/envwriter"`):

```go
func TestPercentEncodeHeaderValue(t *testing.T) {
	cases := map[string]string{
		"Basic dGVzdA==": "Basic%20dGVzdA%3D%3D",
		"a,b":             "a%2Cb",
		"a=b":             "a%3Db",
		"simple":          "simple",
	}
	for in, want := range cases {
		if got := percentEncodeHeaderValue(in); got != want {
			t.Errorf("percentEncodeHeaderValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBasicAuthHeaderValue_RoundTrips(t *testing.T) {
	got := basicAuthHeaderValue("alice@example.com", "tok 123,=")
	const prefix = "Authorization="
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("got %q, want prefix %q", got, prefix)
	}
	decoded, err := url.QueryUnescape(strings.TrimPrefix(got, prefix))
	if err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice@example.com:tok 123,="))
	if decoded != want {
		t.Errorf("decoded = %q, want %q", decoded, want)
	}
}
```

`strings` is not yet imported by `main_test.go` either — add it to the same import block.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd src/wizard && go test ./cmd/wizard/... -run 'TestPercentEncodeHeaderValue|TestBasicAuthHeaderValue_RoundTrips' -v`
Expected: FAIL — `percentEncodeHeaderValue` / `basicAuthHeaderValue` undefined.

- [ ] **Step 3: Implement**

Add to `src/wizard/cmd/wizard/main.go`. Add `"encoding/base64"` and `"strings"` to its import block (it does not yet import either).

```go
// percentEncodeHeaderValue percent-encodes s per RFC 3986 unreserved
// characters (A-Za-z0-9-._~) only — every other byte, including the ","
// and "=" the OTLP header-list format (OTEL_EXPORTER_OTLP_HEADERS) uses as
// its own delimiters, becomes %XX. This guarantees the encoded value can't
// be misread as extra key=value pairs regardless of exactly how/when the
// receiving SDK splits vs. percent-decodes it.
func percentEncodeHeaderValue(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// basicAuthHeaderValue builds the OTEL_EXPORTER_OTLP_HEADERS entry for HTTP
// Basic Auth — Caddy's basic_auth directive validates this at the gateway
// (see docs/superpowers/specs/2026-09-29-production-auth-gateway-design.md).
// Replaces the old Bearer-token entry, which nothing ever validated.
func basicAuthHeaderValue(username, password string) string {
	raw := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "Authorization=" + percentEncodeHeaderValue("Basic "+raw)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd src/wizard && go test ./cmd/wizard/... -run 'TestPercentEncodeHeaderValue|TestBasicAuthHeaderValue_RoundTrips' -v`
Expected: PASS

- [ ] **Step 5: Run the full wizard module build/vet/test**

Run: `cd src/wizard && go build ./... && go vet ./... && go test ./...`
Expected: all pass (no other package references these new functions yet).

- [ ] **Step 6: Commit**

```bash
git add src/wizard/cmd/wizard/main.go src/wizard/cmd/wizard/main_test.go
git commit -m "feat(wizard): add OTLP header percent-encoding and Basic Auth helper"
```

---

## Task 2: Wizard — switch the OTel ingest prompt to Basic Auth (email + token)

**Files:**
- Modify: `src/wizard/cmd/wizard/main.go`
- Test: `src/wizard/cmd/wizard/main_test.go`

**Interfaces:**
- Consumes: `basicAuthHeaderValue(username, password string) string` from Task 1.
- Produces: `otelHeaderVars(ingestEmail, ingestToken string) []envwriter.Var` — pattern later tasks and any future caller can reuse; `defaultEmail` local variable in `run()`, reused by Task 3.

- [ ] **Step 1: Write the failing tests**

Add to `src/wizard/cmd/wizard/main_test.go`:

```go
func TestOtelHeaderVars_EmptyWhenEitherMissing(t *testing.T) {
	if got := otelHeaderVars("", "tok"); got != nil {
		t.Errorf("got %v, want nil", got)
	}
	if got := otelHeaderVars("alice@example.com", ""); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestOtelHeaderVars_BuildsBasicAuthWhenBothSet(t *testing.T) {
	got := otelHeaderVars("alice@example.com", "tok123")
	if len(got) != 1 || got[0].Name != "OTEL_EXPORTER_OTLP_HEADERS" {
		t.Fatalf("got %v, want one OTEL_EXPORTER_OTLP_HEADERS var", got)
	}
	want := basicAuthHeaderValue("alice@example.com", "tok123")
	if got[0].Value != want {
		t.Errorf("Value = %q, want %q", got[0].Value, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd src/wizard && go test ./cmd/wizard/... -run TestOtelHeaderVars -v`
Expected: FAIL — `otelHeaderVars` undefined.

- [ ] **Step 3: Implement**

Add this function to `src/wizard/cmd/wizard/main.go`, near `basicAuthHeaderValue`:

```go
// otelHeaderVars returns the OTEL_EXPORTER_OTLP_HEADERS var to write, or
// nil when no ingest credentials were entered (a purely local stack, where
// nothing validates the header anyway).
func otelHeaderVars(ingestEmail, ingestToken string) []envwriter.Var {
	if ingestEmail == "" || ingestToken == "" {
		return nil
	}
	return []envwriter.Var{{Name: "OTEL_EXPORTER_OTLP_HEADERS", Value: basicAuthHeaderValue(ingestEmail, ingestToken)}}
}
```

Now modify `run()` in the same file. Find:

```go
	chosen, rejected := wizard.ChooseAccounts(out, stdin, accounts)

	fmt.Fprintln(out, "\n── OTel endpoint ───────────────────────────────────────────")
	endpoint := wizard.AskLine(out, stdin, "OTel endpoint", "http://localhost:47317")
	token := wizard.AskLine(out, stdin, "OTel token", "")
```

Replace with:

```go
	chosen, rejected := wizard.ChooseAccounts(out, stdin, accounts)
	defaultEmail := ""
	if len(chosen) > 0 {
		defaultEmail = chosen[0].Email
	}

	fmt.Fprintln(out, "\n── OTel endpoint ───────────────────────────────────────────")
	endpoint := wizard.AskLine(out, stdin, "OTel endpoint", "http://localhost:47317")
	ingestEmail := wizard.AskLine(out, stdin, "OTel ingest email (blank for a purely local stack)", defaultEmail)
	ingestToken := wizard.AskLine(out, stdin, "OTel ingest token", "")
```

Then find, inside the `telemetryVars` construction:

```go
	if token != "" {
		telemetryVars = append(telemetryVars, envwriter.Var{
			Name:  "OTEL_EXPORTER_OTLP_HEADERS",
			Value: "Authorization=Bearer%20" + token,
		})
	}
```

Replace with:

```go
	telemetryVars = append(telemetryVars, otelHeaderVars(ingestEmail, ingestToken)...)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd src/wizard && go test ./cmd/wizard/... -run TestOtelHeaderVars -v`
Expected: PASS

- [ ] **Step 5: Run the full wizard module build/vet/test**

Run: `cd src/wizard && go build ./... && go vet ./... && go test ./...`
Expected: all pass — confirms `run()` still compiles with the renamed variables and the deleted `token` variable has no other references.

- [ ] **Step 6: Commit**

```bash
git add src/wizard/cmd/wizard/main.go src/wizard/cmd/wizard/main_test.go
git commit -m "feat(wizard): switch OTel ingest prompt to Basic Auth (email + token)"
```

---

## Task 3: Wizard — Loki ingest (collector) prompt, LOKI_URL with embedded credentials

**Files:**
- Modify: `src/wizard/cmd/wizard/main.go`
- Test: `src/wizard/cmd/wizard/main_test.go`

**Interfaces:**
- Consumes: `defaultEmail` local variable from Task 2.
- Produces: `lokiIngestURL(endpoint, username, password string) (string, error)`; the collector's `collectorVars` gains a new `LOKI_URL` entry, consumed by `src/collector/cmd/collector/main.go`'s existing `envOr("LOKI_URL", "http://localhost:47100")` (no change needed there — see Global Constraints).

- [ ] **Step 1: Write the failing tests**

Add to `src/wizard/cmd/wizard/main_test.go` (the `url` and `net/url`-derived helpers below reuse the `"net/url"` import added in Task 1):

```go
func TestLokiIngestURL_UnchangedWhenNoCredentials(t *testing.T) {
	got, err := lokiIngestURL("http://localhost:47100", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://localhost:47100" {
		t.Errorf("got %q, want endpoint unchanged", got)
	}
}

func TestLokiIngestURL_EmbedsCredentials(t *testing.T) {
	got, err := lokiIngestURL("https://loki-ingest.example.com", "alice@example.com", "tok:123")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.User.Username() != "alice@example.com" {
		t.Errorf("username = %q, want %q", parsed.User.Username(), "alice@example.com")
	}
	password, ok := parsed.User.Password()
	if !ok || password != "tok:123" {
		t.Errorf("password = %q (set=%v), want %q", password, ok, "tok:123")
	}
	if parsed.Scheme != "https" || parsed.Host != "loki-ingest.example.com" {
		t.Errorf("got %q, want scheme/host preserved", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd src/wizard && go test ./cmd/wizard/... -run TestLokiIngestURL -v`
Expected: FAIL — `lokiIngestURL` undefined.

- [ ] **Step 3: Implement**

Add `"net/url"` to `main.go`'s import block (not yet imported there). Add this function near `otelHeaderVars`:

```go
// lokiIngestURL builds the collector's LOKI_URL: endpoint unchanged when no
// credentials were entered (a purely local stack), or endpoint with
// username:password embedded as URL userinfo otherwise. Go's net/http
// client applies HTTP Basic Auth from a URL's userinfo automatically (see
// docs/superpowers/specs/2026-09-29-production-auth-gateway-design.md), so
// src/collector needs no code change — only this value changes.
func lokiIngestURL(endpoint, username, password string) (string, error) {
	if username == "" || password == "" {
		return endpoint, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid Loki endpoint %q: %w", endpoint, err)
	}
	u.User = url.UserPassword(username, password)
	return u.String(), nil
}
```

Now modify `run()`. Find the collector-vars block:

```go
	var collectorVars []envwriter.Var
	if len(chosen) > 0 {
		collectorVars = append(collectorVars, envwriter.Var{Name: "CLAUDE_DIR", Value: chosen[0].Dir})
		if len(chosen) > 1 {
			extra := chosen[1].Dir
			for _, a := range chosen[2:] {
				extra += ":" + a.Dir
			}
			collectorVars = append(collectorVars, envwriter.Var{Name: "CLAUDE_OBSERVABILITY_EXTRA_DIRS", Value: extra})
		}
		// See resolveExporterStream: ...
		streamValue, err := resolveExporterStream(home)
		if err != nil {
			return err
		}
		collectorVars = append(collectorVars, envwriter.Var{Name: "EXPORTER_STREAM", Value: streamValue})
	}
```

Add, right before the block's closing `}` (after the `EXPORTER_STREAM` append, still inside `if len(chosen) > 0`):

```go
		fmt.Fprintln(out, "\n── Loki ingest (collector) ─────────────────────────────────")
		lokiEndpoint := wizard.AskLine(out, stdin, "Loki endpoint", "http://localhost:47100")
		lokiEmail := wizard.AskLine(out, stdin, "Loki ingest email (blank for a purely local stack)", defaultEmail)
		lokiToken := wizard.AskLine(out, stdin, "Loki ingest token", "")
		lokiURLValue, err := lokiIngestURL(lokiEndpoint, lokiEmail, lokiToken)
		if err != nil {
			return err
		}
		collectorVars = append(collectorVars, envwriter.Var{Name: "LOKI_URL", Value: lokiURLValue})
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd src/wizard && go test ./cmd/wizard/... -run TestLokiIngestURL -v`
Expected: PASS

- [ ] **Step 5: Run the full wizard module build/vet/test**

Run: `cd src/wizard && go build ./... && go vet ./... && go test ./...`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add src/wizard/cmd/wizard/main.go src/wizard/cmd/wizard/main_test.go
git commit -m "feat(wizard): prompt for Loki ingest credentials, embed in LOKI_URL"
```

---

## Task 4: Caddy gateway config

**Files:**
- Create: `config/Caddyfile`
- Create: `config/caddy-ingest-users.txt.example`
- Modify: `.gitignore`

**Interfaces:**
- Produces: three site blocks (`{$OTEL_DOMAIN}`, `{$LOKI_INGEST_DOMAIN}`, `{$GRAFANA_DOMAIN}`) and the credentials-file import path `/etc/caddy/caddy-ingest-users.txt`, both consumed by Task 5's `docker-compose.server.yaml` and Task 6's `scripts/manage-tokens.sh`.

- [ ] **Step 1: Create `config/Caddyfile`**

```caddyfile
# Production auth gateway — the only service a real-server deployment
# exposes publicly. See docs/superpowers/specs/2026-09-29-production-auth-gateway-design.md.
#
# Requires Caddy >= 2.8.0 (the "basic_auth" directive; older Caddy names it
# "basicauth"). config/caddy-ingest-users.txt (git-ignored — see
# config/caddy-ingest-users.txt.example) holds one "<email> <bcrypt-hash>"
# line per person, managed by scripts/manage-tokens.sh.
#
# All credential checking happens here — otel-collector and Loki are never
# given their own auth config, and the internal otel-collector -> loki
# write stays exactly as unauthenticated as it is today.

{$OTEL_DOMAIN} {
	basic_auth * {
		import /etc/caddy/caddy-ingest-users.txt
	}
	# otel-collector's OTLP gRPC receiver speaks cleartext HTTP/2
	# internally (TLS is Caddy's job, at the public edge) — without this,
	# Caddy defaults to HTTP/1.1 to the backend, which gRPC cannot use.
	reverse_proxy otel-collector:4317 {
		transport http {
			versions h2c
		}
	}
}

{$LOKI_INGEST_DOMAIN} {
	basic_auth * {
		import /etc/caddy/caddy-ingest-users.txt
	}
	reverse_proxy loki:3100
}

{$GRAFANA_DOMAIN} {
	# Grafana authenticates itself (Google OAuth — see
	# docker-compose.server.yaml); Caddy just proxies.
	reverse_proxy grafana:3000
}
```

- [ ] **Step 2: Create `config/caddy-ingest-users.txt.example`**

```
# One line per person: <email> <bcrypt-hash>, generated and managed by
# scripts/manage-tokens.sh — never hand-edit a hash in here, only let the
# script add/replace/remove whole lines.
#
# Copy this file to config/caddy-ingest-users.txt (git-ignored — it holds
# real credential hashes) before bringing up docker-compose.server.yaml:
#   cp config/caddy-ingest-users.txt.example config/caddy-ingest-users.txt
#
# Apply changes to a running gateway without downtime:
#   docker compose -f docker-compose.yaml -f docker-compose.server.yaml exec caddy caddy reload --config /etc/caddy/Caddyfile
```

- [ ] **Step 3: Add the real credentials file to `.gitignore`**

Add, in the section listing other "modelo versionado ao lado" entries:

```gitignore
# Real per-person Caddy credentials (bcrypt hashes) — template alongside at
# config/caddy-ingest-users.txt.example. See scripts/manage-tokens.sh.
config/caddy-ingest-users.txt
```

- [ ] **Step 4: Validate the Caddyfile parses and enforces auth (positive case)**

```bash
cp config/caddy-ingest-users.txt.example config/caddy-ingest-users.txt
docker run --rm \
  -v "$(pwd)/config/Caddyfile:/etc/caddy/Caddyfile:ro" \
  -v "$(pwd)/config/caddy-ingest-users.txt:/etc/caddy/caddy-ingest-users.txt:ro" \
  -e OTEL_DOMAIN=otel.example.com \
  -e LOKI_INGEST_DOMAIN=loki-ingest.example.com \
  -e GRAFANA_DOMAIN=grafana.example.com \
  caddy:2 caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
```

Expected: exits 0, prints "Valid configuration".

- [ ] **Step 5: Confirm a missing credentials file fails loudly, not silently**

```bash
docker run --rm \
  -v "$(pwd)/config/Caddyfile:/etc/caddy/Caddyfile:ro" \
  -e OTEL_DOMAIN=otel.example.com \
  -e LOKI_INGEST_DOMAIN=loki-ingest.example.com \
  -e GRAFANA_DOMAIN=grafana.example.com \
  caddy:2 caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
```

Expected: **non-zero exit**, an error mentioning the missing `caddy-ingest-users.txt` (Docker turns the un-mounted bind source into an empty directory, which `import` cannot read as a file). This is the behavior Review Focus's fifth item requires — a missing credentials file must never come up as "no auth" instead of "won't start."

- [ ] **Step 6: Commit**

```bash
git add config/Caddyfile config/caddy-ingest-users.txt.example .gitignore
git commit -m "feat: add Caddy gateway config for production auth"
```

---

## Task 5: docker-compose.server.yaml overlay

**Files:**
- Create: `docker-compose.server.yaml`
- Create: `.env.example`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: `config/Caddyfile`, `config/caddy-ingest-users.txt` (Task 4).
- Produces: the `caddy` service and env-var names (`OTEL_DOMAIN`, `LOKI_INGEST_DOMAIN`, `GRAFANA_DOMAIN`, `GRAFANA_GOOGLE_CLIENT_ID`, `GRAFANA_GOOGLE_CLIENT_SECRET`, `GRAFANA_GOOGLE_ALLOWED_DOMAINS`, `GRAFANA_ADMIN_PASSWORD`) Task 8's docs reference.

- [ ] **Step 1: Create `docker-compose.server.yaml`**

```yaml
# Overlay for a real server deployment: adds Caddy as the only publicly
# exposed service and switches Grafana to individual Google-account login.
# Does NOT replace docker-compose.yaml — it's merged on top of it:
#
#   cp .env.example .env   # fill in real values first
#   docker compose -f docker-compose.yaml -f docker-compose.server.yaml up -d
#
# Plain `docker compose up -d` (no this file) is completely unaffected —
# today's local, unauthenticated single-machine setup doesn't change.
# See docs/superpowers/specs/2026-09-29-production-auth-gateway-design.md.

services:
  caddy:
    image: caddy:2
    ports:
      - "0.0.0.0:80:80"
      - "0.0.0.0:443:443"
    environment:
      - OTEL_DOMAIN=${OTEL_DOMAIN:?set OTEL_DOMAIN in .env — see .env.example}
      - LOKI_INGEST_DOMAIN=${LOKI_INGEST_DOMAIN:?set LOKI_INGEST_DOMAIN in .env — see .env.example}
      - GRAFANA_DOMAIN=${GRAFANA_DOMAIN:?set GRAFANA_DOMAIN in .env — see .env.example}
    volumes:
      - ./config/Caddyfile:/etc/caddy/Caddyfile:ro
      - ./config/caddy-ingest-users.txt:/etc/caddy/caddy-ingest-users.txt:ro
      - caddy-data:/data
      - caddy-config:/config
    depends_on:
      - otel-collector
      - loki
      - grafana
    restart: unless-stopped

  grafana:
    environment:
      - GF_AUTH_ANONYMOUS_ENABLED=false
      - GF_AUTH_GOOGLE_ENABLED=true
      - GF_AUTH_GOOGLE_CLIENT_ID=${GRAFANA_GOOGLE_CLIENT_ID:?set GRAFANA_GOOGLE_CLIENT_ID in .env — see .env.example}
      - GF_AUTH_GOOGLE_CLIENT_SECRET=${GRAFANA_GOOGLE_CLIENT_SECRET:?set GRAFANA_GOOGLE_CLIENT_SECRET in .env — see .env.example}
      - GF_AUTH_GOOGLE_ALLOWED_DOMAINS=${GRAFANA_GOOGLE_ALLOWED_DOMAINS:?set GRAFANA_GOOGLE_ALLOWED_DOMAINS in .env — see .env.example}
      - GF_SECURITY_ADMIN_PASSWORD=${GRAFANA_ADMIN_PASSWORD:?set GRAFANA_ADMIN_PASSWORD in .env — see .env.example}

volumes:
  caddy-data:
  caddy-config:
```

`environment` merges key-by-key across `-f` files regardless of array vs. mapping syntax (Compose normalizes both to a map before merging) — so this overlay only touches the four `GF_*` keys it lists; `docker-compose.yaml`'s `GF_SECURITY_ADMIN_USER`, `GF_USERS_DEFAULT_THEME`, and the alerting/analytics-disable flags survive untouched. Step 4 below proves this concretely rather than trusting it by assumption.

- [ ] **Step 2: Create `.env.example`**

```
# Copy to .env and fill in before `docker compose -f docker-compose.yaml -f docker-compose.server.yaml up -d`.
# See docs/superpowers/specs/2026-09-29-production-auth-gateway-design.md.

# DNS: each must resolve to this server's public IP, and ports 80/443 must
# be reachable from the internet (Caddy's automatic HTTPS uses Let's
# Encrypt's HTTP-01 challenge).
OTEL_DOMAIN=otel.example.com
LOKI_INGEST_DOMAIN=loki-ingest.example.com
GRAFANA_DOMAIN=grafana.example.com

# One-time manual setup in Google Cloud Console (OAuth 2.0 Client ID, Web
# application, authorized redirect URI https://<GRAFANA_DOMAIN>/login/google).
GRAFANA_GOOGLE_CLIENT_ID=REPLACE_ME
GRAFANA_GOOGLE_CLIENT_SECRET=REPLACE_ME
GRAFANA_GOOGLE_ALLOWED_DOMAINS=example.com

# Grafana's built-in admin account stays enabled as a break-glass fallback
# even with OAuth on — pick a strong password, store it outside git.
GRAFANA_ADMIN_PASSWORD=REPLACE_ME
```

- [ ] **Step 3: Add `.env` to `.gitignore`**

Add, next to the `config/caddy-ingest-users.txt` entry from Task 4:

```gitignore
# Real production secrets (Google OAuth client, admin password, domains) —
# template alongside at .env.example.
.env
```

- [ ] **Step 4: Validate the overlay merges correctly**

```bash
docker compose -f docker-compose.yaml -f docker-compose.server.yaml --env-file .env.example config > /tmp/merged-server-config.yaml
grep -q "GF_UNIFIED_ALERTING_ENABLED=\"false\"" /tmp/merged-server-config.yaml && echo "base grafana env survived: OK"
grep -q "GF_AUTH_ANONYMOUS_ENABLED=\"false\"" /tmp/merged-server-config.yaml && echo "override grafana env applied: OK"
grep -q "GF_SECURITY_ADMIN_USER=\"admin\"" /tmp/merged-server-config.yaml && echo "untouched base key survived: OK"
```

Expected: all three lines print "OK" — proving the merge is key-by-key, not a wholesale replacement of Grafana's environment.

- [ ] **Step 5: Commit**

```bash
git add docker-compose.server.yaml .env.example .gitignore
git commit -m "feat: add docker-compose.server.yaml overlay for the auth gateway"
```

---

## Task 6: `scripts/manage-tokens.sh` — token admin script

**Files:**
- Create: `scripts/manage-tokens.sh`

**Interfaces:**
- Consumes: `config/caddy-ingest-users.txt` format (Task 4); `caddy:2`'s `caddy hash-password` subcommand via `docker run`.
- Produces: the `add`/`remove` CLI other tasks (7) and the README (Task 8) reference; `CADDY_USERS_FILE` env var override, used by Task 7's smoke test to point at a throwaway file.

- [ ] **Step 1: Create `scripts/manage-tokens.sh`**

```bash
#!/usr/bin/env bash
# Adds, rotates, or removes one person's Caddy ingest credential in
# config/caddy-ingest-users.txt (see config/Caddyfile and
# docs/superpowers/specs/2026-09-29-production-auth-gateway-design.md).
# One credential is valid on BOTH ingest routes (OTel Collector, collector
# -> Loki) — there is only ever one line per email.
#
# Usage:
#   scripts/manage-tokens.sh add <email>       # prints the plaintext token ONCE
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

if [ ! -f "$file" ]; then
  echo "$file não existe — copie o .example antes: cp ${file}.example $file" >&2
  exit 1
fi

case "$action" in
  add)
    token="$(openssl rand -hex 24)"
    hash="$(docker run --rm caddy:2 caddy hash-password --plaintext "$token")"
    tmp="$(mktemp)"
    grep -v "^${email} " "$file" > "$tmp" || true
    printf '%s %s\n' "$email" "$hash" >> "$tmp"
    mv "$tmp" "$file"
    echo "Token gerado para $email (mostrado uma única vez, entregue por um canal seguro):"
    echo "$token"
    echo
    echo "Aplique no gateway rodando:"
    echo "  docker compose -f docker-compose.yaml -f docker-compose.server.yaml exec caddy caddy reload --config /etc/caddy/Caddyfile"
    ;;
  remove)
    if ! grep -q "^${email} " "$file"; then
      echo "nenhuma credencial encontrada para $email em $file" >&2
      exit 1
    fi
    tmp="$(mktemp)"
    grep -v "^${email} " "$file" > "$tmp" || true
    mv "$tmp" "$file"
    echo "credencial de $email removida. Aplique no gateway rodando:"
    echo "  docker compose -f docker-compose.yaml -f docker-compose.server.yaml exec caddy caddy reload --config /etc/caddy/Caddyfile"
    ;;
  *)
    echo "ação '$action' inválida — use 'add' ou 'remove'" >&2
    exit 1
    ;;
esac
```

- [ ] **Step 2: Make it executable**

```bash
chmod +x scripts/manage-tokens.sh
```

- [ ] **Step 3: Test — add creates a well-formed bcrypt line**

```bash
tmp_users="$(mktemp)"
echo "# test file" > "$tmp_users"
CADDY_USERS_FILE="$tmp_users" scripts/manage-tokens.sh add alice@example.com
grep -E '^alice@example\.com \$2[aby]\$' "$tmp_users" && echo "add: OK"
```

Expected: the script prints a token once, and the grep finds one line for `alice@example.com` whose second field starts with a bcrypt hash prefix (`$2a$`, `$2b$`, or `$2y$`) — printing "add: OK".

- [ ] **Step 4: Test — re-adding the same email replaces, not duplicates, the line (Review Focus item 3)**

```bash
CADDY_USERS_FILE="$tmp_users" scripts/manage-tokens.sh add alice@example.com
count="$(grep -c '^alice@example\.com ' "$tmp_users")"
[ "$count" -eq 1 ] && echo "rotation replaces in place: OK"
```

Expected: prints "rotation replaces in place: OK" — exactly one line for `alice@example.com`, not two.

- [ ] **Step 5: Test — remove deletes the line**

```bash
CADDY_USERS_FILE="$tmp_users" scripts/manage-tokens.sh remove alice@example.com
grep -q '^alice@example\.com ' "$tmp_users" || echo "remove: OK"
rm -f "$tmp_users"
```

Expected: prints "remove: OK" — the line is gone.

- [ ] **Step 6: Commit**

```bash
git add scripts/manage-tokens.sh
git commit -m "feat: add scripts/manage-tokens.sh for gateway credential admin"
```

---

## Task 7: End-to-end gateway auth smoke test

**Files:**
- Create: `scripts/smoketest-gateway-auth.sh`

**Interfaces:**
- Consumes: `scripts/manage-tokens.sh` (Task 6, via `CADDY_USERS_FILE`), the `basic_auth`/`import` mechanism from `config/Caddyfile` (Task 4, reproduced standalone here on throwaway ports so this test needs neither real DNS/TLS nor the rest of the stack running).

- [ ] **Step 1: Create `scripts/smoketest-gateway-auth.sh`**

```bash
#!/usr/bin/env bash
# Proves the exact mechanism config/Caddyfile relies on — basic_auth +
# import of config/caddy-ingest-users.txt's format + zero-downtime
# revocation via `caddy reload` — on throwaway ports, with no real
# DNS/TLS/stack required. Does NOT prove gRPC-through-Caddy works (needs a
# real OTLP client and TLS — see the spec's own flagged gap and Task 8's
# manual verification note).
set -euo pipefail

workdir="$(mktemp -d)"
trap 'docker rm -f gateway-smoketest >/dev/null 2>&1 || true; rm -rf "$workdir"' EXIT

users_file="$workdir/users.txt"
echo "# smoketest" > "$users_file"

cat > "$workdir/Caddyfile" <<'EOF'
:8080 {
	basic_auth * {
		import /etc/caddy/users.txt
	}
	respond "ok"
}
EOF

CADDY_USERS_FILE="$users_file" scripts/manage-tokens.sh add alice@example.com > "$workdir/alice.out"
alice_token="$(tail -n 5 "$workdir/alice.out" | grep -A1 'uma única vez' | tail -n1)"
CADDY_USERS_FILE="$users_file" scripts/manage-tokens.sh add bob@example.com > "$workdir/bob.out"
bob_token="$(tail -n 5 "$workdir/bob.out" | grep -A1 'uma única vez' | tail -n1)"

docker run -d --name gateway-smoketest -p 18080:8080 \
  -v "$workdir/Caddyfile:/etc/caddy/Caddyfile:ro" \
  -v "$users_file:/etc/caddy/users.txt:ro" \
  caddy:2 > /dev/null
sleep 2

code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

got="$(code http://localhost:18080/)"
[ "$got" = "401" ] && echo "no credentials -> 401: OK" || { echo "no credentials -> $got, want 401" >&2; exit 1; }

got="$(code -u "alice@example.com:$alice_token" http://localhost:18080/)"
[ "$got" = "200" ] && echo "alice's credential -> 200: OK" || { echo "alice's credential -> $got, want 200" >&2; exit 1; }

got="$(code -u "bob@example.com:$bob_token" http://localhost:18080/)"
[ "$got" = "200" ] && echo "bob's credential -> 200: OK" || { echo "bob's credential -> $got, want 200" >&2; exit 1; }

CADDY_USERS_FILE="$users_file" scripts/manage-tokens.sh remove alice@example.com > /dev/null
docker exec gateway-smoketest caddy reload --config /etc/caddy/Caddyfile > /dev/null

got="$(code -u "alice@example.com:$alice_token" http://localhost:18080/)"
[ "$got" = "401" ] && echo "alice revoked -> 401: OK" || { echo "alice revoked -> $got, want 401" >&2; exit 1; }

got="$(code -u "bob@example.com:$bob_token" http://localhost:18080/)"
[ "$got" = "200" ] && echo "bob still works after alice's revocation -> 200: OK" || { echo "bob after revocation -> $got, want 200" >&2; exit 1; }

echo "all gateway auth checks passed"
```

- [ ] **Step 2: Make it executable and run it**

```bash
chmod +x scripts/smoketest-gateway-auth.sh
scripts/smoketest-gateway-auth.sh
```

Expected: prints six "OK" lines ending in "all gateway auth checks passed", exit 0. This is the task's own test — a shell script whose job *is* verifying live Caddy behavior has no separate "unit test" layer underneath it; Step 1 writing the script and Step 2 running it stand in for the usual write-test/see-it-fail/implement/see-it-pass cycle, since there's no separate implementation to write afterward.

If the `alice_token`/`bob_token` extraction (`grep -A1 'uma única vez'`) doesn't match `scripts/manage-tokens.sh`'s actual output shape from Task 6, adjust the extraction to match — the token is the line printed immediately after the "mostrado uma única vez" message.

- [ ] **Step 3: Commit**

```bash
git add scripts/smoketest-gateway-auth.sh
git commit -m "test: add end-to-end smoke test for gateway auth and revocation"
```

---

## Task 8: Documentation — production deployment

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: `docker-compose.server.yaml`, `.env.example` (Task 5), `scripts/manage-tokens.sh` (Task 6), the wizard's new prompts (Tasks 2–3).

- [ ] **Step 1: Add a "Production deployment" section to `README.md`**

Insert a new `## Production deployment` section immediately after the existing `## Managing the stack` section (before `## Troubleshooting`):

```markdown
## Production deployment

Running the stack on a real server, reachable by more than one person's
machine, needs the auth gateway described in
`docs/superpowers/specs/2026-09-29-production-auth-gateway-design.md`. This
is additive — plain `docker compose up -d` (no extra flags) keeps working
exactly as it does today, fully local and unauthenticated.

**One-time setup:**

1. Point three DNS A records at the server: one each for the OTel ingest,
   Loki ingest, and Grafana domains (ports 80/443 must be reachable from
   the internet — Caddy's automatic HTTPS needs this for Let's Encrypt).
2. Register an OAuth 2.0 Client ID in Google Cloud Console (Web
   application; authorized redirect URI `https://<your-grafana-domain>/login/google`).
3. `cp .env.example .env` and fill in the domains, Google OAuth client
   ID/secret, allowed email domain, and a Grafana admin password.
4. `cp config/caddy-ingest-users.txt.example config/caddy-ingest-users.txt`

**Bring the stack up:**

```bash
docker compose -f docker-compose.yaml -f docker-compose.server.yaml up -d
```

**Add a person:**

```bash
scripts/manage-tokens.sh add alice@example.com
```

This prints a token once — hand it to them over a secure channel (it is
never shown again). They enter it, along with their email, into the
ingest email/token fields inside the wizard's "OTel endpoint" and "Loki
ingest (collector)" sections when they run setup pointed at your domains
instead of `localhost`. Their Grafana access is separate: anyone signing
in with an `@<your-allowed-domain>` Google account can log in — no token
needed there.

**Revoke a person:**

```bash
scripts/manage-tokens.sh remove alice@example.com
docker compose -f docker-compose.yaml -f docker-compose.server.yaml exec caddy caddy reload --config /etc/caddy/Caddyfile
```

This only removes their ingest credential — Grafana access for a
domain-restricted Google account is revoked by removing them from your
Google Workspace, not from anything here.
```

- [ ] **Step 2: Verify the section renders sensibly**

Run: `grep -n "^## Production deployment" README.md`
Expected: one match, positioned between `## Managing the stack` and `## Troubleshooting` (confirm with `grep -n "^## "  README.md`).

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: add production deployment section for the auth gateway"
```
