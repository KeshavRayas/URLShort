# Go URL Shortener — production shape

A URL shortener built the way the service should look after the tutorial ends:
**Postgres as the source of truth, Redis as a cache, collision-checked code
allocation, real expiry semantics, per-account auth and rate limiting, and
asynchronous click analytics.**

```
client ──► LB / ingress ──► API (Go, N×) ──► Redis (cache, rate limits)
                                              │ miss
                                              └──► Postgres (links, users, clicks)
```

The architecture drawing lives at [`docs/architecture.html`](docs/architecture.html).

## Why this differs from the tutorial

| Concern | Tutorial | This service |
|---|---|---|
| Code allocation | `sha256(url+user)` truncated, no collision check | random base62, unique PK, retry on conflict |
| Source of truth | Redis only | Postgres; Redis may be flushed at any time |
| Link lifetime | 6 h TTL (a cache policy) | permanent by default, optional `expires_at` |
| Errors | `panic` → 500 | sentinel errors → 404 / 409 / 400 |
| Identity | `user_id` in the body, never stored | API key → user; ownership enforced |
| Abuse | none | per-account fixed-window limiter |
| Clicks | none | buffered async writer, event log + counter |
| Shutdown | process exit | drain HTTP, then drain the click queue |

## Quick start

Requires Go 1.27+ and Docker.

```bash
# 1. dependencies
docker compose up -d postgres redis

# 2. the API (PowerShell shown; bash uses export)
$env:BOOTSTRAP_USERS="local:local-dev-key-change-me"
$env:API_KEY_PEPPER="local-dev-pepper"
go run ./cmd/api
```

Or run everything in containers:

```bash
docker compose --profile app up -d --build
```

Create a link:

```bash
curl -s -X POST http://localhost:8080/api/v1/links \
  -H "X-API-Key: local-dev-key-change-me" \
  -H "Content-Type: application/json" \
  -d '{"target_url":"https://example.com/a/very/long/path"}'
```

```json
{
  "code": "L4iOe41",
  "short_url": "http://localhost:8080/L4iOe41",
  "target_url": "https://example.com/a/very/long/path",
  "created_at": "2026-11-16T07:52:21Z",
  "expires_at": null,
  "expired": false,
  "clicks": 0
}
```

Open `http://localhost:8080/L4iOe41` and you get a `302`.

## API

All management routes require `X-API-Key: <key>` or `Authorization: Bearer <key>`.
The redirect route is public.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/links` | create a link (`target_url`, optional `custom_code`, `expires_at`) |
| `GET` | `/api/v1/links` | list your links (`limit`, `offset`) |
| `GET` | `/api/v1/links/{code}` | one link, owner only |
| `DELETE` | `/api/v1/links/{code}` | soft-delete and evict from cache |
| `GET` | `/{code}` | public `302` redirect |
| `GET` | `/healthz` | liveness, no dependencies |
| `GET` | `/readyz` | readiness, checks Postgres |

Status codes: `400` invalid input, `401` missing/bad key, `404` unknown or
not yours, `409` alias taken, `429` rate limited with `Retry-After`.

#### Custom alias and expiry

```bash
curl -s -X POST http://localhost:8080/api/v1/links \
  -H "X-API-Key: local-dev-key-change-me" \
  -H "Content-Type: application/json" \
  -d '{"target_url":"https://example.com/sale","custom_code":"spring26","expires_at":"2027-01-01T00:00:00Z"}'
```

Aliases are 3–32 alphanumerics; router-owned words (`api`, `healthz`, `readyz`,
`metrics`, `admin`, …) are rejected.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `HTTP_ADDR` | `:8080` | listen address |
| `PUBLIC_BASE_URL` | `http://localhost:8080` | used to build `short_url` and to reject self-links |
| `DATABASE_URL` | local compose DSN | Postgres connection string |
| `REDIS_ADDR` | `localhost:6379` | cache and rate limiter |
| `API_KEY_PEPPER` | dev value | **must** change in production (`APP_ENV=production` refuses the default) |
| `BOOTSTRAP_USERS` | empty | `name:key,name:key`, created at startup if missing |
| `CACHE_TTL` | `24h` | capped by each link's own expiry |
| `NEGATIVE_CACHE_TTL` | `30s` | how long a 404 is remembered |
| `RATE_LIMIT_PER_MINUTE` | `120` | per account |
| `CLICK_BUFFER` | `2048` | events buffered before writes |
| `TRUSTED_PROXY_CIDRS` | empty | peers allowed to set `X-Forwarded-For`; empty ignores it |
| `ALLOW_PRIVATE_TARGETS` | `false` | keep false publicly; redirects to internal hosts are an abuse vector |

See [`.env.example`](.env.example).

## Design notes

**Code allocation.** Seven base62 characters (`62^7 ≈ 3.5 × 10^12`). Codes come
from `crypto/rand` with rejection sampling so the modulo is unbiased. The
insert is the uniqueness check: `links.code` is the primary key, a duplicate
returns `23505`, and the service retries up to five times. Custom aliases skip
the retry and return `409`.

**Cache-aside, not cache-as-primary.** A redirect reads Redis first; a miss
falls through to Postgres and fills the cache. A miss on both is remembered for
30 seconds. Deletes and alias creation evict both keys, so revocation is
immediate.

**Expiry is a property of the link, not the cache.** `expires_at` is stored in
Postgres and filtered in the resolve query; the cache TTL is clamped to the
remaining lifetime so an expiring link cannot outlive itself in Redis.

**Clicks never block redirects.** The handler pushes an event onto a buffered
channel; two workers write `click_events` and bump `links.clicks` in one
transaction. If the buffer fills, telemetry is dropped and the redirect still
happens — the counter is deliberately the less important write.

**Rate limiting fails open.** The limiter is a Lua `INCR`+`EXPIRE` so a counter
can never leak without a TTL. If Redis is unreachable the request is allowed:
a cache outage should degrade performance, not availability.

**Readiness checks Postgres only.** Redis is optional for serving; failing
readiness on it would pull healthy instances out of rotation for a cache blip.

**Any instance serves any request.** There is no session state: credentials are
looked up in Postgres, counters and cache live in Redis, so a load balancer can
round-robin without sticky sessions. It should health-check `/readyz`, stop
sending traffic before the process shuts down, and terminate TLS. Set
`TRUSTED_PROXY_CIDRS` to the proxy ranges so `X-Forwarded-For` is honoured for
logs and visitor hashes — with it empty, forwarded headers are ignored and every
caller behind the proxy looks like the proxy.

## Layout

```
cmd/api/                  process wiring, signals, graceful shutdown
internal/config/          env parsing and validation
internal/link/            domain: model, validation, code allocation, service
internal/httpapi/         router, middleware, handlers, error mapping
internal/storage/postgres schema, links, clicks, bootstrap
internal/storage/rediscache  cache-aside implementation
internal/ratelimit/       Redis fixed-window limiter
internal/analytics/       buffered click recorder
internal/auth/            API key hashing and user lookup
e2e/                      real-binary end-to-end suite
```

## Testing

```powershell
./scripts/e2e.ps1
```

The script resets the Compose stack, runs the suite against a freshly built
binary, and prints the report. It leaves:

- `e2e-report.md` — scenario table with evidence per assertion
- `e2e-report.json` — same data, machine readable

Fourteen scenarios cover the redirect path, cache behaviour (including serving
a deliberately stale row), asynchronous click writes, forwarded client
addresses, negative caching, input validation, alias conflicts, ownership
isolation, expiry, deletion, durability across a process restart, rate limiting
and authentication.

## Known limits

- Single region; no read replicas or key-range sharding yet.
- No custom domains or per-account namespaces.
- URLs are validated but not fetched or blocklist-checked.
- Analytics is fire-and-forget by design; a full pipeline would use a queue.
- Schema is applied idempotently at boot; a versioned migration tool is the
  next step for multi-team changes.
- No metrics/tracing endpoint yet — structured JSON logs and request IDs only.
