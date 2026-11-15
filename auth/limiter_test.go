package auth

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock safe for concurrent use.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestLimiterCountsPerKey(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	limiter := NewLimiter(3, time.Minute, clock.Now)
	for i := range 3 {
		if !limiter.Allow("192.0.2.1") {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if limiter.Allow("192.0.2.1") {
		t.Error("fourth attempt allowed")
	}
	if limiter.Allow("192.0.2.1") {
		t.Error("refused attempts must stay refused within the window")
	}
	if !limiter.Allow("192.0.2.2") {
		t.Error("another key must have its own count")
	}
}

func TestLimiterWindowResets(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	limiter := NewLimiter(1, time.Minute, clock.Now)
	if !limiter.Allow("k") || limiter.Allow("k") {
		t.Fatal("first attempt must pass and the second must fail")
	}
	clock.Advance(time.Minute - time.Nanosecond)
	if limiter.Allow("k") {
		t.Error("allowed before the window passed")
	}
	clock.Advance(time.Nanosecond)
	if !limiter.Allow("k") {
		t.Error("refused after the window passed")
	}
	if limiter.Allow("k") {
		t.Error("new window allowed more than count attempts")
	}
}

func TestLimiterZeroCountRefusesAll(t *testing.T) {
	t.Parallel()
	limiter := NewLimiter(0, time.Minute, newFakeClock().Now)
	if limiter.Allow("k") {
		t.Error("count 0 allowed an attempt")
	}
}

func TestLimiterSweepsOldKeys(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	limiter := NewLimiter(1, time.Minute, clock.Now)
	for _, key := range []string{"a", "b", "c"} {
		limiter.Allow(key)
	}
	if len(limiter.windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(limiter.windows))
	}
	clock.Advance(30 * time.Second)
	limiter.Allow("d")
	if len(limiter.windows) != 4 {
		t.Errorf("windows = %d, want 4 (no sweep inside the window)", len(limiter.windows))
	}
	clock.Advance(31 * time.Second)
	limiter.Allow("e")
	if _, ok := limiter.windows["a"]; ok || len(limiter.windows) != 2 {
		t.Errorf("windows = %v, want only d and e", limiter.windows)
	}
}

func TestLimiterConcurrent(t *testing.T) {
	t.Parallel()
	limiter := NewLimiter(10, time.Minute, newFakeClock().Now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 50 {
		wg.Go(func() {
			if limiter.Allow("k") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != 10 {
		t.Errorf("allowed = %d, want 10", allowed)
	}
}
