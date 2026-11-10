package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Horizon-Tempvoice/sunrise-proxy/internal/route"
)

const nameRetry = 10 * time.Minute

// botNames remembers when each bot's username may be looked up next.
type botNames struct {
	mu   sync.Mutex
	next map[string]time.Time
}

// learnName looks up a bot's username once per process, in the background, so
// dashboards can show names next to bot ids. It costs one request per bot.
func (s *Server) learnName(id route.Identity, auth string) {
	if id.Kind != route.KindBot || id.BotID == "" {
		return
	}
	now := time.Now()
	s.names.mu.Lock()
	if t, ok := s.names.next[id.BotID]; ok && now.Before(t) {
		s.names.mu.Unlock()
		return
	}
	s.names.next[id.BotID] = now.Add(nameRetry)
	s.names.mu.Unlock()
	go s.lookupName(id, auth)
}

func (s *Server) lookupName(id route.Identity, auth string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.global().Take(ctx, id.GlobalKey(), s.Globals.Limit(id.BotID)); err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.Upstream+"/api/v10/users/@me", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", auth)
	resp, err := s.upstream.Do(req)
	if err != nil {
		s.rlLog.Debug("could not look up the bot's name", "bot", id.BotID, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		s.Invalid.Add(time.Now())
	}
	if resp.StatusCode != http.StatusOK {
		s.rlLog.Debug("could not look up the bot's name", "bot", id.BotID, "status", resp.StatusCode)
		return
	}
	var u struct {
		Username      string `json:"username"`
		Discriminator string `json:"discriminator"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&u); err != nil || u.Username == "" {
		return
	}
	name := u.Username
	if u.Discriminator != "" && u.Discriminator != "0" {
		name += "#" + u.Discriminator
	}
	s.m.BotInfo.WithLabelValues(id.BotID, name).Set(1)
	s.names.mu.Lock()
	s.names.next[id.BotID] = time.Now().Add(100 * 365 * 24 * time.Hour)
	s.names.mu.Unlock()
	s.rlLog.Info("learned the bot's name", "bot", id.BotID, "name", name)
}
