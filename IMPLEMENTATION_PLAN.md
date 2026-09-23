# URL Shortener — Implementation Plan

Companion to `architecture.md`. That file says *what* and *why*, this file
says *in what order*, so you're never sitting there wondering what to build
next or accidentally building something before its dependency exists.

Each step lists what you're building, what it depends on, and a "done when"
check, a concrete way to know you're actually finished before moving on,
rather than just feeling done.

One addition not explicit in the architecture doc: **input validation and
error responses**. The doc specs the happy path well but doesn't say what
the API returns when a request is malformed, that's folded into the steps
below where it belongs, since an API that only works when used correctly
isn't really done.

---

## Phase 0 — Project Scaffolding

**0.1 — Initialize the module and folder structure**

```
urlshortener/
├── cmd/
│   └── api/
│       └── main.go
├── internal/
│   ├── handler/
│   ├── service/
│   ├── repository/    (or fold into ent's generated client directly)
│   ├── ratelimit/
│   └── model/
├── ent/                (generated, after step 0.3)
├── go.mod
└── go.sum
```

Run `go mod init`, commit an empty structure before writing any logic. This
matches the layered structure from your Day 8 backend material, nothing new
to figure out here.

**Done when:** `go build ./...` succeeds on an empty `main.go` that just
prints "starting" and exits.

**0.2 — Get Neon Postgres reachable**

Create the Neon project, grab the connection string, confirm you can
connect from your machine before writing a single line of Go.

```bash
psql "postgres://user:pass@ep-xxx.neon.tech/dbname?sslmode=require"
```

**Done when:** you can run `SELECT 1;` from `psql` against the Neon
instance.

**0.3 — Install and initialize ent**

```bash
go get entgo.io/ent/cmd/ent
go run entgo.io/ent/cmd/ent new Link
```

This generates the skeleton `ent/schema/link.go` file. Don't fill in fields
yet, just confirm the generator runs and produces files without error.

**Done when:** `ent/schema/link.go` exists and `go generate ./ent` runs
clean with an empty schema.

---

## Phase 1 — Data Layer

**1.1 — Define the `Link` schema in ent**

Translate §6 of the architecture doc directly into `ent/schema/link.go`:
`short_code` (unique, indexed), `original_url` (indexed), `click_count`
(default 0), `created_at` (default now), `expires_at` (optional).

Run `go generate ./ent` after defining fields. This produces the typed
client you'll use everywhere else, so this step blocks nearly everything
after it.

**Done when:** the generated `ent/link/link.go` and `ent/link_query.go`
exist, and a throwaway `main.go` snippet can successfully insert and read
back one row against your Neon instance.

**1.2 — Add the DB-level uniqueness constraint**

Confirm ent actually created a `UNIQUE` constraint on `short_code` in
Postgres, not just an application-level check. This is item 1 on the
architecture doc's correctness checklist, verify it now rather than at the
end.

```sql
\d links   -- in psql, confirm the unique index is listed
```

**Done when:** attempting to insert two rows with the same `short_code`
directly via `psql` fails with a constraint violation.

---

## Phase 2 — Code Generation (§5)

Build and test this in isolation, as a pure function, before it's wired
into any HTTP handler. It has no dependencies on the web layer, so there's
no reason to build it last or tangled up with routing code.

**2.1 — Base62 encoder**

Write `internal/service/shortcode.go` with a function
`EncodeBase62(id int64) string`. Pure function, no DB, no HTTP.

**Done when:** a unit test confirms `EncodeBase62(0) == "0"`,
`EncodeBase62(61) == "Z"` (or whatever your char set's last symbol is), and
encoding is stable (same input always produces same output).

**2.2 — Wire it to the Postgres sequence**

In your create-link service function: insert a row (letting `id` come from
`BIGSERIAL`), then use the returned `id` to compute `short_code`, then
update that same row with the computed code. This is a two-step write
(insert, then update `short_code`), that's expected and fine, don't try to
compute the code before the ID exists, it can't.

**Done when:** creating ten links in a row via a test produces ten distinct,
correctly-encoded short codes with no collisions.

**2.3 — Idempotent creation check**

Before the insert in 2.2, add the `original_url` lookup from §5. If a row
already exists with that URL, return its existing `short_code` and skip
insert/encode entirely.

**Done when:** shortening the same URL twice returns the same short code
both times, and only one row exists in the DB for it.

---

## Phase 3 — Core HTTP Endpoints

Now that code generation works standalone, wire it behind HTTP. Build
these in this specific order, each one is easiest to test when the one
before it already works.

**3.1 — `POST /shorten`**

- Parse and validate the request body. At minimum: is `url` present, is it
  a syntactically valid URL (`net/url.ParseRequestURI` or similar). Reject
  with `400 Bad Request` and a clear JSON error body if not, don't let an
  invalid URL reach the service layer.
- Call the service function from Phase 2.
- Return `201 Created` with the short code and full short URL.

**Done when:** `curl -X POST -d '{"url":"https://example.com"}' localhost:8080/shorten`
returns a short code, and doing it again returns the same one (idempotency
check from 2.3, now verified through the actual API).

**3.2 — `GET /{code}`**

- Look up `short_code`, indexed lookup, this is the path §3's p99 < 50ms
  target applies to, so keep this handler minimal.
- Not found → `404`.
- Found → `302` redirect to `original_url`. Confirm it's 302, not 301
  (checklist item 2), a browser will cache a 301 and stop hitting your
  server on repeat visits, silently breaking your click counts before
  you've even built them.

**Done when:** visiting the short URL in an actual browser redirects
correctly, and a second visit still hits your server (check your server
logs), confirming the 302 isn't being cached.

**3.3 — `GET /{code}/stats`**

- Same lookup as 3.2, but return JSON (`click_count`, `created_at`)
  instead of redirecting.

**Done when:** stats endpoint returns `0` for a freshly created link (click
counting isn't built yet, this just confirms the field exists and reads
correctly).

**3.4 — `DELETE /{code}`**

- Delete the row. Decide now whether this needs any protection (the
  architecture doc left this open), even a simple shared-secret header is
  fine for a learning project, document whatever you pick.

**Done when:** a deleted code's redirect returns `404`, not a stale
redirect.

---

## Phase 4 — Async Click Counting (§7)

Build this after the redirect endpoint exists and works synchronously
(Phase 3.2), since you need something to hook the async path into.

**4.1 — The channel and message shape**

Define the delta message type and the buffered channel (capacity 100) in
`internal/service/`. Wire the channel so it's created once at startup, not
per-request.

**4.2 — The consumer goroutine**

Start one consumer goroutine in `main.go` at startup, before the HTTP
server starts listening. It should:
- Read from the channel in a loop.
- Batch deltas per `short_code` over a short window (§7: every 200ms or N
  messages).
- Flush batches as a single `UPDATE ... click_count = click_count + $1`
  per code.

**4.3 — Wire the redirect handler to push, not block**

Update 3.2's handler: after writing the 302 response, push
`{short_code, delta: 1}` onto the channel. Use a non-blocking send
(`select` with a `default` case) so a full channel degrades gracefully
(drop or log, don't block the redirect) rather than stalling a request.

**Done when:**
- `go test -race ./...` passes on the consumer (checklist item 3).
- A quick load test (even just a shell loop firing 50 concurrent curl
  requests at one code) results in a `click_count` that matches the
  number of requests, within one flush window.

---

## Phase 5 — Rate Limiting (§8)

**5.1 — Token bucket implementation**

Write `internal/ratelimit/tokenbucket.go`: a struct holding tokens
remaining, max capacity, and refill rate, guarded by a `sync.Mutex`. One
bucket per IP, stored in a map.

**5.2 — Middleware**

Wrap `POST /shorten` (only that route, per §8) with middleware that:
- Extracts client IP from the request.
- Checks/consumes a token.
- Returns `429 Too Many Requests` with a `Retry-After` header if none left.

**Done when:**
- `go test -race ./...` passes on the limiter (checklist item 4).
- Firing requests past the bucket's capacity in a tight loop starts
  returning `429`, and easing off lets it recover.

---

## Phase 6 — Expiry (§9)

**6.1 — Accept `expires_in` on creation**

Extend 3.1's request body to accept an optional expiry duration, compute
and store `expires_at`.

**6.2 — Sweeper goroutine**

Start a second background goroutine at startup (alongside 4.2's consumer),
driven by a `time.Ticker` (1–5 min interval per §9). Each tick: query rows
where `expires_at < now()`, mark them inactive (or delete, your call,
document which).

**6.3 — Redirect handler respects the flag**

Update 3.2: if a link is flagged inactive/expired, return `410 Gone`
instead of a redirect. This check is against the flag the sweeper set, the
handler itself still does not compute or compare timestamps on every
request, that's the whole point of §9's design.

**Done when:** creating a link with a short `expires_in`, waiting past one
sweep interval, and hitting it returns `410`, not a redirect (checklist
item 5).

---

## Phase 7 — Final Correctness Pass

Go back through `architecture.md` §12 as a literal checklist now that
everything's built, tick each box for real, don't assume:

- [ ] DB-level uniqueness on `short_code` (verified in 1.2)
- [ ] 302, not 301 (verified in 3.2)
- [ ] Click counter race-safe (verified in 4)
- [ ] Rate limiter race-safe (verified in 5)
- [ ] Expired links return 410 (verified in 6.3)
- [ ] Idempotent creation actually short-circuits before generating a new
      code (verified in 2.3, re-check it still holds through the full
      HTTP path in 3.1)

Also worth doing here, not before: a short `README.md` documenting how to
run it locally (env vars for the Neon connection string, how to run
`go generate ./ent`, how to start the server). Future-you, six months from
now, opening this repo cold, will want that far more than you expect right
now.

---

## Suggested Order Summary

If you just want the flat sequence with nothing else:

1. Scaffold project + connect to Neon + init ent
2. Define schema, generate, confirm unique constraint in Postgres
3. Build + unit test base62 encoding, standalone
4. Wire encoding to sequence + idempotent lookup, standalone
5. `POST /shorten` endpoint
6. `GET /{code}` endpoint (redirect, confirm 302)
7. `GET /{code}/stats` endpoint
8. `DELETE /{code}` endpoint
9. Click-count channel + consumer goroutine, wire into redirect handler
10. Token bucket + rate-limit middleware on `/shorten`
11. Expiry field + sweeper goroutine + 410 handling
12. Full checklist pass + README

Each numbered item only depends on items before it, nothing later in the
list is required to test or verify anything earlier.