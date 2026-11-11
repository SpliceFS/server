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
	l := NewLimiter(3, time.Minute, clock.Now)
	for i := range 3 {
		if !l.Allow("192.0.2.1") {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if l.Allow("192.0.2.1") {
		t.Error("fourth attempt allowed")
	}
	if l.Allow("192.0.2.1") {
		t.Error("refused attempts must stay refused within the window")
	}
	if !l.Allow("192.0.2.2") {
		t.Error("another key must have its own count")
	}
}

func TestLimiterWindowResets(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l := NewLimiter(1, time.Minute, clock.Now)
	if !l.Allow("k") || l.Allow("k") {
		t.Fatal("first attempt must pass and the second must fail")
	}
	clock.Advance(time.Minute - time.Nanosecond)
	if l.Allow("k") {
		t.Error("allowed before the window passed")
	}
	clock.Advance(time.Nanosecond)
	if !l.Allow("k") {
		t.Error("refused after the window passed")
	}
	if l.Allow("k") {
		t.Error("new window allowed more than count attempts")
	}
}

func TestLimiterZeroCountRefusesAll(t *testing.T) {
	t.Parallel()
	l := NewLimiter(0, time.Minute, newFakeClock().Now)
	if l.Allow("k") {
		t.Error("count 0 allowed an attempt")
	}
}

func TestLimiterSweepsOldKeys(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l := NewLimiter(1, time.Minute, clock.Now)
	for _, key := range []string{"a", "b", "c"} {
		l.Allow(key)
	}
	if len(l.windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(l.windows))
	}
	clock.Advance(30 * time.Second)
	l.Allow("d")
	if len(l.windows) != 4 {
		t.Errorf("windows = %d, want 4 (no sweep inside the window)", len(l.windows))
	}
	clock.Advance(31 * time.Second)
	l.Allow("e")
	if _, ok := l.windows["a"]; ok || len(l.windows) != 2 {
		t.Errorf("windows = %v, want only d and e", l.windows)
	}
}

func TestLimiterConcurrent(t *testing.T) {
	t.Parallel()
	l := NewLimiter(10, time.Minute, newFakeClock().Now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow("k") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 10 {
		t.Errorf("allowed = %d, want 10", allowed)
	}
}
