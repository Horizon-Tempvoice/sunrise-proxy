package cluster

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func members(names ...string) []Member {
	out := make([]Member, len(names))
	for i, n := range names {
		out[i] = Member{Name: n, Addr: n + ":8080"}
	}
	return out
}

func TestRingOwnerStability(t *testing.T) {
	keys := make([]string, 5000)
	for i := range keys {
		keys[i] = fmt.Sprintf("bot:1|channels/%d", 100000+i)
	}
	full := NewRing(members("a", "b", "c", "d"))

	owners := map[string]string{}
	perNode := map[string]int{}
	for _, k := range keys {
		m, _ := full.Owner(k)
		owners[k] = m.Name
		perNode[m.Name]++
	}
	for name, n := range perNode {
		if n < len(keys)/4*7/10 || n > len(keys)/4*13/10 {
			t.Errorf("node %s owns %d of %d keys, expected roughly a quarter", name, n, len(keys))
		}
	}

	tests := []struct {
		name  string
		ring  *Ring
		moves func(oldOwner string) bool
	}{
		{"node c leaves", NewRing(members("a", "b", "d")), func(o string) bool { return o == "c" }},
		{"node e joins", NewRing(members("a", "b", "c", "d", "e")), nil},
		{"order of members does not matter", NewRing(members("d", "c", "b", "a")), func(string) bool { return false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			moved := 0
			for _, k := range keys {
				m, _ := tt.ring.Owner(k)
				if m.Name == owners[k] {
					continue
				}
				moved++
				switch {
				case tt.moves != nil && !tt.moves(owners[k]):
					t.Fatalf("key %s moved from %s to %s although its owner is unchanged", k, owners[k], m.Name)
				case tt.moves == nil && m.Name != "e":
					t.Fatalf("key %s moved from %s to %s, only moves to the new node are allowed", k, owners[k], m.Name)
				}
			}
			if tt.moves == nil && (moved < len(keys)/5*7/10 || moved > len(keys)/5*13/10) {
				t.Errorf("%d keys moved to the new node, expected roughly a fifth", moved)
			}
		})
	}
}

func TestSignature(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	now := time.UnixMilli(1_700_000_000_000)
	a := NewSigner(secret, "node-a")

	signed := func() http.Header {
		h := http.Header{}
		a.Sign(h, "POST", "/api/v10/channels/1/messages?x=1", now)
		return h
	}

	tests := []struct {
		name   string
		header func() http.Header
		method string
		target string
		at     time.Time
		verify *Signer
		reason string
	}{
		{"valid", signed, "POST", "/api/v10/channels/1/messages?x=1", now.Add(time.Second), a, ""},
		{"other node with same secret", signed, "POST", "/api/v10/channels/1/messages?x=1", now, NewSigner(secret, "node-b"), ""},
		{"missing headers", func() http.Header { return http.Header{} }, "POST", "/", now, a, "missing"},
		{"tampered path", signed, "POST", "/api/v10/channels/2/messages?x=1", now, a, "invalid"},
		{"tampered query", signed, "POST", "/api/v10/channels/1/messages?x=2", now, a, "invalid"},
		{"tampered method", signed, "DELETE", "/api/v10/channels/1/messages?x=1", now, a, "invalid"},
		{"wrong secret", signed, "POST", "/api/v10/channels/1/messages?x=1", now, NewSigner("another-secret-another-secret-xx", "node-a"), "invalid"},
		{"too old", signed, "POST", "/api/v10/channels/1/messages?x=1", now.Add(MaxSignatureAge + time.Millisecond), a, "stale"},
		{"from the future", signed, "POST", "/api/v10/channels/1/messages?x=1", now.Add(-MaxSignatureAge - time.Millisecond), a, "stale"},
		{"spoofed node name", func() http.Header {
			h := signed()
			h.Set(HeaderNode, "node-evil")
			return h
		}, "POST", "/api/v10/channels/1/messages?x=1", now, a, "invalid"},
		{"garbage signature", func() http.Header {
			h := signed()
			h.Set(HeaderSignature, "zz")
			return h
		}, "POST", "/api/v10/channels/1/messages?x=1", now, a, "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, reason, ok := tt.verify.Verify(tt.header(), tt.method, tt.target, tt.at)
			if ok != (tt.reason == "") || reason != tt.reason {
				t.Fatalf("got ok=%v reason=%q, want reason %q", ok, reason, tt.reason)
			}
		})
	}
}
