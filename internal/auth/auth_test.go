package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var secret = []byte(strings.Repeat("s", 40))

func verifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := NewVerifier(secret, []string{"telegram", "web"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestValidTokenIdentifiesItsAdapter(t *testing.T) {
	v := verifier(t)
	for _, name := range []string{"telegram", "web"} { // one secret serves every listed adapter
		tok, err := Mint(secret, name, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := v.Verify(tok); err != nil || got != name {
			t.Fatalf("%s: got %q, %v", name, got, err)
		}
	}
}

func TestEveryKindOfBadTokenIsRefused(t *testing.T) {
	v := verifier(t)
	claims := func(iss string, iat, exp time.Time) jwt.RegisteredClaims {
		return jwt.RegisteredClaims{Issuer: iss, IssuedAt: jwt.NewNumericDate(iat), ExpiresAt: jwt.NewNumericDate(exp)}
	}
	now := time.Now()
	sign := func(m jwt.SigningMethod, c jwt.RegisteredClaims, key any) string {
		tok, err := jwt.NewWithClaims(m, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	expired := sign(jwt.SigningMethodHS256, claims("telegram", now.Add(-2*time.Hour), now.Add(-time.Hour)), secret)
	noExpiry := sign(jwt.SigningMethodHS256, jwt.RegisteredClaims{Issuer: "telegram", IssuedAt: jwt.NewNumericDate(now)}, secret)
	algNone := sign(jwt.SigningMethodNone, claims("telegram", now, now.Add(time.Hour)), jwt.UnsafeAllowNoneSignatureType)
	hs512 := sign(jwt.SigningMethodHS512, claims("telegram", now, now.Add(time.Hour)), secret)
	wrongSecret, _ := Mint([]byte(strings.Repeat("x", 40)), "telegram", time.Minute)
	notListed := sign(jwt.SigningMethodHS256, claims("discord", now, now.Add(time.Hour)), secret) // right secret, adapter not on the list
	noIssuer := sign(jwt.SigningMethodHS256, claims("", now, now.Add(time.Hour)), secret)
	good, _ := Mint(secret, "telegram", time.Minute)
	tampered := good[:len(good)-3] + "AAA"

	for name, tok := range map[string]string{
		"empty": "", "garbage": "not.a.jwt", "expired": expired, "no expiry": noExpiry,
		"alg none": algNone, "other hmac size": hs512, "wrong secret": wrongSecret,
		"adapter not on the list": notListed, "no adapter named": noIssuer, "tampered signature": tampered,
	} {
		if got, err := v.Verify(tok); err == nil || !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: accepted as %q (err %v)", name, got, err)
		}
	}
}

func TestShortOrMissingConfigIsRefusedAtStartup(t *testing.T) {
	if _, err := NewVerifier(secret, nil); err == nil {
		t.Error("an empty adapter list must be refused")
	}
	if _, err := NewVerifier([]byte("short"), []string{"telegram"}); err == nil || !strings.Contains(err.Error(), "openssl rand") {
		t.Errorf("a short secret must be refused with a hint on making one: %v", err)
	}
	if _, err := Mint([]byte("short"), "telegram", time.Minute); err == nil {
		t.Error("a token must not be signed with a short secret")
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8787": true, "localhost:8787": true, "[::1]:8787": true,
		":8787": false, "0.0.0.0:8787": false, "192.168.1.5:8787": false, "nonsense": false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
