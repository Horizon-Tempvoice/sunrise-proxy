package ratelimit

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	ScopeUser   = "user"
	ScopeGlobal = "global"
	ScopeShared = "shared"
)

// Headers is what one Discord response tells us about its ratelimit.
type Headers struct {
	Status     int
	Bucket     string
	HasLimits  bool
	Limit      int
	Remaining  int
	ResetAfter time.Duration
	Scope      string
	RetryAfter time.Duration
}

func ParseHeaders(status int, h http.Header) Headers {
	out := Headers{
		Status: status,
		Bucket: h.Get("X-RateLimit-Bucket"),
		Scope:  strings.ToLower(h.Get("X-RateLimit-Scope")),
	}
	if status == http.StatusTooManyRequests && out.Scope == "" && strings.EqualFold(h.Get("X-RateLimit-Global"), "true") {
		out.Scope = ScopeGlobal
	}
	out.RetryAfter = seconds(h.Get("Retry-After"))
	out.ResetAfter = seconds(h.Get("X-RateLimit-Reset-After"))

	limit, errL := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	remaining, errR := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if errL == nil && errR == nil && limit > 0 && remaining >= 0 {
		out.HasLimits = true
		out.Limit = limit
		out.Remaining = remaining
	}
	return out
}

// IsRatelimited reports a 429 that should pause something on our side.
func (h Headers) IsRatelimited() bool {
	return h.Status == http.StatusTooManyRequests
}

// Pause is how long a 429 asks us to wait, falling back to one second.
func (h Headers) Pause() time.Duration {
	d := h.RetryAfter
	if d <= 0 {
		d = h.ResetAfter
	}
	if d <= 0 {
		d = time.Second
	}
	return d
}

func seconds(v string) time.Duration {
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}
