package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"koya-booking/internal/booking"
)

type Server struct {
	store   booking.Store
	apiKey  string
	limiter *RateLimiter
	page    []byte
	now     func() time.Time // swappable in tests
	log     *slog.Logger
}

func NewServer(store booking.Store, apiKey string, page []byte, log *slog.Logger) *Server {
	return &Server{
		store:   store,
		apiKey:  apiKey,
		limiter: NewRateLimiter(5, time.Minute),
		page:    page,
		now:     time.Now,
		log:     log,
	}
}

func (s *Server) Limiter() *RateLimiter { return s.limiter }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public: the web page and a health check for the host.
	mux.HandleFunc("GET /{$}", s.servePage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// API: every request needs X-API-Key (level 2).
	mux.Handle("POST /reset", s.auth(s.handleReset))
	mux.Handle("GET /slots", s.auth(s.handleListSlots))
	mux.Handle("POST /bookings", s.auth(s.handleCreateBooking))
	mux.Handle("GET /bookings", s.auth(s.handleListBookings))
	mux.Handle("GET /bookings/{id}", s.auth(s.handleGetBooking))
	mux.Handle("DELETE /bookings/{id}", s.auth(s.handleCancelBooking))

	// Anything else: JSON 404
	mux.Handle("/", s.auth(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	}))

	return s.recoverer(s.logRequests(mux))
}

func (s *Server) servePage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(s.page)
}

// auth rejects requests without the right X-API-Key with 401.
// ConstantTimeCompare avoids leaking the key through response timing.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-API-Key")
		if got == "" {
			writeError(w, http.StatusUnauthorized, "missing API key")
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.apiKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		next(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start).Round(time.Microsecond))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic", "err", v, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"internal error"}`)
	}
	writeRaw(w, status, body)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

type errorBody struct {
	Error string `json:"error"`
}

const maxBodyBytes = 1 << 20

// decodeJSON reads one JSON object from the body (max 1 MB).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return errors.New("request body must be valid JSON")
	}
	return nil
}
