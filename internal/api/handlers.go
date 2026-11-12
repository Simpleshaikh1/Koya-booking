package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"koya-booking/internal/booking"
)

type slotJSON struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Start     string `json:"start"`
	End       string `json:"end"`
	Available *bool  `json:"available,omitempty"`
}

type bookingJSON struct {
	ID        string `json:"id"`
	SlotID    string `json:"slot_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func toBookingJSON(b booking.Booking) bookingJSON {
	return bookingJSON{
		ID:        b.ID,
		SlotID:    b.SlotID,
		Name:      b.Name,
		Email:     b.Email,
		Status:    b.Status,
		CreatedAt: b.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

// ---------- POST /reset (level 1) ----------

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slots *[]slotJSON `json:"slots"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Slots == nil {
		writeError(w, http.StatusBadRequest, "slots is required")
		return
	}

	slots := make([]booking.Slot, 0, len(*req.Slots))
	seen := map[string]bool{}
	for i, in := range *req.Slots {
		if strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.Provider) == "" {
			writeError(w, http.StatusBadRequest, "slots["+strconv.Itoa(i)+"]: id and provider are required")
			return
		}
		if seen[in.ID] {
			writeError(w, http.StatusBadRequest, "duplicate slot id "+strconv.Quote(in.ID))
			return
		}
		seen[in.ID] = true
		start, err1 := time.Parse(time.RFC3339, in.Start)
		end, err2 := time.Parse(time.RFC3339, in.End)
		if err1 != nil || err2 != nil {
			writeError(w, http.StatusBadRequest, "slots["+strconv.Itoa(i)+"]: start and end must be RFC 3339 date-times")
			return
		}
		slots = append(slots, booking.Slot{
			ID: in.ID, Provider: in.Provider,
			Start: start, End: end, StartRaw: in.Start, EndRaw: in.End,
		})
	}

	if err := s.store.Reset(r.Context(), slots); err != nil {
		s.serverError(w, "reset", err)
		return
	}
	s.limiter.Reset()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "slots": len(slots)})
}

// ---------- GET /slots (level 1) ----------

func (s *Server) handleListSlots(w http.ResponseWriter, r *http.Request) {
	views, err := s.store.ListSlots(r.Context(), r.URL.Query().Get("provider"))
	if err != nil {
		s.serverError(w, "list slots", err)
		return
	}
	out := make([]slotJSON, len(views))
	for i, v := range views {
		available := v.Available
		out[i] = slotJSON{ID: v.ID, Provider: v.Provider, Start: v.StartRaw, End: v.EndRaw, Available: &available}
	}
	writeJSON(w, http.StatusOK, map[string]any{"slots": out})
}

// ---------- POST /bookings (levels 1-5) ----------

type bookingRequest struct {
	SlotID string `json:"slot_id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
}

// hash fingerprints the request body for Idempotency-Key comparison.
func (b bookingRequest) hash() string {
	canon, _ := json.Marshal(b)
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

type response struct {
	status     int
	body       []byte
	retryAfter int // seconds; >0 adds a Retry-After header
}

func jsonResponse(status int, v any) response {
	body, _ := json.Marshal(v)
	return response{status: status, body: body}
}

func errResponse(status int, msg string) response {
	return jsonResponse(status, errorBody{Error: msg})
}

func (resp response) write(w http.ResponseWriter) {
	if resp.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(resp.retryAfter))
	}
	writeRaw(w, resp.status, resp.body)
}

const (
	idemWaitTimeout = 10 * time.Second // how long a duplicate waits for the original to finish
	idemPollEvery   = 25 * time.Millisecond
	idemStaleAfter  = 30 * time.Second // a claim older than this is considered abandoned
)

func (s *Server) handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	var req bookingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		s.book(r.Context(), req).write(w)
		return
	}

	hash := req.hash()
	claimed, rec, err := s.claimKey(r.Context(), key, hash)
	if err != nil {
		s.serverError(w, "claim idempotency key", err)
		return
	}
	if !claimed {
		switch {
		case rec.RequestHash != hash:
			writeError(w, http.StatusUnprocessableEntity, "Idempotency-Key was already used with a different request body")
		case rec.Done:
			w.Header().Set("Idempotent-Replayed", "true")
			writeRaw(w, rec.StatusCode, rec.Body)
		default:
			writeError(w, http.StatusConflict, "a request with this Idempotency-Key is still being processed")
		}
		return
	}

	resp := s.book(r.Context(), req)

	ctx := context.WithoutCancel(r.Context())
	if resp.status == http.StatusTooManyRequests || resp.status >= 500 {

		err = s.store.ReleaseIdempotencyKey(ctx, key)

	} else {
		err = s.store.CompleteIdempotencyKey(ctx, key, resp.status, resp.body)
	}
	if err != nil {
		s.log.Error("store idempotency result", "err", err)
	}
	resp.write(w)
}

// claimKey tries to take ownership of key. If another request with the same
// key is still running it waits for that one to finish.
func (s *Server) claimKey(ctx context.Context, key, hash string) (bool, booking.IdempotencyRecord, error) {
	deadline := time.Now().Add(idemWaitTimeout)
	for {
		claimed, rec, err := s.store.ClaimIdempotencyKey(ctx, key, hash, idemStaleAfter)
		if err != nil || claimed || rec.Done || rec.RequestHash != hash || time.Now().After(deadline) {
			return claimed, rec, err
		}
		select {
		case <-ctx.Done():
			return false, rec, ctx.Err()
		case <-time.After(idemPollEvery):
		}
	}
}

var emailRE = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// book runs the rate limit, validation and the actual booking, and returns
// the response to send.
func (s *Server) book(ctx context.Context, req bookingRequest) response {
	now := s.now()
	slotID := strings.TrimSpace(req.SlotID)
	name := strings.TrimSpace(req.Name)
	email := strings.ToLower(strings.TrimSpace(req.Email))

	// Level 5: 5 attempts per email per minute, counted before validation.
	if email != "" {
		if ok, wait := s.limiter.Allow(email, now); !ok {
			resp := errResponse(http.StatusTooManyRequests, "too many booking attempts for this email, try again later")
			resp.retryAfter = max(1, int(math.Ceil(wait.Seconds())))
			return resp
		}
	}

	// Level 2: validation.
	switch {
	case slotID == "":
		return errResponse(http.StatusBadRequest, "slot_id is required")
	case name == "":
		return errResponse(http.StatusBadRequest, "name is required")
	case email == "":
		return errResponse(http.StatusBadRequest, "email is required")
	case !emailRE.MatchString(email):
		return errResponse(http.StatusBadRequest, "email is not valid")
	}

	b, err := s.store.CreateBooking(ctx, booking.Booking{
		ID:        newID(),
		SlotID:    slotID,
		Name:      name,
		Email:     email,
		CreatedAt: now.UTC().Truncate(time.Millisecond),
	}, now)
	switch {
	case err == nil:
		return jsonResponse(http.StatusCreated, toBookingJSON(b))
	case errors.Is(err, booking.ErrSlotNotFound):
		return errResponse(http.StatusNotFound, "slot not found")
	case errors.Is(err, booking.ErrSlotStarted):
		return errResponse(http.StatusBadRequest, "slot has already started")
	case errors.Is(err, booking.ErrSlotTaken):
		return errResponse(http.StatusConflict, "slot is already booked")
	default:
		s.log.Error("create booking", "err", err)
		return errResponse(http.StatusInternalServerError, "internal error")
	}
}

// newID returns a random, URL-safe booking id like "bk_7GQ2...".
func newID() string { return "bk_" + rand.Text() }

// ---------- GET /bookings?email=&limit=&cursor= (level 4) ----------

func (s *Server) handleListBookings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	email := strings.ToLower(strings.TrimSpace(q.Get("email")))
	if email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	limit := 10
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > 50 {
			writeError(w, http.StatusBadRequest, "limit must be an integer from 1 to 50")
			return
		}
		limit = n
	}

	var after int64
	if c := q.Get("cursor"); c != "" {
		seq, ok := decodeCursor(c)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		after = seq
	}

	// Ask for one extra row: if it exists, there is a next page.
	rows, err := s.store.ListBookingsByEmail(r.Context(), email, after, limit+1)
	if err != nil {
		s.serverError(w, "list bookings", err)
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := encodeCursor(rows[limit-1].Seq)
		next = &c
	}

	out := make([]bookingJSON, len(rows))
	for i, b := range rows {
		out[i] = toBookingJSON(b)
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookings": out, "next_cursor": next})
}

func encodeCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("seq:" + strconv.FormatInt(seq, 10)))
}

func decodeCursor(c string) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	n, ok := strings.CutPrefix(string(raw), "seq:")
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseInt(n, 10, 64)
	return seq, err == nil && seq >= 0
}

// ---------- GET /bookings/{id} (level 1) ----------

func (s *Server) handleGetBooking(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.GetBooking(r.Context(), r.PathValue("id"))
	if errors.Is(err, booking.ErrBookingNotFound) {
		writeError(w, http.StatusNotFound, "booking not found")
		return
	}
	if err != nil {
		s.serverError(w, "get booking", err)
		return
	}
	writeJSON(w, http.StatusOK, toBookingJSON(b))
}

// ---------- DELETE /bookings/{id} (level 3) ----------

func (s *Server) handleCancelBooking(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.CancelBooking(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, booking.ErrBookingNotFound):
		writeError(w, http.StatusNotFound, "booking not found")
	case errors.Is(err, booking.ErrAlreadyCancelled):
		writeError(w, http.StatusConflict, "booking is already cancelled")
	case err != nil:
		s.serverError(w, "cancel booking", err)
	default:
		writeJSON(w, http.StatusOK, toBookingJSON(b))
	}
}

func (s *Server) serverError(w http.ResponseWriter, op string, err error) {
	s.log.Error(op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}
