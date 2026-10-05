package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Minimal HS256 JWT implementation (stdlib only) for session tokens.
// Tokens carry the user's openid as subject and an expiry; they are signed
// with the server's JWT_SECRET and are compatible with standard JWT verifiers.

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	Sub string `json:"sub"` // openid
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func sign(secret []byte, signingInput string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	return b64(mac.Sum(nil))
}

// IssueToken creates a signed session token for the given openid.
func IssueToken(secret []byte, openid string, ttl time.Duration) (string, error) {
	now := time.Now()
	header, _ := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	claims, _ := json.Marshal(jwtClaims{
		Sub: openid,
		Iat: now.Unix(),
		Exp: now.Add(ttl).Unix(),
	})
	signingInput := b64(header) + "." + b64(claims)
	return signingInput + "." + sign(secret, signingInput), nil
}

// VerifyToken checks the signature and expiry, returning the openid (subject).
func VerifyToken(secret []byte, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("malformed token")
	}
	signingInput := parts[0] + "." + parts[1]
	expected := sign(secret, signingInput)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[2])) != 1 {
		return "", fmt.Errorf("invalid signature")
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("malformed claims")
	}
	var claims jwtClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return "", fmt.Errorf("malformed claims")
	}
	if claims.Exp < time.Now().Unix() {
		return "", fmt.Errorf("token expired")
	}
	if claims.Sub == "" {
		return "", fmt.Errorf("token missing subject")
	}
	return claims.Sub, nil
}
