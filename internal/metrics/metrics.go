package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const ns = "sunrise"

// Sources lets the metrics read live values from other packages at scrape time.
type Sources struct {
	Buckets       func() (live, maxDepth int)
	InvalidWindow func() int
	LockedTokens  func() int
	DeadWebhooks  func() int
	Members       func() int
}

type Metrics struct {
	Registry *prometheus.Registry

	Errors         *prometheus.CounterVec
	Requests       *prometheus.HistogramVec
	Generated      *prometheus.CounterVec
	OpenConns      *prometheus.GaugeVec
	RoutedSent     prometheus.Counter
	RoutedReceived prometheus.Counter
	RoutedFailed   prometheus.Counter

	Waiting  *prometheus.GaugeVec
	WaitTime *prometheus.HistogramVec
	Remaps   prometheus.Counter

	ForwardTime     prometheus.Histogram
	ForwardErrors   *prometheus.CounterVec
	SignatureReject *prometheus.CounterVec
	OwnerChanges    prometheus.Counter
	LeaseRequests   *prometheus.CounterVec
	LeaseFallbacks  prometheus.Counter

	InvalidResponses *prometheus.CounterVec
	HardBrake        prometheus.Counter
}

func New(version, commit, goVersion string, src Sources) *Metrics {
	reg := prometheus.NewRegistry()
	// The prefixed copy lets dashboards pick Sunrise's own memory and CPU even when
	// several services share one job and instance label.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{Namespace: ns}),
	)

	durations := []float64{.025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20}
	m := &Metrics{
		Registry: reg,
		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "errors_total",
			Help: "Errors while handling requests, by kind.",
		}, []string{"kind"}),
		Requests: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "requests_seconds",
			Help:    "Requests sent to Discord and how long Discord took to answer. Status is the HTTP code, or 429_shared for shared scope 429s.",
			Buckets: durations,
		}, []string{"method", "status", "route", "bot"}),
		Generated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "generated_responses_total",
			Help: "Responses Sunrise answered itself instead of asking Discord, by reason.",
		}, []string{"reason"}),
		OpenConns: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Name: "open_connections",
			Help: "Client requests currently open on this node.",
		}, []string{"method", "route"}),
		RoutedSent: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "routed_sent_total",
			Help: "Requests this node forwarded to the owning node.",
		}),
		RoutedReceived: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "routed_received_total",
			Help: "Requests this node received from other nodes.",
		}),
		RoutedFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "routed_failed_total",
			Help: "Requests this node failed to forward to the owning node.",
		}),
		Waiting: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Name: "requests_waiting",
			Help: "Requests currently waiting for a ratelimit slot. Stage is bucket or global.",
		}, []string{"stage"}),
		WaitTime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Name: "ratelimit_wait_seconds",
			Help:    "Time requests spent waiting for a ratelimit slot. Stage is bucket or global.",
			Buckets: []float64{.001, .01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"stage"}),
		Remaps: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "bucket_remaps_total",
			Help: "Routes that were moved onto a bucket learned from Discord's bucket header.",
		}),
		ForwardTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: ns, Name: "forward_seconds",
			Help:    "Time from forwarding a request to the owning node until its response headers arrived, waiting included.",
			Buckets: durations,
		}),
		ForwardErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "forward_errors_total",
			Help: "Failed forwards to the owning node, by reason.",
		}, []string{"reason"}),
		SignatureReject: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "signature_rejected_total",
			Help: "Cluster requests rejected because of a missing, stale or invalid signature.",
		}, []string{"reason"}),
		OwnerChanges: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "owner_changes_total",
			Help: "Recently used routing keys that moved to another node after a membership change.",
		}),
		LeaseRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "global_lease_requests_total",
			Help: "Requests for global ratelimit tokens sent to the owning node, by result.",
		}, []string{"result"}),
		LeaseFallbacks: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "global_lease_fallbacks_total",
			Help: "Times this node used its local share of the global limit because the owner was unreachable.",
		}),
		InvalidResponses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Name: "invalid_responses_total",
			Help: "Discord responses that count toward the invalid request limit, by status and scope.",
		}, []string{"status", "scope"}),
		HardBrake: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "hard_brake_total",
			Help: "Requests answered with a generated 429 by the hard brake.",
		}),
	}

	reg.MustRegister(
		m.Errors, m.Requests, m.Generated, m.OpenConns, m.RoutedSent, m.RoutedReceived, m.RoutedFailed,
		m.Waiting, m.WaitTime, m.Remaps,
		m.ForwardTime, m.ForwardErrors, m.SignatureReject, m.OwnerChanges, m.LeaseRequests, m.LeaseFallbacks,
		m.InvalidResponses, m.HardBrake,
	)

	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "build_info",
		Help: "Always 1. Labels carry the Sunrise version, commit and Go version.",
	}, []string{"version", "commit", "go_version"})
	build.WithLabelValues(version, commit, goVersion).Set(1)
	reg.MustRegister(build)

	gauge := func(name, help string, f func() float64) {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: ns, Name: name, Help: help}, f))
	}
	if src.Buckets != nil {
		gauge("buckets", "Ratelimit buckets currently tracked on this node.", func() float64 {
			n, _ := src.Buckets()
			return float64(n)
		})
		gauge("bucket_queue_depth_max", "Length of the longest bucket queue on this node.", func() float64 {
			_, d := src.Buckets()
			return float64(d)
		})
	}
	if src.InvalidWindow != nil {
		gauge("invalid_requests_window", "Invalid responses (401, 403, non-shared 429) this node received in the last 10 minutes. Discord bans the IP at 10000.", func() float64 {
			return float64(src.InvalidWindow())
		})
	}
	if src.LockedTokens != nil {
		gauge("locked_tokens", "Tokens locked after a 401, answered locally with 401.", func() float64 {
			return float64(src.LockedTokens())
		})
	}
	if src.DeadWebhooks != nil {
		gauge("webhook_404_cached", "Webhooks answered locally with 404 after Discord returned 404 for them.", func() float64 {
			return float64(src.DeadWebhooks())
		})
	}
	if src.Members != nil {
		gauge("cluster_members", "Nodes this node currently sees in the cluster, itself included.", func() float64 {
			return float64(src.Members())
		})
	}
	return m
}
