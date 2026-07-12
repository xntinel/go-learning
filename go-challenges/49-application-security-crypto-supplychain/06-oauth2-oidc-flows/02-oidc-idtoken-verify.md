# Exercise 2: Offline OIDC ID Token Verification

The single most security-critical line in an OIDC system is where a resource
server or client decides an ID token is trustworthy. This exercise builds that
decision as a validator that runs the full checklist — signature, issuer,
audience, expiry, algorithm, and nonce — with no network round trip, so it is
fully deterministic and gate-shaped.

This module is self-contained: it mints its own RS256 tokens with the standard
library and feeds the public key into a `StaticKeySet`, so no live JWKS endpoint
is involved.

## What you'll build

```text
idtokenverify/                independent module: example.com/idtokenverify
  go.mod                      go 1.26; requires github.com/coreos/go-oidc/v3
  verify.go                   Validator{Validate}; Claims; ErrNonceMismatch, ErrAccessTokenHash
  cmd/
    demo/
      main.go                 mint an RS256 token, verify it, print the claims
  verify_test.go              hand-minted RS256 JWTs; table of every rejection path
```

- Files: `verify.go`, `cmd/demo/main.go`, `verify_test.go`.
- Implement: a `Validator` wrapping an `oidc.IDTokenVerifier` built from an `oidc.StaticKeySet`, plus caller-side nonce enforcement and custom-claims unmarshal.
- Test: mint RS256 JWTs with `crypto/rsa`; table-test the happy path plus wrong audience, expired, tampered signature, wrong key, alg mismatch, nonce mismatch, and at_hash mismatch.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get github.com/coreos/go-oidc/v3/oidc@latest
```

### Why the library does most, but not all, of the checklist

`oidc.NewVerifier(issuer, keySet, config)` builds a verifier that, on
`Verify(ctx, rawIDToken)`, checks: the signature against the `keySet`, that the
signing algorithm is one of `config.SupportedSigningAlgs`, that `iss` equals the
issuer string, that `aud` contains `config.ClientID`, and that `exp`/`iat` are
within skew of `config.Now()`. Pinning `SupportedSigningAlgs` to
`[]string{oidc.RS256}` is what closes the JOSE alg-confusion hole: a token that
arrives as `alg: none` or `alg: HS256` is rejected before its signature is even
considered.

What `Verify` deliberately does **not** do is check the `nonce`. That is by
design: the library has no way to know which nonce your `/login` handler stored
for this browser. `IDToken.Nonce` is exposed precisely so your code compares it.
Forgetting that comparison silently disables OIDC replay protection, so the
`Validate` method here does it explicitly, in constant time, and returns the
package sentinel `ErrNonceMismatch` on failure.

`StaticKeySet{PublicKeys: []crypto.PublicKey{...}}` is the offline key source: it
tries each provided public key against the token's signature and ignores JWKS
fetching entirely. In production you would use `oidc.NewRemoteKeySet` so key
rotation is handled for you; `StaticKeySet` is for tests and pinned keys, which is
exactly what makes this exercise deterministic.

`IDToken.VerifyAccessToken(accessToken)` recomputes the `at_hash` (leftmost half
of the SHA-256 of the access token, for an RS256 ID token) and compares it to the
`at_hash` claim, proving the ID token and access token were issued together. That
is the mechanism that lets you *bind* the two rather than trust them separately.

Create `verify.go`:

```go
package idtokenverify

import (
	"context"
	"crypto"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Sentinel errors for the checks the library does not perform for us.
var (
	ErrNonceMismatch   = errors.New("idtoken: nonce mismatch")
	ErrAccessTokenHash = errors.New("idtoken: access token hash (at_hash) mismatch")
)

// Claims are the custom claims we extract after the ID token verifies.
type Claims struct {
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Groups        []string `json:"groups"`
}

// Validator verifies OIDC ID tokens offline against a fixed set of public keys.
type Validator struct {
	verifier *oidc.IDTokenVerifier
}

// NewValidator builds a validator. algs pins the accepted signing algorithms
// (e.g. []string{oidc.RS256}); now is a clock seam for deterministic expiry
// tests (nil means time.Now).
func NewValidator(issuer string, keys []crypto.PublicKey, clientID string, algs []string, now func() time.Time) *Validator {
	ks := &oidc.StaticKeySet{PublicKeys: keys}
	cfg := &oidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: algs,
		Now:                  now,
	}
	return &Validator{verifier: oidc.NewVerifier(issuer, ks, cfg)}
}

// Validate runs the full checklist: library-side signature/iss/aud/exp/alg, then
// caller-side nonce, then optional at_hash binding, then custom claims. A missing
// accessToken skips the at_hash check.
func (v *Validator) Validate(ctx context.Context, rawIDToken, wantNonce, accessToken string) (*oidc.IDToken, *Claims, error) {
	idt, err := v.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, nil, fmt.Errorf("verify id token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(wantNonce)) != 1 {
		return nil, nil, fmt.Errorf("%w", ErrNonceMismatch)
	}
	if accessToken != "" {
		if err := idt.VerifyAccessToken(accessToken); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrAccessTokenHash, err)
		}
	}
	var c Claims
	if err := idt.Claims(&c); err != nil {
		return nil, nil, fmt.Errorf("decode claims: %w", err)
	}
	return idt, &c, nil
}
```

### The runnable demo

The demo mints a fresh RSA key, hand-assembles a valid RS256 ID token (header,
payload, `rsa.SignPKCS1v15` over the SHA-256 of the signing input), verifies it,
and prints the claims. The key is random each run but the claims are fixed, so the
output is stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"example.com/idtokenverify"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// atHash is the leftmost half of SHA-256 of the access token (RS256 -> SHA-256),
// base64url encoded, as defined by OIDC Core.
func atHash(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return b64(sum[:len(sum)/2])
}

func mint(key *rsa.PrivateKey, claims map[string]any) string {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "demo-key"}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signingInput := b64(hb) + "." + b64(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return signingInput + "." + b64(sig)
}

func main() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const issuer = "https://idp.example.com"
	const clientID = "web-app"
	const accessToken = "opaque-access-token"

	raw := mint(key, map[string]any{
		"iss":            issuer,
		"aud":            clientID,
		"sub":            "user-123",
		"exp":            now.Add(time.Hour).Unix(),
		"iat":            now.Unix(),
		"nonce":          "n-login-1",
		"at_hash":        atHash(accessToken),
		"email":          "alice@example.com",
		"email_verified": true,
		"groups":         []string{"eng", "admins"},
	})

	v := idtokenverify.NewValidator(issuer, []crypto.PublicKey{&key.PublicKey}, clientID,
		[]string{"RS256"}, func() time.Time { return now })

	idt, claims, err := v.Validate(context.Background(), raw, "n-login-1", accessToken)
	if err != nil {
		panic(err)
	}
	fmt.Println("subject:", idt.Subject)
	fmt.Println("email:", claims.Email, "verified:", claims.EmailVerified)
	fmt.Println("groups:", claims.Groups)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
subject: user-123
email: alice@example.com verified: true
groups: [eng admins]
```

### Tests

The tests mint RS256 tokens with a helper so no external signing library is
needed, then table-test every rejection. The clock is pinned via `oidc.Config.Now`
so the expired case is deterministic. Each row varies exactly one thing — a wrong
audience, a past `exp`, a flipped signature byte, a different key, an unaccepted
alg, a mismatched nonce, or a tampered access token — and asserts the specific
failure. The two caller-side checks assert against the package sentinels with
`errors.Is`.

Create `verify_test.go`:

```go
package idtokenverify

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	testIssuer   = "https://idp.example.com"
	testClientID = "web-app"
)

var testNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func atHash(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return b64(sum[:len(sum)/2])
}

// signRS256 hand-assembles a compact JWS with the given alg header and RSA key.
func signRS256(t *testing.T, alg string, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": alg, "typ": "JWT", "kid": "test-key"}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signingInput := b64(hb) + "." + b64(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + b64(sig)
}

func validClaims() map[string]any {
	return map[string]any{
		"iss":            testIssuer,
		"aud":            testClientID,
		"sub":            "user-123",
		"exp":            testNow.Add(time.Hour).Unix(),
		"iat":            testNow.Unix(),
		"nonce":          "n-1",
		"email":          "alice@example.com",
		"email_verified": true,
		"groups":         []string{"eng", "admins"},
	}
}

func TestValidateHappyPath(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	claims := validClaims()
	claims["at_hash"] = atHash("access-abc")
	raw := signRS256(t, "RS256", key, claims)

	v := NewValidator(testIssuer, []crypto.PublicKey{&key.PublicKey}, testClientID,
		[]string{oidc.RS256}, func() time.Time { return testNow })

	idt, got, err := v.Validate(context.Background(), raw, "n-1", "access-abc")
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if idt.Subject != "user-123" {
		t.Errorf("Subject = %q, want user-123", idt.Subject)
	}
	if got.Email != "alice@example.com" || !got.EmailVerified {
		t.Errorf("claims = %+v, want alice@example.com verified", got)
	}
	if len(got.Groups) != 2 || got.Groups[0] != "eng" {
		t.Errorf("Groups = %v, want [eng admins]", got.Groups)
	}
}

func TestValidateRejections(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		token       func() string
		algs        []string
		wantNonce   string
		accessToken string
		wantSentAs  error // non-nil to assert errors.Is
	}{
		{
			name: "wrong audience",
			token: func() string {
				c := validClaims()
				c["aud"] = "other-client"
				return signRS256(t, "RS256", key, c)
			},
			algs: []string{oidc.RS256}, wantNonce: "n-1",
		},
		{
			name: "expired",
			token: func() string {
				c := validClaims()
				c["exp"] = testNow.Add(-time.Hour).Unix()
				return signRS256(t, "RS256", key, c)
			},
			algs: []string{oidc.RS256}, wantNonce: "n-1",
		},
		{
			name: "tampered signature",
			token: func() string {
				raw := signRS256(t, "RS256", key, validClaims())
				i := strings.LastIndex(raw, ".") // start of the signature segment
				return raw[:i+1] + flip(raw[i+1:i+2]) + raw[i+2:]
			},
			algs: []string{oidc.RS256}, wantNonce: "n-1",
		},
		{
			name: "wrong signing key",
			token: func() string {
				return signRS256(t, "RS256", otherKey, validClaims())
			},
			algs: []string{oidc.RS256}, wantNonce: "n-1",
		},
		{
			name: "alg not accepted",
			token: func() string {
				return signRS256(t, "RS256", key, validClaims())
			},
			algs: []string{oidc.ES256}, wantNonce: "n-1",
		},
		{
			name: "nonce mismatch",
			token: func() string {
				return signRS256(t, "RS256", key, validClaims())
			},
			algs: []string{oidc.RS256}, wantNonce: "wrong-nonce",
			wantSentAs: ErrNonceMismatch,
		},
		{
			name: "at_hash mismatch",
			token: func() string {
				c := validClaims()
				c["at_hash"] = atHash("real-access-token")
				return signRS256(t, "RS256", key, c)
			},
			algs: []string{oidc.RS256}, wantNonce: "n-1",
			accessToken: "different-access-token",
			wantSentAs:  ErrAccessTokenHash,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := NewValidator(testIssuer, []crypto.PublicKey{&key.PublicKey}, testClientID,
				tc.algs, func() time.Time { return testNow })
			_, _, err := v.Validate(context.Background(), tc.token(), tc.wantNonce, tc.accessToken)
			if err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
			if tc.wantSentAs != nil && !errors.Is(err, tc.wantSentAs) {
				t.Errorf("Validate() error = %v, want errors.Is %v", err, tc.wantSentAs)
			}
		})
	}
}

// flip changes a base64url character so the signature no longer verifies (while
// staying valid base64url). Applied to the first signature character, it always
// alters real signature bytes.
func flip(s string) string {
	if s == "" {
		return s
	}
	last := s[len(s)-1]
	repl := byte('A')
	if last == 'A' {
		repl = 'B'
	}
	return s[:len(s)-1] + string(repl)
}

func Example_atHash() {
	// OIDC at_hash: leftmost 128 bits of SHA-256(access_token), base64url.
	sum := sha256.Sum256([]byte("jHkWEdUXMU1BwAsC4vtUsZwnNvTIxEl0z9K3vx5KntU"))
	fmt.Println(base64.RawURLEncoding.EncodeToString(sum[:16]))
	// Output: T7VF8gELfbwjUBkK04GEhg
}
```

## Review

The validator is correct when every checklist item is enforced and none is
skippable. The library covers signature, `iss`, `aud`, `exp`, and the
`SupportedSigningAlgs` pin; `Validate` adds the two the library cannot: the nonce
comparison (constant-time, returning `ErrNonceMismatch`) and the optional
`at_hash` binding (returning `ErrAccessTokenHash`). The table proves each rejection
independently — the "alg not accepted" row is the alg-confusion defense, and it
fails only because `SupportedSigningAlgs` is pinned.

The mistakes to avoid: never set `SkipClientIDCheck`, `SkipExpiryCheck`,
`SkipIssuerCheck`, or `InsecureSkipSignatureCheck` to make a test pass — drive
`oidc.Config.Now` with a fixed clock instead, as these tests do. Never assume the
library checked the nonce; the explicit comparison is yours to make. And never
substitute a UserInfo call for ID-token verification — UserInfo tells you what an
access token can fetch, not that a signed assertion of identity is valid. For a
live IdP swap `StaticKeySet` for `oidc.NewRemoteKeySet` so key rotation is handled;
the `StaticKeySet` here exists to keep verification offline and deterministic.

## Resources

- [`github.com/coreos/go-oidc/v3/oidc`](https://pkg.go.dev/github.com/coreos/go-oidc/v3/oidc) — `NewVerifier`, `StaticKeySet`, `Config`, `IDToken`, `VerifyAccessToken`.
- [OpenID Connect Core 1.0](https://openid.net/specs/openid-connect-core-1_0.html) — ID Token validation rules, `nonce`, and the `at_hash` definition.
- [RFC 7518 — JSON Web Algorithms](https://datatracker.ietf.org/doc/html/rfc7518) — the JOSE `alg` values and why `none` must never be accepted.

---

Back to [01-authcode-pkce-flow.md](01-authcode-pkce-flow.md) | Next: [03-client-credentials-m2m.md](03-client-credentials-m2m.md)
