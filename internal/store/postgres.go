package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"koya-booking/internal/booking"
)

//go:embed schema.sql
var schemaSQL string

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) Reset(ctx context.Context, slots []booking.Slot) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `TRUNCATE idempotency_keys, bookings, slots RESTART IDENTITY`); err != nil {
			return err
		}
		rows := make([][]any, len(slots))
		for i, s := range slots {
			rows[i] = []any{s.ID, s.Provider, s.Start, s.End, s.StartRaw, s.EndRaw}
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"slots"},
			[]string{"id", "provider", "start_at", "end_at", "start_raw", "end_raw"},
			pgx.CopyFromRows(rows))
		return err
	})
}

func (p *Postgres) ListSlots(ctx context.Context, provider string) ([]booking.SlotView, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT s.id, s.provider, s.start_at, s.end_at, s.start_raw, s.end_raw,
		       NOT EXISTS (SELECT 1 FROM bookings b
		                   WHERE b.slot_id = s.id AND b.status = 'confirmed') AS available
		FROM slots s
		WHERE $1 = '' OR s.provider = $1
		ORDER BY s.start_at, s.id`, provider)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (booking.SlotView, error) {
		var v booking.SlotView
		err := r.Scan(&v.ID, &v.Provider, &v.Start, &v.End, &v.StartRaw, &v.EndRaw, &v.Available)
		return v, err
	})
}

func (p *Postgres) CreateBooking(ctx context.Context, b booking.Booking, now time.Time) (booking.Booking, error) {
	var start time.Time
	err := p.pool.QueryRow(ctx, `SELECT start_at FROM slots WHERE id = $1`, b.SlotID).Scan(&start)
	if errors.Is(err, pgx.ErrNoRows) {
		return booking.Booking{}, booking.ErrSlotNotFound
	}
	if err != nil {
		return booking.Booking{}, err
	}
	if !now.Before(start) {
		return booking.Booking{}, booking.ErrSlotStarted
	}

	// The partial unique index makes this the single point of truth: if a
	// confirmed booking for the slot exists (or is being inserted right now
	// by another request), this insert does nothing and returns no row.
	b.Status = booking.StatusConfirmed
	err = p.pool.QueryRow(ctx, `
		INSERT INTO bookings (id, slot_id, name, email, status, created_at)
		VALUES ($1, $2, $3, $4, 'confirmed', $5)
		ON CONFLICT (slot_id) WHERE status = 'confirmed' DO NOTHING
		RETURNING seq`,
		b.ID, b.SlotID, b.Name, b.Email, b.CreatedAt).Scan(&b.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return booking.Booking{}, booking.ErrSlotTaken
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // FK: slot deleted by a concurrent /reset
		return booking.Booking{}, booking.ErrSlotNotFound
	}
	if err != nil {
		return booking.Booking{}, err
	}
	return b, nil
}

const bookingCols = `seq, id, slot_id, name, email, status, created_at`

func scanBooking(r pgx.Row) (booking.Booking, error) {
	var b booking.Booking
	err := r.Scan(&b.Seq, &b.ID, &b.SlotID, &b.Name, &b.Email, &b.Status, &b.CreatedAt)
	return b, err
}

func (p *Postgres) GetBooking(ctx context.Context, id string) (booking.Booking, error) {
	b, err := scanBooking(p.pool.QueryRow(ctx, `SELECT `+bookingCols+` FROM bookings WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return booking.Booking{}, booking.ErrBookingNotFound
	}
	return b, err
}

func (p *Postgres) CancelBooking(ctx context.Context, id string) (booking.Booking, error) {
	// Conditional update: only one concurrent cancel can flip the status.
	b, err := scanBooking(p.pool.QueryRow(ctx, `
		UPDATE bookings SET status = 'cancelled'
		WHERE id = $1 AND status = 'confirmed'
		RETURNING `+bookingCols, id))
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return booking.Booking{}, err
	}
	// Nothing updated: either it doesn't exist or it was already cancelled.
	if _, err := p.GetBooking(ctx, id); err != nil {
		return booking.Booking{}, err
	}
	return booking.Booking{}, booking.ErrAlreadyCancelled
}

func (p *Postgres) ListBookingsByEmail(ctx context.Context, email string, afterSeq int64, limit int) ([]booking.Booking, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+bookingCols+` FROM bookings
		WHERE email = $1 AND seq > $2
		ORDER BY seq
		LIMIT $3`, email, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (booking.Booking, error) { return scanBooking(r) })
}

func (p *Postgres) ClaimIdempotencyKey(ctx context.Context, key, hash string, staleAfter time.Duration) (bool, booking.IdempotencyRecord, error) {
	// The primary key decides who owns the key: exactly one INSERT succeeds.
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (key, request_hash) VALUES ($1, $2)
		ON CONFLICT (key) DO NOTHING`, key, hash)
	if err != nil {
		return false, booking.IdempotencyRecord{}, err
	}
	if tag.RowsAffected() == 1 {
		return true, booking.IdempotencyRecord{}, nil
	}

	var (
		rec       booking.IdempotencyRecord
		status    *int32
		claimedAt time.Time
	)
	err = p.pool.QueryRow(ctx, `
		SELECT request_hash, status_code, response, claimed_at
		FROM idempotency_keys WHERE key = $1`, key).Scan(&rec.RequestHash, &status, &rec.Body, &claimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Released between our INSERT and SELECT; let the caller retry.
		return false, booking.IdempotencyRecord{RequestHash: hash}, nil
	}
	if err != nil {
		return false, booking.IdempotencyRecord{}, err
	}
	if status != nil {
		rec.Done, rec.StatusCode = true, int(*status)
		return false, rec, nil
	}
	if rec.RequestHash == hash && time.Since(claimedAt) > staleAfter {
		// Abandoned claim (e.g. crash mid-request). Take it over, but only if
		// nobody else did first (compare-and-swap on claimed_at).
		tag, err := p.pool.Exec(ctx, `
			UPDATE idempotency_keys SET claimed_at = now()
			WHERE key = $1 AND status_code IS NULL AND claimed_at = $2`, key, claimedAt)
		if err != nil {
			return false, booking.IdempotencyRecord{}, err
		}
		if tag.RowsAffected() == 1 {
			return true, booking.IdempotencyRecord{}, nil
		}
	}
	return false, rec, nil
}

func (p *Postgres) CompleteIdempotencyKey(ctx context.Context, key string, status int, body []byte) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE idempotency_keys SET status_code = $2, response = $3 WHERE key = $1`, key, status, body)
	return err
}

func (p *Postgres) ReleaseIdempotencyKey(ctx context.Context, key string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE key = $1 AND status_code IS NULL`, key)
	return err
}
