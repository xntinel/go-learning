# Exercise 2: Opaque v4.local Session Tokens with Rotation and Binding

When the payload must stay server-confidential — session state, internal ids,
roles — a signed token leaks it. This exercise builds an encrypted `v4.local`
session service (XChaCha20-Poly1305) with two production concerns wired in: a
`kid`-in-footer keyring so keys can rotate, and an implicit assertion that binds
each token to its tenant so it cannot be replayed across tenants.

This module is fully self-contained: its own `go mod init`, its own service,
demo, and tests. Nothing here imports another exercise.

## What you'll build

```text
sessiontoken/                independent module: example.com/sessiontoken
  go.mod                     go 1.25; requires aidanwoods.dev/go-paseto
  sessiontoken.go            Service, SessionData; ErrInvalidToken, ErrUnknownKeyID
  cmd/
    demo/
      main.go                runnable demo: issue, prove opacity, verify, reject cross-tenant
  sessiontoken_test.go       //go:build paseto: round-trip, confidentiality, rotation, binding
```

- Files: `sessiontoken.go`, `cmd/demo/main.go`, `sessiontoken_test.go`.
- Implement: a `Service` holding a `kid`-keyed keyring and a current `kid`; `Issue` encrypts a `v4.local` token with a `kid` footer and a tenant-derived implicit assertion; `Verify` reads the footer `kid` with `UnsafeParseFooter`, selects the key, and decrypts with the same implicit assertion.
- Test: encrypt-then-decrypt recovers claims; the raw token does not contain a plaintext claim value (confidentiality); a wrong key fails; a changed implicit assertion fails (binding); a token under a known `kid` decrypts while an unknown `kid` is rejected with `ErrUnknownKeyID`.
- Verify: `go test -tags paseto -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/05-paseto-vs-jwt-tokens/02-v4-local-encrypted-tokens/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/05-paseto-vs-jwt-tokens/02-v4-local-encrypted-tokens
go mod edit -go=1.25
go get aidanwoods.dev/go-paseto@latest
```

### Why local, why a footer kid, why an implicit assertion

A session token holds data the client should never read: which roles the session
has, internal identifiers, anything you would not print in a log. `v4.local`
encrypts the payload with XChaCha20-Poly1305, so the token is *opaque* — a random
blob to anyone but the holder of the symmetric key — while still being
tamper-evident. That is the difference from a signed token (or a `v4.public` one),
where the claims are merely readable.

A single symmetric key that never changes is a liability: if it leaks, every token
ever issued is compromised and you have no way to roll forward. So the service
keeps a *keyring* — several keys indexed by a short key id (`kid`) — and stamps
the `kid` into the token's **footer**. The footer is authenticated but not
encrypted, which is exactly right here: the verifier must read the `kid` *before*
it can pick a key to decrypt with, and it can, because the footer is plaintext;
and an attacker cannot swap the `kid` to point at a different key, because any
change to the footer breaks the MAC and decryption fails. Rotation then means:
add a new key to the ring, point `currentKID` at it, and keep the old keys around
until the tokens they signed have expired.

The implicit assertion is the second lever. We derive a byte string from the
tenant (`"paseto-session|tenant=" + tenant`) and pass it as the trailing
`implicit []byte` to both `V4Encrypt` and `ParseV4Local`. It is mixed into the
authentication tag but never written into the token, so a token minted for tenant
`acme` only decrypts when the verifier asserts `acme`. A token stolen from one
tenant's traffic cannot be replayed against another tenant, and the binding value
never appears on the wire. The contract is strict: the two sides must pass
byte-identical implicit assertions, so both derive it from the same helper.

### Reading the footer before decrypting

`parser.UnsafeParseFooter(paseto.V4Local, tainted)` returns the footer bytes
*without* verifying the token — "unsafe" only means the signature/MAC has not been
checked yet. That is acceptable for routing: we use the `kid` only to *select* a
candidate key, and the subsequent `ParseV4Local` is what actually authenticates.
If the footer was tampered with to name a different `kid`, one of two things
happens: the named `kid` is not in the ring (`ErrUnknownKeyID`), or it is, but the
key is wrong for this ciphertext and decryption fails (`ErrInvalidToken`). Either
way the token is rejected. The claims we care about are read only after a
successful decrypt, including the non-string `roles` slice via
`Token.Get("roles", &roles)`, which round-trips JSON into a `[]string`.

Create `sessiontoken.go`:

```go
package sessiontoken

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	paseto "aidanwoods.dev/go-paseto"
)

// ErrInvalidToken wraps every cryptographic or rule failure (wrong key, wrong
// implicit assertion, tampered body, expired). ErrUnknownKeyID is returned when
// the footer names a kid the keyring does not hold.
var (
	ErrInvalidToken = errors.New("invalid token")
	ErrUnknownKeyID = errors.New("unknown key id")
)

// SessionData is the confidential payload carried inside a v4.local token.
type SessionData struct {
	Subject string
	Roles   []string
	Tenant  string
}

type footerMeta struct {
	KID string `json:"kid"`
}

// implicitFor derives the implicit assertion that binds a token to a tenant.
// Encrypt and decrypt must pass byte-identical bytes, so both call this.
func implicitFor(tenant string) []byte {
	return []byte("paseto-session|tenant=" + tenant)
}

// Service issues and verifies encrypted v4.local session tokens against a
// kid-keyed keyring, rotating by changing currentKID.
type Service struct {
	keys       map[string]paseto.V4SymmetricKey
	currentKID string
	ttl        time.Duration
}

// NewService builds a Service over a keyring; currentKID must name a key in it.
func NewService(keys map[string]paseto.V4SymmetricKey, currentKID string, ttl time.Duration) (*Service, error) {
	if _, ok := keys[currentKID]; !ok {
		return nil, fmt.Errorf("%w: current %q", ErrUnknownKeyID, currentKID)
	}
	return &Service{keys: keys, currentKID: currentKID, ttl: ttl}, nil
}

// Issue encrypts data into a v4.local token under the current key, stamping the
// kid into the footer and binding the token to data.Tenant via the implicit
// assertion.
func (s *Service) Issue(data SessionData, now time.Time) (string, error) {
	key, ok := s.keys[s.currentKID]
	if !ok {
		return "", fmt.Errorf("%w: current %q", ErrUnknownKeyID, s.currentKID)
	}

	t := paseto.NewToken()
	t.SetSubject(data.Subject)
	t.SetIssuedAt(now)
	t.SetNotBefore(now)
	t.SetExpiration(now.Add(s.ttl))
	if err := t.Set("roles", data.Roles); err != nil {
		return "", fmt.Errorf("set roles: %w", err)
	}

	footer, err := json.Marshal(footerMeta{KID: s.currentKID})
	if err != nil {
		return "", fmt.Errorf("marshal footer: %w", err)
	}
	t.SetFooter(footer)

	return t.V4Encrypt(key, implicitFor(data.Tenant)), nil
}

// Verify selects the key named by the footer kid, decrypts and authenticates the
// token at reference time now, and re-derives the tenant binding. It returns the
// recovered SessionData or an error wrapping ErrUnknownKeyID / ErrInvalidToken.
func (s *Service) Verify(tainted, tenant string, now time.Time) (*SessionData, error) {
	parser := paseto.NewParserWithoutExpiryCheck()
	parser.SetRules([]paseto.Rule{paseto.ValidAt(now)})

	footerBytes, err := parser.UnsafeParseFooter(paseto.V4Local, tainted)
	if err != nil {
		return nil, fmt.Errorf("%w: bad footer: %v", ErrInvalidToken, err)
	}
	var meta footerMeta
	if err := json.Unmarshal(footerBytes, &meta); err != nil {
		return nil, fmt.Errorf("%w: footer json: %v", ErrInvalidToken, err)
	}
	key, ok := s.keys[meta.KID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKeyID, meta.KID)
	}

	tok, err := parser.ParseV4Local(key, tainted, implicitFor(tenant))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	sub, err := tok.GetSubject()
	if err != nil {
		return nil, fmt.Errorf("%w: missing sub: %v", ErrInvalidToken, err)
	}
	var roles []string
	if err := tok.Get("roles", &roles); err != nil {
		return nil, fmt.Errorf("%w: missing roles: %v", ErrInvalidToken, err)
	}

	return &SessionData{Subject: sub, Roles: roles, Tenant: tenant}, nil
}
```

### The runnable demo

The demo builds a two-key ring, issues a session, proves the token is opaque (the
role name and subject do not appear as plaintext in the token string), verifies
it, and shows a cross-tenant verification is rejected. Output is deterministic:
booleans and recovered values only.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"
	"time"

	paseto "aidanwoods.dev/go-paseto"

	"example.com/sessiontoken"
)

func main() {
	keys := map[string]paseto.V4SymmetricKey{
		"key-a": paseto.NewV4SymmetricKey(),
		"key-b": paseto.NewV4SymmetricKey(),
	}
	svc, err := sessiontoken.NewService(keys, "key-b", time.Hour)
	if err != nil {
		fmt.Println("setup error:", err)
		return
	}

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := sessiontoken.SessionData{Subject: "user-42", Roles: []string{"admin", "billing"}, Tenant: "acme"}

	token, err := svc.Issue(data, now)
	if err != nil {
		fmt.Println("issue error:", err)
		return
	}
	fmt.Println("payload opaque:", !strings.Contains(token, "admin") && !strings.Contains(token, "user-42"))

	got, err := svc.Verify(token, "acme", now.Add(time.Minute))
	if err != nil {
		fmt.Println("verify error:", err)
		return
	}
	fmt.Printf("verified: sub=%s roles=%s\n", got.Subject, strings.Join(got.Roles, ","))

	_, err = svc.Verify(token, "evil-tenant", now.Add(time.Minute))
	fmt.Println("cross-tenant rejected:", err != nil)
}
```

Run it:

```bash
go run -tags paseto ./cmd/demo
```

Expected output:

```
payload opaque: true
verified: sub=user-42 roles=admin,billing
cross-tenant rejected: true
```

### Tests

Behind `//go:build paseto`, the table proves each property in isolation:
confidentiality (the plaintext role and subject do not appear in the token
string), rotation (a token minted under one `kid` decrypts when the verifier holds
that `kid`, and an unknown `kid` is rejected with `ErrUnknownKeyID`), wrong-key
rejection, and binding (a different tenant fails). A fixed symmetric key
(`V4SymmetricKeyFromHex`) makes the round-trip reproducible.

Create `sessiontoken_test.go`:

```go
//go:build paseto

package sessiontoken

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	paseto "aidanwoods.dev/go-paseto"
)

// A fixed 32-byte symmetric key (hex) for reproducible round trips. Test only.
const fixedKeyHex = "0011223344556677889900aabbccddeeff00112233445566778899aabbccddee"

func mustKey(t *testing.T, hexKey string) paseto.V4SymmetricKey {
	t.Helper()
	k, err := paseto.V4SymmetricKeyFromHex(hexKey)
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	return k
}

func TestRoundTripAndConfidentiality(t *testing.T) {
	t.Parallel()

	keys := map[string]paseto.V4SymmetricKey{"key-a": mustKey(t, fixedKeyHex)}
	svc, err := NewService(keys, "key-a", time.Hour)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := SessionData{Subject: "user-42", Roles: []string{"admin", "billing"}, Tenant: "acme"}

	token, err := svc.Issue(data, now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if strings.Contains(token, "admin") || strings.Contains(token, "user-42") {
		t.Fatalf("token leaked a plaintext claim value: %q", token)
	}

	got, err := svc.Verify(token, "acme", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != "user-42" || strings.Join(got.Roles, ",") != "admin,billing" {
		t.Fatalf("got = %+v; want sub=user-42 roles=[admin billing]", got)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := SessionData{Subject: "user-42", Roles: []string{"admin"}, Tenant: "acme"}

	issuer, _ := NewService(map[string]paseto.V4SymmetricKey{"key-a": paseto.NewV4SymmetricKey()}, "key-a", time.Hour)
	token, err := issuer.Issue(data, now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Same kid, different key material: footer routes to key-a, decrypt fails.
	verifier, _ := NewService(map[string]paseto.V4SymmetricKey{"key-a": paseto.NewV4SymmetricKey()}, "key-a", time.Hour)
	if _, err := verifier.Verify(token, "acme", now.Add(time.Minute)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong key err = %v; want ErrInvalidToken", err)
	}
}

func TestUnknownKeyIDRejected(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := SessionData{Subject: "user-42", Roles: []string{"admin"}, Tenant: "acme"}

	// Issue under kid "key-z".
	issuer, _ := NewService(map[string]paseto.V4SymmetricKey{"key-z": paseto.NewV4SymmetricKey()}, "key-z", time.Hour)
	token, err := issuer.Issue(data, now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Verifier's ring has no "key-z".
	verifier, _ := NewService(map[string]paseto.V4SymmetricKey{"key-a": paseto.NewV4SymmetricKey()}, "key-a", time.Hour)
	if _, err := verifier.Verify(token, "acme", now.Add(time.Minute)); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("unknown kid err = %v; want ErrUnknownKeyID", err)
	}
}

func TestImplicitAssertionBinding(t *testing.T) {
	t.Parallel()

	keys := map[string]paseto.V4SymmetricKey{"key-a": mustKey(t, fixedKeyHex)}
	svc, _ := NewService(keys, "key-a", time.Hour)

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	token, err := svc.Issue(SessionData{Subject: "u", Roles: []string{"r"}, Tenant: "acme"}, now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := svc.Verify(token, "acme", now.Add(time.Minute)); err != nil {
		t.Fatalf("same-tenant Verify: %v", err)
	}
	if _, err := svc.Verify(token, "other", now.Add(time.Minute)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("cross-tenant err = %v; want ErrInvalidToken", err)
	}
}

func TestRotation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	oldKey := paseto.NewV4SymmetricKey()
	newKey := paseto.NewV4SymmetricKey()

	// Tokens minted before rotation used key-a.
	before, _ := NewService(map[string]paseto.V4SymmetricKey{"key-a": oldKey}, "key-a", time.Hour)
	oldToken, err := before.Issue(SessionData{Subject: "u", Roles: []string{"r"}, Tenant: "acme"}, now)
	if err != nil {
		t.Fatalf("Issue old: %v", err)
	}

	// After rotation the ring holds both keys; current is key-b.
	after, _ := NewService(map[string]paseto.V4SymmetricKey{"key-a": oldKey, "key-b": newKey}, "key-b", time.Hour)
	if _, err := after.Verify(oldToken, "acme", now.Add(time.Minute)); err != nil {
		t.Fatalf("post-rotation verify of old token: %v", err)
	}

	newToken, err := after.Issue(SessionData{Subject: "u", Roles: []string{"r"}, Tenant: "acme"}, now)
	if err != nil {
		t.Fatalf("Issue new: %v", err)
	}
	if _, err := after.Verify(newToken, "acme", now.Add(time.Minute)); err != nil {
		t.Fatalf("verify of new token: %v", err)
	}
}

func Example() {
	keys := map[string]paseto.V4SymmetricKey{"key-a": mustKeyExample()}
	svc, _ := NewService(keys, "key-a", time.Hour)

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	token, _ := svc.Issue(SessionData{Subject: "user-42", Roles: []string{"admin"}, Tenant: "acme"}, now)

	got, err := svc.Verify(token, "acme", now.Add(time.Minute))
	fmt.Println(err == nil, got.Subject, strings.Join(got.Roles, ","))
	// Output: true user-42 admin
}

func mustKeyExample() paseto.V4SymmetricKey {
	k, _ := paseto.V4SymmetricKeyFromHex(fixedKeyHex)
	return k
}
```

## Review

The service is correct when a token round-trips into the same `SessionData`, the
token string never contains a plaintext claim value, and every failure path is a
wrapped `ErrInvalidToken` or `ErrUnknownKeyID`. The design points worth keeping:
confidentiality is what `v4.local` buys over a signed token, so the payload is the
right place for roles and internal ids; the footer is authenticated plaintext, so
it carries the `kid` (never a secret); and rotation works because the verifier
reads the `kid` with `UnsafeParseFooter` and keeps retired keys in the ring until
their tokens expire. The two subtle mistakes to avoid: confusing the footer with
the implicit assertion (the footer is `SetFooter`, the binding is the trailing
`[]byte` to `V4Encrypt`/`ParseV4Local`), and mismatching that implicit assertion
between issue and verify — both sides must derive it from `implicitFor`, or every
legitimate token fails. Confirm with `go test -tags paseto -race`, which covers
round trip, confidentiality, wrong key, unknown `kid`, binding, and rotation.

## Resources

- [aidanwoods.dev/go-paseto](https://pkg.go.dev/aidanwoods.dev/go-paseto) — `V4Encrypt`, `ParseV4Local`, `UnsafeParseFooter`, `SetFooter`, `Set`/`Get`.
- [PASETO v4 protocol specification](https://github.com/paseto-standard/paseto-spec/blob/master/docs/01-Protocol-Versions/Version4.md) — `v4.local` encryption, footers, and implicit assertions.
- [PASETO home and rationale](https://paseto.io/) — local vs public as a data-classification decision.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-v4-public-token-service.md](01-v4-public-token-service.md) | Next: [03-jose-alg-confusion-hardened-verifier.md](03-jose-alg-confusion-hardened-verifier.md)
