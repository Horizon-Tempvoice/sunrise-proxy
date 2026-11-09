package guard

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// Discord bans an IP for a while after 10,000 responses with status 401, 403 or 429
// (shared scope excluded) within 10 minutes.
const (
	InvalidWindow = 10 * time.Minute
	InvalidLimit  = 10_000
	slots         = int(InvalidWindow / time.Second)
)

// Invalid counts invalid responses of this node in a sliding ten minute window,
// one counter per second.
type Invalid struct {
	mu         sync.Mutex
	counts     [slots]int
	stamps     [slots]int64
	thresholds []int
	warned     int
	brake      bool
	brakeAt    int
	braking    bool
	log        *slog.Logger
}

func NewInvalid(thresholds []int, brake bool, brakeAt int, log *slog.Logger) *Invalid {
	return &Invalid{thresholds: thresholds, brake: brake, brakeAt: brakeAt, log: log}
}

func (iv *Invalid) Add(now time.Time) {
	sec := now.Unix()
	i := int(sec % int64(slots))
	iv.mu.Lock()
	if iv.stamps[i] != sec {
		iv.stamps[i] = sec
		iv.counts[i] = 0
	}
	iv.counts[i]++
	n := iv.countLocked(sec)
	var crossed int
	for iv.warned < len(iv.thresholds) && n >= iv.thresholds[iv.warned] {
		crossed = iv.thresholds[iv.warned]
		iv.warned++
	}
	iv.mu.Unlock()
	if crossed > 0 {
		iv.log.Warn("invalid responses in the last 10 minutes crossed a threshold",
			"count", n, "threshold", crossed, "ban_at", InvalidLimit)
	}
}

func (iv *Invalid) Count(now time.Time) int {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	return iv.countLocked(now.Unix())
}

func (iv *Invalid) countLocked(sec int64) int {
	n := 0
	oldest := sec - int64(slots)
	for i := range iv.stamps {
		if iv.stamps[i] > oldest {
			n += iv.counts[i]
		}
	}
	return n
}

// Braking reports whether the hard brake is on and the window is at its threshold.
func (iv *Invalid) Braking() bool {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	return iv.braking
}

// Run re-evaluates the window once per second so warnings re-arm and the brake
// releases as old responses fall out of the window.
func (iv *Invalid) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			iv.tick(now)
		}
	}
}

func (iv *Invalid) tick(now time.Time) {
	iv.mu.Lock()
	n := iv.countLocked(now.Unix())
	for iv.warned > 0 && n < iv.thresholds[iv.warned-1] {
		iv.warned--
	}
	was := iv.braking
	iv.braking = iv.brake && n >= iv.brakeAt
	iv.mu.Unlock()

	switch {
	case iv.braking && !was:
		iv.log.Error("hard brake engaged: answering every request with a generated 429 until the window drains",
			"count", n, "threshold", iv.brakeAt)
	case !iv.braking && was:
		iv.log.Warn("hard brake released", "count", n)
	}
}

// Locks remembers tokens that got a 401 during normal operation.
type Locks struct {
	mu       sync.Mutex
	until    map[string]time.Time
	duration time.Duration
}

// NewLocks creates the lock table. A zero duration locks for the lifetime of the process.
func NewLocks(duration time.Duration) *Locks {
	return &Locks{until: map[string]time.Time{}, duration: duration}
}

func (l *Locks) Lock(tokenKey string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, exists := l.until[tokenKey]
	var until time.Time
	if l.duration > 0 {
		until = now.Add(l.duration)
	}
	l.until[tokenKey] = until
	return !exists
}

func (l *Locks) Locked(tokenKey string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.until[tokenKey]
	if !ok {
		return false
	}
	if !until.IsZero() && now.After(until) {
		delete(l.until, tokenKey)
		return false
	}
	return true
}

func (l *Locks) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.until)
}

// Webhooks remembers webhooks that answered 404. Discord asks clients to stop using them.
type Webhooks struct {
	cache *expirable.LRU[string, struct{}]
}

const maxDeadWebhooks = 10_000

func NewWebhooks(ttl time.Duration) *Webhooks {
	return &Webhooks{cache: expirable.NewLRU[string, struct{}](maxDeadWebhooks, nil, ttl)}
}

func (w *Webhooks) Mark(id string) bool {
	if w.cache.Contains(id) {
		return false
	}
	w.cache.Add(id, struct{}{})
	return true
}

func (w *Webhooks) Dead(id string) bool {
	_, ok := w.cache.Get(id)
	return ok
}

func (w *Webhooks) Len() int {
	return w.cache.Len()
}
