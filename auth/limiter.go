package auth

import (
	"sync"
	"time"
)

// Limiter allows a fixed number of attempts per key in each fixed window. It is safe for
// concurrent use.
type Limiter struct {
	count int
	per   time.Duration
	now   func() time.Time

	mu        sync.Mutex
	windows   map[string]window
	nextSweep time.Time
}

// window is the attempts made by one key since start.
type window struct {
	start time.Time
	used  int
}

// NewLimiter returns a Limiter that allows count attempts per key in each window of length
// per. A count of zero or less refuses every attempt.
func NewLimiter(count int, per time.Duration, now func() time.Time) *Limiter {
	return &Limiter{count: count, per: per, now: now, windows: make(map[string]window)}
}

// Allow records an attempt by key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	w, ok := l.windows[key]
	if !ok || l.expired(w, now) {
		w = window{start: now}
	}
	allowed := w.used < l.count
	if allowed {
		w.used++
	}
	l.windows[key] = w
	return allowed
}

// sweep drops the windows that have passed, at most once per window length, so the map
// cannot grow without bound. The caller holds l.mu.
func (l *Limiter) sweep(now time.Time) {
	if now.Before(l.nextSweep) {
		return
	}
	for key, w := range l.windows {
		if l.expired(w, now) {
			delete(l.windows, key)
		}
	}
	l.nextSweep = now.Add(l.per)
}

// expired reports whether the window w has passed at now.
func (l *Limiter) expired(w window, now time.Time) bool {
	return now.Sub(w.start) >= l.per
}
