# Exercise 1: A PASETO v4.public Access-Token Service

An access token is a signed, readable claim set that a resource server verifies
statelessly. This exercise builds that service on PASETO `v4.public` — Ed25519
signatures, one primitive, no `alg` to confuse — with a hardened verifier that
enforces expiry, issuer, and audience as rules rather than reading them for show.

This module is fully self-contained. It has its own `go mod init`, defines the
issuer, the verifier, and its claims, and ships its own demo and tests. Nothing
here imports another exercise.

## What you'll build

```text
accesstoken/                 independent module: example.com/accesstoken
  go.mod                     go 1.25; requires aidanwoods.dev/go-paseto
  accesstoken.go             Issuer, Verifier, Claims; ErrInvalidToken sentinel
  cmd/
    demo/
      main.go                runnable demo: issue, verify, tamper -> reject
  accesstoken_test.go        //go:build paseto: round-trip + fail-closed table tests
```

- Files: `accesstoken.go`, `cmd/demo/main.go`, `accesstoken_test.go`.
- Implement: an `Issuer` that mints `v4.public` tokens with `iss/aud/sub/iat/nbf/exp` and a `tenant` claim, and a `Verifier` that parses with `ValidAt/IssuedBy/ForAudience` rules and returns typed `Claims`, wrapping every failure in `ErrInvalidToken`.
- Test: sign-then-verify recovers the claims; each rule fails closed (expired, wrong audience, wrong issuer); a single mutated byte is rejected; a token from a different key is rejected; a deterministic fixed-key round trip.
- Verify: `go test -tags paseto -count=1 -race ./...`

Set up the module. `go-paseto` is an external dependency, so this lesson builds
and tests only with network access to fetch it:

```bash
mkdir -p ~/go-exercises/accesstoken/cmd/demo
cd ~/go-exercises/accesstoken
go mod init example.com/accesstoken
go mod edit -go=1.25
go get aidanwoods.dev/go-paseto@latest
```

### Why v4.public, and why the verifier is small on purpose

The token here is an *access token*: a resource server reads its claims to make an
authorization decision. That is exactly the case for `v4.public` — the payload is
Ed25519-signed (authentic and tamper-evident) and base64url-readable (the resource
server, and anyone else with the token, can read the claims). We accept that
readability because there is nothing secret in an access token's claims; the next
exercise handles the confidential case with `v4.local`.

Notice what the verifier does *not* do: it does not read an algorithm from the
token and look up a primitive. There is no algorithm to read. `parser.ParseV4Public`
verifies an Ed25519 signature and nothing else is negotiable. The whole
RS256-to-HS256 / `alg:none` class of bugs is absent by construction, so the
verifier's job shrinks to enforcing the *claims* contract: is the token unexpired
at the reference time, from the issuer we trust, and scoped to our audience.

The `Issue`/`Verify` methods take an explicit `now time.Time`. That is deliberate:
a verifier that reads the wall clock internally is untestable without sleeping or
mocking. By verifying against a caller-supplied reference time we make expiry
deterministic and we make leeway an explicit choice (pass `now` plus your skew
budget). Production callers pass `time.Now()`; tests pass a fixed instant.

### The parser: opt into expiry, enforce the contract

`paseto.NewParser()` preloads a `NotExpired` rule that reads the real wall clock.
Because we want the reference time to be an argument, the verifier instead uses
`paseto.NewParserWithoutExpiryCheck()` (which preloads *no* rules) and then adds
exactly the rules it wants: `ValidAt(now)` (which requires `iat <= now <= exp` and
`nbf <= now`, so it covers both not-yet-valid and expired in one check),
`IssuedBy(issuer)`, and `ForAudience(audience)`. If any rule fails, or the
signature fails, `ParseV4Public` returns an error, and the verifier wraps it in
the package sentinel `ErrInvalidToken` with `%w` so callers can branch on
`errors.Is(err, ErrInvalidToken)` without depending on the library's error
strings.

Create `accesstoken.go`:

```go
package accesstoken

import (
	"errors"
	"fmt"
	"time"

	paseto "aidanwoods.dev/go-paseto"
)

// ErrInvalidToken wraps every reason a token fails verification: a bad
// signature, a failed rule (expired, wrong issuer, wrong audience), a wrong
// key, or a mutated body. Callers branch with errors.Is(err, ErrInvalidToken).
var ErrInvalidToken = errors.New("invalid token")

// Claims is the authenticated identity a verified access token carries.
type Claims struct {
	Subject   string
	Issuer    string
	Audience  string
	Tenant    string
	ExpiresAt time.Time
}

// Issuer mints v4.public access tokens signed with an Ed25519 secret key.
type Issuer struct {
	secret   paseto.V4AsymmetricSecretKey
	issuer   string
	audience string
	ttl      time.Duration
}

// NewIssuer builds an Issuer bound to one signing key, issuer name, audience,
// and token lifetime.
func NewIssuer(secret paseto.V4AsymmetricSecretKey, issuer, audience string, ttl time.Duration) *Issuer {
	return &Issuer{secret: secret, issuer: issuer, audience: audience, ttl: ttl}
}

// Issue mints a signed access token for subject in tenant, valid from now for
// the issuer's TTL. The returned string is a v4.public token.
func (i *Issuer) Issue(subject, tenant string, now time.Time) string {
	t := paseto.NewToken()
	t.SetIssuer(i.issuer)
	t.SetAudience(i.audience)
	t.SetSubject(subject)
	t.SetIssuedAt(now)
	t.SetNotBefore(now)
	t.SetExpiration(now.Add(i.ttl))
	t.SetString("tenant", tenant)
	return t.V4Sign(i.secret, nil)
}

// Verifier checks v4.public tokens against a public key and a claims contract.
type Verifier struct {
	public   paseto.V4AsymmetricPublicKey
	issuer   string
	audience string
}

// NewVerifier builds a Verifier that trusts one public key and requires the
// given issuer and audience.
func NewVerifier(public paseto.V4AsymmetricPublicKey, issuer, audience string) *Verifier {
	return &Verifier{public: public, issuer: issuer, audience: audience}
}

// Verify parses and verifies tainted at reference time now, enforcing signature,
// validity window, issuer, and audience. It returns typed Claims or an error
// wrapping ErrInvalidToken.
func (v *Verifier) Verify(tainted string, now time.Time) (*Claims, error) {
	parser := paseto.NewParserWithoutExpiryCheck()
	parser.AddRule(
		paseto.ValidAt(now),
		paseto.IssuedBy(v.issuer),
		paseto.ForAudience(v.audience),
	)

	tok, err := parser.ParseV4Public(v.public, tainted, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	sub, err := tok.GetSubject()
	if err != nil {
		return nil, fmt.Errorf("%w: missing sub: %v", ErrInvalidToken, err)
	}
	tenant, err := tok.GetString("tenant")
	if err != nil {
		return nil, fmt.Errorf("%w: missing tenant: %v", ErrInvalidToken, err)
	}
	exp, err := tok.GetExpiration()
	if err != nil {
		return nil, fmt.Errorf("%w: missing exp: %v", ErrInvalidToken, err)
	}
	iss, _ := tok.GetIssuer()
	aud, _ := tok.GetAudience()

	return &Claims{
		Subject:   sub,
		Issuer:    iss,
		Audience:  aud,
		Tenant:    tenant,
		ExpiresAt: exp,
	}, nil
}
```

### The runnable demo

The demo generates a fresh keypair, issues a token, verifies it, then flips one
byte of the token body and shows the verifier rejects it. To keep the output
deterministic it prints recovered claim values and booleans, never the token
string or library error text.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	paseto "aidanwoods.dev/go-paseto"

	"example.com/accesstoken"
)

func main() {
	secret := paseto.NewV4AsymmetricSecretKey()
	iss := accesstoken.NewIssuer(secret, "auth.example.com", "api.example.com", time.Hour)
	ver := accesstoken.NewVerifier(secret.Public(), "auth.example.com", "api.example.com")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	token := iss.Issue("user-42", "acme", now)

	claims, err := ver.Verify(token, now.Add(time.Minute))
	if err != nil {
		fmt.Println("unexpected verify error:", err)
		return
	}
	fmt.Printf("verified: sub=%s aud=%s tenant=%s\n", claims.Subject, claims.Audience, claims.Tenant)

	// Flip one byte in the token body; the Ed25519 signature must not match.
	body := []byte(token)
	body[len(body)-10] ^= 0x01
	_, err = ver.Verify(string(body), now.Add(time.Minute))
	fmt.Println("tampered token rejected:", err != nil)
}
```

Run it:

```bash
go run -tags paseto ./cmd/demo
```

Expected output:

```
verified: sub=user-42 aud=api.example.com tenant=acme
tampered token rejected: true
```

### Tests

The tests are behind `//go:build paseto` so they run only when you opt in with
`-tags paseto`, keeping the external dependency out of an unrelated default build.
The suite proves the two properties that matter: a legitimate token round-trips,
and every rejection path fails closed. A deterministic fixed-key case
(`NewV4AsymmetricSecretKeyFromHex` with a constant 64-byte private key) makes one
round trip reproducible across runs; the other cases use fresh keys. Failures are
asserted against the `ErrInvalidToken` sentinel with `errors.Is`, not against
library strings.

Create `accesstoken_test.go`:

```go
//go:build paseto

package accesstoken

import (
	"errors"
	"fmt"
	"testing"
	"time"

	paseto "aidanwoods.dev/go-paseto"
)

// A fixed, valid Ed25519 v4 secret key (64-byte private key, hex) so one round
// trip is reproducible. Never ship a test key in production.
const fixedSecretHex = "707261736574302d76342d7075626c69632d746573742d736565642d303100005d175c1eb70c76dd05bd394deabae91f5d8bfeaa19993a5de878be9e74101c21"

func mustIssuer(t *testing.T, secret paseto.V4AsymmetricSecretKey) *Issuer {
	t.Helper()
	return NewIssuer(secret, "auth.example.com", "api.example.com", time.Hour)
}

func TestRoundTripRecoversClaims(t *testing.T) {
	t.Parallel()

	secret, err := paseto.NewV4AsymmetricSecretKeyFromHex(fixedSecretHex)
	if err != nil {
		t.Fatalf("load fixed key: %v", err)
	}
	iss := mustIssuer(t, secret)
	ver := NewVerifier(secret.Public(), "auth.example.com", "api.example.com")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	token := iss.Issue("user-42", "acme", now)

	claims, err := ver.Verify(token, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "user-42" || claims.Tenant != "acme" {
		t.Fatalf("claims = %+v; want sub=user-42 tenant=acme", claims)
	}
	if claims.Issuer != "auth.example.com" || claims.Audience != "api.example.com" {
		t.Fatalf("claims = %+v; want iss/aud set", claims)
	}
}

func TestFailsClosed(t *testing.T) {
	t.Parallel()

	secret := paseto.NewV4AsymmetricSecretKey()
	iss := mustIssuer(t, secret)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		verifier *Verifier
		token    string
		at       time.Time
	}{
		{
			name:     "expired",
			verifier: NewVerifier(secret.Public(), "auth.example.com", "api.example.com"),
			token:    iss.Issue("user-42", "acme", now),
			at:       now.Add(2 * time.Hour), // TTL is one hour
		},
		{
			name:     "wrong audience",
			verifier: NewVerifier(secret.Public(), "auth.example.com", "other.example.com"),
			token:    iss.Issue("user-42", "acme", now),
			at:       now.Add(time.Minute),
		},
		{
			name:     "wrong issuer",
			verifier: NewVerifier(secret.Public(), "evil.example.com", "api.example.com"),
			token:    iss.Issue("user-42", "acme", now),
			at:       now.Add(time.Minute),
		},
		{
			name:     "wrong key",
			verifier: NewVerifier(paseto.NewV4AsymmetricSecretKey().Public(), "auth.example.com", "api.example.com"),
			token:    iss.Issue("user-42", "acme", now),
			at:       now.Add(time.Minute),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.verifier.Verify(tc.token, tc.at)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("Verify(%s) err = %v; want ErrInvalidToken", tc.name, err)
			}
		})
	}
}

func TestMutatedByteRejected(t *testing.T) {
	t.Parallel()

	secret := paseto.NewV4AsymmetricSecretKey()
	iss := mustIssuer(t, secret)
	ver := NewVerifier(secret.Public(), "auth.example.com", "api.example.com")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	token := iss.Issue("user-42", "acme", now)

	body := []byte(token)
	body[len(body)-8] ^= 0x01
	if _, err := ver.Verify(string(body), now.Add(time.Minute)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("mutated token err = %v; want ErrInvalidToken", err)
	}
}

func Example() {
	secret, _ := paseto.NewV4AsymmetricSecretKeyFromHex(fixedSecretHex)
	iss := NewIssuer(secret, "auth.example.com", "api.example.com", time.Hour)
	ver := NewVerifier(secret.Public(), "auth.example.com", "api.example.com")

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	token := iss.Issue("user-42", "acme", now)

	claims, err := ver.Verify(token, now.Add(time.Minute))
	fmt.Println(err == nil, claims.Subject, claims.Audience, claims.Tenant)
	// Output: true user-42 api.example.com acme
}
```

## Review

The service is correct when a token round-trips into the same claims and every
other path is an error wrapping `ErrInvalidToken`. The reasoning worth
internalizing: verification safety came for free from the *protocol*, not from
your care — `v4.public` has no algorithm field, so there was no naive
`Parse(token, keyFunc)` to get wrong, and the verifier's real work is the claims
contract. The three enforcement decisions to keep straight are that expiry is
opt-in (this verifier uses `NewParserWithoutExpiryCheck` plus `ValidAt`, so
omitting `ValidAt` would accept expired tokens), that the reference time is an
argument so tests are deterministic and leeway is explicit, and that issuer and
audience are enforced as rules, not merely read into `Claims`. The classic mistake
here is treating a `v4.public` payload as private: it is readable, so nothing
secret goes in it. Confirm correctness with `go test -tags paseto -race`, which
exercises the round trip, all four fail-closed paths, the mutated-byte path, and
the `Example`'s deterministic output.

## Resources

- [aidanwoods.dev/go-paseto](https://pkg.go.dev/aidanwoods.dev/go-paseto) — `NewToken`, `V4Sign`, `NewParser`, `ParseV4Public`, and the rule constructors used here.
- [PASETO v4 protocol specification](https://github.com/paseto-standard/paseto-spec/blob/master/docs/01-Protocol-Versions/Version4.md) — what `v4.public` signs and how the header is fixed.
- [PASETO home and rationale](https://paseto.io/) — why versioned, non-negotiable primitives replace algorithm agility.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-v4-local-encrypted-tokens.md](02-v4-local-encrypted-tokens.md)
