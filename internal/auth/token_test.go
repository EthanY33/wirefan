package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignAndVerifyToken(t *testing.T) {
	secret := "topsecret"
	socketID := "01HZABC"
	channel := "private-room1"
	tok, err := SignToken(secret, socketID, channel, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyToken(secret, socketID, channel, tok); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyTokenWrongSocket(t *testing.T) {
	secret := "s"
	tok, _ := SignToken(secret, "sock1", "private-x", time.Now().Add(time.Minute))
	if err := VerifyToken(secret, "sock2", "private-x", tok); err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestVerifyTokenExpired(t *testing.T) {
	secret := "s"
	tok, _ := SignToken(secret, "sock1", "private-x", time.Now().Add(-time.Minute))
	if err := VerifyToken(secret, "sock1", "private-x", tok); err == nil {
		t.Fatal("expected expired error")
	}
}

func TestVerifyTokenTampered(t *testing.T) {
	secret := "s"
	tok, _ := SignToken(secret, "sock1", "private-x", time.Now().Add(time.Minute))
	if err := VerifyToken(secret, "sock1", "private-x", tok+"X"); err == nil {
		t.Fatal("expected tamper error")
	}
}

func TestVerifyTokenAgainstCachePreventsReplay(t *testing.T) {
	secret := "s"
	tok, _ := SignToken(secret, "sock1", "private-x", time.Now().Add(time.Minute))
	cache := NewReplayCache()
	if err := VerifyTokenAgainst(secret, "sock1", "private-x", tok, cache); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if err := VerifyTokenAgainst(secret, "sock1", "private-x", tok, cache); err == nil {
		t.Fatal("expected ErrTokenReplayed on second use, got nil")
	}
}

func TestVerifyTokenAgainstNilCache(t *testing.T) {
	secret := "s"
	tok, _ := SignToken(secret, "sock1", "private-x", time.Now().Add(time.Minute))
	if err := VerifyTokenAgainst(secret, "sock1", "private-x", tok, nil); err != nil {
		t.Fatalf("first verify with nil cache: %v", err)
	}
	if err := VerifyTokenAgainst(secret, "sock1", "private-x", tok, nil); err != nil {
		t.Fatalf("second verify with nil cache: %v", err)
	}
}

func TestReplayCacheSweepRemovesExpired(t *testing.T) {
	c := NewReplayCache()
	c.CheckAndRecord("expired", time.Now().Add(-time.Minute))
	c.CheckAndRecord("alive", time.Now().Add(time.Minute))
	if got := c.Sweep(); got != 1 {
		t.Errorf("Sweep removed %d, want 1", got)
	}
	if got, want := c.Len(), 1; got != want {
		t.Errorf("post-Sweep Len = %d, want %d", got, want)
	}
}

func TestSignTokenJtiUnique(t *testing.T) {
	secret := "s"
	a, _ := SignToken(secret, "sock1", "x", time.Now().Add(time.Minute))
	b, _ := SignToken(secret, "sock1", "x", time.Now().Add(time.Minute))
	if a == b {
		t.Fatal("two SignToken calls with identical args produced identical tokens — jti collision or absent")
	}
}

// TestTokenFieldShiftForgery is the G1 regression. With the old MAC input
// "%d|%s|%s|%s" (exp, socket_id, channel, jti) nothing stopped a '|' inside
// a field. An app server that passes the browser's socket_id through
// verbatim signs socket_id "S|private-victim" for channel
// "private-attacker"; moving "private-attacker|" into the jti field of the
// returned token then yields the identical MAC input for socket S on
// private-victim, a channel the app server never authorized.
func TestTokenFieldShiftForgery(t *testing.T) {
	const secret = "s"
	const socket = "01J8Z4Q0M3W6B9XKAV5T2C7RPD"
	tok, err := SignToken(secret, socket+"|private-victim", "private-attacker", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(tok, ":", 3)
	if len(parts) != 3 {
		t.Fatalf("token %q: want 3 parts", tok)
	}
	forged := parts[0] + ":private-attacker|" + parts[1] + ":" + parts[2]
	if err := VerifyToken(secret, socket, "private-victim", forged); err == nil {
		t.Fatal("field-shifted token verified for a channel it was never issued for")
	}
}

// TestMACPayloadUnambiguous checks the MAC input itself, independent of the
// jti format check: shifting bytes between socket_id and channel, in either
// direction, must change the payload.
func TestMACPayloadUnambiguous(t *testing.T) {
	const jti = "0123456789abcdef0123456789abcdef"
	pairs := [][2][2]string{
		{{"S|private-victim", "private-attacker"}, {"S", "private-victim|private-attacker"}},
		{{"S|", "c"}, {"S", "|c"}},
		{{"S1:", "c"}, {"S1", ":c"}},
		{{"", "S|c"}, {"S", "c"}},
	}
	for _, p := range pairs {
		a := macPayload(1, p[0][0], p[0][1], jti)
		b := macPayload(1, p[1][0], p[1][1], jti)
		if a == b {
			t.Errorf("(%q, %q) and (%q, %q) share MAC input %q", p[0][0], p[0][1], p[1][0], p[1][1], a)
		}
	}
	if got := macPayload(1, "S", "c", jti); !strings.HasPrefix(got, "v1|") {
		t.Errorf("MAC input %q lacks the v1 version tag", got)
	}
}

// TestVerifyTokenRejectsNonCanonicalJTI: SignToken only ever emits 32
// lowercase hex chars, so anything else in the jti slot is a forgery attempt
// or corruption and is rejected before the MAC is even computed.
func TestVerifyTokenRejectsNonCanonicalJTI(t *testing.T) {
	const secret = "s"
	tok, err := SignToken(secret, "sock1", "private-x", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(tok, ":", 3)
	for _, jti := range []string{
		"",
		strings.ToUpper(parts[1]),
		parts[1][:31],
		parts[1] + "0",
		"private-x|" + parts[1],
		strings.Repeat("g", 32),
	} {
		bad := parts[0] + ":" + jti + ":" + parts[2]
		if err := VerifyToken(secret, "sock1", "private-x", bad); !errors.Is(err, ErrTokenMalformed) {
			t.Errorf("jti %q: want ErrTokenMalformed, got %v", jti, err)
		}
	}
	if err := VerifyToken(secret, "sock1", "private-x", tok); err != nil {
		t.Fatalf("canonical token: %v", err)
	}
}
