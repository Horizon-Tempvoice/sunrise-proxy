package route

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

func snowflake(t time.Time) string {
	return strconv.FormatUint(uint64(t.UnixMilli()-discordEpochMillis)<<22, 10)
}

func TestParse(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	fresh := snowflake(now.Add(-time.Minute))
	hourOld := snowflake(now.Add(-2 * time.Hour))
	monthOld := snowflake(now.Add(-30 * 24 * time.Hour))
	const whToken = "VSOzAqY1OZFF5WJVtbIzFtmjGupk-84Hn0A_ZzToF_CHsPIeCk0Q9Uok_mjxR0dNtApI"
	const itToken = interactionTokenPrefix + "MTEwMzA0OTQyMDkzMDU2ODMyMjpOZUllWHdNU2J4RXBFMHVYRjBpU0pHMDdEb3BhM3ZlYklBODlMUmtl"

	tests := []struct {
		name, method, path string
		key, metric, major string
		webhook            string
		callback, pathTok  bool
		interaction        bool
	}{
		{name: "guild major", method: "GET", path: "/api/v10/guilds/111111111111111111/members/222222222222222222",
			key: "GET /guilds/111111111111111111/members/!", metric: "/guilds/!/members/!", major: "guilds/111111111111111111"},
		{name: "channel messages", method: "POST", path: "/api/v10/channels/333333333333333333/messages",
			key: "POST /channels/333333333333333333/messages", metric: "/channels/!/messages", major: "channels/333333333333333333"},
		{name: "unversioned", method: "GET", path: "/api/channels/333333333333333333",
			key: "GET /channels/333333333333333333", metric: "/channels/!", major: "channels/333333333333333333"},
		{name: "reaction emoji and user collapse", method: "PUT", path: "/api/v10/channels/1/messages/2/reactions/%F0%9F%98%8B/@me",
			key: "PUT /channels/1/messages/!/reactions/!", metric: "/channels/!/messages/!/reactions/!", major: "channels/1"},
		{name: "reaction named emoji", method: "DELETE", path: "/api/v10/channels/1/messages/2/reactions/blob:863985751205085195/99",
			key: "DELETE /channels/1/messages/!/reactions/!", metric: "/channels/!/messages/!/reactions/!", major: "channels/1"},
		{name: "delete all reactions", method: "DELETE", path: "/api/v10/channels/1/messages/2/reactions",
			key: "DELETE /channels/1/messages/!/reactions", metric: "/channels/!/messages/!/reactions", major: "channels/1"},
		{name: "delete fresh message", method: "DELETE", path: "/api/v10/channels/1/messages/" + fresh,
			key: "DELETE /channels/1/messages/!", metric: "/channels/!/messages/!", major: "channels/1"},
		{name: "delete message older than 14 days", method: "DELETE", path: "/api/v10/channels/1/messages/" + monthOld,
			key: "DELETE /channels/1/messages/!14d", metric: "/channels/!/messages/!14d", major: "channels/1"},
		{name: "delete message 2h old stays normal", method: "DELETE", path: "/api/v10/channels/1/messages/" + hourOld,
			key: "DELETE /channels/1/messages/!", metric: "/channels/!/messages/!", major: "channels/1"},
		{name: "edit message older than 1 hour", method: "PATCH", path: "/api/v10/channels/1/messages/" + hourOld,
			key: "PATCH /channels/1/messages/!1h", metric: "/channels/!/messages/!1h", major: "channels/1"},
		{name: "webhook without token", method: "GET", path: "/api/v10/webhooks/444444444444444444",
			key: "GET /webhooks/444444444444444444", metric: "/webhooks/!", major: "webhooks/444444444444444444",
			webhook: "444444444444444444"},
		{name: "webhook execute with token", method: "POST", path: "/api/v10/webhooks/444444444444444444/" + whToken + "?wait=true",
			key: "POST /webhooks/444444444444444444/" + shortHash(whToken), metric: "/webhooks/!/!",
			major: "webhooks/444444444444444444/" + shortHash(whToken), webhook: "444444444444444444", pathTok: true},
		{name: "webhook message edit is not the webhook itself", method: "PATCH", path: "/api/webhooks/4/" + whToken + "/messages/5",
			key: "PATCH /webhooks/4/" + shortHash(whToken) + "/messages/!", metric: "/webhooks/!/!/messages/!",
			major: "webhooks/4/" + shortHash(whToken), pathTok: true},
		{name: "interaction followup", method: "PATCH", path: "/api/v10/webhooks/555/" + itToken + "/messages/@original",
			key: "PATCH /webhooks/555/" + shortHash(itToken) + "/messages/@original", metric: "/webhooks/!/!/messages/@original",
			major: "webhooks/555/" + shortHash(itToken), pathTok: true, interaction: true},
		{name: "interaction callback", method: "POST", path: "/api/v10/interactions/666/" + itToken + "/callback",
			key: "POST /interactions/666/!/callback", metric: "/interactions/!/!/callback", major: "interactions/666",
			callback: true, pathTok: true, interaction: true},
		{name: "invite code collapses", method: "GET", path: "/api/v10/invites/example",
			key: "GET /invites/!", metric: "/invites/!"},
		{name: "guild template code collapses", method: "GET", path: "/api/v10/guilds/templates/hgM48av5Q69A",
			key: "GET /guilds/templates/!", metric: "/guilds/templates/!"},
		{name: "application commands", method: "PUT", path: "/api/v10/applications/777/guilds/888/commands",
			key: "PUT /applications/!/guilds/!/commands", metric: "/applications/!/guilds/!/commands"},
		{name: "users me", method: "GET", path: "/api/v10/users/@me",
			key: "GET /users/@me", metric: "/users/@me"},
		{name: "unknown random segment collapses", method: "GET", path: "/api/v10/users/@me/SomeName",
			key: "GET /users/@me/!", metric: "/users/@me/!"},
		{name: "not an api path", method: "GET", path: "/favicon.ico",
			key: "GET /!other", metric: "/!other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, _, _ := strings.Cut(tt.path, "?")
			r := Parse(tt.method, path, now)
			if r.Key != tt.key {
				t.Errorf("key: got %q, want %q", r.Key, tt.key)
			}
			if r.Metric != tt.metric {
				t.Errorf("metric: got %q, want %q", r.Metric, tt.metric)
			}
			if r.Major != tt.major {
				t.Errorf("major: got %q, want %q", r.Major, tt.major)
			}
			if r.Webhook != tt.webhook {
				t.Errorf("webhook: got %q, want %q", r.Webhook, tt.webhook)
			}
			if r.Callback != tt.callback || r.PathToken != tt.pathTok || r.Interaction != tt.interaction {
				t.Errorf("flags: callback=%v pathToken=%v interaction=%v, want %v %v %v",
					r.Callback, r.PathToken, r.Interaction, tt.callback, tt.pathTok, tt.interaction)
			}
		})
	}
}

// fakeToken builds a token-shaped string at runtime, so no literal in the source
// looks like a real bot token.
func fakeToken(botID string) string {
	return base64.RawStdEncoding.EncodeToString([]byte(botID)) + ".GabcDe." + strings.Repeat("x", 38)
}

func TestIdentify(t *testing.T) {
	tests := []struct {
		auth  string
		kind  Kind
		label string
	}{
		{"", KindNone, "none"},
		{"Bot " + fakeToken("123456789012345678"), KindBot, "123456789012345678"},
		{fakeToken("123456789012345678"), KindBot, "123456789012345678"},
		{"Bot not-a-token", KindBot, "unknown"},
		{"Bearer abcdef", KindBearer, "bearer"},
	}
	for _, tt := range tests {
		id := Identify(tt.auth)
		if id.Kind != tt.kind || id.Label() != tt.label {
			t.Errorf("Identify(%q) = kind %v label %q, want %v %q", tt.auth, id.Kind, id.Label(), tt.kind, tt.label)
		}
	}
}
