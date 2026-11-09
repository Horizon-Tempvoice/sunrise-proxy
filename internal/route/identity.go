package route

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

type Kind uint8

const (
	KindNone Kind = iota
	KindBot
	KindBearer
)

type Identity struct {
	Kind Kind
	// BotID is decoded from the first token segment; empty if that fails.
	BotID string
	// TokenKey is a hash of the Authorization header, used as a map key. Never log it.
	TokenKey string
}

func Identify(authorization string) Identity {
	auth := strings.TrimSpace(authorization)
	if auth == "" {
		return Identity{Kind: KindNone}
	}
	sum := sha256.Sum256([]byte(auth))
	id := Identity{TokenKey: hex.EncodeToString(sum[:16])}
	if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		id.Kind = KindBearer
		return id
	}
	id.Kind = KindBot
	token := auth
	if len(token) > 4 && strings.EqualFold(token[:4], "Bot ") {
		token = strings.TrimSpace(token[4:])
	}
	id.BotID = decodeBotID(token)
	return id
}

// Label is the value used in metric labels and logs: a bot id, never a token.
func (id Identity) Label() string {
	switch id.Kind {
	case KindNone:
		return "none"
	case KindBearer:
		return "bearer"
	}
	if id.BotID == "" {
		return "unknown"
	}
	return id.BotID
}

// GlobalKey groups requests that share one global ratelimit.
func (id Identity) GlobalKey() string {
	if id.Kind == KindBot && id.BotID != "" {
		return "bot:" + id.BotID
	}
	return "token:" + id.TokenKey
}

func decodeBotID(token string) string {
	first, _, ok := strings.Cut(token, ".")
	if !ok || first == "" {
		return ""
	}
	first = strings.TrimRight(first, "=")
	raw, err := base64.RawStdEncoding.DecodeString(first)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(first)
		if err != nil {
			return ""
		}
	}
	s := string(raw)
	if !isNumeric(s) {
		return ""
	}
	return s
}
