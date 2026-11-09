package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/metrics"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/ratelimit"
)

const LeasePath = "/_sunrise/v1/global/lease"

// Leases shares each bot's global limit across the cluster. The node that owns a bot
// counts its window. Other nodes take small batches of that window's slots and spend
// them locally, so a request costs no network round trip in the common case.
type Leases struct {
	node    *Node
	signer  *Signer
	client  *http.Client
	globals *ratelimit.Globals
	m       *metrics.Metrics
	log     *slog.Logger

	mu     sync.Mutex
	leases map[string]*lease
}

type lease struct {
	mu            sync.Mutex
	tokens        int
	validUntil    time.Time
	want          int
	pausedUntil   time.Time
	emptyUntil    time.Time
	fallback      *ratelimit.Window
	fallbackUntil time.Time
	fetching      chan struct{}
	failing       bool
}

type leaseRequest struct {
	Key     string `json:"key"`
	Limit   int    `json:"limit"`
	Want    int    `json:"want"`
	PauseMS int64  `json:"pause_ms,omitempty"`
}

type leaseResponse struct {
	Granted  int   `json:"granted"`
	ValidMS  int64 `json:"valid_ms"`
	PausedMS int64 `json:"paused_ms,omitempty"`
}

func NewLeases(node *Node, signer *Signer, client *http.Client, globals *ratelimit.Globals, m *metrics.Metrics, log *slog.Logger) *Leases {
	return &Leases{node: node, signer: signer, client: client, globals: globals, m: m, log: log, leases: map[string]*lease{}}
}

func ownerKey(key string) string {
	return "global|" + key
}

func (l *Leases) get(key string) *lease {
	l.mu.Lock()
	defer l.mu.Unlock()
	ls, ok := l.leases[key]
	if !ok {
		ls = &lease{fallback: ratelimit.NewWindow(1)}
		l.leases[key] = ls
	}
	return ls
}

func (l *Leases) Take(ctx context.Context, key string, limit int) error {
	owner, self := l.node.Owner(ownerKey(key))
	if self {
		return l.globals.Window(key, limit).Take(ctx)
	}
	ls := l.get(key)
	for {
		ls.mu.Lock()
		now := time.Now()
		var wait time.Duration
		switch {
		case now.Before(ls.pausedUntil):
			wait = ls.pausedUntil.Sub(now)
		case ls.tokens > 0 && now.Before(ls.validUntil):
			ls.tokens--
			ls.mu.Unlock()
			return nil
		case now.Before(ls.fallbackUntil):
			fb := ls.fallback
			ls.mu.Unlock()
			return fb.Take(ctx)
		case now.Before(ls.emptyUntil):
			wait = ls.emptyUntil.Sub(now)
		case ls.fetching != nil:
			ch := ls.fetching
			ls.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		default:
			l.fetch(ls, owner, key, limit, now)
			continue
		}
		ls.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// fetch is called with ls.mu held and returns with it released.
func (l *Leases) fetch(ls *lease, owner Member, key string, limit int, now time.Time) {
	if ls.tokens > 0 {
		ls.want = max(1, ls.want/2)
	} else if !ls.validUntil.IsZero() && now.Before(ls.validUntil.Add(50*time.Millisecond)) {
		ls.want = min(limit, ls.want*2)
	}
	if ls.want < 1 {
		ls.want = max(1, limit/(2*max(1, l.node.Ring().Len())))
	}
	want := ls.want
	done := make(chan struct{})
	ls.fetching = done
	ls.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	resp, err := l.request(ctx, owner, leaseRequest{Key: key, Limit: limit, Want: want})
	cancel()

	ls.mu.Lock()
	ls.fetching = nil
	close(done)
	now = time.Now()
	if err != nil {
		share := max(1, limit/max(1, l.node.Ring().Len()))
		ls.fallback.SetLimit(share)
		ls.fallbackUntil = now.Add(time.Second)
		ls.tokens = 0
		l.m.LeaseRequests.WithLabelValues("error").Inc()
		l.m.LeaseFallbacks.Inc()
		if !ls.failing {
			l.log.Warn("global limit owner unreachable, using local share", "owner", owner.Name, "share_per_second", share, "error", err)
		}
		ls.failing = true
		ls.mu.Unlock()
		return
	}
	if ls.failing {
		l.log.Info("global limit owner reachable again", "owner", owner.Name)
		ls.failing = false
	}
	ls.tokens = resp.Granted
	ls.validUntil = now.Add(time.Duration(resp.ValidMS) * time.Millisecond)
	if resp.PausedMS > 0 {
		ls.pausedUntil = now.Add(time.Duration(resp.PausedMS) * time.Millisecond)
	}
	if resp.Granted == 0 {
		ls.emptyUntil = ls.validUntil
		l.m.LeaseRequests.WithLabelValues("empty").Inc()
	} else {
		l.m.LeaseRequests.WithLabelValues("granted").Inc()
	}
	ls.mu.Unlock()
}

// Pause stops the bot on this node and tells the owner, which passes it on to every
// node with its next lease answer.
func (l *Leases) Pause(key string, limit int, until time.Time) {
	owner, self := l.node.Owner(ownerKey(key))
	if self {
		l.globals.Window(key, limit).Pause(until)
		return
	}
	ls := l.get(key)
	ls.mu.Lock()
	if until.After(ls.pausedUntil) {
		ls.pausedUntil = until
	}
	ls.tokens = 0
	ls.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := l.request(ctx, owner, leaseRequest{Key: key, Limit: limit, PauseMS: time.Until(until).Milliseconds()}); err != nil {
			l.log.Warn("could not tell the global limit owner about a global 429", "owner", owner.Name, "error", err)
		}
	}()
}

func (l *Leases) request(ctx context.Context, owner Member, body leaseRequest) (*leaseResponse, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+owner.Addr+LeasePath, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	l.signer.Sign(req.Header, http.MethodPost, LeasePath, time.Now())
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("owner answered %d", resp.StatusCode)
	}
	var out leaseResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding lease answer: %w", err)
	}
	return &out, nil
}

// ServeLease answers a lease request from another node. The caller has already
// checked the signature.
func (l *Leases) ServeLease(w http.ResponseWriter, r *http.Request) {
	var req leaseRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Key == "" || req.Limit < 1 {
		http.Error(w, "bad lease request", http.StatusBadRequest)
		return
	}
	win := l.globals.Window(req.Key, req.Limit)
	now := time.Now()
	if req.PauseMS > 0 {
		win.Pause(now.Add(time.Duration(req.PauseMS) * time.Millisecond))
		l.log.Warn("global 429 reported by another node, pausing bot cluster wide",
			"from", r.Header.Get(HeaderNode), "pause", time.Duration(req.PauseMS)*time.Millisecond)
	}
	granted, valid, paused := win.Grant(now, max(0, req.Want))
	out := leaseResponse{Granted: granted, ValidMS: valid.Milliseconds()}
	if paused.After(now) {
		out.PausedMS = paused.Sub(now).Milliseconds()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
