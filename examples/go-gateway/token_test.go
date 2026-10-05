package main

import (
	"testing"
	"time"
)

func TestTokenRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef")
	openid := "o_test_openid_123"

	tok, err := IssueToken(secret, openid, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	got, err := VerifyToken(secret, tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != openid {
		t.Fatalf("openid mismatch: got %q want %q", got, openid)
	}
}

func TestTokenWrongSecret(t *testing.T) {
	tok, _ := IssueToken([]byte("0123456789abcdef"), "o1", time.Hour)
	if _, err := VerifyToken([]byte("ffffffffffffffff"), tok); err == nil {
		t.Fatal("expected signature failure with wrong secret")
	}
}

func TestTokenTampered(t *testing.T) {
	secret := []byte("0123456789abcdef")
	tok, _ := IssueToken(secret, "o1", time.Hour)
	// Flip the last character of the signature.
	tampered := tok[:len(tok)-1] + flip(tok[len(tok)-1])
	if _, err := VerifyToken(secret, tampered); err == nil {
		t.Fatal("expected failure for tampered token")
	}
}

func TestTokenExpired(t *testing.T) {
	secret := []byte("0123456789abcdef")
	tok, _ := IssueToken(secret, "o1", -time.Minute) // already expired
	if _, err := VerifyToken(secret, tok); err == nil {
		t.Fatal("expected expiry failure")
	}
}

func flip(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}
