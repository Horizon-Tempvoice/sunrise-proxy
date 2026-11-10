package ratelimit

import (
	"testing"
	"time"
)

type step struct {
	at  time.Duration
	op  string // take, apply, merge, abort
	seq uint64
	h   Headers

	ok   bool
	wait time.Duration
}

func limits(limit, remaining int, resetAfter time.Duration) Headers {
	return Headers{Status: 200, HasLimits: true, Limit: limit, Remaining: remaining, ResetAfter: resetAfter, Bucket: "abc"}
}

func TestBucketWindow(t *testing.T) {
	s := time.Second
	ms := time.Millisecond
	tests := []struct {
		name       string
		concurrent bool
		steps      []step
	}{
		{
			name: "first request goes alone, then up to remaining", concurrent: true,
			steps: []step{
				{op: "take", ok: true},
				{op: "take", ok: false, wait: 0},
				{op: "apply", seq: 1, h: limits(3, 2, s)},
				{op: "take", ok: true},
				{op: "take", ok: true},
				{op: "take", ok: false, wait: s},
			},
		},
		{
			name: "out of order responses never raise what is left", concurrent: true,
			steps: []step{
				{op: "take", ok: true},
				{op: "apply", seq: 1, h: limits(5, 4, 2*s)},
				{op: "take", ok: true},
				{op: "take", ok: true},
				{op: "take", ok: true},
				// seq 3 says 2 left, but seq 2 and 4 are still out and may not be counted yet.
				{at: 100 * ms, op: "apply", seq: 3, h: limits(5, 2, 1900*ms)},
				{at: 100 * ms, op: "apply", seq: 2, h: limits(5, 3, 1950*ms)},
				{at: 100 * ms, op: "take", ok: false, wait: 1950 * ms},
			},
		},
		{
			name: "fixed window refills at reset, then waits for fresh headers", concurrent: true,
			steps: []step{
				{op: "take", ok: true},
				{op: "apply", seq: 1, h: limits(2, 1, s)},
				{op: "take", ok: true},
				{op: "take", ok: false, wait: s},
				{at: s, op: "take", ok: true},
				{at: s, op: "take", ok: true},
				{at: s, op: "take", ok: false, wait: 0},
				{at: s + 50*ms, op: "apply", seq: 3, h: limits(2, 1, 950*ms)},
				{at: s + 50*ms, op: "take", ok: false, wait: 950 * ms},
			},
		},
		{
			name: "a late response from the previous window does not refill again", concurrent: true,
			steps: []step{
				{op: "take", ok: true},
				{op: "apply", seq: 1, h: limits(2, 1, s)},
				{op: "take", ok: true},
				{at: s, op: "take", ok: true},
				{at: s + 20*ms, op: "apply", seq: 2, h: limits(2, 0, 0)},
				{at: s + 30*ms, op: "take", ok: true},
				{at: s + 30*ms, op: "take", ok: false, wait: 0},
			},
		},
		{
			name: "regained slots in a sliding window wait for the reset", concurrent: true,
			steps: []step{
				{op: "take", ok: true},
				{op: "apply", seq: 1, h: limits(3, 2, 3*s)},
				{op: "take", ok: true},
				{op: "take", ok: true},
				{op: "take", ok: false, wait: 3 * s},
				{at: 1500 * ms, op: "apply", seq: 3, h: limits(3, 1, 1500*ms)},
				{at: 1500 * ms, op: "take", ok: false, wait: 1500 * ms},
			},
		},
		{
			name: "user 429 pauses the bucket for retry-after", concurrent: true,
			steps: []step{
				{op: "take", ok: true},
				{op: "apply", seq: 1, h: Headers{Status: 429, Scope: ScopeUser, RetryAfter: 3 * s}},
				{at: s, op: "take", ok: false, wait: 2 * s},
				{at: 3 * s, op: "take", ok: true},
			},
		},
		{
			name: "global 429 leaves the bucket alone", concurrent: true,
			steps: []step{
				{op: "take", ok: true},
				{op: "apply", seq: 1, h: limits(5, 4, s)},
				{op: "take", ok: true},
				{op: "apply", seq: 2, h: Headers{Status: 429, Scope: ScopeGlobal, RetryAfter: 5 * s}},
				{op: "take", ok: true},
			},
		},
		{
			name: "routes without ratelimit headers run concurrently", concurrent: true,
			steps: []step{
				{op: "take", ok: true},
				{op: "apply", seq: 1, h: Headers{Status: 200}},
				{op: "take", ok: true},
				{op: "take", ok: true},
			},
		},
		{
			name: "concurrency off keeps one request in flight", concurrent: false,
			steps: []step{
				{op: "take", ok: true},
				{op: "apply", seq: 1, h: limits(5, 4, s)},
				{op: "take", ok: true},
				{op: "take", ok: false, wait: 0},
				{op: "apply", seq: 2, h: limits(5, 3, s)},
				{op: "take", ok: true},
			},
		},
		{
			name: "a probe from another route lowers the shared bucket", concurrent: true,
			steps: []step{
				{op: "take", ok: true},
				{op: "apply", seq: 1, h: limits(2, 1, s)},
				{at: 10 * ms, op: "merge", h: limits(2, 0, 990*ms)},
				{at: 10 * ms, op: "take", ok: false, wait: 990 * ms},
			},
		},
		{
			name: "a failed request frees the probe", concurrent: true,
			steps: []step{
				{op: "take", ok: true},
				{op: "take", ok: false, wait: 0},
				{op: "abort"},
				{op: "take", ok: true},
			},
		},
	}

	base := time.Unix(1_700_000_000, 0)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var st state
			for i, sp := range tt.steps {
				now := base.Add(sp.at)
				switch sp.op {
				case "take":
					_, wait, ok := st.take(now, tt.concurrent)
					if ok != sp.ok || wait != sp.wait {
						t.Fatalf("step %d take: got ok=%v wait=%v, want ok=%v wait=%v", i, ok, wait, sp.ok, sp.wait)
					}
				case "apply":
					st.apply(sp.seq, sp.h, now)
				case "merge":
					st.merge(sp.h, now)
				case "abort":
					st.abort()
				}
			}
		})
	}
}

func TestGlobalWindow(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	w := NewWindow(3)

	for i := range 3 {
		if _, ok := w.tryTake(base.Add(time.Duration(i) * 100 * time.Millisecond)); !ok {
			t.Fatalf("slot %d should be free", i)
		}
	}
	// A plain one second window would allow this; the margin keeps it closed.
	if wait, ok := w.tryTake(base.Add(time.Second)); ok || wait != 100*time.Millisecond {
		t.Fatalf("got ok=%v wait=%v, want closed for 100ms", ok, wait)
	}
	if _, ok := w.tryTake(base.Add(globalPeriod)); !ok {
		t.Fatal("first slot should be free again after the period")
	}

	// Leased slots count until the end of their validity.
	l := NewWindow(2)
	if n, valid, _ := l.Grant(base, 5); n != 2 || valid != leaseValid {
		t.Fatalf("grant: got %d valid %v, want 2 valid %v", n, valid, leaseValid)
	}
	if n, retry, _ := l.Grant(base.Add(globalPeriod), 1); n != 0 || retry != leaseValid {
		t.Fatalf("grant after one period: got %d retry %v, want 0 retry %v", n, retry, leaseValid)
	}
	if n, _, _ := l.Grant(base.Add(globalPeriod+leaseValid), 1); n != 1 {
		t.Fatalf("grant after period plus validity: got %d, want 1", n)
	}
}
