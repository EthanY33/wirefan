package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestGenerateSecret(t *testing.T) {
	s, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 64 { // 32 bytes hex-encoded
		t.Fatalf("want 64 hex chars, got %d", len(s))
	}
}

func TestHashSecret(t *testing.T) {
	h := HashSecret("abc")
	expected := sha256.Sum256([]byte("abc"))
	if h != hex.EncodeToString(expected[:]) {
		t.Fatal("hash mismatch")
	}
}

func TestVerifySecret(t *testing.T) {
	h := HashSecret("password")
	if !VerifySecret("password", h) {
		t.Fatal("expected true")
	}
	if VerifySecret("wrong", h) {
		t.Fatal("expected false")
	}
}
