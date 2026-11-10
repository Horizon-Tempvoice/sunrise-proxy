package ratelimit

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// state is the bucket bookkeeping without any locking or waiting, so the window math
// can be tested on its own.
type state struct {
	known       bool // a response carried ratelimit headers
	free        bool // responses carry no ratelimit headers, so nothing to pace
	limit       int
	remaining   int
	windowKnown bool // resetAt is valid
	resetAt     time.Time
	pausedUntil time.Time
	inflight    int
	sent        uint64
	winStart    uint64 // first sequence number sent in the current window
}

// take decides whether one request may go out now. When it may not, a positive wait
// says when to look again and a zero wait means "wait until a response arrives".
func (s *state) take(now time.Time, concurrent bool) (seq uint64, wait time.Duration, ok bool) {
	if now.Before(s.pausedUntil) {
		return 0, s.pausedUntil.Sub(now), false
	}
	if !s.known {
		if s.inflight > 0 && (!s.free || !concurrent) {
			return 0, 0, false
		}
		return s.send(), 0, true
	}
	if s.windowKnown && !now.Before(s.resetAt) {
		s.remaining = s.limit
		s.windowKnown = false
		s.winStart = s.sent + 1
	}
	if !concurrent && s.inflight > 0 {
		return 0, 0, false
	}
	if s.remaining > 0 {
		s.remaining--
		return s.send(), 0, true
	}
	if s.windowKnown {
		return 0, s.resetAt.Sub(now), false
	}
	if s.inflight > 0 {
		return 0, 0, false
	}
	return s.send(), 0, true
}

func (s *state) send() uint64 {
	s.inflight++
	s.sent++
	return s.sent
}

// apply folds a response into the state. Concurrent responses arrive in a different
// order than Discord counted them, so a response can never raise what is left in the
// current window; it can only lower it, for example when another client shares the
// bucket. Requests still in flight may not be counted yet and are subtracted. A late
// response from the previous window says nothing about the current one.
func (s *state) apply(seq uint64, h Headers, now time.Time) {
	if s.inflight > 0 {
		s.inflight--
	}
	if h.IsRatelimited() {
		s.pause(h, now)
		return
	}
	if !h.HasLimits {
		if !s.known && h.Status < 400 {
			s.free = true
		}
		return
	}
	rem := max(0, h.Remaining-s.inflight)
	reset := now.Add(h.ResetAfter)
	if !s.known {
		s.known = true
		s.free = false
		s.limit = h.Limit
		s.remaining = rem
		s.resetAt = reset
		s.windowKnown = true
		return
	}
	s.limit = h.Limit
	if seq < s.winStart {
		return
	}
	s.remaining = min(s.remaining, rem)
	if !s.windowKnown || reset.After(s.resetAt) {
		s.resetAt = reset
		s.windowKnown = true
	}
}

func (s *state) pause(h Headers, now time.Time) {
	if h.Scope == ScopeGlobal {
		return
	}
	until := now.Add(h.Pause())
	if until.After(s.pausedUntil) {
		s.pausedUntil = until
	}
	if h.HasLimits {
		s.known = true
		s.free = false
		s.limit = h.Limit
	}
	s.remaining = 0
	s.windowKnown = true
	if until.After(s.resetAt) {
		s.resetAt = until
	}
}

// merge folds in a response for a request that went through another queue but was
// counted by Discord in this bucket, which happens when a new route probes a bucket
// that is already known. Within the same window it can only lower what is left.
func (s *state) merge(h Headers, now time.Time) {
	if h.IsRatelimited() {
		s.pause(h, now)
		return
	}
	if !h.HasLimits {
		return
	}
	rem := max(0, h.Remaining-s.inflight)
	reset := now.Add(h.ResetAfter)
	switch {
	case !s.known:
		s.remaining = rem
		s.resetAt = reset
		s.windowKnown = true
	case s.windowKnown && reset.After(s.resetAt.Add(50*time.Millisecond)):
		s.remaining = rem
		s.resetAt = reset
	default:
		s.remaining = min(s.remaining, rem)
		if !s.windowKnown {
			s.resetAt = reset
			s.windowKnown = true
		}
	}
	s.known = true
	s.free = false
	s.limit = h.Limit
}

// abort releases a request that never got a response. Its slot stays used because
// we cannot know whether Discord counted it.
func (s *state) abort() {
	if s.inflight > 0 {
		s.inflight--
	}
}

// Bucket queues requests for one Discord bucket in arrival order.
type Bucket struct {
	mu         sync.Mutex
	st         state
	queue      list.List
	redirect   *Bucket
	learnedID  string
	concurrent bool
	lastUsed   time.Time
}

type waiter struct {
	ch chan struct{}
}

func newBucket(concurrent bool, now time.Time) *Bucket {
	return &Bucket{concurrent: concurrent, lastUsed: now}
}

// acquire waits for a slot. If the bucket was merged into another one while waiting,
// it returns that bucket and the caller queues there instead.
func (b *Bucket) acquire(ctx context.Context) (uint64, *Bucket, error) {
	b.mu.Lock()
	w := &waiter{ch: make(chan struct{}, 1)}
	e := b.queue.PushBack(w)
	b.lastUsed = time.Now()

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		if b.redirect != nil {
			target := b.redirect
			b.queue.Remove(e)
			b.mu.Unlock()
			return 0, target, nil
		}
		var wait time.Duration
		if b.queue.Front() == e {
			seq, d, ok := b.st.take(time.Now(), b.concurrent)
			if ok {
				b.queue.Remove(e)
				b.wakeFrontLocked()
				b.mu.Unlock()
				return seq, nil, nil
			}
			wait = d
		}
		b.mu.Unlock()

		var tick <-chan time.Time
		if wait > 0 {
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				timer.Reset(wait)
			}
			tick = timer.C
		}
		select {
		case <-w.ch:
		case <-tick:
		case <-ctx.Done():
			b.mu.Lock()
			b.queue.Remove(e)
			b.wakeFrontLocked()
			b.mu.Unlock()
			return 0, nil, ctx.Err()
		}
		b.mu.Lock()
	}
}

func (b *Bucket) wakeFrontLocked() {
	if f := b.queue.Front(); f != nil {
		select {
		case f.Value.(*waiter).ch <- struct{}{}:
		default:
		}
	}
}

func (b *Bucket) release(seq uint64, h Headers) {
	b.mu.Lock()
	b.st.apply(seq, h, time.Now())
	b.lastUsed = time.Now()
	b.wakeFrontLocked()
	b.mu.Unlock()
}

func (b *Bucket) abort() {
	b.mu.Lock()
	b.st.abort()
	b.lastUsed = time.Now()
	b.wakeFrontLocked()
	b.mu.Unlock()
}

func (b *Bucket) merge(h Headers) {
	b.mu.Lock()
	b.st.merge(h, time.Now())
	b.wakeFrontLocked()
	b.mu.Unlock()
}

// redirectTo moves every waiter over to target, in queue order.
func (b *Bucket) redirectTo(target *Bucket) {
	b.mu.Lock()
	b.redirect = target
	for e := b.queue.Front(); e != nil; e = e.Next() {
		select {
		case e.Value.(*waiter).ch <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
}

func (b *Bucket) idle(now time.Time, ttl time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.queue.Len() == 0 && b.st.inflight == 0 && now.Sub(b.lastUsed) > ttl
}

func (b *Bucket) depth() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.queue.Len()
}
