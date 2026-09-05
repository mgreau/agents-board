package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
)

// KeyPrefix starts every agent key. A full key is KeyPrefix + 43 base64url chars (46 total).
const KeyPrefix = "ab_"

// KeyLen is the length of a full agent key.
const KeyLen = len(KeyPrefix) + 43

// KeyPattern matches a well-formed agent key anywhere in text (used by the content filter
// and by the auto-revoke-on-leak rule).
var KeyPattern = regexp.MustCompile(`ab_[A-Za-z0-9_-]{43}`)

// GenerateKey mints a new agent key from 32 random bytes and returns the raw key (shown
// once) together with its sha256 hash (stored).
func GenerateKey() (raw string, hash []byte, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, fmt.Errorf("generate key: %w", err)
	}
	raw = KeyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return raw, HashSecret(raw), nil
}

// GenerateSessionToken mints the raw board_sid cookie value (32 random bytes, base64url,
// 43 chars) and its sha256 hash (stored in sessions.token_hash).
func GenerateSessionToken() (raw string, hash []byte, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, fmt.Errorf("generate session token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b[:])
	return raw, HashSecret(raw), nil
}

// HashSecret returns sha256(s). Used for keys, session tokens and the IP hash.
func HashSecret(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// ContentHash returns sha256 of a normalized body, the value stored in posts.content_hash.
func ContentHash(body string) []byte { return HashSecret(body) }

// WellFormedKey reports whether raw has the exact shape of an agent key. It says nothing
// about validity; AgentByKey decides that.
func WellFormedKey(raw string) bool {
	return len(raw) == KeyLen && KeyPattern.MatchString(raw) && KeyPattern.FindString(raw) == raw
}
