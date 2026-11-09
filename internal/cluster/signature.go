package cluster

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	HeaderNode      = "X-Sunrise-Node"
	HeaderTimestamp = "X-Sunrise-Timestamp"
	HeaderSignature = "X-Sunrise-Signature"

	MaxSignatureAge = 30 * time.Second
)

func deriveKey(secret, purpose string, n int) []byte {
	key, err := hkdf.Key(sha256.New, []byte(secret), []byte("sunrise"), purpose, n)
	if err != nil {
		panic(err)
	}
	return key
}

// GossipKey is the AES-256 key memberlist uses to encrypt gossip.
func GossipKey(secret string) []byte {
	return deriveKey(secret, "gossip encryption v1", 32)
}

// Signer signs and verifies requests between nodes with HMAC-SHA256.
type Signer struct {
	key  []byte
	node string
}

func NewSigner(secret, node string) *Signer {
	return &Signer{key: deriveKey(secret, "request signing v1", 32), node: node}
}

func (s *Signer) Node() string {
	return s.node
}

func (s *Signer) mac(method, target, ts, node string) []byte {
	m := hmac.New(sha256.New, s.key)
	for _, part := range []string{"sunrise-v1", method, target, ts, node} {
		m.Write([]byte(part))
		m.Write([]byte{'\n'})
	}
	return m.Sum(nil)
}

func (s *Signer) Sign(h http.Header, method, target string, now time.Time) {
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	h.Set(HeaderNode, s.node)
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderSignature, hex.EncodeToString(s.mac(method, target, ts, s.node)))
}

// Verify checks the signature headers. On failure, reason is a short label for
// metrics: missing, malformed, stale or invalid.
func (s *Signer) Verify(h http.Header, method, target string, now time.Time) (node, reason string, ok bool) {
	node = h.Get(HeaderNode)
	ts := h.Get(HeaderTimestamp)
	sig := h.Get(HeaderSignature)
	if node == "" || ts == "" || sig == "" {
		return node, "missing", false
	}
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return node, "malformed", false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return node, "malformed", false
	}
	age := now.Sub(time.UnixMilli(ms))
	if age > MaxSignatureAge || age < -MaxSignatureAge {
		return node, "stale", false
	}
	if !hmac.Equal(got, s.mac(method, target, ts, node)) {
		return node, "invalid", false
	}
	return node, "", true
}

// HasSunriseHeaders reports whether a request carries any header Sunrise uses between nodes.
func HasSunriseHeaders(h http.Header) bool {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-sunrise-") {
			return true
		}
	}
	return false
}

func StripSunriseHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-sunrise-") {
			delete(h, k)
		}
	}
}
