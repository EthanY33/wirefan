package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrTokenMalformed = errors.New("token malformed")
	ErrTokenExpired   = errors.New("token expired")
	ErrTokenInvalid   = errors.New("token invalid")
	ErrTokenReplayed  = errors.New("token already used")
)

// Token format: "<expMs>:<jti>:<base64mac>"
// jti is a random 16-byte hex string (32 chars) embedded in the HMAC payload
// so it can't be swapped out without breaking the signature. ReplayCache
// records jti -> expiry; a second VerifyTokenAgainst call with the same
// jti returns ErrTokenReplayed.

// SignToken signs a one-time-use token. Each call generates a fresh jti, so
// distinct calls with the same args still produce distinct tokens.
func SignToken(secret, socketID, channel string, expiry time.Time) (string, error) {
	expMs := expiry.UnixMilli()
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", err
	}
	jti := hex.EncodeToString(jtiBytes)
	payload := fmt.Sprintf("%d|%s|%s|%s", expMs, socketID, channel, jti)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return strconv.FormatInt(expMs, 10) + ":" + jti + ":" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyToken validates a token without replay protection. Provided for
// backward-compatibility; production callers should construct a ReplayCache
// and call VerifyTokenAgainst.
func VerifyToken(secret, socketID, channel, tok string) error {
	return VerifyTokenAgainst(secret, socketID, channel, tok, nil)
}

// VerifyTokenAgainst is VerifyToken with optional replay protection.
// When cache is non-nil and the token verifies cleanly, the jti is recorded;
// a subsequent call with the same jti returns ErrTokenReplayed.
func VerifyTokenAgainst(secret, socketID, channel, tok string, cache *ReplayCache) error {
	parts := strings.SplitN(tok, ":", 3)
	if len(parts) != 3 {
		return ErrTokenMalformed
	}
	expMs, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return ErrTokenMalformed
	}
	if time.Now().UnixMilli() > expMs {
		return ErrTokenExpired
	}
	jti := parts[1]
	if jti == "" {
		return ErrTokenMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ErrTokenMalformed
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d|%s|%s|%s", expMs, socketID, channel, jti)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return ErrTokenInvalid
	}
	if cache != nil {
		if !cache.CheckAndRecord(jti, time.UnixMilli(expMs)) {
			return ErrTokenReplayed
		}
	}
	return nil
}

// ReplayCache records token jti -> expiry. Safe for concurrent use. Memory
// stays bounded by Sweep, which removes expired entries; callers that don't
// run Sweep periodically will accumulate memory at the rate of issued
// tokens until the next Sweep.
type ReplayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func NewReplayCache() *ReplayCache {
	return &ReplayCache{seen: map[string]time.Time{}}
}

// CheckAndRecord returns true the first time it sees jti, false thereafter.
func (c *ReplayCache) CheckAndRecord(jti string, exp time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[jti]; ok {
		return false
	}
	c.seen[jti] = exp
	return true
}

// Sweep removes entries whose expiry has passed. Returns the count removed.
func (c *ReplayCache) Sweep() int {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	swept := 0
	for jti, exp := range c.seen {
		if !now.Before(exp) {
			delete(c.seen, jti)
			swept++
		}
	}
	return swept
}

// Len reports the current number of cached entries (for tests / metrics).
func (c *ReplayCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}
