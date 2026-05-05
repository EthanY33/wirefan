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
