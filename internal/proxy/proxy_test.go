package proxy

import (
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/metrics"
)

// Built at runtime so the source holds nothing that looks like a real bot token.
var botAuth = "Bot " + base64.RawStdEncoding.EncodeToString([]byte("123456789012345678")) + ".GabcDe." + strings.Repeat("x", 38)

// fakeDiscord serves one shared bucket (limit 2 per second) for two routes of
// channel 1, and a route of channel 2 that always answers 429.
type fakeDiscord struct {
	mu          sync.Mutex
	windowStart time.Time
	used        int
	over        int
	typing      int
	auth        []string
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	h := w.Header()
	switch r.URL.Path {
	case "/api/v10/channels/1/messages", "/api/v10/channels/1/pins":
		now := time.Now()
		if now.Sub(f.windowStart) >= time.Second {
			f.windowStart, f.used = now, 0
		}
		f.used++
		reset := time.Second - now.Sub(f.windowStart)
		h.Set("X-RateLimit-Bucket", "hashA")
		h.Set("X-RateLimit-Limit", "2")
		h.Set("X-RateLimit-Remaining", strconv.Itoa(max(0, 2-f.used)))
		h.Set("X-RateLimit-Reset-After", fmt.Sprintf("%.3f", reset.Seconds()))
		if f.used > 2 {
			f.over++
			h.Set("X-RateLimit-Scope", "user")
			h.Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	case "/api/v10/channels/2/typing":
		f.typing++
		h.Set("X-RateLimit-Bucket", "hashB")
		h.Set("X-RateLimit-Limit", "5")
		h.Set("X-RateLimit-Remaining", "0")
		h.Set("X-RateLimit-Reset-After", "1")
		h.Set("X-RateLimit-Scope", "user")
		h.Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message": "You are being rate limited.", "retry_after": 1, "global": false}`)
	default:
		http.NotFound(w, r)
	}
}

func TestProxyAgainstFakeDiscord(t *testing.T) {
	fake := &fakeDiscord{}
	upstream := httptest.NewServer(fake)
	defer upstream.Close()

	srv := New(Options{
		Upstream:           upstream.URL,
		RequestTimeout:     5 * time.Second,
		DisableHTTP2:       true,
		Concurrent:         true,
		MaxBearers:         16,
		GlobalLimit:        50,
		Webhook404Duration: time.Hour,
	}, slog.New(slog.DiscardHandler))
	m := metrics.New("test", "test", "test", metrics.Sources{})
	srv.SetMetrics(m)
	front := httptest.NewServer(srv)
	defer front.Close()

	frontURL, _ := url.Parse(front.URL)
	// Absolute-form requests, the way a client configured with an HTTP proxy sends them.
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(frontURL)}}

	type result struct {
		status int
		header http.Header
		body   string
		took   time.Duration
	}
	do := func(method, path string) result {
		t.Helper()
		req, _ := http.NewRequest(method, "http://discord.invalid"+path, nil)
		req.Header.Set("Authorization", botAuth)
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return result{resp.StatusCode, resp.Header, string(body), time.Since(start)}
	}

	res := do("GET", "/api/v10/channels/1/messages")
	if res.status != http.StatusOK || res.body != `{"ok":true}` {
		t.Fatalf("first request: got %d %q", res.status, res.body)
	}

	res = do("GET", "/api/v10/channels/1/pins")
	if res.status != http.StatusOK {
		t.Fatalf("pins: got %d", res.status)
	}
	var remaps dto.Metric
	_ = m.Remaps.Write(&remaps)
	if got := remaps.GetCounter().GetValue(); got != 1 {
		t.Fatalf("expected pins to join the learned bucket of messages, remaps = %v", got)
	}

	res = do("GET", "/api/v10/channels/1/messages")
	if res.status != http.StatusOK {
		t.Fatalf("third request in a full bucket: got %d, want 200 after waiting", res.status)
	}
	if res.took < 500*time.Millisecond {
		t.Fatalf("third request did not wait for the shared bucket to reset (took %v)", res.took)
	}

	res = do("POST", "/api/v10/channels/2/typing")
	if res.status != http.StatusTooManyRequests || res.header.Get("X-Sunrise-Generated") != "" || res.header.Get("Retry-After") != "1" {
		t.Fatalf("Discord's 429 must reach the client unchanged: got %d generated=%q", res.status, res.header.Get("X-Sunrise-Generated"))
	}
	if res.body == "" {
		t.Fatal("429 body was not passed through")
	}
	res = do("POST", "/api/v10/channels/2/typing")
	if res.status != http.StatusTooManyRequests || res.took < 800*time.Millisecond {
		t.Fatalf("request after a 429 should wait for retry-after: status %d, took %v", res.status, res.took)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.over != 0 {
		t.Errorf("proxy let %d requests past the shared bucket limit", fake.over)
	}
	if fake.typing != 2 {
		t.Errorf("typing reached Discord %d times, want 2 (no retries)", fake.typing)
	}
	for _, a := range fake.auth {
		if a != botAuth {
			t.Fatalf("Authorization header was not forwarded unchanged")
		}
	}
}
