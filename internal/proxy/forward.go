package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/cluster"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/logging"
	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/route"
)

// forward hands the request to the node that owns its bucket. The owner never
// forwards it again because the signature marks it as already routed.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, cs *clusterState, owner cluster.Member, rt route.Route) {
	ctx := r.Context()
	body, length, err := readBody(r)
	if err != nil {
		s.m.Errors.WithLabelValues("request_body").Inc()
		return
	}
	target := requestTarget(r.URL)
	req, err := http.NewRequestWithContext(ctx, r.Method, "http://"+owner.Addr+target, body)
	if err != nil {
		s.m.Errors.WithLabelValues("build_request").Inc()
		s.tooMany(w, "owner-unreachable")
		return
	}
	copyHeaders(req.Header, r.Header)
	cluster.StripSunriseHeaders(req.Header)
	req.ContentLength = length
	cs.signer.Sign(req.Header, r.Method, target, time.Now())

	start := time.Now()
	resp, err := cs.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			s.log.Debug("client left while the owner was handling its request", "owner", owner.Name, "route", rt.Metric)
			return
		}
		reason := forwardReason(err)
		s.m.RoutedFailed.Inc()
		s.m.ForwardErrors.WithLabelValues(reason).Inc()
		s.clLog.Warn("owner unreachable, answering with a generated 429 so the client retries",
			"owner", owner.Name, "address", owner.Addr, "route", rt.Metric, "reason", reason, "error", err)
		s.tooMany(w, "owner-unreachable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	s.m.RoutedSent.Inc()
	s.m.ForwardTime.Observe(time.Since(start).Seconds())

	if err := streamResponse(w, resp); err != nil {
		s.log.Debug("could not finish writing the forwarded response", "owner", owner.Name, "error", err)
	}
	s.log.Log(ctx, logging.LevelTrace, "forwarded to owner",
		"method", r.Method, "route", rt.Metric, "owner", owner.Name, "status", resp.StatusCode, "took", time.Since(start))
}

func forwardReason(err error) string {
	var opErr *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "connect"
	}
	return "other"
}
