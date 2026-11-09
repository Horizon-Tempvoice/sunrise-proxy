package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/memberlist"

	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/logging"
)

type Options struct {
	Name          string
	BindIP        string
	Port          int
	AdvertiseAddr string
	ProxyPort     int
	Version       string
	Secret        string
	Members       []string
	DNS           string
	DNSInterval   time.Duration
	Log           *slog.Logger
	GossipLog     *slog.Logger
	// OnChange runs after every membership change, in order, outside memberlist's locks.
	OnChange func(old, new *Ring)
}

type meta struct {
	Port    int    `json:"p"`
	Version string `json:"v"`
}

type Node struct {
	opts    Options
	ml      *memberlist.Memberlist
	ring    atomic.Pointer[Ring]
	joined  atomic.Bool
	mu      sync.Mutex
	members map[string]Member
	changes chan [2]*Ring
}

func Start(opts Options) (*Node, error) {
	n := &Node{opts: opts, members: map[string]Member{}, changes: make(chan [2]*Ring, 64)}
	n.ring.Store(NewRing(nil))

	cfg := memberlist.DefaultLANConfig()
	cfg.Name = opts.Name
	cfg.BindAddr = opts.BindIP
	cfg.BindPort = opts.Port
	cfg.AdvertisePort = opts.Port
	if opts.AdvertiseAddr != "" {
		cfg.AdvertiseAddr = opts.AdvertiseAddr
	}
	cfg.SecretKey = GossipKey(opts.Secret)
	cfg.Delegate = &delegate{n: n}
	cfg.Events = &events{n: n}
	cfg.Logger = log.New(&gossipWriter{log: opts.GossipLog}, "", 0)

	ml, err := memberlist.Create(cfg)
	if err != nil {
		return nil, fmt.Errorf("starting gossip on port %d: %w", opts.Port, err)
	}
	n.ml = ml
	go n.dispatchChanges()
	opts.Log.Info("gossip started",
		"bind", net.JoinHostPort(opts.BindIP, strconv.Itoa(opts.Port)),
		"advertise", ml.LocalNode().Address(),
		"encryption", "AES-256-GCM, key derived from the cluster secret",
		"timing", "LAN profile")
	return n, nil
}

func (n *Node) Name() string {
	return n.opts.Name
}

func (n *Node) Ring() *Ring {
	return n.ring.Load()
}

func (n *Node) Joined() bool {
	return n.joined.Load()
}

// Owner returns the member that owns key and whether that member is this node.
func (n *Node) Owner(key string) (Member, bool) {
	m, ok := n.Ring().Owner(key)
	if !ok {
		return Member{}, true
	}
	return m, m.Name == n.opts.Name
}

// Join runs discovery once and joins whatever it finds. A node that finds nobody
// starts a new cluster.
func (n *Node) Join() {
	addrs := n.discover()
	mode, source := "static list", strings.Join(n.opts.Members, ",")
	if len(n.opts.Members) == 0 {
		mode, source = "dns", n.opts.DNS
		if strings.HasPrefix(n.opts.DNS, "_") {
			mode = "dns srv"
		}
	}
	n.opts.Log.Info("discovery resolved", "mode", mode, "source", source,
		"addresses", strings.Join(addrs, ","), "found", len(addrs), "repeat_every", n.opts.DNSInterval)
	defer func() {
		names := []string{}
		for _, m := range n.Ring().Members() {
			names = append(names, m.Name+"@"+m.Addr)
		}
		n.opts.Log.Info("ring ready", "members", len(names), "nodes", strings.Join(names, ","))
	}()
	if len(addrs) == 0 {
		n.opts.Log.Info("discovery found no other members, starting a new cluster")
		n.joined.Store(true)
		return
	}
	count, err := n.ml.Join(addrs)
	if err != nil && count == 0 {
		n.opts.Log.Warn("could not reach any discovered member, running alone until discovery finds one",
			"tried", strings.Join(addrs, ","), "error", err)
	} else {
		n.opts.Log.Info("joined cluster", "contacted", count, "members", n.ml.NumMembers())
	}
	n.joined.Store(true)
}

// Run re-runs discovery periodically so nodes find each other again after restarts or a partition.
func (n *Node) Run(ctx context.Context) {
	t := time.NewTicker(n.opts.DNSInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.rediscover()
		}
	}
}

func (n *Node) rediscover() {
	known := map[string]bool{}
	for _, m := range n.ml.Members() {
		known[net.JoinHostPort(m.Addr.String(), strconv.Itoa(int(m.Port)))] = true
	}
	var fresh []string
	for _, a := range n.discover() {
		if !known[a] {
			fresh = append(fresh, a)
		}
	}
	if len(fresh) == 0 {
		return
	}
	count, err := n.ml.Join(fresh)
	if count > 0 {
		n.opts.Log.Info("discovery found new members", "addresses", strings.Join(fresh, ","), "contacted", count)
	} else {
		n.opts.Log.Debug("discovery found addresses that did not answer", "addresses", strings.Join(fresh, ","), "error", err)
	}
}

// discover resolves the static member list or the DNS name into gossip addresses,
// without this node's own address.
func (n *Node) discover() []string {
	var out []string
	self := ""
	if n.ml != nil {
		ln := n.ml.LocalNode()
		self = net.JoinHostPort(ln.Addr.String(), strconv.Itoa(int(ln.Port)))
	}
	add := func(host string, port int) {
		ips, err := net.LookupIP(host)
		if err != nil {
			n.opts.Log.Debug("could not resolve member", "host", host, "error", err)
			return
		}
		for _, ip := range ips {
			a := net.JoinHostPort(ip.String(), strconv.Itoa(port))
			if a != self {
				out = append(out, a)
			}
		}
	}

	switch {
	case len(n.opts.Members) > 0:
		for _, m := range n.opts.Members {
			host, port := m, n.opts.Port
			if h, p, err := net.SplitHostPort(m); err == nil {
				if pn, err := strconv.Atoi(p); err == nil {
					host, port = h, pn
				}
			}
			add(host, port)
		}
	case strings.HasPrefix(n.opts.DNS, "_"):
		_, srvs, err := net.LookupSRV("", "", n.opts.DNS)
		if err != nil {
			n.opts.Log.Warn("SRV lookup failed", "name", n.opts.DNS, "error", err)
			return nil
		}
		for _, s := range srvs {
			add(strings.TrimSuffix(s.Target, "."), int(s.Port))
		}
	case n.opts.DNS != "":
		add(n.opts.DNS, n.opts.Port)
	}
	return out
}

// Leave announces the departure so other nodes take over our keys right away.
func (n *Node) Leave(timeout time.Duration) {
	if err := n.ml.Leave(timeout); err != nil {
		n.opts.Log.Warn("leave broadcast did not finish", "error", err)
	}
	if err := n.ml.Shutdown(); err != nil {
		n.opts.Log.Warn("gossip shutdown failed", "error", err)
	}
}

func (n *Node) update(node *memberlist.Node, alive bool) {
	n.mu.Lock()
	old := n.ring.Load()
	if alive {
		var md meta
		_ = json.Unmarshal(node.Meta, &md)
		n.members[node.Name] = Member{
			Name:    node.Name,
			Addr:    net.JoinHostPort(node.Addr.String(), strconv.Itoa(md.Port)),
			Version: md.Version,
		}
	} else {
		delete(n.members, node.Name)
	}
	list := make([]Member, 0, len(n.members))
	for _, m := range n.members {
		list = append(list, m)
	}
	nr := NewRing(list)
	n.ring.Store(nr)
	n.mu.Unlock()

	select {
	case n.changes <- [2]*Ring{old, nr}:
	default:
		n.opts.Log.Warn("membership change queue full, skipping owner change accounting")
	}
}

func (n *Node) dispatchChanges() {
	for c := range n.changes {
		if n.opts.OnChange != nil {
			n.opts.OnChange(c[0], c[1])
		}
	}
}

type delegate struct{ n *Node }

func (d *delegate) NodeMeta(limit int) []byte {
	b, _ := json.Marshal(meta{Port: d.n.opts.ProxyPort, Version: d.n.opts.Version})
	if len(b) > limit {
		return nil
	}
	return b
}
func (d *delegate) NotifyMsg([]byte)                           {}
func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }
func (d *delegate) LocalState(join bool) []byte                { return nil }
func (d *delegate) MergeRemoteState(buf []byte, join bool)     {}

type events struct{ n *Node }

func (e *events) NotifyJoin(node *memberlist.Node) {
	e.n.opts.Log.Info("node joined", "node", node.Name, "address", node.Address(), "proxy", proxyAddr(node), "version", version(node))
	e.n.update(node, true)
}

func (e *events) NotifyLeave(node *memberlist.Node) {
	e.n.opts.Log.Info("node left", "node", node.Name, "address", node.Address())
	e.n.update(node, false)
}

func (e *events) NotifyUpdate(node *memberlist.Node) {
	e.n.opts.Log.Debug("node updated", "node", node.Name, "address", node.Address(), "proxy", proxyAddr(node))
	e.n.update(node, true)
}

func proxyAddr(node *memberlist.Node) string {
	var md meta
	_ = json.Unmarshal(node.Meta, &md)
	return net.JoinHostPort(node.Addr.String(), strconv.Itoa(md.Port))
}

func version(node *memberlist.Node) string {
	var md meta
	_ = json.Unmarshal(node.Meta, &md)
	return md.Version
}

// gossipWriter turns memberlist's "[LEVEL] memberlist: ..." lines into slog records.
type gossipWriter struct{ log *slog.Logger }

func (w *gossipWriter) Write(p []byte) (int, error) {
	line := string(bytes.TrimSpace(p))
	level := slog.LevelDebug
	for prefix, l := range map[string]slog.Level{
		"[DEBUG]": logging.LevelTrace,
		"[INFO]":  slog.LevelDebug,
		"[WARN]":  slog.LevelWarn,
		"[ERR]":   slog.LevelError,
		"[ERROR]": slog.LevelError,
	} {
		if strings.HasPrefix(line, prefix) {
			level = l
			line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
			break
		}
	}
	line = strings.TrimPrefix(line, "memberlist: ")
	w.log.Log(context.Background(), level, line)
	return len(p), nil
}
