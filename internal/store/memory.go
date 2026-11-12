package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"koya-booking/internal/booking"
)

type Memory struct {
	mu       sync.Mutex
	slots    map[string]booking.Slot
	bookings map[string]*booking.Booking // by booking id
	order    []*booking.Booking          // by Seq ascending
	seq      int64
	idem     map[string]*memIdem
}

type memIdem struct {
	rec       booking.IdempotencyRecord
	claimedAt time.Time
}

func NewMemory() *Memory {
	m := &Memory{}
	m.clear()
	return m
}

func (m *Memory) clear() {
	m.slots = map[string]booking.Slot{}
	m.bookings = map[string]*booking.Booking{}
	m.order = nil
	m.idem = map[string]*memIdem{}
}

func (m *Memory) Reset(_ context.Context, slots []booking.Slot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clear()
	for _, s := range slots {
		m.slots[s.ID] = s
	}
	return nil
}

func (m *Memory) ListSlots(_ context.Context, provider string) ([]booking.SlotView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	taken := map[string]bool{}
	for _, b := range m.order {
		if b.Status == booking.StatusConfirmed {
			taken[b.SlotID] = true
		}
	}
	out := make([]booking.SlotView, 0, len(m.slots))
	for _, s := range m.slots {
		if provider != "" && s.Provider != provider {
			continue
		}
		out = append(out, booking.SlotView{Slot: s, Available: !taken[s.ID]})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *Memory) CreateBooking(_ context.Context, b booking.Booking, now time.Time) (booking.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	slot, ok := m.slots[b.SlotID]
	if !ok {
		return booking.Booking{}, booking.ErrSlotNotFound
	}
	if !now.Before(slot.Start) {
		return booking.Booking{}, booking.ErrSlotStarted
	}
	for _, existing := range m.order {
		if existing.SlotID == b.SlotID && existing.Status == booking.StatusConfirmed {
			return booking.Booking{}, booking.ErrSlotTaken
		}
	}
	m.seq++
	b.Seq = m.seq
	b.Status = booking.StatusConfirmed
	stored := b
	m.bookings[b.ID] = &stored
	m.order = append(m.order, &stored)
	return stored, nil
}

func (m *Memory) GetBooking(_ context.Context, id string) (booking.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bookings[id]
	if !ok {
		return booking.Booking{}, booking.ErrBookingNotFound
	}
	return *b, nil
}

func (m *Memory) CancelBooking(_ context.Context, id string) (booking.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bookings[id]
	if !ok {
		return booking.Booking{}, booking.ErrBookingNotFound
	}
	if b.Status == booking.StatusCancelled {
		return booking.Booking{}, booking.ErrAlreadyCancelled
	}
	b.Status = booking.StatusCancelled
	return *b, nil
}

func (m *Memory) ListBookingsByEmail(_ context.Context, email string, afterSeq int64, limit int) ([]booking.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []booking.Booking{}
	for _, b := range m.order {
		if b.Seq <= afterSeq || b.Email != email {
			continue
		}
		out = append(out, *b)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (m *Memory) ClaimIdempotencyKey(_ context.Context, key, hash string, staleAfter time.Duration) (bool, booking.IdempotencyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	e, ok := m.idem[key]
	if !ok {
		m.idem[key] = &memIdem{rec: booking.IdempotencyRecord{RequestHash: hash}, claimedAt: now}
		return true, booking.IdempotencyRecord{}, nil
	}
	if !e.rec.Done && e.rec.RequestHash == hash && now.Sub(e.claimedAt) > staleAfter {
		e.claimedAt = now // take over an abandoned claim
		return true, booking.IdempotencyRecord{}, nil
	}
	return false, e.rec, nil
}

func (m *Memory) CompleteIdempotencyKey(_ context.Context, key string, status int, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.idem[key]; ok {
		e.rec.Done = true
		e.rec.StatusCode = status
		e.rec.Body = body
	}
	return nil
}

func (m *Memory) ReleaseIdempotencyKey(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.idem, key)
	return nil
}
