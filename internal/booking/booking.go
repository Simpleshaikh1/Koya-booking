package booking

import (
	"context"
	"errors"
	"time"
)

const (
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"
)

// Slot is a bookable appointment window for one provider.
type Slot struct {
	ID       string
	Provider string
	Start    time.Time
	End      time.Time
	StartRaw string
	EndRaw   string
}

// SlotView is a slot plus whether it can still be booked.
type SlotView struct {
	Slot
	Available bool
}

// Booking is one patient's claim on a slot.
type Booking struct {
	Seq       int64
	ID        string
	SlotID    string
	Name      string
	Email     string
	Status    string
	CreatedAt time.Time
}

// IdempotencyRecord is what we remember about an Idempotency-Key.
type IdempotencyRecord struct {
	RequestHash string
	Done        bool
	StatusCode  int
	Body        []byte
}

var (
	ErrSlotNotFound     = errors.New("slot not found")
	ErrSlotStarted      = errors.New("slot has already started")
	ErrSlotTaken        = errors.New("slot is already booked")
	ErrBookingNotFound  = errors.New("booking not found")
	ErrAlreadyCancelled = errors.New("booking is already cancelled")
)

type Store interface {
	// Reset deletes every booking and idempotency key and replaces all slots.
	Reset(ctx context.Context, slots []Slot) error

	// ListSlots returns slots ordered by start (earliest first).
	ListSlots(ctx context.Context, provider string) ([]SlotView, error)

	// CreateBooking books b.SlotID. Returns ErrSlotNotFound, ErrSlotStarted
	// (slot start <= now) or ErrSlotTaken (already has a confirmed booking).
	CreateBooking(ctx context.Context, b Booking, now time.Time) (Booking, error)
	GetBooking(ctx context.Context, id string) (Booking, error)

	// CancelBooking marks the booking cancelled, which frees its slot.
	CancelBooking(ctx context.Context, id string) (Booking, error)

	// ListBookingsByEmail returns up to limit bookings for email (already
	// lower-cased) with Seq > afterSeq, ordered by Seq ascending.
	ListBookingsByEmail(ctx context.Context, email string, afterSeq int64, limit int) ([]Booking, error)

	// ClaimIdempotencyKey atomically reserves key for this request.
	ClaimIdempotencyKey(ctx context.Context, key, requestHash string, staleAfter time.Duration) (claimed bool, rec IdempotencyRecord, err error)
	CompleteIdempotencyKey(ctx context.Context, key string, statusCode int, body []byte) error

	// ReleaseIdempotencyKey forgets a claimed key so a later retry runs fresh.
	ReleaseIdempotencyKey(ctx context.Context, key string) error
}
