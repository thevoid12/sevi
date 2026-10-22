// Package auth decides who may call the Core API. An adapter (Telegram, a web
// page...) proves itself with a short-lived signed token (JWT, HS256) made from a
// secret only the adapters and Core know. The token names the adapter, and only
// adapters on the configured list are accepted. A request with no valid token is
// refused before it reaches any handler.
//
// All adapters share one secret, so a token's adapter name is bookkeeping (it
// keeps each adapter's threads apart and shows in the log), not a second lock:
// anything holding the secret could name any adapter.
package auth

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// MinSecretBytes is the shortest secret accepted: 32 bytes is what HS256 is
// designed for; shorter ones are guessable.
const MinSecretBytes = 32

// Mint creates a token for adapter that is valid for ttl.
func Mint(secret []byte, adapter string, ttl time.Duration) (string, error) {
	if len(secret) < MinSecretBytes {
		return "", fmt.Errorf("the API secret is shorter than %d bytes", MinSecretBytes)
	}
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    adapter,
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)), // tolerate small clock drift
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// Verifier checks tokens against the shared secret and the list of adapters.
type Verifier struct {
	secret   []byte
	adapters map[string]bool
}

// NewVerifier refuses a short secret and an empty adapter list.
func NewVerifier(secret []byte, adapters []string) (*Verifier, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("the API secret is shorter than %d bytes (make one with: openssl rand -hex 32)", MinSecretBytes)
	}
	if len(adapters) == 0 {
		return nil, errors.New("no adapters are configured, so nothing could call the API")
	}
	set := make(map[string]bool, len(adapters))
	for _, a := range adapters {
		set[a] = true
	}
	return &Verifier{secret: secret, adapters: set}, nil
}

// ErrUnauthorized is returned for every failure, whatever the reason, so a caller
// learns nothing about what was wrong. The specific reason is for the log only.
var ErrUnauthorized = errors.New("unauthorized")

// Verify returns the adapter a valid token names. The reason for a refusal is
// wrapped with ErrUnauthorized for logging.
func (v *Verifier) Verify(token string) (adapter string, err error) {
	if token == "" {
		return "", fmt.Errorf("%w: no token", ErrUnauthorized)
	}
	var claims jwt.RegisteredClaims
	_, err = jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return v.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), // never "none", never an asymmetric method
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(5*time.Second),
	)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if !v.adapters[claims.Issuer] {
		return "", fmt.Errorf("%w: unknown adapter %q", ErrUnauthorized, claims.Issuer)
	}
	return claims.Issuer, nil
}

// IsLoopback reports whether a listen address only accepts connections from this machine.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
