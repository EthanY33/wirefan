package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrTokenMalformed = errors.New("token malformed")
	ErrTokenExpired   = errors.New("token expired")
	ErrTokenInvalid   = errors.New("token invalid")
)

// SignToken returns "<expMillis>:<base64mac>".
func SignToken(secret, socketID, channel string, expiry time.Time) (string, error) {
	expMs := expiry.UnixMilli()
	payload := fmt.Sprintf("%d|%s|%s", expMs, socketID, channel)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return strconv.FormatInt(expMs, 10) + ":" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyToken(secret, socketID, channel, tok string) error {
	parts := strings.SplitN(tok, ":", 2)
	if len(parts) != 2 {
		return ErrTokenMalformed
	}
	expMs, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return ErrTokenMalformed
	}
	if time.Now().UnixMilli() > expMs {
		return ErrTokenExpired
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrTokenMalformed
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d|%s|%s", expMs, socketID, channel)))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return ErrTokenInvalid
	}
	return nil
}
