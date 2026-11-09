package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Window counts requests in fixed one second windows, the way Discord states its
// global limit. It also holds a pause set by a global 429.
type Window struct {
	mu          sync.Mutex
	limit       int
	start       time.Time
	used        int
	pausedUntil time.Time
}

func NewWindow(limit int) *Window {
	return &Window{limit: limit}
}

func (w *Window) roll(now time.Time) {
	if now.Sub(w.start) >= time.Second {
		w.start = now
		w.used = 0
	}
}

func (w *Window) tryTake(now time.Time) (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.Before(w.pausedUntil) {
		return w.pausedUntil.Sub(now), false
	}
	w.roll(now)
	if w.used < w.limit {
		w.used++
		return 0, true
	}
	return w.start.Add(time.Second).Sub(now), false
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

// Grant hands out up to want slots of the current window to another node. The slots
// are valid for validFor, the rest of this window.
func (w *Window) Grant(now time.Time, want int) (granted int, validFor time.Duration, pausedUntil time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now.Before(w.pausedUntil) {
		return 0, 0, w.pausedUntil
	}
	w.roll(now)
	free := w.limit - w.used
	if want > free {
		want = free
	}
	if want < 0 {
		want = 0
	}
	w.used += want
	return want, w.start.Add(time.Second).Sub(now), w.pausedUntil
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

func (w *Window) SetLimit(limit int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.limit = limit
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
