package ratelimit

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Discord counts the global limit per second from when requests arrive. Network
// jitter can squeeze sends from two of our seconds into one of Discord's, so a slot
// is only reused after a little more than a second.
const (
	globalPeriod = 1100 * time.Millisecond
	leaseValid   = 250 * time.Millisecond
)

// Window is a sliding window over the last slots handed out: at most limit slots in
// any globalPeriod. It also holds a pause set by a global 429.
type Window struct {
	mu          sync.Mutex
	slots       []time.Time
	pausedUntil time.Time
}

func NewWindow(limit int) *Window {
	return &Window{slots: make([]time.Time, max(1, limit))}
}

// oldestLocked returns the index of the slot that frees up first and when it is free.
func (w *Window) oldestLocked() (int, time.Time) {
	idx := 0
	for i, t := range w.slots {
		if t.Before(w.slots[idx]) {
			idx = i
		}
	}
	return idx, w.slots[idx].Add(globalPeriod)
}

func (w *Window) tryTake(now time.Time) (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.Before(w.pausedUntil) {
		return w.pausedUntil.Sub(now), false
	}
	idx, free := w.oldestLocked()
	if now.Before(free) {
		return free.Sub(now), false
	}
	w.slots[idx] = now
	return 0, true
}

// Take blocks until the request may go, or ctx ends.
func (w *Window) Take(ctx context.Context) error {
	for {
		wait, ok := w.tryTake(time.Now())
		if ok {
			return nil
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Grant hands out up to want slots to another node, valid for validFor. A slot is
// recorded at the end of its validity, the latest moment it can be spent. When
// nothing is free, validFor says how long until the next slot frees up.
func (w *Window) Grant(now time.Time, want int) (granted int, validFor time.Duration, pausedUntil time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.Before(w.pausedUntil) {
		return 0, w.pausedUntil.Sub(now), w.pausedUntil
	}
	for granted < want {
		idx, free := w.oldestLocked()
		if now.Before(free) {
			break
		}
		w.slots[idx] = now.Add(leaseValid)
		granted++
	}
	if granted == 0 {
		_, free := w.oldestLocked()
		return 0, max(free.Sub(now), time.Millisecond), w.pausedUntil
	}
	return granted, leaseValid, w.pausedUntil
}

func (w *Window) Pause(until time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if until.After(w.pausedUntil) {
		w.pausedUntil = until
	}
}

func (w *Window) PausedUntil() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pausedUntil
}

// SetLimit resizes the window, keeping the most recent slots.
func (w *Window) SetLimit(limit int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	limit = max(1, limit)
	if limit == len(w.slots) {
		return
	}
	sorted := append([]time.Time(nil), w.slots...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].After(sorted[j]) })
	next := make([]time.Time, limit)
	copy(next, sorted)
	w.slots = next
}

// Globals keeps one window per bot.
type Globals struct {
	mu           sync.Mutex
	windows      map[string]*Window
	defaultLimit int
	overrides    map[string]int
}

func NewGlobals(defaultLimit int, overrides map[string]int) *Globals {
	return &Globals{windows: map[string]*Window{}, defaultLimit: defaultLimit, overrides: overrides}
}

func (g *Globals) Limit(botID string) int {
	if n, ok := g.overrides[botID]; ok {
		return n
	}
	return g.defaultLimit
}

func (g *Globals) Window(key string, limit int) *Window {
	g.mu.Lock()
	defer g.mu.Unlock()
	w, ok := g.windows[key]
	if !ok {
		w = NewWindow(limit)
		g.windows[key] = w
	}
	return w
}
