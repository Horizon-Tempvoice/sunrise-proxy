package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxBufferedBody = 1 << 20

var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func copyHeaders(dst, src http.Header) {
	drop := map[string]bool{}
	for _, v := range src.Values("Connection") {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				drop[http.CanonicalHeaderKey(f)] = true
			}
		}
	}
	for _, h := range hopByHop {
		drop[h] = true
	}
	for k, vv := range src {
		if drop[http.CanonicalHeaderKey(k)] {
			continue
		}
		dst[k] = append([]string(nil), vv...)
	}
}

func requestTarget(u *url.URL) string {
	t := u.EscapedPath()
	if u.RawQuery != "" {
		t += "?" + u.RawQuery
	}
	return t
}

// readBody reads small bodies completely before the request waits in a queue. The
// server only notices a client disconnect once the body is consumed, so this is what
// lets a waiting request leave the queue when its client goes away.
func readBody(r *http.Request) (io.Reader, int64, error) {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return http.NoBody, 0, nil
	}
	if r.ContentLength > maxBufferedBody {
		return r.Body, r.ContentLength, nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxBufferedBody+1))
	if err != nil {
		return nil, 0, err
	}
	if len(buf) <= maxBufferedBody {
		if len(buf) == 0 {
			return http.NoBody, 0, nil
		}
		return bytes.NewReader(buf), int64(len(buf)), nil
	}
	return io.MultiReader(bytes.NewReader(buf), r.Body), r.ContentLength, nil
}

func streamResponse(w http.ResponseWriter, resp *http.Response) error {
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, err := io.Copy(w, resp.Body)
	return err
}

type discordError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

type ratelimitBody struct {
	Message    string  `json:"message"`
	RetryAfter float64 `json:"retry_after"`
	Global     bool    `json:"global"`
}

func (s *Server) generated(w http.ResponseWriter, status int, reason string, body any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Sunrise-Generated", "true")
	h.Set("X-Sunrise-Reason", reason)
	if status == http.StatusTooManyRequests {
		h.Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
	s.m.Generated.WithLabelValues(reason).Inc()
}

func (s *Server) tooMany(w http.ResponseWriter, reason string) {
	s.generated(w, http.StatusTooManyRequests, reason, ratelimitBody{Message: "You are being rate limited.", RetryAfter: 1})
}

// statusLabel writes statuses as "404 Not Found", and shared scope 429s as "429 Shared",
// so they do not count toward the invalid request limit in queries.
func statusLabel(status int, scope string) string {
	if status == http.StatusTooManyRequests && scope == "shared" {
		return "429 Shared"
	}
	if text := http.StatusText(status); text != "" {
		return strconv.Itoa(status) + " " + text
	}
	return strconv.Itoa(status)
}

func newUpstreamClient(outboundIP string, disableHTTP2 bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if outboundIP != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(outboundIP)}
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	if !disableHTTP2 {
		protocols.SetHTTP2(true)
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			Protocols:             protocols,
			TLSHandshakeTimeout:   10 * time.Second,
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
			DisableCompression:    true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newClusterClient() *http.Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
