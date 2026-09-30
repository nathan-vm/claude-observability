# Production Auth Gateway — Design

Date: 2026-09-29
Status: approved-pending-review

## Context

Today the whole stack (`otel-collector`, `loki`, `grafana`, `dash-generator`)
runs unauthenticated on `127.0.0.1` — safe only because it's single-user,
single-machine. `docker-compose.yaml` documents this explicitly: every port
is bound to loopback "on purpose," because Loki has no native auth at all
(`auth_enabled: false` in `config/loki-config.yaml` controls multi-tenancy,
not authentication), Grafana runs with
`GF_AUTH_ANONYMOUS_ENABLED=true`/`admin`/`admin`, and the OTel Collector's
OTLP receiver accepts anything sent to it.

This is project #2 from `docs/specs/2026-09-23-local-setup-wizard-design.md`'s
"Open items" — moving Grafana/Loki/OTel Collector onto a real server that
multiple people's machines send data to, protected by auth instead of by
being unreachable. That spec explicitly deferred per-user OTLP tokens,
exposing the OTel Collector beyond loopback, and external Grafana
provisioning to this project.

Two ingest paths reach Loki today, and an investigation during brainstorming
confirmed they must stay separate rather than being unified behind a single
OTLP pipeline:

- **Claude Code → OTel Collector → Loki**: many machines, native OTLP,
  write-only.
- **`collector` (host process, one per user machine) → Loki, direct**: not
  just a one-time historical backfill — `src/collector/internal/transcriptscan`
  runs this same code path forever, every `POLL_SECONDS` (default 60s). Every
  pass it also **queries** Loki (`RebuildSkillRecords`, `ResolveEmails`,
  `ProjectOwners`, `SeedSeenFromLoki`) to recover real skill/MCP names OTel
  redacts and to resolve token attribution — reads the OTel Collector's own
  ingest path cannot serve, since OTLP has no query capability. `PushToLoki`
  also depends on Loki's synchronous per-chunk HTTP response to distinguish
  "rejected as too old, retry later" from "rejected, drop" — routing this
  through a fire-and-forget OTLP pipeline would break that.

So the `collector` process is a genuine Loki client (read + write), not just
an emitter, and needs its own authenticated path to Loki that is distinct
from the OTLP ingest path Claude Code uses.

## Goals

- TLS on every publicly reachable endpoint.
- Per-person credentials on both ingest paths (OTel Collector, and the
  `collector` → Loki path), individually revocable without affecting anyone
  else.
- Individual Grafana accounts — no anonymous access, no shared admin
  password.
- A single public entry point, so there is one place TLS certs and
  credential revocation are managed, not one per component.
- Zero behavior change for today's local, single-machine setup — this is a
  new, additive deployment mode, not a replacement of it.

## Non-goals

- Unifying the two ingest paths — investigated and rejected (see Context).
- Scoping the `collector → Loki` path's *read* access per user. It already
  reads across every user's data for attribution (`ProjectOwners`,
  `RebuildSkillRecords` don't filter by owner); per-person credentials here
  buy revocation (contain a single compromised credential), not data
  isolation. Changing that read pattern is a separate, larger project.
- Fixing the wizard's plaintext token storage (`OTEL_EXPORTER_OTLP_HEADERS`
  written into `~/.zshrc`/`~/.bashrc` unencrypted). Pre-existing, not solved
  by adding a server-side gateway, worth its own follow-up.
- mTLS, a Tailscale/WireGuard overlay, hosted Grafana Cloud — evaluated
  during brainstorming; a single gateway with per-person credentials fits
  the actual scale here (a handful of people, one server) better than any of
  the alternatives' added operational cost.
- Automating Google OAuth client setup in Grafana — documented as a manual
  one-time admin step, not scripted.
- HTTP OTLP ingest (port 4318). The wizard only ever configures the gRPC
  exporter; only that path is fronted by the gateway.

## Architecture

```
                 Internet (TLS, 443)
                        │
                 ┌──────▼──────┐
                 │    Caddy    │   only publicly exposed service
                 └──┬───┬───┬──┘
        otel.<dom>  │   │   │  grafana.<dom>
    loki-ingest.<dom>┘   └───┐
                 │           │
       ┌─────────▼───┐   ┌───▼─────┐   ┌─────────┐
       │otel-collector│   │  Loki   │◄──┤ Grafana │
       └──────┬───────┘   └────▲────┘   └─────────┘
              └────────────────┘   (internal, unauthenticated,
               internal write        docker network only)
                                          ▲
                                          │ push + query, per-user cred
                                    ┌─────┴─────┐
                                    │ collector │  (host process,
                                    │ (per user)│   one per machine)
                                    └───────────┘
```

Caddy is the only service with a published port on `0.0.0.0`. Three
subdomains (one each — gRPC's method path collides with path-based routing,
so this design uses subdomains throughout rather than mixing schemes):

- `otel.<domain>` → `otel-collector:4317` (gRPC)
- `loki-ingest.<domain>` → `loki:3100` (push + query, used only by the
  `collector` process)
- `grafana.<domain>` → `grafana:3000`

`loki`, `otel-collector`, and `dash-generator` are never reachable from
outside the docker network. The `otel-collector → loki` write inside that
network stays exactly as unauthenticated as it is today — nothing crosses
a trust boundary there, so nothing needs to change about it.

## Auth mechanism

**All credential checking happens in Caddy, nowhere else** — otel-collector
and Loki stay exactly as auth-blind as they are today; only Caddy gets new
config. This was a deliberate simplification over the alternative (OTel
Collector's own `bearertokenauth` extension for its route, Caddy's
`basic_auth` for the other): one mechanism, one file to edit for
revocation, one format to reason about.

- **Both ingest routes** (`otel.<domain>`, `loki-ingest.<domain>`) get
  Caddy's `basic_auth` directive, checked against a shared credentials file
  (`config/caddy-ingest-users.txt`, `caddy hash-password` bcrypt hashes,
  `import`ed into the Caddyfile). **One credential per person, valid on both
  routes** — a person's Claude Code and their `collector` process both
  authenticate as them; splitting into two credentials per person would
  double the distribution/rotation burden for no real isolation benefit,
  since both routes already trust that person broadly.
- Revocation: delete the person's line from the credentials file, then
  `caddy reload` (via `docker compose exec caddy caddy reload`) — no
  restart, no other credential affected.
- **The wizard's OTel token field changes format**: it already writes
  `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20<token>` today (dead
  code — nothing validates it yet). This becomes
  `Authorization=Basic%20<base64(email:token)>` so the same header check in
  Caddy covers this route too, instead of adding a second, differently-shaped
  auth mechanism inside otel-collector.
- **Grafana** gets Google OAuth (`GF_AUTH_GOOGLE_ENABLED`, restricted via
  `GF_AUTH_GOOGLE_ALLOWED_DOMAINS`) instead of Basic Auth — it's a person at
  a browser, not a machine-to-machine credential, and OAuth means no
  password to generate, store, or rotate. `GF_AUTH_ANONYMOUS_ENABLED` and the
  default admin account are disabled in this mode. **Revocation caveat**:
  `ALLOWED_DOMAINS` gates on the email domain, not the individual — anyone
  with an `@<domain>` Google account can log in. Per-person revocation in
  this mode means removing them from Google Workspace (the org's identity
  provider) rather than anything Grafana-side; if that's too coarse (e.g. a
  contractor with a company email who should lose Grafana specifically, not
  the whole domain's other tools), Grafana's exact per-user allowlisting
  options need checking against current docs during planning — not verified
  here.

**Why Basic Auth over the URL, for the `collector`'s side, needs no code
change**: `PushToLoki` and `lokiclient.QueryRange` build their request URL
via `url.Parse(lokiURL)` and issue it through `net/http`'s `Client.Do`,
which applies HTTP Basic Auth automatically from a URL's userinfo
(`https://user:pass@host/...`) when no `Authorization` header is already
set. So `LOKI_URL=https://<email>:<token>@loki-ingest.<domain>` is sufficient
— confirm this holds for `lokiclient.QueryRange` specifically (not read
during brainstorming) before relying on it in the plan.

## New components

- **`docker-compose.server.yaml`** — an overlay, not a replacement.
  `docker compose -f docker-compose.yaml -f docker-compose.server.yaml up -d`
  adds the `caddy` service (image `caddy:2`, `0.0.0.0:80`/`0.0.0.0:443`
  published, `config/Caddyfile` and `config/caddy-ingest-users.txt` mounted,
  a volume for its TLS cert cache) and overrides `grafana`'s environment for
  OAuth. Plain `docker compose up -d` (today's command, no overlay) is
  completely unaffected — local single-machine users see no change.
- **`config/Caddyfile`** — the three site blocks described above.
- **`config/caddy-ingest-users.txt`** — bcrypt-hashed per-person credentials,
  `import`ed by the Caddyfile.
- **A small token-admin script** (`bin/manage-tokens` or similar) —
  generates a random token, shells out to `caddy hash-password`, appends the
  line to `caddy-ingest-users.txt`, and prints the plaintext token once (it
  is the operator's job to hand that to the person out of band — Slack DM,
  1Password, etc. — this script never stores or re-displays it).
- **Wizard changes** (`src/wizard`): the existing OTel endpoint+token prompt
  switches its header encoding to Basic (see above). A second, parallel
  prompt is added for the `collector`'s Loki endpoint+token, written as the
  `LOKI_URL` environment variable into whichever service installer runs
  (launchd plist / systemd unit / Scheduled Task) — same shape as the
  existing OTel prompt, defaulting to today's `http://localhost:47100` when
  left blank so a purely local install is unaffected.
- **Grafana OAuth setup docs** — a README/docs section covering the one-time
  manual step (registering an OAuth client in Google Cloud Console, setting
  the env vars) — not scripted, since it happens once per organization.

## Testing / validation

- Both ingest routes reject requests with no/wrong credentials (401) and
  accept the right one, including a real gRPC OTLP export through Caddy end
  to end (not just a curl — gRPC/HTTP2 through Caddy's `basic_auth` is the
  one piece of this design not yet proven against this stack).
- `collector`'s `LOKI_URL` with embedded userinfo succeeds on both
  `PushToLoki` and `lokiclient.QueryRange` through the gateway.
- Revoking one person's credential (edit file + `caddy reload`) rejects only
  that person; a second person's credential keeps working without any
  restart.
- Grafana: OAuth login works for an allowed-domain account; anonymous access
  and `admin`/`admin` no longer work.
- Regression: `docker compose up -d` (no overlay) and
  `docker-compose.dev.yaml` both still come up and work exactly as before,
  fully unauthenticated on loopback.

## Open items (not resolved here)

- The wizard's plaintext token storage in shell rc files — a pre-existing,
  separate risk this design doesn't address (see Non-goals).
- Whether `caddy-ingest-users.txt` itself should be provisioned/synced by
  some lighter-weight tooling than manual script + reload, if the number of
  people ever grows enough to make that painful. Out of scope at today's
  scale.
