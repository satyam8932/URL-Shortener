# URL Shortener

A small web service that turns long URLs into short codes and redirects
visitors from a short code to the original URL. It is written in Go and uses
Postgres (hosted on Neon) to store links and Redis to cache hot links and
rate limit link creation.

This README is a guide to the whole project: what each part does, how the
parts talk to each other, what every setting means, and what to watch out
for. The reasoning behind the design lives in
[ARCHITECTURE.md](ARCHITECTURE.md). The order it was built in lives in
[IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md).

## Contents

1. [What it does](#what-it-does)
2. [Quick start](#quick-start)
3. [The big picture](#the-big-picture)
4. [Project layout](#project-layout)
5. [How a request flows](#how-a-request-flows)
6. [Startup and shutdown](#startup-and-shutdown)
7. [Data model](#data-model)
8. [How each feature works](#how-each-feature-works)
9. [What lives in Redis](#what-lives-in-redis)
10. [Configuration](#configuration)
11. [Built-in limits](#built-in-limits)
12. [API reference](#api-reference)
13. [Testing](#testing)
14. [Performance and load testing](#performance-and-load-testing)
15. [Docker and deployment](#docker-and-deployment)
16. [Known limitations and gotchas](#known-limitations-and-gotchas)
17. [Troubleshooting](#troubleshooting)

## What it does

- `POST /shorten` takes a long URL and returns a short code, such as `4c92`.
- `GET /4c92` sends the visitor to the long URL with a `302` redirect.
- `GET /4c92/stats` shows how many times the link was visited.
- `DELETE /4c92` removes a link. It needs an admin token.
- Links can have a custom name (`alias`) and an expiry time (`expires_in`).
- Shortening the same URL twice returns the same code.
- Each client can only create a limited number of links per minute.

## Quick start

You need Go 1.27 or newer and Docker.

```bash
cp .env.example .env          # then fill in the Neon URLs and an ADMIN_TOKEN
docker compose up -d redis    # start Redis in the background
make migrate                  # create or update the links table
make run                      # start the API on http://localhost:8080
```

Try it:

```bash
curl -X POST localhost:8080/shorten -d '{"url": "https://example.com"}'
curl -i localhost:8080/<short_code>
```

`make run` and `make migrate` load `.env` for you. The programs themselves
only read real environment variables, so `go run ./cmd/api` on its own fails
with "DATABASE_URL_POOLED is required". Always go through `make`.

Run `make help` to see every command.

To run everything in Docker instead, including the API:

```bash
make up    # builds the image, starts Redis, runs the migration, starts the API
```

## The big picture

The service is made of two programs and two data stores.

```
                 ┌──────────────────────────────┐
  HTTP clients ─▶│            api               │
                 │  handlers ─▶ service ─▶ repo │───▶ Postgres (Neon, via its pooler)
                 │     │           │            │
                 │     │           └────────────│───▶ Redis (cache)
                 │     └────────────────────────│───▶ Redis (rate limits)
                 │  background: click counter,  │
                 │  expiry sweeper              │───▶ Postgres
                 └──────────────────────────────┘

                 ┌──────────────────────────────┐
  once per deploy│          migrate             │───▶ Postgres (direct, not pooled)
                 └──────────────────────────────┘
```

- **api** ([cmd/api](cmd/api/main.go)) is the web server. It runs all the time.
- **migrate** ([cmd/migrate](cmd/migrate/main.go)) creates or updates the
  `links` table and then exits. Run it before starting a new version of the
  API.
- **Postgres** is the source of truth. Every link lives there.
- **Redis** is a helper. It makes redirects fast and keeps rate limits shared
  between API instances. If Redis goes down, the service keeps working. It is
  slower and has no rate limit until Redis comes back.

Both programs are built into the same Docker image.

## Project layout

```
cmd/
  api/main.go          starts the web server and wires every part together
  migrate/main.go      applies the database schema and exits
ent/
  schema/link.go       the links table, written in Go (edit this one)
  generate.go          the command that turns the schema into Go code
  *                    everything else is generated; never edit by hand
internal/
  config/              reads and checks environment variables
  database/            opens the Postgres pool and the Redis client
  handler/             HTTP layer: routes, input checks, JSON responses, middleware
  service/             the rules of the app: short codes, caching, clicks, expiry
  repository/          talks to Postgres through ent
  cache/               talks to Redis for cached links
  ratelimit/           token bucket rate limiter stored in Redis (Lua script)
  model/               plain types and errors shared by all layers
  logging/             JSON logger setup
  buildinfo/           version string stamped in at build time
loadtest/              k6 scripts for load testing
Dockerfile             two-stage build that produces a small image
compose.yaml           local Redis + migrate + api
Makefile               every command you need
```

### How the layers depend on each other

Each layer only knows about the one below it:

```
handler  ──▶  service  ──▶  repository  ──▶  ent  ──▶  Postgres
                 │
                 └────────▶  cache  ──▶  Redis
handler  ──▶  ratelimit  ──▶  Redis
```

- **handler** knows HTTP (status codes, JSON, headers) but nothing about SQL.
- **service** holds the rules. It asks for small interfaces
  (`LinkStore`, `LinkCache`, `ClickStore`, `ExpiryStore`) instead of concrete
  types, so tests can pass in simple in-memory fakes.
- **repository** is the only code that knows the data lives in Postgres.
- **model** is shared by everyone and imports nothing, so no layer has to
  import the generated ent types.

[cmd/api/main.go](cmd/api/main.go) builds every piece and hands each one to
the layer above it. No package uses global variables.

## How a request flows

Every request passes through the same wrappers (middleware) before reaching
its handler:

```
request
  │
  ▼  limitDuration   gives the request a deadline (HTTP_WRITE_TIMEOUT, 10s)
  ▼  logRequests     logs method, path, status, size and time when done
  ▼  recoverPanics   turns a crash in a handler into a 500 instead of a dropped connection
  ▼  ServeMux        picks the handler from the method and path
  ▼  handler
```

`POST /shorten` also passes through the rate limiter, and `DELETE /{code}`
also passes through the admin token check.

### Redirect: `GET /{code}` (the hot path)

```
1. Is the code shaped like a real code? (1-10 letters, digits, - or _)
      no  ─▶ 404, without touching any database
2. Ask Redis for link:{code}
      found ─▶ 302 redirect  (about 1 ms)
3. Not in Redis: ask Postgres (only one request per code at a time does this;
   others asking for the same code wait and share the answer)
      not found ─▶ 404
      expired   ─▶ 410
4. Save the URL in Redis for next time
5. 302 redirect
6. Record one click in memory (only for GET, not HEAD)
```

The redirect is always `302` and never `301`. Browsers remember a `301`
forever and stop asking the server, which would stop click counting.

### Create: `POST /shorten`

```
1. Rate limiter: does this client have a token left?     no  ─▶ 429
2. Read the JSON body (max 8 KB)                         bad ─▶ 400 (or 413 if too big)
3. Check url, alias and expires_in                       bad ─▶ 400
4. Plain request (no alias, no expiry)?
      look for an existing permanent link with the same URL
      found ─▶ 200 with the existing code
5. Reserve the next id from the Postgres sequence
6. Code = alias if given, otherwise base62(id)
7. Insert the row
      code already taken by an alias ─▶ try again with a new id (up to 3 times)
      alias already taken            ─▶ 409
8. 201 with the new code
```

### Stats: `GET /{code}/stats`

Reads straight from Postgres every time and is never cached, because the
click count must be current. The count can lag the real number by about
200 ms (see [click counting](#click-counting)).

### Delete: `DELETE /{code}`

Checks the `Authorization: Bearer <ADMIN_TOKEN>` header, deletes the row,
then removes the link from Redis.

## Startup and shutdown

### Startup, in order

1. Read and check all settings. If anything is wrong, print **every** problem
   and exit.
2. Connect to Postgres through the pooled URL and check it answers (waits up
   to 15 s, because a sleeping Neon database takes a few seconds to wake).
3. Create the Redis client and check it answers. If Redis is down, log a
   warning and **carry on**.
4. Build the repository, cache, service, click counter, expiry sweeper and
   rate limiter.
5. Start the background workers: click counter and expiry sweeper.
6. Start the HTTP server.

### Shutdown, in order

When the process gets Ctrl+C or `SIGTERM` (what Docker sends on stop):

1. Stop accepting new connections and let in-flight requests finish (up to
   `HTTP_SHUTDOWN_TIMEOUT`, 15 s).
2. Stop the background workers. The click counter writes any clicks still in
   memory (up to 5 s).
3. Close Redis and Postgres.

The order matters. Workers stop only after the server is done, so clicks from
the last requests are still saved. A second Ctrl+C kills the process at once.

Your platform must give the process at least ~25 s to stop. Docker's default
is 10 s, so [compose.yaml](compose.yaml) sets `stop_grace_period: 30s`.
Set the same on any other platform.

## Data model

One table, `links`, defined in [ent/schema/link.go](ent/schema/link.go):

| Column | Type | Notes |
|---|---|---|
| `id` | bigint, identity | Comes from a Postgres sequence. The short code is built from it. |
| `short_code` | varchar(10), unique | What appears in the URL. Never changes. |
| `original_url` | text | Where the link goes. Never changes. |
| `click_count` | bigint, default 0 | Updated in batches by the click counter. |
| `created_at` | timestamptz | Set when the row is inserted. |
| `expires_at` | timestamptz, nullable | Empty means the link never expires. |
| `expired` | boolean, default false | Set to true by the expiry sweeper. |

Indexes:

- `short_code` unique index: fast redirects, and the database itself refuses
  duplicate codes.
- `original_url` **hash** index: fast "does this URL already have a link?"
  checks. A hash index is used because the normal (B-tree) index cannot store
  entries bigger than about 2.7 KB, and long URLs would fail to insert.
- `expires_at` partial index (only rows with an expiry): keeps the sweeper
  fast.

To change the table: edit `ent/schema/link.go`, run `make generate`, then
`make migrate`. Migrations only add things; they never drop columns or
indexes.

## How each feature works

### Short codes

Each new link reserves the next number from the Postgres sequence
(`nextval`) and writes that number in base62, using the characters `0-9`,
then `a-z`, then `A-Z`. For example `61` becomes `Z`, `62` becomes `10` and
`1000000` becomes `4c92`. Ten characters cover more IDs than this service
will ever need.

The number is reserved *before* the insert, so each link is written with one
insert and a row never exists without a code.

### Custom aliases

`alias` lets a user pick the code, such as `promo`. Rules: 3 to 10 characters,
only letters, digits, `-` and `_`. The names `healthz`, `readyz` and
`shorten` are reserved because those paths are already taken. Codes are
case-sensitive, so `Promo` and `promo` are different links.

An alias can happen to match a code that the sequence will produce later
(for example someone takes `k9`). When the generator reaches that number, the
insert fails, and the service simply reserves the next number and tries
again.

### Returning the same code for the same URL

A plain request (no alias, no expiry) first looks for an existing link to
the same URL that never expires. If one exists, its code is returned with
status `200` instead of `201`. Requests with an alias or an expiry always
create a new link, so nobody gets handed a link that expires when they asked
for a permanent one.

### Caching

Redirects read through Redis:

- Key `link:{code}`, value is the original URL.
- It stays for `CACHE_TTL` (10 minutes by default), but never longer than the
  time left before the link expires. A link that has already expired is
  never cached.
- Deleting a link removes its key.
- If many requests ask for the same uncached code at the same moment, only
  one of them queries Postgres; the rest wait and reuse its answer
  ("singleflight"). Without this, a sudden burst of traffic to a new link
  would queue hundreds of identical queries on the 10 database connections.
- If Redis fails, the service logs a warning and reads from Postgres instead.

### Click counting

Counting must never slow down a redirect, so it happens in the background:

1. After sending the redirect, the handler drops the code into an in-memory
   queue that holds 10,000 clicks. This never waits. If the queue is full, the
   click is thrown away and a warning with the number of lost clicks is
   logged.
2. A background worker takes clicks off the queue and adds them up per code.
3. Every 200 ms, or as soon as 100 different codes are waiting, it writes all
   the totals to Postgres in **one** `UPDATE` statement.
4. If that write fails, those clicks are logged as lost. Click counts are
   best-effort by design.

Clicks still in the queue when the process is killed without a clean
shutdown are lost.

### Rate limiting

Only `POST /shorten` is limited. Redirects and stats are not, because they
are cheap and are the whole point of the service.

It uses a **token bucket** per client:

- Each client has a bucket that holds up to `RATE_LIMIT_BURST` tokens (5).
- Each request takes one token. No token left means `429 Too Many Requests`.
- Tokens come back at `RATE_LIMIT_PER_MINUTE` per minute (10), which is one
  token every 6 seconds.
- The `Retry-After` header on a `429` says how many seconds until the next
  token.

With the defaults, a new client can create 5 links right away, then one every
6 seconds. A client that stops for 30 seconds is back to a full bucket.

Details:

- A "client" is an IPv4 address, or an IPv6 **/64 network**. Home internet
  users usually get a whole IPv6 /64, so limiting single IPv6 addresses would
  let one person use billions of addresses.
- Behind a proxy or load balancer, every request seems to come from the
  proxy. Set `CLIENT_IP_HEADER` to the header your proxy fills with the real
  client IP (for example `Fly-Client-IP`). Only do this if the proxy always
  overwrites that header, otherwise clients can fake it. Never use
  `X-Forwarded-For`: it can hold a list that clients partly control.
- The bucket lives in Redis and is updated by a small Lua script
  ([ratelimit/tokenbucket.lua](internal/ratelimit/tokenbucket.lua)). Redis
  runs the whole script as one step, so two requests can never both take the
  last token. The script uses Redis's clock, so servers with slightly wrong
  clocks still agree.
- If Redis is down, requests are **allowed** (the limiter "fails open") and a
  warning is logged. Keeping link creation working was judged more important
  than blocking abuse during an outage. This is a one-line change in
  [handler/ratelimit.go](internal/handler/ratelimit.go) if you want the
  opposite.

### Expiry

`expires_in` is a number of seconds (1 to 31,536,000, which is one year).
The API stores `expires_at = now + expires_in`.

A background worker (the sweeper) runs once at startup and then every
`EXPIRY_SWEEP_INTERVAL` (1 minute). It sets `expired = true` on every link
whose `expires_at` has passed. Redirects only check that flag; they never
compare times. So a link can keep working for up to one sweep interval after
it expires. After that, it returns `410 Gone`. Expired rows are kept, not
deleted, which is what makes `410` possible.

### Admin token

`DELETE /{code}` needs `Authorization: Bearer <ADMIN_TOKEN>`. The token must
be at least 32 characters. It is compared in constant time, so response
timing does not reveal how much of a guess was right. Generate one with
`openssl rand -hex 32`.

### Health checks

- `GET /healthz` returns `200` whenever the process is running. Use it to
  decide whether to restart the container.
- `GET /readyz` returns `200` only if Postgres answers within 2 s, otherwise
  `503`. Use it to decide whether to send traffic. It does not check Redis,
  because the service works without Redis.

## What lives in Redis

| Key | Type | Value | Expires after |
|---|---|---|---|
| `link:{code}` | string | original URL | `CACHE_TTL`, or sooner if the link expires sooner |
| `ratelimit:{client}` | hash | `tokens`, `updated` (Unix seconds) | time to refill a full bucket (30 s by default) |

Every key has an expiry, so Redis never fills up with forgotten keys. On a
managed Redis with a memory limit, set the eviction policy to
`volatile-lru`. With `noeviction` (often the default), a full Redis rejects
writes, the limiter fails open, and rate limiting silently stops working.

Look inside a running local Redis with:

```bash
docker compose exec redis redis-cli --scan
docker compose exec redis redis-cli HGETALL "ratelimit:::1"
```

## Configuration

All settings come from environment variables. Only the first four have no
default. Durations use Go's format: `100ms`, `5s`, `10m`, `1h`.

| Variable | Default | Used by | Meaning |
|---|---|---|---|
| `DATABASE_URL` | required | migrate | Direct Neon URL. Used only for schema changes. |
| `DATABASE_URL_POOLED` | required | api | Pooled Neon URL (host contains `-pooler`). |
| `REDIS_URL` | required | api | Redis URL. `redis://` or `rediss://` (TLS). |
| `ADMIN_TOKEN` | required | api | Token for `DELETE`. At least 32 characters. |
| `PUBLIC_BASE_URL` | `http://localhost:8080` | api | Put in front of codes to build `short_url`. **Set this in production.** |
| `CACHE_TTL` | `10m` | api | How long a redirect stays in Redis. Also the longest a deleted link can keep working (see gotchas). |
| `REDIS_TIMEOUT` | `100ms` | api | Longest wait for any Redis call (connect, read, write) before falling back. Raise it for a Redis far away. |
| `RATE_LIMIT_PER_MINUTE` | `10` | api | Tokens added per minute per client. |
| `RATE_LIMIT_BURST` | `5` | api | Most tokens a client can hold. |
| `CLIENT_IP_HEADER` | empty | api | Header with the real client IP, set by your proxy. Empty means use the connection address. |
| `EXPIRY_SWEEP_INTERVAL` | `1m` | api | How often expired links are flagged. |
| `HTTP_ADDR` | `:8080` | api | Address to listen on. |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | api | Longest time a client may take to send headers. |
| `HTTP_READ_TIMEOUT` | `10s` | api | Longest time a client may take to send the whole request. |
| `HTTP_WRITE_TIMEOUT` | `10s` | api | Longest time to produce a response. Also each request's deadline for database and Redis work. |
| `HTTP_IDLE_TIMEOUT` | `60s` | api | How long an unused keep-alive connection stays open. |
| `HTTP_SHUTDOWN_TIMEOUT` | `15s` | api | How long to wait for in-flight requests on shutdown. |
| `DB_MAX_OPEN_CONNS` | `10` | both | Most connections to Postgres at once. |
| `DB_MAX_IDLE_CONNS` | `10` | both | Connections kept open while idle. Must not exceed `DB_MAX_OPEN_CONNS`. |
| `DB_CONN_MAX_LIFETIME` | `30m` | both | Replace a connection after this long. |
| `DB_CONN_MAX_IDLE_TIME` | `5m` | both | Close a connection unused for this long. |
| `LOG_LEVEL` | `info` | both | `debug`, `info`, `warn` or `error`. |

### About the two database URLs

Neon gives two URLs. The pooled one goes through PgBouncer, which shares a
few real Postgres connections between many clients, and is what the API
uses. With the default settings the API keeps 10 connections open to the
pooler, but the pooler usually needs only 1 to 3 real Postgres connections
to serve them, because each query holds a real connection for about a
millisecond. That is why Neon's graphs can show a single "server" connection
while the API holds 10 "client" connections.

Schema changes do not work reliably through PgBouncer, so `migrate` uses the
direct URL.

The pooler also cannot keep prepared statements between queries, so the API
tells the driver (pgx) not to create them. Query parameters are still sent
separately from the SQL, so this is still safe from SQL injection.

## Built-in limits

These are constants in the code, not settings:

| Limit | Value | Where |
|---|---|---|
| Longest URL | 2,048 characters | [handler/validate.go](internal/handler/validate.go) |
| Longest request body | 8 KB | [handler/links.go](internal/handler/links.go) |
| Alias length | 3 to 10 characters | [handler/validate.go](internal/handler/validate.go) |
| Longest expiry | 365 days | [handler/validate.go](internal/handler/validate.go) |
| Click queue size | 10,000 clicks | [service/clicks.go](internal/service/clicks.go) |
| Click flush | every 200 ms or 100 codes | [service/clicks.go](internal/service/clicks.go) |
| Click flush time limit | 5 s | [service/clicks.go](internal/service/clicks.go) |
| Shared cache-miss lookup time limit | 5 s | [service/links.go](internal/service/links.go) |
| Retries when a code is taken by an alias | 3 | [service/links.go](internal/service/links.go) |
| Readiness check time limit | 2 s | [handler/health.go](internal/handler/health.go) |
| Postgres startup check time limit | 15 s | [database/database.go](internal/database/database.go) |

## API reference

All responses are JSON. Every error has the same shape:

```json
{"error": "a message for humans"}
```

### `POST /shorten`

```bash
curl -X POST localhost:8080/shorten \
  -d '{"url": "https://example.com/some/long/path", "alias": "promo", "expires_in": 3600}'
```

| Field | Required | Meaning |
|---|---|---|
| `url` | yes | Absolute `http` or `https` URL, at most 2,048 characters. |
| `alias` | no | Your own code. 3-10 letters, digits, `-` or `_`. |
| `expires_in` | no | Seconds until the link stops working, 1 to 31,536,000. |

Unknown fields are rejected.

```json
{
  "short_code": "promo",
  "short_url": "http://localhost:8080/promo",
  "original_url": "https://example.com/some/long/path",
  "expires_at": "2026-09-24T12:00:00Z"
}
```

| Status | When |
|---|---|
| `201` | New link created |
| `200` | Existing link returned (same URL, no alias, no expiry) |
| `400` | Invalid JSON or invalid field |
| `409` | Alias already taken |
| `413` | Body bigger than 8 KB |
| `429` | Rate limited; see `Retry-After` |

### `GET /{code}`

| Status | When |
|---|---|
| `302` | Redirect; the target is in the `Location` header |
| `404` | No such link |
| `410` | Link has expired |

### `GET /{code}/stats`

```json
{
  "short_code": "promo",
  "original_url": "https://example.com/some/long/path",
  "click_count": 42,
  "created_at": "2026-09-23T12:00:00Z",
  "expires_at": "2026-09-24T12:00:00Z",
  "expired": false
}
```

Returns `404` for unknown codes. Expired links still return their stats.

### `DELETE /{code}`

```bash
curl -X DELETE localhost:8080/promo -H "Authorization: Bearer $ADMIN_TOKEN"
```

| Status | When |
|---|---|
| `204` | Deleted |
| `401` | Missing or wrong token |
| `404` | No such link |

### `GET /healthz`, `GET /readyz`

See [Health checks](#health-checks).

### Other status codes

`503 {"error": "request timed out"}` means a request hit its deadline
(`HTTP_WRITE_TIMEOUT`), usually because Postgres is slow or waking up.

## Testing

```bash
make test       # every test, with Go's race detector turned on
make lint       # formatting check and go vet
```

Tests run without Postgres or Redis. Service tests use in-memory fakes of the
store and cache. Cache and rate limiter tests use
[miniredis](https://github.com/alicebob/miniredis), an in-memory Redis that
runs inside the test and can move its clock forward.

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) also fails if the
generated ent code is out of date with `ent/schema`.

These unit tests check each part on its own. To check the whole running
service against real Postgres and Redis, including expiry, the rate limiter
and every error status, run the end-to-end script (73 checks, about 11 s);
see [Running the tests yourself](#running-the-tests-yourself):

```bash
make loadtest SCRIPT=e2e
```

## Performance and load testing

All numbers below were measured, not estimated. Load tests use
[k6](https://k6.io) and run in Docker, so there is nothing to install.
Scripts live in [loadtest/](loadtest/).

### Terms

- **Throughput**: requests handled per second.
- **p50** (median): half of all requests were faster than this.
- **p90 / p95 / p99**: 90% / 95% / 99% of requests were faster than this.
  These show the slow tail that some real users will hit.
- **Dropped**: requests k6 was supposed to send but could not start in time,
  because earlier ones had not finished. A sign the system is saturated.
- **Cold / warm cache**: whether the links were already in Redis when the
  test started.

### Test setup

| Part | Where |
|---|---|
| Machine | Apple M4 laptop, 10 cores; Docker VM with 10 CPUs and 8 GB |
| API | one instance, running directly on the laptop |
| Redis | `redis:8-alpine` in Docker on the same laptop |
| Postgres | Neon, through its pooler, **~95-105 ms away** per round trip |
| Load generator | k6 in Docker on the same laptop |
| API settings | defaults: 10 database connections, 10 minute cache TTL |

k6, the API and Redis all shared the same 10 cores, so the highest redirect
numbers are a lower bound: some of the CPU went to generating the load.

### Summary

| Path | Sustained throughput (one instance) | p50 | p95 | p99 | What limits it |
|---|---|---|---|---|---|
| Redirect, link in Redis | **~10,000 / s** | 0.50 ms | 1.43 ms | 2.96 ms | CPU (shared with k6 in this test) |
| Redirect, link not in Redis yet | first request per link: ~100 ms | | | | Postgres round trip |
| Redirect to unknown code (404) | ~100 / s | 106 ms | 114 ms | 215 ms | database connections × round trip |
| Redirect to expired link (410) | ~100 / s | 105 ms | 115 ms | 222 ms | database connections × round trip |
| Stats | ~100 / s | 95 ms | 112 ms | 181 ms | database connections × round trip |
| Create a link | **~30 / s** | 315 ms | 334 ms | 401 ms | 3 database round trips per create |
| Rejected request (400) | CPU-bound, thousands / s | 0.53 ms | 0.69 ms | 1.1 ms | nothing (no database) |

The 404, 410 and stats rows share the same ~100 database queries per second
(see [the capacity math](#capacity-math)).

### Redirects from a warm cache

Twenty links, all already in Redis, requested at a fixed rate for 30 s:

| Target rate | Achieved | p50 | p90 | p95 | p99 | max | Dropped | API CPU |
|---|---|---|---|---|---|---|---|---|
| 1,000 / s | 1,000 / s | 0.36 ms | 0.44 ms | 0.47 ms | 0.67 ms | 22 ms | 0 | 16% |
| 5,000 / s | 4,995 / s | 0.26 ms | 0.33 ms | 0.46 ms | 1.23 ms | 27 ms | 0.09% | 37% |
| 10,000 / s | 9,974 / s | 0.50 ms | 1.10 ms | 1.43 ms | 2.96 ms | 35 ms | 0.26% | 81% |
| 20,000 / s | 19,212 / s | 2.4 ms | 10.1 ms | 15.6 ms | 180 ms | 1.05 s | 3.9% | 129% |

API CPU is a share of one core (100% = one full core).

At 10,000 per second everything is still well inside the 50 ms p99 target. At
20,000 per second the laptop is saturated: the tail jumps to 180 ms, 4% of
requests could not be sent, and Docker Desktop's engine restarted at the end
of the run (Redis disappeared; the API kept serving from Postgres as
designed).

### Redirects from a cold cache

| Test | p50 | p95 | p99 | max |
|---|---|---|---|---|
| 500 / s, 50 new links, **before** singleflight | 0.56 ms | 0.84 ms | 2,240 ms | 4.4 s |
| 2,000 / s, 50 new links, after singleflight | 0.38 ms | 0.53 ms | 8.1 ms | 661 ms |
| 500 / s, first run after the API starts | 0.64 ms | 0.96 ms | 453 ms | 1.19 s |

Before singleflight, the first second of traffic sent every request for an
uncached link to Postgres, and they queued behind 10 connections. Now one
request per link goes to Postgres and the rest wait for its answer.

Right after the API starts, it has no open connections to Neon yet. Opening
each one takes several round trips (TCP, TLS, login), so the first burst is
slower until all 10 connections exist.

### Database-bound paths

Stats requests always read Postgres (30 s each):

| Target rate | Achieved | Dropped | Errors |
|---|---|---|---|
| 50 / s | 50 / s | 0 | 0% |
| 100 / s | 99 / s | 0.9% | 0% |
| 150 / s | 114 / s | 24% | 1.3% |

Link creation, each request a new URL (30 s each):

| Target rate | Achieved | p50 | p90 | p95 | p99 | max | Errors |
|---|---|---|---|---|---|---|---|
| 20 / s | 20 / s | 316 ms | 332 ms | 336 ms | 417 ms | 440 ms | 0% |
| 30 / s | 30 / s | 315 ms | 330 ms | 334 ms | 401 ms | 476 ms | 0% |
| 40 / s | 35 / s | 2.23 s | 5.2 s | 6.41 s | 8.76 s | 10 s | 0.47% |

At 40 per second, requests queue for a database connection until some hit
the 10 s request deadline and get `503`.

### Everything at once

[loadtest/mixed.js](loadtest/mixed.js) runs all kinds of traffic together
for 1 minute. 63,599 requests, **every** response had the expected status:

| Traffic | Rate | Expected | p50 | p90 | p95 | p99 | max |
|---|---|---|---|---|---|---|---|
| Hot redirects | 1,000 / s | 302 | 0.40 ms | 0.47 ms | 0.51 ms | 0.65 ms | 19 ms |
| Expired links | 20 / s | 410 | 105 ms | 113 ms | 115 ms | 222 ms | 726 ms |
| Unknown codes | 20 / s | 404 | 106 ms | 113 ms | 114 ms | 215 ms | 741 ms |
| Stats | 10 / s | 200 | 95 ms | 108 ms | 112 ms | 181 ms | 685 ms |
| Creates (half with expiry) | 5 / s | 201 | 290 ms | 323 ms | 325 ms | 576 ms | 1.13 s |
| Invalid creates | 5 / s | 400 | 0.53 ms | 0.65 ms | 0.69 ms | 1.1 ms | 10 ms |

Hot redirects stayed under 1 ms at p99 while the database-bound traffic was
running next to them, because they never wait for a database connection.

### Capacity math

These rules of thumb explain every number above and let you predict other
setups.

**1. Each database query costs one network round trip.** From this laptop
to Neon that is ~100 ms. Postgres itself answers in about 1 ms; almost all
the time is travel.

**2. One connection does about 10 queries per second** (1 s / 100 ms). The
API has 10 connections (`DB_MAX_OPEN_CONNS`), so one instance can do about
**100 database queries per second**. The stats test saturated between 100 and
150 per second, which matches.

**3. Each request needs this many queries:**

| Request | Queries |
|---|---|
| Redirect, link in Redis | 0 |
| Redirect, first request for a link (then cached for 10 min) | 1 |
| Redirect to an unknown code (404) or expired link (410) | 1, every time (not cached) |
| Stats | 1 |
| Create, plain URL | 3 (existing-link lookup, reserve id, insert) |
| Create with alias or expiry | 2 (no lookup) |
| Delete | 1 |
| Click counter | 1 every 200 ms = 5 per second, however many clicks |
| Expiry sweeper | 1 per `EXPIRY_SWEEP_INTERVAL` |

**4. So the database budget per instance is:**

```
uncached redirects + 404s + 410s + stats + 3 × creates + deletes + 5  ≤  ~100 per second
```

For example, 30 creates per second use 90 queries, which is why creation
tops out at ~30 per second. One create waits for 3 round trips in a row, so
its latency is about 3 × 100 ms = 300 ms, which matches the measured p50 of
315 ms.

**5. Redirects from Redis do not touch the budget.** They cost about 0.1 ms
of API CPU and one Redis command each. One instance handled 10,000 per second
using ~0.8 of a core, so each core can serve roughly 10,000-12,000 cached
redirects per second. Redis itself can handle roughly 100,000 simple
commands per second.

**6. Compared with the targets in [ARCHITECTURE.md](ARCHITECTURE.md):**

| | Target | Measured capacity | Headroom |
|---|---|---|---|
| Redirects | 38.6 / s average, 193 / s peak (100M per month) | ~10,000 / s | ~50× over peak |
| Creates | 0.39 / s average, ~2 / s peak (1M per month) | ~30 / s | ~15× over peak |
| Redirect p99 | < 50 ms | 0.65-2.96 ms from Redis | met |

At 10,000 redirects per second, one instance could serve about 860 million
redirects per day.

**7. How to get more:**

| Change | Effect |
|---|---|
| Run the API in the same region as Neon | Round trip drops from ~100 ms to ~1 ms: creates take ~5 ms and the database budget grows to thousands of queries per second per instance. **Biggest single win.** |
| Raise `DB_MAX_OPEN_CONNS` | Database budget grows in step (20 connections ≈ 200 queries/s from here). Neon's pooler accepts thousands of client connections. |
| Add API instances | Cached redirects and the database budget both grow per instance; rate limits stay correct because they live in Redis. |
| Cache "not found" | Would take 404 traffic off the database (see gotchas). |

### Running the tests yourself

Start Redis, then start the API with settings that let the scripts work:

```bash
docker compose up -d redis

# For e2e.js and mixed.js: act as many clients, and expire links quickly
CLIENT_IP_HEADER=X-Client-IP EXPIRY_SWEEP_INTERVAL=1s LOG_LEVEL=warn make run

# For redirect.js, stats.js and shorten.js: raise the rate limit instead
RATE_LIMIT_PER_MINUTE=10000000 RATE_LIMIT_BURST=1000000 LOG_LEVEL=warn make run
```

`make run` loads `.env` after these values, so any variable that `.env`
also sets wins over the command line. Edit `.env` if that happens.

Never start the API with `CLIENT_IP_HEADER=X-Client-IP` in production
unless a proxy overwrites that header, or anyone can dodge the rate limit.

Then, in another terminal:

```bash
make loadtest SCRIPT=e2e       # check every endpoint and status code once (~11 s)
make loadtest SCRIPT=mixed     # all traffic kinds together for 1 minute

make loadtest SCRIPT=redirect K6_ENV="-e RATE=5000 -e DURATION=30s -e WARM=1"
make loadtest SCRIPT=stats    K6_ENV="-e RATE=50 -e DURATION=30s"
make loadtest SCRIPT=shorten  K6_ENV="-e RATE=20 -e DURATION=30s"

make loadtest-clean            # delete every link the tests created
```

| Script | Settings (`-e NAME=value`) |
|---|---|
| `e2e.js` | `BURST` (must match `RATE_LIMIT_BURST`, default 5), `SWEEP_WAIT` |
| `redirect.js` | `RATE`, `DURATION`, `LINKS`, `WARM=1` |
| `stats.js` | `RATE`, `DURATION`, `LINKS` |
| `shorten.js` | `RATE`, `DURATION`, `CLEANUP=1` (delete each link right away) |
| `mixed.js` | `DURATION`, `HOT_RATE`, `EXPIRED_RATE`, `UNKNOWN_RATE`, `STATS_RATE`, `CREATE_RATE`, `INVALID_RATE` |

Reading the k6 summary:

```
{ scenario:redirects }: min=... med=... p(90)=... p(95)=... p(99)=... max=...
checks_failed ...          should be 0.00%
dropped_iterations ...     requests k6 could not send in time
```

Each script fails the run if its limits are crossed. For example,
`redirect.js` fails when p99 goes over 50 ms. While a test runs, watch the
API log: `click buffer full` means clicks are being lost.

For numbers that include real network time between users and the API, run
k6 from another machine against a deployed instance:

```bash
docker run --rm -i -v "$PWD/loadtest:/scripts:ro" -e BASE_URL=https://your.domain \
  grafana/k6 run /scripts/redirect.js
```

## Docker and deployment

The [Dockerfile](Dockerfile) has two stages:

1. **build**: a full Go image compiles both programs. It runs on the
   builder's own CPU type and cross-compiles for the target, so
   `linux/amd64` and `linux/arm64` images both build quickly. Go module
   downloads and build results are cached between builds.
2. **runtime**: a "distroless" base image with only CA certificates (needed
   for TLS to Neon and Redis), time zone data and a non-root user. It has no
   shell, so there is very little an attacker could use. Only the two
   binaries are copied in. The image is about 60 MB.

[.dockerignore](.dockerignore) lists only the files the build needs, so
`.env` can never end up inside an image.

### CI and GitHub Container Registry

[.github/workflows/ci.yml](.github/workflows/ci.yml) runs on every push and
pull request:

1. Checks the generated ent code is up to date, runs lint and tests.
2. Builds the image for amd64 and arm64.
3. On `main` and on `v*.*.*` tags, pushes to `ghcr.io/<owner>/<repo>` with
   tags for the commit, the version and `latest`.

### Deploying a new version

Always migrate first, then start the API:

```bash
docker run --rm --env-file prod.env --entrypoint /usr/local/bin/migrate ghcr.io/<owner>/<repo>:<tag>
docker run -d -p 8080:8080 --stop-timeout 30 --env-file prod.env ghcr.io/<owner>/<repo>:<tag>
```

Checklist:

- `PUBLIC_BASE_URL` set to the real domain.
- `CLIENT_IP_HEADER` set if the API is behind a proxy (see
  [Rate limiting](#rate-limiting)).
- `REDIS_TIMEOUT` raised if Redis is not in the same region.
- Redis eviction policy set to `volatile-lru`.
- At least 30 s allowed for shutdown.
- `docker run --env-file` keeps quotes as part of the value, so `prod.env`
  must not quote values. Compose's `env_file` removes quotes, which is why
  `.env` can have them.

## Known limitations and gotchas

These are known and accepted for now. Each one says what would fix it.

- **Short codes are sequential.** `1`, `2`, `3`... anyone can walk through
  every code and see every link. Do not shorten private URLs. Fix: scramble
  the id with a reversible permutation (ARCHITECTURE.md §5) before encoding.
- **A deleted link can keep working for up to `CACHE_TTL`.** If a redirect
  reads the link from Postgres just before it is deleted, it can put the link
  back into Redis right after the delete removed it. Fix: shorter `CACHE_TTL`
  or delete the key a second time after a short delay.
- **Two identical requests at the same instant can create two links** for the
  same URL, because both check "does it exist?" before either inserts. The
  hash index cannot enforce uniqueness. Harmless apart from the duplicate.
- **Unknown codes always reach Postgres.** Only found links are cached, so
  someone requesting random valid-looking codes causes one database query
  each. Fix: cache "not found" briefly, or rate limit redirects per client.
- **Clicks can be lost** if the process is killed without a clean shutdown,
  if the queue of 10,000 fills, or if a batch write fails. All are logged.
- **Redis outages produce a warning per request.** At high traffic that is a
  lot of log lines.
- **Shortening a URL of this service itself** is allowed and creates a
  redirect loop.
- **Unknown paths like `/a/b` return a plain-text 404**, and wrong methods a
  plain-text 405, from Go's router, not the usual JSON error.
- **`/readyz` is public** and pings Postgres on every call.
- **A sleeping Neon database can fail `/readyz`**, because waking up can take
  longer than its 2 s limit. A load balancer may briefly mark the instance as
  not ready.
- **Codes are case-sensitive.** `abc` and `ABC` are different links.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `DATABASE_URL_POOLED is required` on start | Ran `go run` directly. Use `make run`, which loads `.env`. |
| `redis unreachable, running without cache and rate limiting` | Redis not started. Run `docker compose up -d redis`. |
| Every `POST /shorten` returns `429` | Rate limit used up. Wait `Retry-After` seconds, or raise the limits for testing. |
| `short_url` shows `localhost` in production | `PUBLIC_BASE_URL` not set. |
| All clients share one rate limit in production | API is behind a proxy and `CLIENT_IP_HEADER` is not set. |
| `click buffer full, clicks dropped` in logs | Postgres writes are too slow for the traffic. Check database latency and pool size. |
| First requests after idle take seconds | Neon's database was asleep and is waking up. |
| `503 request timed out` | Postgres did not answer within `HTTP_WRITE_TIMEOUT`. |
| CI fails on "generated ent code" | You edited `ent/schema` without running `make generate`. |
| Neon graph shows 1 pooled connection | Normal. That is the server side of PgBouncer; the API holds 10 client connections. |
| Creates slow down past ~30 per second | Database budget used up; see [capacity math](#capacity-math). |
