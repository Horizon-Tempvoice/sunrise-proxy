package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/cluster"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/guard"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/logging"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/metrics"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/ratelimit"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/route"
)

const (
	DefaultUpstream = "https://discord.com"
	internalPrefix  = "/_sunrise/"
	recentKeys      = 10_000
)

type Options struct {
	Upstream           string
	RequestTimeout     time.Duration
	OutboundIP         string
	DisableHTTP2       bool
	Concurrent         bool
	MaxBearers         int
	GlobalLimit        int
	Overrides          map[string]int
	Disable401Lock     bool
	TokenLockDuration  time.Duration
	Webhook404Duration time.Duration
	WarnThresholds     []int
	HardBrake          bool
	HardBrakeAt        int
	Clustered          bool
}

// GlobalLimiter enforces a bot's global limit, locally or across the cluster.
type GlobalLimiter interface {
	Take(ctx context.Context, key string, limit int) error
	Pause(key string, limit int, until time.Time)
}

type localGlobal struct{ g *ratelimit.Globals }

func (l localGlobal) Take(ctx context.Context, key string, limit int) error {
	return l.g.Window(key, limit).Take(ctx)
}

func (l localGlobal) Pause(key string, limit int, until time.Time) {
	l.g.Window(key, limit).Pause(until)
}

type clusterState struct {
	node   *cluster.Node
	signer *cluster.Signer
	leases *cluster.Leases
	client *http.Client
}

type Server struct {
	opts     Options
	log      *slog.Logger
	rlLog    *slog.Logger
	guardLog *slog.Logger
	clLog    *slog.Logger
	m        *metrics.Metrics

	Registry *ratelimit.Registry
	Globals  *ratelimit.Globals
	Invalid  *guard.Invalid
	Locks    *guard.Locks
	Webhooks *guard.Webhooks

	local    localGlobal
	upstream *http.Client
	cluster  atomic.Pointer[clusterState]
	recent   *lru.Cache[string, struct{}]
}

func New(opts Options, log *slog.Logger) *Server {
	if opts.Upstream == "" {
		opts.Upstream = DefaultUpstream
	}
	globals := ratelimit.NewGlobals(opts.GlobalLimit, opts.Overrides)
	recent, _ := lru.New[string, struct{}](recentKeys)
	guardLog := log.With("component", "guard")
	return &Server{
		opts:     opts,
		log:      log.With("component", "proxy"),
		rlLog:    log.With("component", "ratelimit"),
		guardLog: guardLog,
		clLog:    log.With("component", "cluster"),
		Registry: ratelimit.NewRegistry(opts.Concurrent, opts.MaxBearers, opts.GlobalLimit),
		Globals:  globals,
		Invalid:  guard.NewInvalid(opts.WarnThresholds, opts.HardBrake, opts.HardBrakeAt, guardLog),
		Locks:    guard.NewLocks(opts.TokenLockDuration),
		Webhooks: guard.NewWebhooks(opts.Webhook404Duration),
		local:    localGlobal{g: globals},
		upstream: newUpstreamClient(opts.OutboundIP, opts.DisableHTTP2),
		recent:   recent,
	}
}

func (s *Server) SetMetrics(m *metrics.Metrics) {
	s.m = m
}

// AttachCluster switches the server to cluster mode once the node has joined.
func (s *Server) AttachCluster(node *cluster.Node, signer *cluster.Signer) {
	client := newClusterClient()
	s.cluster.Store(&clusterState{
		node:   node,
		signer: signer,
		leases: cluster.NewLeases(node, signer, client, s.Globals, s.m, s.clLog),
		client: client,
	})
}

// LogBoot describes what the server has set up, for the startup log.
func (s *Server) LogBoot() {
	if len(s.opts.Overrides) == 0 {
		s.rlLog.Info("no bot overrides, every bot uses the default global limit", "per_second", s.opts.GlobalLimit)
	} else {
		ids := make([]string, 0, len(s.opts.Overrides))
		for id := range s.opts.Overrides {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			s.rlLog.Info("global limit override loaded", "bot", id, "per_second", s.opts.Overrides[id])
		}
	}
	s.rlLog.Info("bucket registry ready",
		"bucket_identity", "discord bucket hash + major parameter",
		"concurrent_requests", s.opts.Concurrent,
		"bearer_lru_size", s.opts.MaxBearers,
		"idle_bucket_ttl", "10m")
	mode := "local, fixed 1s windows"
	if s.opts.Clustered {
		mode = "cluster, owner node leases slots per window"
	}
	s.rlLog.Info("global limit ready", "mode", mode, "default_per_second", s.opts.GlobalLimit,
		"not_counted", "tokenless requests, interaction endpoints", "bearer", "always local")

	protocols := "HTTP/1.1"
	if !s.opts.DisableHTTP2 {
		protocols = "HTTP/1.1, HTTP/2"
	}
	outbound := s.opts.OutboundIP
	if outbound == "" {
		outbound = "system default"
	}
	s.log.Info("upstream client ready", "target", s.opts.Upstream, "protocols", protocols,
		"outbound_ip", outbound, "timeout", s.opts.RequestTimeout,
		"compression", "passed through", "redirects", "passed through", "buffered_body_max", "1MiB")

	brake := "off"
	if s.opts.HardBrake {
		brake = fmt.Sprintf("at %d", s.opts.HardBrakeAt)
	}
	lock := "until restart"
	switch {
	case s.opts.Disable401Lock:
		lock = "disabled"
	case s.opts.TokenLockDuration > 0:
		lock = s.opts.TokenLockDuration.String()
	}
	s.guardLog.Info("cloudflare protection ready",
		"window", guard.InvalidWindow, "ban_at", guard.InvalidLimit, "warn_at", fmt.Sprint(s.opts.WarnThresholds),
		"hard_brake", brake, "token_lock_after_401", lock, "webhook_404_for", s.opts.Webhook404Duration)
}

func (s *Server) Run(ctx context.Context) {
	go s.Registry.Run(ctx)
	go s.Invalid.Run(ctx)
}

// MembershipChanged counts how many recently used routing keys now have another owner.
func (s *Server) MembershipChanged(old, cur *cluster.Ring) {
	if old.Len() == 0 {
		return
	}
	moved, fromDeparted := 0, 0
	keys := s.recent.Keys()
	for _, k := range keys {
		a, _ := old.Owner(k)
		b, _ := cur.Owner(k)
		if a.Name != b.Name {
			moved++
			if !cur.Has(a.Name) {
				fromDeparted++
			}
		}
	}
	if moved > 0 {
		s.m.OwnerChanges.Add(float64(moved))
	}
	s.clLog.Info("membership changed",
		"members", cur.Len(), "tracked_keys", len(keys), "moved", moved, "moved_from_departed", fromDeparted)
}

func (s *Server) global() GlobalLimiter {
	if cs := s.cluster.Load(); cs != nil {
		return cs.leases
	}
	return s.local
}

func routingKey(id route.Identity, rt route.Route) string {
	switch id.Kind {
	case route.KindBearer:
		return "bearer|" + id.TokenKey
	case route.KindBot:
		if rt.Major != "" {
			return id.GlobalKey() + "|" + rt.Major
		}
		return id.GlobalKey() + "|" + rt.Key
	}
	if rt.Major != "" {
		return "none|" + rt.Major
	}
	return "none|" + rt.Key
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method == http.MethodConnect {
		s.log.Warn("rejected CONNECT request, point the client at Sunrise as its base URL instead of an HTTPS proxy",
			"remote", r.RemoteAddr, "target", r.Host)
		s.generated(w, http.StatusMethodNotAllowed, "connect-not-supported",
			discordError{Message: "CONNECT is not supported. Use Sunrise as the API base URL."})
		return
	}
	if strings.HasPrefix(r.URL.Path, internalPrefix) {
		s.serveInternal(w, r)
		return
	}
	if route.HasTraversal(r.URL.Path) {
		s.log.Warn("rejected path traversal", "remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
		s.generated(w, http.StatusBadRequest, "path-traversal", discordError{Message: "Path traversal is not allowed."})
		return
	}

	cs := s.cluster.Load()
	forwardedBy := ""
	if cluster.HasSunriseHeaders(r.Header) {
		if cs == nil {
			s.rejectSignature(w, r, "", "unexpected")
			return
		}
		node, reason, ok := cs.signer.Verify(r.Header, r.Method, requestTarget(r.URL), start)
		if !ok {
			s.rejectSignature(w, r, node, reason)
			return
		}
		forwardedBy = node
		cluster.StripSunriseHeaders(r.Header)
		s.m.RoutedReceived.Inc()
	}

	rt := route.Parse(r.Method, r.URL.Path, start)
	id := route.Identify(r.Header.Get("Authorization"))

	conns := s.m.OpenConns.WithLabelValues(r.Method, rt.Metric)
	conns.Inc()
	defer conns.Dec()

	if s.Invalid.Braking() {
		s.m.HardBrake.Inc()
		s.tooMany(w, "hard-brake")
		return
	}
	if id.Kind != route.KindNone && !rt.PathToken && s.Locks.Locked(id.TokenKey, start) {
		s.generated(w, http.StatusUnauthorized, "token-locked", discordError{Message: "401: Unauthorized"})
		return
	}
	if rt.Webhook != "" && s.Webhooks.Dead(rt.Webhook) {
		s.generated(w, http.StatusNotFound, "webhook-404", discordError{Message: "Unknown Webhook", Code: 10015})
		return
	}

	if s.opts.Clustered && cs == nil {
		s.tooMany(w, "cluster-joining")
		return
	}
	// Callbacks are used once per interaction and are not bound to the global limit.
	// GET /gateway needs no token, and tokenless limits apply per IP.
	if cs != nil && forwardedBy == "" && !rt.Callback && !rt.Gateway {
		key := routingKey(id, rt)
		s.recent.Add(key, struct{}{})
		if owner, self := cs.node.Owner(key); !self {
			s.forward(w, r, cs, owner, rt)
			return
		}
	}
	s.handle(w, r, rt, id, forwardedBy, start)
}

func (s *Server) rejectSignature(w http.ResponseWriter, r *http.Request, node, reason string) {
	s.m.SignatureReject.WithLabelValues(reason).Inc()
	s.clLog.Warn("rejected request with a bad cluster signature",
		"remote", r.RemoteAddr, "claimed_node", node, "reason", reason, "method", r.Method, "path", r.URL.Path)
	s.generated(w, http.StatusUnauthorized, "invalid-signature", discordError{Message: "Invalid cluster signature."})
}

func (s *Server) serveInternal(w http.ResponseWriter, r *http.Request) {
	cs := s.cluster.Load()
	if cs == nil {
		s.generated(w, http.StatusNotFound, "unknown-endpoint", discordError{Message: "Not found."})
		return
	}
	if _, reason, ok := cs.signer.Verify(r.Header, r.Method, requestTarget(r.URL), time.Now()); !ok {
		s.rejectSignature(w, r, r.Header.Get(cluster.HeaderNode), reason)
		return
	}
	if r.URL.Path == cluster.LeasePath && r.Method == http.MethodPost {
		cs.leases.ServeLease(w, r)
		return
	}
	s.generated(w, http.StatusNotFound, "unknown-endpoint", discordError{Message: "Not found."})
}

func needsGlobal(id route.Identity, rt route.Route) bool {
	return id.Kind != route.KindNone && !rt.Interaction
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request, rt route.Route, id route.Identity, forwardedBy string, start time.Time) {
	ctx := r.Context()
	body, length, err := readBody(r)
	if err != nil {
		s.log.Debug("could not read request body", "remote", r.RemoteAddr, "error", err)
		s.m.Errors.WithLabelValues("request_body").Inc()
		return
	}

	client := s.Registry.Client(id)
	waitStart := time.Now()
	s.m.Waiting.WithLabelValues("bucket").Inc()
	slot, err := client.Acquire(ctx, rt.Key, rt.Major)
	s.m.Waiting.WithLabelValues("bucket").Dec()
	s.m.WaitTime.WithLabelValues("bucket").Observe(time.Since(waitStart).Seconds())
	if err != nil {
		s.log.Debug("client left while waiting for a bucket slot", "route", rt.Metric, "waited", time.Since(waitStart))
		return
	}

	if needsGlobal(id, rt) {
		globalStart := time.Now()
		s.m.Waiting.WithLabelValues("global").Inc()
		if client.Global != nil {
			err = client.Global.Take(ctx)
		} else {
			err = s.global().Take(ctx, id.GlobalKey(), s.Globals.Limit(id.BotID))
		}
		s.m.Waiting.WithLabelValues("global").Dec()
		s.m.WaitTime.WithLabelValues("global").Observe(time.Since(globalStart).Seconds())
		if err != nil {
			slot.Abort()
			s.log.Debug("client left while waiting for the global limit", "route", rt.Metric, "bot", id.Label())
			return
		}
	}

	upCtx, cancel := context.WithTimeout(ctx, s.opts.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(upCtx, r.Method, s.opts.Upstream+requestTarget(r.URL), body)
	if err != nil {
		slot.Abort()
		s.m.Errors.WithLabelValues("build_request").Inc()
		s.generated(w, http.StatusBadRequest, "bad-request", discordError{Message: "Could not build the request."})
		return
	}
	copyHeaders(req.Header, r.Header)
	req.ContentLength = length

	sent := time.Now()
	resp, err := s.upstream.Do(req)
	if err != nil {
		slot.Abort()
		switch {
		case ctx.Err() != nil:
			s.log.Debug("client left while Discord was answering", "route", rt.Metric)
		case errors.Is(upCtx.Err(), context.DeadlineExceeded):
			s.m.Errors.WithLabelValues("upstream_timeout").Inc()
			s.log.Warn("Discord did not answer within the request timeout", "method", r.Method, "route", rt.Metric,
				"bot", id.Label(), "timeout", s.opts.RequestTimeout)
			s.generated(w, http.StatusRequestTimeout, "upstream-timeout", discordError{Message: "Discord did not answer in time."})
		default:
			s.m.Errors.WithLabelValues("upstream").Inc()
			s.log.Warn("request to Discord failed", "method", r.Method, "route", rt.Metric, "bot", id.Label(), "error", err)
			s.generated(w, http.StatusBadGateway, "upstream-error", discordError{Message: "Could not reach Discord."})
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()

	h := ratelimit.ParseHeaders(resp.StatusCode, resp.Header)
	if slot.Finish(h) {
		s.m.Remaps.Inc()
		s.rlLog.Debug("route moved onto a shared bucket", "route", rt.Metric, "method", r.Method, "bucket", h.Bucket, "bot", id.Label())
	}
	s.observe(r, rt, id, h, time.Since(sent))

	if err := streamResponse(w, resp); err != nil {
		s.log.Debug("could not finish writing the response", "route", rt.Metric, "error", err)
	}
	s.log.Log(ctx, logging.LevelTrace, "request done",
		"method", r.Method, "route", rt.Metric, "status", resp.StatusCode, "bot", id.Label(),
		"bucket", h.Bucket, "remaining", h.Remaining, "via", forwardedOrLocal(forwardedBy), "took", time.Since(start))
}

func forwardedOrLocal(node string) string {
	if node == "" {
		return "local"
	}
	return node
}

// observe records metrics and runs the Cloudflare protections for one Discord response.
func (s *Server) observe(r *http.Request, rt route.Route, id route.Identity, h ratelimit.Headers, took time.Duration) {
	status := h.Status
	s.m.Requests.WithLabelValues(r.Method, statusLabel(status, h.Scope), rt.Metric, id.Label()).Observe(took.Seconds())

	invalid := status == http.StatusUnauthorized || status == http.StatusForbidden ||
		(status == http.StatusTooManyRequests && h.Scope != ratelimit.ScopeShared)
	if invalid {
		s.Invalid.Add(time.Now())
		scope := h.Scope
		if scope == "" {
			scope = "none"
		}
		s.m.InvalidResponses.WithLabelValues(statusLabel(status, ""), scope).Inc()
	}

	if h.IsRatelimited() {
		switch h.Scope {
		case ratelimit.ScopeGlobal:
			until := time.Now().Add(h.Pause())
			switch id.Kind {
			case route.KindBearer:
				s.Registry.Client(id).Global.Pause(until)
			case route.KindBot:
				s.global().Pause(id.GlobalKey(), s.Globals.Limit(id.BotID), until)
			}
			s.rlLog.Warn("global ratelimit hit, pausing the bot", "bot", id.Label(), "route", rt.Metric, "retry_after", h.Pause())
		case ratelimit.ScopeShared:
			s.rlLog.Debug("shared resource ratelimit hit", "bot", id.Label(), "route", rt.Metric, "bucket", h.Bucket, "retry_after", h.Pause())
		default:
			s.rlLog.Warn("unexpected 429", "bot", id.Label(), "method", r.Method, "route", rt.Metric,
				"bucket", h.Bucket, "scope", orNone(h.Scope), "retry_after", h.Pause())
		}
	}

	if status == http.StatusUnauthorized && id.Kind != route.KindNone && !rt.PathToken {
		if s.opts.Disable401Lock {
			s.guardLog.Error("received 401 during normal operation, the token is probably invalid (lock disabled)",
				"bot", id.Label(), "route", rt.Metric)
		} else if s.Locks.Lock(id.TokenKey, time.Now()) {
			lock := "until restart"
			if s.opts.TokenLockDuration > 0 {
				lock = s.opts.TokenLockDuration.String()
			}
			s.guardLog.Error("received 401 during normal operation, locking the token and answering 401 locally",
				"bot", id.Label(), "route", rt.Metric, "lock", lock)
		}
	}

	if status == http.StatusNotFound && rt.Webhook != "" && s.Webhooks.Mark(rt.Webhook) {
		s.guardLog.Info("webhook returned 404, answering 404 locally for it",
			"webhook", rt.Webhook, "for", s.opts.Webhook404Duration)
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
