package ratelimit

import (
	"context"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/route"
)

const bucketIdleTTL = 10 * time.Minute

// Client holds the buckets of one token (or of all tokenless requests).
type Client struct {
	mu         sync.Mutex
	routes     map[string]*Bucket
	learned    map[string]*Bucket
	concurrent bool
	// Global is set for Bearer clients, whose global limit is always handled locally.
	Global *Window
}

func newClient(concurrent bool) *Client {
	return &Client{routes: map[string]*Bucket{}, learned: map[string]*Bucket{}, concurrent: concurrent}
}

// Slot is one granted request. Finish or Abort must be called exactly once.
type Slot struct {
	client   *Client
	bucket   *Bucket
	seq      uint64
	routeKey string
	major    string
}

// Acquire waits in the route's bucket queue until a request may go.
func (c *Client) Acquire(ctx context.Context, routeKey, major string) (*Slot, error) {
	b := c.bucket(routeKey)
	for {
		seq, next, err := b.acquire(ctx)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return &Slot{client: c, bucket: b, seq: seq, routeKey: routeKey, major: major}, nil
		}
		b = next
	}
}

func (c *Client) bucket(routeKey string) *Bucket {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.routes[routeKey]
	if !ok {
		b = newBucket(c.concurrent, time.Now())
		c.routes[routeKey] = b
	}
	return b
}

// Finish applies the response headers and reports whether the route moved onto a
// bucket it shares with other routes.
func (s *Slot) Finish(h Headers) (remapped bool) {
	s.bucket.release(s.seq, h)
	if h.Bucket == "" {
		return false
	}
	return s.client.learn(s.routeKey, s.major, s.bucket, h)
}

func (s *Slot) Abort() {
	s.bucket.abort()
}

// learn ties a route to Discord's bucket. A bucket is Discord's hash plus the major
// parameter, because the same hash is reused across channels and guilds.
func (c *Client) learn(routeKey, major string, b *Bucket, h Headers) bool {
	id := h.Bucket + "|" + major
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.routes[routeKey] != b {
		return false
	}
	existing := c.learned[id]
	switch {
	case existing == b:
		return false
	case existing == nil && b.learnedID == "":
		b.learnedID = id
		c.learned[id] = b
		return false
	case existing == nil:
		nb := newBucket(c.concurrent, time.Now())
		nb.learnedID = id
		nb.merge(h)
		c.learned[id] = nb
		c.routes[routeKey] = nb
		return true
	}
	existing.merge(h)
	c.routes[routeKey] = existing
	if b.learnedID == "" {
		b.redirectTo(existing)
	}
	return true
}

func (c *Client) sweep(now time.Time, prune bool) (live, maxDepth int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[*Bucket]struct{}{}
	for k, b := range c.routes {
		if prune && b.idle(now, bucketIdleTTL) {
			delete(c.routes, k)
			continue
		}
		seen[b] = struct{}{}
	}
	for k, b := range c.learned {
		if _, used := seen[b]; !used && prune && b.idle(now, bucketIdleTTL) {
			delete(c.learned, k)
			continue
		}
		seen[b] = struct{}{}
	}
	for b := range seen {
		if d := b.depth(); d > maxDepth {
			maxDepth = d
		}
	}
	return len(seen), maxDepth
}

// Registry holds every client: bots and tokenless requests in a map, Bearer tokens in an LRU.
type Registry struct {
	mu         sync.Mutex
	clients    map[string]*Client
	bearers    *lru.Cache[string, *Client]
	concurrent bool
	bearerRate int
}

func NewRegistry(concurrent bool, maxBearers, bearerRate int) *Registry {
	bearers, err := lru.New[string, *Client](maxBearers)
	if err != nil {
		panic(err)
	}
	return &Registry{
		clients:    map[string]*Client{},
		bearers:    bearers,
		concurrent: concurrent,
		bearerRate: bearerRate,
	}
}

func (r *Registry) Client(id route.Identity) *Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id.Kind == route.KindBearer {
		c, ok := r.bearers.Get(id.TokenKey)
		if !ok {
			c = newClient(r.concurrent)
			c.Global = NewWindow(r.bearerRate)
			r.bearers.Add(id.TokenKey, c)
		}
		return c
	}
	key := "none"
	if id.Kind == route.KindBot {
		key = id.TokenKey
	}
	c, ok := r.clients[key]
	if !ok {
		c = newClient(r.concurrent)
		r.clients[key] = c
	}
	return c
}

// Run drops idle buckets until ctx ends.
func (r *Registry) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, c := range r.all() {
				c.sweep(now, true)
			}
		}
	}
}

// Stats counts live buckets and the longest queue, for the metrics endpoint.
func (r *Registry) Stats() (buckets, maxDepth int) {
	for _, c := range r.all() {
		live, depth := c.sweep(time.Now(), false)
		buckets += live
		if depth > maxDepth {
			maxDepth = depth
		}
	}
	return buckets, maxDepth
}

func (r *Registry) all() []*Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Client, 0, len(r.clients)+r.bearers.Len())
	for _, c := range r.clients {
		out = append(out, c)
	}
	for _, k := range r.bearers.Keys() {
		if c, ok := r.bearers.Peek(k); ok {
			out = append(out, c)
		}
	}
	return out
}
