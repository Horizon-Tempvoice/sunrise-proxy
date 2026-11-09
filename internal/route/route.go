package route

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

const discordEpochMillis = 1420070400000

// base64 of "interaction:", the start of every interaction token.
const interactionTokenPrefix = "aW50ZXJhY3Rpb246"

type Route struct {
	// Key identifies the synthetic bucket: method plus path with ids collapsed, major ids kept.
	Key string
	// Metric is the path with every id collapsed, safe as a metric label.
	Metric string
	// Major holds the top-level resource ("channels/123"), empty if the route has none.
	Major string

	Callback    bool   // POST /interactions/{id}/{token}/callback
	Interaction bool   // authenticated by an interaction token in the path
	Webhook     string // webhook id when the request targets the webhook itself
	PathToken   bool   // a webhook or interaction token in the path authenticates the request
	Gateway     bool   // GET /gateway, needs no token
}

func Parse(method, path string, now time.Time) Route {
	rest, ok := stripAPIPrefix(path)
	if !ok {
		return Route{Key: method + " /!other", Metric: "/!other"}
	}

	var segs []string
	for _, s := range strings.Split(rest, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return Route{Key: method + " /", Metric: "/"}
	}

	r := Route{}
	key := make([]string, 0, len(segs))
	metric := make([]string, 0, len(segs))
	keep := func(k, m string) {
		key = append(key, k)
		metric = append(metric, m)
	}

	i := 0
	switch segs[0] {
	case "channels", "guilds":
		if len(segs) > 1 && isNumeric(segs[1]) {
			r.Major = segs[0] + "/" + segs[1]
			keep(segs[0], segs[0])
			keep(segs[1], "!")
			i = 2
		}
	case "webhooks":
		if len(segs) > 1 && isNumeric(segs[1]) {
			keep(segs[0], segs[0])
			keep(segs[1], "!")
			i = 2
			if len(segs) > 2 {
				token := segs[2]
				h := shortHash(token)
				r.Major = "webhooks/" + segs[1] + "/" + h
				r.PathToken = true
				r.Interaction = strings.HasPrefix(token, interactionTokenPrefix)
				keep(h, "!")
				i = 3
				if !r.Interaction && (len(segs) == 3 || (len(segs) == 4 && (segs[3] == "slack" || segs[3] == "github"))) {
					r.Webhook = segs[1]
				}
			} else {
				r.Major = "webhooks/" + segs[1]
				r.Webhook = segs[1]
			}
		}
	case "interactions":
		if len(segs) > 2 && isNumeric(segs[1]) {
			r.Major = "interactions/" + segs[1]
			r.Interaction = true
			r.PathToken = true
			r.Callback = len(segs) == 4 && segs[3] == "callback" && method == "POST"
			keep(segs[0], segs[0])
			keep(segs[1], "!")
			keep("!", "!")
			i = 3
		}
	case "invites":
		keep("invites", "invites")
		if len(segs) > 1 {
			keep("!", "!")
		}
		i = len(segs)
	case "gateway":
		r.Gateway = len(segs) == 1 && method == "GET"
	}

	for ; i < len(segs); i++ {
		seg := segs[i]
		prev := ""
		if i > 0 {
			prev = segs[i-1]
		}
		switch {
		case isNumeric(seg):
			k := "!"
			if segs[0] == "channels" && r.Major != "" && prev == "messages" && i == len(segs)-1 {
				k = oldMessageBucket(method, seg, now)
			}
			keep(k, k)
		case seg == "reactions" && segs[0] == "channels":
			keep(seg, seg)
			if i+1 < len(segs) {
				keep("!", "!")
			}
			i = len(segs)
		case prev == "templates" || prev == "activity-instances" || len(seg) >= 64 || !isLiteral(seg):
			keep("!", "!")
		default:
			keep(seg, seg)
		}
	}

	r.Key = method + " /" + strings.Join(key, "/")
	r.Metric = "/" + strings.Join(metric, "/")
	return r
}

// Deleting messages older than 14 days and editing messages older than one hour
// fall into separate, slower buckets on Discord's side.
func oldMessageBucket(method, id string, now time.Time) string {
	created, ok := SnowflakeTime(id)
	if !ok {
		return "!"
	}
	age := now.Sub(created)
	switch {
	case method == "DELETE" && age >= 14*24*time.Hour:
		return "!14d"
	case method == "PATCH" && age >= time.Hour:
		return "!1h"
	}
	return "!"
}

func SnowflakeTime(id string) (time.Time, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(n>>22) + discordEpochMillis), true
}

func stripAPIPrefix(path string) (string, bool) {
	if path != "/api" && !strings.HasPrefix(path, "/api/") {
		return "", false
	}
	rest := path[len("/api"):]
	if strings.HasPrefix(rest, "/v") {
		end := 2
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end > 2 && (end == len(rest) || rest[end] == '/') {
			rest = rest[end:]
		}
	}
	return rest, true
}

// HasTraversal reports whether a decoded path contains a ".." segment.
func HasTraversal(path string) bool {
	for _, s := range strings.Split(path, "/") {
		if s == ".." {
			return true
		}
	}
	return false
}

func isNumeric(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isLiteral accepts the lowercase words Discord uses in route names. Anything else
// (codes, names, tokens) is collapsed so labels stay bounded.
func isLiteral(s string) bool {
	if len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c == '@':
		case i > 0 && (c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'):
		default:
			return false
		}
	}
	return true
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
