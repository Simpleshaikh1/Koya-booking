package api

import (
	"sync"
	"time"
)

type RateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

// Allow records an attempt for key at now.
func (l *RateLimiter) Allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	recent := pruned(l.hits[key], now.Add(-l.window))
	if len(recent) >= l.limit {
		l.hits[key] = recent
		return false, recent[0].Add(l.window).Sub(now)
	}
	l.hits[key] = append(recent, now)
	return true, 0
}

// Reset forgets every key (used by POST /reset so test runs start clean).
func (l *RateLimiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits = map[string][]time.Time{}
}

// Sweep drops keys with no attempts inside the window so memory stays bounded.
func (l *RateLimiter) Sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, ts := range l.hits {
		if recent := pruned(ts, now.Add(-l.window)); len(recent) == 0 {
			delete(l.hits, k)
		} else {
			l.hits[k] = recent
		}
	}
}

// pruned returns the timestamps strictly after cutoff (ts is sorted).
func pruned(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}
