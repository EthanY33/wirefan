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
// jti is a random 16-byte hex string (32 lowercase chars) embedded in the
// HMAC payload so it can't be swapped out without breaking the signature.
// ReplayCache records jti -> expiry; a second VerifyTokenAgainst call with
// the same jti returns ErrTokenReplayed. The MAC input is built by
// macPayload; the token string itself carries only expMs, jti and the MAC.

// jtiHexLen is the length of the hex-encoded 16-byte jti SignToken emits.
const jtiHexLen = 32

// SignToken signs a one-time-use token. Each call generates a fresh jti, so
// distinct calls with the same args still produce distinct tokens.
func SignToken(secret, socketID, channel string, expiry time.Time) (string, error) {
	expMs := expiry.UnixMilli()
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", err
	}
	jti := hex.EncodeToString(jtiBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(macPayload(expMs, socketID, channel, jti)))
	return strconv.FormatInt(expMs, 10) + ":" + jti + ":" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// macPayload builds the HMAC input:
//
//	v1|<expMs>|<len>:<socket_id>|<len>:<channel>|<jti>
//
// socket_id and channel are length-prefixed so no bytes can move from one
// field into its neighbor. The old "%d|%s|%s|%s" layout had no such
// boundary: an app server that signs a browser-supplied socket_id
// "S|private-victim" for "private-attacker" produced a MAC that, with
// "private-attacker|" moved into the jti slot, also verified for socket S
// on private-victim. jti needs no prefix because VerifyTokenAgainst accepts
// only canonical hex there. The version tag keeps any future layout from
// colliding with this one.
func macPayload(expMs int64, socketID, channel, jti string) string {
	return fmt.Sprintf("v1|%d|%d:%s|%d:%s|%s", expMs, len(socketID), socketID, len(channel), channel, jti)
}

// validJTI reports whether s is exactly jtiHexLen lowercase hex characters,
// the only form SignToken produces.
func validJTI(s string) bool {
	if len(s) != jtiHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
	if !validJTI(jti) {
		return ErrTokenMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ErrTokenMalformed
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(macPayload(expMs, socketID, channel, jti)))
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
