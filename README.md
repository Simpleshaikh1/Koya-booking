# Clinic booking API

A Go API that lets patients list appointment slots, book one, look it up, cancel it and page through their bookings, plus a web page that uses it. Built to the [Koya assessment spec](https://koyaacademy.com/assessment-api) (levels 1–5).

- **Language:** Go 1.25, standard library `net/http` (no web framework)
- **Database:** Postgres via `pgx` (Neon in production)
- **Hosting:** Render (Docker), web page served by the same binary at `/`

## Run locally

```bash
# In-memory store (no database needed; data is lost on restart)
API_KEY=dev-key go run ./cmd/server

# With Postgres
API_KEY=dev-key DATABASE_URL=postgres://user:pass@localhost:5432/clinic go run ./cmd/server
```

Open http://localhost:8080, paste the API key at the top, then use **Admin → Reset with demo slots**.

## Tests

```bash
go test ./...                                          # in-memory store
TEST_DATABASE_URL=postgres://... go test ./...         # also runs every test against Postgres
```

One test per level, including 50 concurrent bookings for one slot (exactly one 201) and 10 concurrent retries with one `Idempotency-Key` (one booking, same id).

## Layout

```
cmd/server/          main: config, store choice, graceful shutdown
internal/booking/    domain types, errors, the Store interface
internal/store/      Postgres store (+ schema.sql) and in-memory store
internal/api/        routes, auth, validation, idempotency, rate limiter
web/                 the booking page, embedded into the binary
```

## How each level works

| Level | Requirement | How |
|---|---|---|
| 1 | List slots, book, look up | `GET /slots` (earliest first, `?provider=` exact match), `POST /bookings` → 201, `GET /bookings/{id}`. `POST /reset` replaces all slots and deletes all bookings. |
| 2 | API key, validation | Middleware checks `X-API-Key` with a constant-time compare → 401. Name/email are trimmed, email lower-cased and checked; missing fields or a started slot → 400, unknown slot → 404. |
| 3 | No double booking, cancel | A **partial unique index** `ON bookings(slot_id) WHERE status='confirmed'` plus `INSERT … ON CONFLICT DO NOTHING`. The database lets exactly one concurrent insert win; the others get no row back → 409. Cancelled rows fall outside the index, so a cancelled slot can be booked again. Cancel is a conditional `UPDATE … WHERE status='confirmed'`. |
| 4 | Idempotent retries, pagination | `Idempotency-Key` is claimed with an `INSERT … ON CONFLICT DO NOTHING` on its primary key, so only one request owns it. The owner stores its status code and body; duplicates wait for that, then replay it. A different body hash → 422. Pagination is keyset (`seq > cursor`), with an opaque base64 cursor, `limit` 1–50. |
| 5 | Rate limit | Sliding-window log: max 5 attempts per email per 60 s, counted before validation so failed attempts count. The 6th gets 429 with `Retry-After` = seconds until the oldest attempt leaves the window. |

### Design choices and trade-offs

- **Idempotency is checked before the rate limit.** A client retrying the same request gets its original answer, not a 429. 429 and 5xx responses are not stored, so a later retry runs for real.
- **The rate limiter lives in memory.** That is correct for a single instance (Render free tier). With more instances it would move to Redis or Postgres. It is cleared by `/reset`.
- **The web page asks for the API key** and keeps it in that browser's `localStorage`, rather than baking the key into public HTML.
- **Two stores behind one interface.** Postgres in production; an in-memory store with one mutex for tests and local runs. The same test suite runs against both.


## Quick curl

```bash
BASE=http://localhost:8080; KEY=dev-key
curl -s -X POST $BASE/reset -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"slots":[{"id":"s1","provider":"Dr Ade","start":"2026-12-10T09:00:00Z","end":"2026-12-10T09:30:00Z"}]}'
curl -s $BASE/slots -H "X-API-Key: $KEY"
curl -s -X POST $BASE/bookings -H "X-API-Key: $KEY" -H "Idempotency-Key: abc" \
  -H "Content-Type: application/json" -d '{"slot_id":"s1","name":"Ada Obi","email":"ada@example.com"}'
curl -s "$BASE/bookings?email=ada@example.com&limit=10" -H "X-API-Key: $KEY"
```
