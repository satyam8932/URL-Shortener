# URL Shortener — Architecture

## 1. Overview

A URL shortening service designed for a read-heavy workload (100:1 read:write),
targeting 100M reads/month at baseline, with headroom for 5x traffic spikes.
Optimized for low-latency redirects, resilient click tracking, and abuse
prevention on the write path.

---

## 2. Functional Requirements

- Shorten a long URL into a short, unique code.
- Redirect a short code to its original URL (`GET /{code}`).
- Return click statistics for a code without redirecting (`GET /{code}/stats`).
- Support optional expiry on a shortened link.
- Support optional custom aliases.
- Delete a link.
- Prevent abuse of the creation endpoint via rate limiting.
- Return the same short code if the same URL is shortened twice (idempotent
  creation).

**Out of scope for this build:** user accounts/auth, custom domains, link
analytics beyond raw click count (referrer, geo, device breakdown).

---

## 3. Non-Functional Requirements

| Requirement | Target | Why it matters here |
|---|---|---|
| Redirect latency | p99 < 50ms | Redirect is the hot path, hit ~100x more than creation |
| Availability | Survive a single DB restart without data loss on confirmed writes | Click counts and link creation must not silently vanish |
| Consistency (click count) | Eventual, bounded staleness ~seconds | Async counting trades strict consistency for redirect speed |
| Consistency (expiry) | Eventual, bounded staleness ~minutes | A link expiring a few minutes late is an acceptable trade for not hitting the DB on every redirect |
| Abuse resistance | Rate-limited creation per IP | Read path is cheap and safe to leave unthrottled at this scale; write path is the actual risk |
| Predictability | Short codes must not be sequentially guessable in practice | See §5 |

---

## 4. Scale Estimation

**Traffic**

- Assumed load: 100,000,000 reads/month (redirects)
- Read:write ratio: 100:1 → ~1,000,000 writes/month
- Average RPS: 100M / (30 × 86,400) ≈ **38.6 RPS** average read load
- Peak (5x average): **~193 RPS** at peak

**Storage**

- ~1KB per link record
- 1,000,000 new links/month × 1KB ≈ 1GB/month
- Over 12 months: ~12GB/year

Storage volume itself is not a constraint at this scale. The constraint is
**read throughput and lookup latency**, not disk space, which is why the
design below optimizes almost entirely around the read path.

---

## 5. Short Code Generation

**Design: base62-encoded Postgres sequence, generated inline in the main
service. No separate pre-generation service.**

- A Postgres `BIGSERIAL` (or explicit `SEQUENCE`) provides a guaranteed
  unique, monotonically increasing integer per insert.
- That integer is base62-encoded (`[0-9a-zA-Z]`) to produce a short,
  URL-safe code (e.g. `1000000` → `4c92`).
- Because the sequence is DB-owned, uniqueness is guaranteed by construction,
  no collision checks, no retry logic needed.
- Sequential integers *are* technically guessable if exposed directly, but a
  base62-encoded ID is not obviously sequential to a casual observer, and this
  service has no sensitive data behind a code (worst case of guessing a code
  is landing on someone else's public redirect target). If stricter
  unpredictability is required later, the integer can be run through a
  reversible bit-mix (e.g. Feistel cipher or XOR-based permutation) before
  encoding, still O(1), still no DB round-trip for uniqueness.

**Idempotent creation:** before inserting a new link, check for an existing
row with the same `original_url` (indexed column, see §6). If found, return
the existing short code instead of creating a new one.

**Why not a pre-generated code pool (considered and rejected):** a separate
service pre-generating codes in batches was evaluated. Rejected because:
1. It introduces a second service and a second failure domain for a problem
   a single encode operation already solves in nanoseconds.
2. If the pool is in-memory only, a restart re-generates from an unknown
   starting point, causing collisions with codes already issued to real
   users before the crash.
3. Making the pool crash-safe requires persisting its cursor position
   somewhere durable, at which point it needs the same DB round-trip it was
   meant to avoid, with none of the benefit.

**Why not hash-based codes (considered and rejected):** hashing the URL
(MD5/SHA-256) produces fixed-length output that must be truncated to be
"short," and truncation reintroduces collisions, between *different* URLs,
not just repeated ones. This defeats the purpose of hashing (collision
avoidance) rather than achieving it. The idempotent-return behavior we want
is better implemented as an explicit `original_url` lookup, decoupled from
code generation entirely.

---

## 6. Data Model

```
Table: links
─────────────────────────────────────────────
id              BIGSERIAL       PRIMARY KEY
short_code      VARCHAR(10)     UNIQUE, INDEXED
original_url    TEXT            INDEXED
click_count     BIGINT          DEFAULT 0
created_at      TIMESTAMPTZ     DEFAULT now()
expires_at      TIMESTAMPTZ     NULLABLE
```

- `short_code` is a separate indexed column, not the primary key. `id`
  (the sequence) is the primary key and is what code generation is derived
  from; `short_code` is the derived, looked-up-on-every-redirect value.
  Keeping them separate means the encoding scheme can change later without
  touching the primary key structure.
- `original_url` is indexed specifically to support the idempotent-creation
  lookup in §5.

---

## 7. Click Counting

**Design: async, batched, via an in-process buffered channel.**

1. On redirect, the handler responds with the 302 **before** touching the
   counter, redirect latency must not depend on write throughput.
2. The handler pushes the short code onto a buffered channel (capacity:
   10,000; originally 100, raised after load testing showed a single flush
   to a remote Postgres takes long enough for 100 slots to overflow).
3. A background consumer goroutine (or small worker pool) drains the
   channel, batches deltas per `short_code` over a short window (e.g. every
   200ms or every N messages, whichever comes first), and applies them as:
   ```sql
   UPDATE links SET click_count = click_count + $1 WHERE short_code = $2;
   ```
4. This is a counter increment, not an event log, no per-click row is
   written. Per-click event logging (timestamp, referrer, etc. per row)
   is a different, heavier design that would only be justified if
   click-level analytics were an actual requirement, which they are not
   here.

**Failure mode to design for:** if the process crashes with undelivered
deltas still in the channel, those increments are lost. Given click_count is
a non-critical metric (not billing-relevant, not used for access control),
this is an acceptable trade for the latency win. If it later becomes
unacceptable, the fix is a durable queue (e.g. a `pending_clicks` outbox
table written synchronously and swept asynchronously) rather than adding
Redis or an external broker at this scale.

---

## 8. Rate Limiting

**Design: token bucket stored in Redis, updated atomically by a Lua script,
applied to `POST /shorten` only.**

- One bucket per client IP, refilled at a fixed rate, capacity capped.
- Applied only to the creation endpoint. The read path (`GET /{code}`) is
  cheap (single indexed lookup) and is the legitimate core function of the
  service, throttling it would hurt real users for no real protection benefit.
- Redis-backed so every instance behind a load balancer shares one limit per
  client. The script reads Redis's own clock, so instances with skewed clocks
  agree, and sets a TTL so idle buckets are dropped automatically.
- Fails open: if Redis is unreachable the request is allowed and a warning is
  logged. Availability of link creation wins over abuse protection during an
  outage.

---

## 9. Expiry Handling

**Design: eventual consistency via a background sweeper goroutine, not a
check on every read.**

- A ticker-driven goroutine runs periodically (e.g. every 1–5 minutes),
  queries for rows where `expires_at < now()`, and either deletes them or
  flags them inactive.
- The redirect path does **not** check `expires_at` on every request. This
  keeps the hot path to a single indexed lookup with no extra conditional
  logic or clock comparison per request.
- Accepted trade-off: a link may remain redirectable for up to one sweep
  interval past its stated expiry. Given expiry here is a convenience
  feature, not a security boundary, this staleness window is acceptable.

---

## 10. Redis Cache and Scaling Path

**Built:** a read-through Redis cache on the `GET /{code}` lookup. Key:
`link:{short_code}`, value: `original_url`. TTL is `CACHE_TTL` (default 10m),
capped at the link's `expires_at` so the cache never serves a link past its
expiry. Only active links are cached; misses and expired links are not.
Deleting a link evicts its key. A redirect that read the row just before the
delete can re-cache it, so a deleted link may keep working for at most one
TTL. Stats are never cached because the click count must be fresh. If Redis
is unreachable, lookups fall back to Postgres. Concurrent misses for the same
code share one Postgres lookup (singleflight), so a burst of traffic to an
uncached link does not stampede the connection pool.

**Design-only, not built:**

The following are documented as the scaling plan if traffic grows well past
100M/month, and are **not implemented in the current build**, current scale
(100M reads/month, ~193 peak RPS) is comfortably served by a single
Postgres instance with proper indexing.

- **Read replicas** for the `GET /{code}` path once a single primary
  can't absorb read volume, writes stay on the primary.
- **Vertical partitioning / sharding** by `short_code` range or hash, only
  once a single (possibly replicated) instance is the bottleneck, not
  before. Not achievable on a free-tier hosted Postgres instance (e.g.
  Neon's free tier); this is architecture-on-paper for the growth scenario,
  not a current dependency.

---

## 11. Tech Stack

| Layer | Choice | Why |
|---|---|---|
| Language | Go | Learning goal for this project |
| HTTP routing | `net/http` (Go 1.22+ method-aware `ServeMux`) | Zero external dependency, no framework overhead at this request volume |
| Database | PostgreSQL (Neon) | Relational integrity for the uniqueness/idempotency requirements in §5 |
| ORM / query layer | `ent` | Compile-time-checked schema and queries, avoids hand-written SQL mistakes without hiding SQL entirely |
| Caching | Redis read-through cache on redirects (§10) | Serves hot links without a Postgres round trip |
| Rate limiting | Redis token bucket via Lua script (§8) | Limits hold across multiple instances |
| Click counting | In-process buffered channel + background consumer | Async, avoids adding write latency to the redirect hot path |
| Expiry | Background ticker goroutine | Avoids per-request overhead; eventual consistency is an acceptable trade here |

---

## 12. Correctness Checklist

- [x] Uniqueness of `short_code` enforced at the DB level (`UNIQUE`
      constraint), not just application logic, belt and suspenders against
      any future concurrent-insert edge case.
- [x] Redirect endpoint returns 302 (not 301), confirm intentionally,
      301 gets cached by browsers and will suppress repeat hits to the
      server, undermining click tracking.
- [x] Click-count consumer batches correctly under concurrent load, verify
      with `go test -race`.
- [x] Rate limiter is correct under concurrent requests from the same IP,
      verify with `go test -race` and a small concurrent load test.
- [x] Expired links return `410 Gone` from the sweeper's flag, not a
      broken redirect, once swept.
- [x] Idempotent creation lookup on `original_url` is actually hit before
      generating a new code, not after.