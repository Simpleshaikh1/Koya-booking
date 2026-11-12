-- Applied on every startup; every statement is idempotent.

CREATE TABLE IF NOT EXISTS slots (
    id        TEXT PRIMARY KEY,
    provider  TEXT        NOT NULL,
    start_at  TIMESTAMPTZ NOT NULL,
    end_at    TIMESTAMPTZ NOT NULL,
    start_raw TEXT        NOT NULL, -- exactly what the client sent, echoed back
    end_raw   TEXT        NOT NULL
);

CREATE INDEX IF NOT EXISTS slots_start_idx ON slots (start_at, id);

CREATE TABLE IF NOT EXISTS bookings (
    seq        BIGSERIAL PRIMARY KEY, -- insertion order: "oldest first" + pagination cursor
    id         TEXT UNIQUE NOT NULL,
    slot_id    TEXT        NOT NULL REFERENCES slots (id) ON DELETE CASCADE,
    name       TEXT        NOT NULL,
    email      TEXT        NOT NULL, -- stored lower-cased
    status     TEXT        NOT NULL CHECK (status IN ('confirmed', 'cancelled')),
    created_at TIMESTAMPTZ NOT NULL
);

-- Level 3: the database itself guarantees at most ONE confirmed booking per
-- slot. Cancelled rows are outside the index, so a cancelled slot can be
-- booked again. Concurrent inserts for the same slot are serialised by this
-- index: exactly one wins, the rest hit ON CONFLICT.
CREATE UNIQUE INDEX IF NOT EXISTS bookings_one_confirmed_per_slot
    ON bookings (slot_id) WHERE status = 'confirmed';

-- Level 4: GET /bookings?email=... pages through (email, seq).
CREATE INDEX IF NOT EXISTS bookings_email_seq_idx ON bookings (email, seq);

-- Level 4: Idempotency-Key -> the response we sent the first time.
-- status_code IS NULL means the first request is still in flight.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    key          TEXT PRIMARY KEY,
    request_hash TEXT        NOT NULL,
    status_code  INT,
    response     BYTEA,
    claimed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
