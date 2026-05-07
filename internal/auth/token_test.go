package auth

import (
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
