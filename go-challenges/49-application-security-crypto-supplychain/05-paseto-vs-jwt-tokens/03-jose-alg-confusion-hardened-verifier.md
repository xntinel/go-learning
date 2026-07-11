# Exercise 3: JOSE Alg-Confusion Lab: Break It, Then Harden the JWT Verifier

You often cannot choose PASETO — an OIDC provider or a gateway hands you a JWT and
you must verify it safely. This exercise builds the attack and the defense in one
module: a deliberately vulnerable verifier that an RS256-to-HS256 forgery walks
straight through, and a hardened `golang-jwt/v5` verifier whose tests prove the
confusion, `alg:none`, and missing/mismatched claims all fail closed.

This module is fully self-contained: its own `go mod init`, both verifiers, a
demo, and tests. Nothing here imports another exercise.

## What you'll build

```text
jwtverify/                   independent module: example.com/jwtverify
  go.mod                     go 1.25; requires github.com/golang-jwt/jwt/v5
  jwtverify.go               PublicKeyPEM, VerifyNaive (vulnerable), VerifyHardened
  cmd/
    demo/
      main.go                runnable demo: forge, break naive, reject with hardened
  jwtverify_test.go          //go:build jwt: confusion, alg=none, exp/aud/iss rules
```

- Files: `jwtverify.go`, `cmd/demo/main.go`, `jwtverify_test.go`.
- Implement: `PublicKeyPEM`; a `VerifyNaive` that reproduces the classic footgun (no `WithValidMethods`, `keyFunc` returns raw bytes); a `VerifyHardened` that pins RS256 with `WithValidMethods`, requires `exp`, and enforces issuer and audience.
- Test: the forged HS256 token is accepted by `VerifyNaive` (the vuln) and rejected by `VerifyHardened`; an `alg:none` token is rejected; a legitimate RS256 token passes; missing `exp`, wrong audience, and wrong issuer each fail with the matching `golang-jwt` sentinel.
- Verify: `go test -tags jwt -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/jwtverify/cmd/demo
cd ~/go-exercises/jwtverify
go mod init example.com/jwtverify
go mod edit -go=1.25
go get github.com/golang-jwt/jwt/v5@latest
```

### The attack, precisely

A JWT names its own algorithm in the header. The verifier for an RSA-signed API
holds the issuer's RSA *public* key — which is not a secret; it may even be
published at a JWKS endpoint. The attack: take a token, change the header to
`alg:HS256`, and compute an HMAC-SHA256 over it using the public key's serialized
bytes as the shared secret. A verifier that trusts the header will look up the
HMAC primitive and call it with the key material it has — the public key bytes —
and the MAC matches. The attacker forged a valid token using only public data.

`VerifyNaive` reproduces this. It commits two sins at once. First, it passes no
`WithValidMethods`, so it accepts whatever algorithm the token declares. Second,
its `keyFunc` returns the public key as raw `[]byte` (`pubPEM`) instead of a typed
key. That second sin is what actually makes the forgery land: when the token says
`HS256`, `golang-jwt` calls `SigningMethodHMAC.Verify` with those bytes as the
secret, recomputes the MAC the attacker computed, and reports success.

### An honest note on golang-jwt v5

`golang-jwt/v5` is partly hardened against this by Go's type system. Its HMAC
verifier requires a `[]byte` key and its RSA verifier requires an `*rsa.PublicKey`;
they refuse the wrong type with `ErrInvalidKeyType`. So a `keyFunc` that returned a
properly typed `*rsa.PublicKey` would already have caused the HS256 path to fail —
the confusion lands here only because `VerifyNaive` returns raw bytes, the exact
anti-pattern that appears when a developer stores "the key" as a PEM string and
hands it back verbatim. Do not rely on that type check as your defense, for three
reasons: it does not help when your keys genuinely are `[]byte` (HMAC or JWK-bytes
setups), it does nothing about `alg:none`, and defense in depth is the whole point.
The robust fix is to pin the algorithm out of band with `WithValidMethods`, which
is evaluated *before* the `keyFunc` runs and rejects any token whose `alg` is not
in your allowed set. That single option closes the confusion path and the `none`
path regardless of what your `keyFunc` returns.

### The hardened verifier

`VerifyHardened` pins RS256, requires an expiry, enforces issuer and audience, and
allows a small clock skew. The `keyFunc` still checks `t.Method` (belt and
suspenders) and returns a typed `*rsa.PublicKey`. When the forged HS256 token
arrives, `WithValidMethods([]string{"RS256"})` rejects it before the `keyFunc` is
ever consulted, returning an error that wraps `jwt.ErrTokenSignatureInvalid`. When
an `alg:none` token arrives, the same option rejects it. When a legitimate RS256
token arrives with a wrong audience or issuer, the corresponding option rejects it
with `jwt.ErrTokenInvalidAudience` / `jwt.ErrTokenInvalidIssuer`; a token with no
`exp` fails `WithExpirationRequired()` with `jwt.ErrTokenRequiredClaimMissing`.

Create `jwtverify.go`:

```go
package jwtverify

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// PublicKeyPEM encodes an RSA public key as PKIX PEM bytes. In the classic
// RS256->HS256 confusion these public bytes double as the attacker's HMAC secret.
func PublicKeyPEM(pub *rsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// VerifyNaive is the VULNERABLE verifier, shown only to demonstrate the attack.
// It commits two sins: it never pins the algorithm (no WithValidMethods), and its
// keyFunc returns raw key bytes. Because golang-jwt dispatches on the token's own
// alg header, an attacker can present an HS256 token signed with pubPEM as the
// HMAC secret and this function "verifies" it. Never write a keyFunc like this.
func VerifyNaive(tokenString string, pubPEM []byte) (*jwt.RegisteredClaims, error) {
	claims := &jwt.RegisteredClaims{}
	if _, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		return pubPEM, nil
	}); err != nil {
		return nil, err
	}
	return claims, nil
}

// VerifyHardened is the correct verifier. WithValidMethods pins RS256 and is
// checked before the keyFunc runs, so algorithm confusion and alg=none are
// rejected up front; the keyFunc adds a defense-in-depth method check and returns
// a properly typed *rsa.PublicKey. Expiry is required and issuer and audience are
// enforced, with a small leeway for clock skew.
func VerifyHardened(tokenString string, pub *rsa.PublicKey, issuer, audience string) (*jwt.RegisteredClaims, error) {
	claims := &jwt.RegisteredClaims{}
	if _, err := jwt.ParseWithClaims(tokenString, claims,
		func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("%w: unexpected alg %q", jwt.ErrTokenSignatureInvalid, t.Method.Alg())
			}
			return pub, nil
		},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithLeeway(30*time.Second),
	); err != nil {
		return nil, err
	}
	return claims, nil
}
```

### The runnable demo

The demo generates an RSA key, forges an HS256 token using the public key PEM as
the HMAC secret, and shows the naive verifier accepting it, the hardened verifier
rejecting it, and the hardened verifier accepting a legitimate RS256 token. Output
is deterministic booleans; no key material is printed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"example.com/jwtverify"
)

func main() {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		fmt.Println("keygen:", err)
		return
	}
	pub := &priv.PublicKey
	pubPEM, err := jwtverify.PublicKeyPEM(pub)
	if err != nil {
		fmt.Println("pem:", err)
		return
	}

	now := time.Now()
	claims := jwt.RegisteredClaims{
		Issuer:    "auth.example.com",
		Subject:   "user-42",
		Audience:  jwt.ClaimStrings{"api.example.com"},
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(now),
	}

	// Attacker forges HS256 using the public key bytes as the HMAC secret.
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(pubPEM)
	if err != nil {
		fmt.Println("forge:", err)
		return
	}

	_, naiveErr := jwtverify.VerifyNaive(forged, pubPEM)
	fmt.Println("naive accepts forged token:", naiveErr == nil)

	_, hardErr := jwtverify.VerifyHardened(forged, pub, "auth.example.com", "api.example.com")
	fmt.Println("hardened rejects forged token:", hardErr != nil)

	legit, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(priv)
	if err != nil {
		fmt.Println("sign legit:", err)
		return
	}
	_, legitErr := jwtverify.VerifyHardened(legit, pub, "auth.example.com", "api.example.com")
	fmt.Println("hardened accepts legit token:", legitErr == nil)
}
```

Run it:

```bash
go run -tags jwt ./cmd/demo
```

Expected output:

```
naive accepts forged token: true
hardened rejects forged token: true
hardened accepts legit token: true
```

### Tests

Behind `//go:build jwt`, the suite makes the attack a regression test. `TestAlgConfusion`
asserts the forged token is *accepted* by `VerifyNaive` (documenting the vuln) and
*rejected* by `VerifyHardened` with `jwt.ErrTokenSignatureInvalid`. `TestAlgNoneRejected`
covers `alg:none` from both sides. `TestHardenedAcceptsLegit` proves a real RS256
token passes, and `TestHardenedClaimRules` proves missing `exp`, wrong audience,
and wrong issuer each fail with the matching sentinel via `errors.Is`.

Create `jwtverify_test.go`:

```go
//go:build jwt

package jwtverify

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return k
}

func baseClaims(now time.Time) jwt.RegisteredClaims {
	return jwt.RegisteredClaims{
		Issuer:    "auth.example.com",
		Subject:   "user-42",
		Audience:  jwt.ClaimStrings{"api.example.com"},
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(now),
	}
}

func signRS256(t *testing.T, c jwt.RegisteredClaims, key *rsa.PrivateKey) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, c).SignedString(key)
	if err != nil {
		t.Fatalf("sign RS256: %v", err)
	}
	return s
}

func signHS256(t *testing.T, c jwt.RegisteredClaims, secret []byte) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(secret)
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	return s
}

func TestAlgConfusion(t *testing.T) {
	t.Parallel()
	priv := testKey(t)
	pub := &priv.PublicKey
	pubPEM, err := PublicKeyPEM(pub)
	if err != nil {
		t.Fatalf("PublicKeyPEM: %v", err)
	}

	now := time.Now()
	forged := signHS256(t, baseClaims(now), pubPEM)

	// The vulnerable verifier accepts the forgery: this documents the bug.
	if _, err := VerifyNaive(forged, pubPEM); err != nil {
		t.Fatalf("naive verifier should have (wrongly) accepted the forged token, got %v", err)
	}

	// The hardened verifier rejects it at method validation.
	if _, err := VerifyHardened(forged, pub, "auth.example.com", "api.example.com"); !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Fatalf("hardened err = %v; want ErrTokenSignatureInvalid", err)
	}
}

func TestAlgNoneRejected(t *testing.T) {
	t.Parallel()
	priv := testKey(t)
	pub := &priv.PublicKey
	pubPEM, _ := PublicKeyPEM(pub)

	now := time.Now()
	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, baseClaims(now)).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}

	if _, err := VerifyHardened(none, pub, "auth.example.com", "api.example.com"); !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Fatalf("hardened none err = %v; want ErrTokenSignatureInvalid", err)
	}
	// v5 also refuses none by default: the keyFunc did not return the unsafe
	// sentinel, so even the naive verifier rejects it.
	if _, err := VerifyNaive(none, pubPEM); !errors.Is(err, jwt.ErrTokenUnverifiable) {
		t.Fatalf("naive none err = %v; want ErrTokenUnverifiable", err)
	}
}

func TestHardenedAcceptsLegit(t *testing.T) {
	t.Parallel()
	priv := testKey(t)
	pub := &priv.PublicKey

	now := time.Now()
	legit := signRS256(t, baseClaims(now), priv)

	claims, err := VerifyHardened(legit, pub, "auth.example.com", "api.example.com")
	if err != nil {
		t.Fatalf("VerifyHardened: %v", err)
	}
	if claims.Subject != "user-42" {
		t.Fatalf("subject = %q; want user-42", claims.Subject)
	}
}

func TestHardenedClaimRules(t *testing.T) {
	t.Parallel()
	priv := testKey(t)
	pub := &priv.PublicKey
	now := time.Now()

	noExp := baseClaims(now)
	noExp.ExpiresAt = nil

	wrongAud := baseClaims(now)
	wrongAud.Audience = jwt.ClaimStrings{"other.example.com"}

	wrongIss := baseClaims(now)
	wrongIss.Issuer = "evil.example.com"

	tests := []struct {
		name   string
		token  string
		target error
	}{
		{"missing exp", signRS256(t, noExp, priv), jwt.ErrTokenRequiredClaimMissing},
		{"wrong audience", signRS256(t, wrongAud, priv), jwt.ErrTokenInvalidAudience},
		{"wrong issuer", signRS256(t, wrongIss, priv), jwt.ErrTokenInvalidIssuer},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := VerifyHardened(tc.token, pub, "auth.example.com", "api.example.com")
			if !errors.Is(err, tc.target) {
				t.Fatalf("%s err = %v; want %v", tc.name, err, tc.target)
			}
		})
	}
}

func Example() {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub := &priv.PublicKey
	pubPEM, _ := PublicKeyPEM(pub)

	now := time.Now()
	forged, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, baseClaims(now)).SignedString(pubPEM)

	_, naiveErr := VerifyNaive(forged, pubPEM)
	_, hardErr := VerifyHardened(forged, pub, "auth.example.com", "api.example.com")
	fmt.Println(naiveErr == nil, hardErr != nil)
	// Output: true true
}
```

## Review

The lab is correct when the forged HS256 token is accepted by `VerifyNaive` and
rejected by `VerifyHardened`, `alg:none` is rejected, a legitimate RS256 token
passes, and each claim rule fails with its matching sentinel. The lesson is the
asymmetry: PASETO gave you safety by removing the algorithm choice, whereas JWT
demands you *impose* that safety in configuration. The single most important line
is `WithValidMethods([]string{"RS256"})`, because it is evaluated before the
`keyFunc` and closes both the confusion and the `none` paths no matter what the
`keyFunc` returns. The traps to avoid: calling `jwt.Parse`/`ParseWithClaims`
without pinning methods and returning a key from the `keyFunc` without checking
`token.Method`; returning raw key bytes instead of a typed key; and reading claims
without enforcing them (the audience and issuer are only safe because the options
turn a mismatch into an error). Confirm with `go test -tags jwt -race`, which runs
the attack as a regression alongside the positive and claim-rule cases.

## Resources

- [github.com/golang-jwt/jwt/v5](https://pkg.go.dev/github.com/golang-jwt/jwt/v5) — `ParseWithClaims`, `WithValidMethods`, `WithExpirationRequired`, `RegisteredClaims`, and the error sentinels.
- [RFC 8725 — JSON Web Token Best Current Practices](https://datatracker.ietf.org/doc/html/rfc8725) — algorithm confusion, `none`, and audience validation guidance.
- [golang-jwt security policy and advisories](https://github.com/golang-jwt/jwt/security) — the library's own notes on method validation.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-v4-local-encrypted-tokens.md](02-v4-local-encrypted-tokens.md) | Next: [../06-oauth2-oidc-flows/00-concepts.md](../06-oauth2-oidc-flows/00-concepts.md)
