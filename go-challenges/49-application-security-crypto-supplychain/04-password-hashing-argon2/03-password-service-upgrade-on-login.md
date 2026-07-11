# Exercise 3: Credential service with transparent upgrade-on-login

This is the on-the-job exercise: a credential service that authenticates against
whatever is stored — legacy bcrypt or current argon2id — and, on a successful
login, hands back a freshly rehashed argon2id credential when the stored one is
below policy. That is how a live user table migrates from bcrypt to argon2id
without a mass re-hash (impossible, since you do not hold the plaintext) and
without forcing password resets.

This module is fully self-contained. It bundles its own argon2id PHC encoding and
its own bcrypt wrapping inline; it does not import Exercises 1 or 2.

## What you'll build

```text
credstore/                 independent module: example.com/credstore
  go.mod                   go 1.26; requires golang.org/x/crypto
  credstore.go             Hasher interface, argon2/bcrypt impls, Service.Authenticate
  cmd/
    demo/
      main.go              seed a legacy bcrypt hash, log in, watch it migrate
  credstore_test.go        migrate, no-op, below-policy, wrong-pw, unknown-algo
```

- Files: `credstore.go`, `cmd/demo/main.go`, `credstore_test.go`.
- Implement: a `Hasher` interface with argon2id and bcrypt implementations, and `Service.Authenticate(stored, password) (ok bool, upgraded string, err error)` that detects the algorithm from the stored prefix, verifies, and returns a rehashed argon2id string when the stored hash is bcrypt or below current argon2id policy.
- Test: bcrypt user migrates, argon2id-at-policy is a no-op, argon2id-below-policy refreshes, wrong password on either algorithm, and an unknown prefix yields `ErrUnknownAlgorithm`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/credstore/cmd/demo
cd ~/go-exercises/credstore
go mod init example.com/credstore
go mod edit -go=1.26
go get golang.org/x/crypto/argon2 golang.org/x/crypto/bcrypt
```

### The shape of the auth path

`Authenticate` is the entire public surface, and its signature is the design:
`(ok bool, upgraded string, err error)`. `ok` is the verification result. `err` is
reserved for *structural* problems — an unrecognized algorithm, a corrupt hash —
never for "wrong password," which is just `ok == false, err == nil`. `upgraded` is
the migration hook: it is a non-empty, freshly-hashed argon2id string exactly when
the caller should re-store the credential, and empty when the stored hash is
already at policy. The caller's login handler does `if ok && upgraded != "" {
db.Update(userID, upgraded) }` and nothing else changes; the migration is a side
effect of normal logins.

Algorithm detection is by prefix, because both formats are self-describing:
`$argon2id$` means the current algorithm, `$2` (covering `$2a$`, `$2b$`) means
legacy bcrypt. A bcrypt hash *always* triggers an upgrade to argon2id. An argon2id
hash triggers an upgrade only when its embedded parameters are below the service's
current policy, which `needsUpgrade` decides by parsing the parameters back out of
the string and comparing each against policy.

### Why the rehash only happens after a successful verify

You can only rehash a password you currently hold in plaintext, and the only
moment you legitimately hold it is inside a successful authentication. That is the
whole reason migration is driven by logins rather than a batch job: there is no
plaintext to batch over. It also means the migration is naturally rate-limited to
your active users, and dormant accounts keep their old hashes until they next log
in — which is fine, because a hash that is never verified is never at risk from a
weak parameter.

The argon2id verify bundled here is the same constant-time verifier from Exercise
1: recompute the key from the candidate password and the stored salt using the
*parsed* parameters, then `subtle.ConstantTimeCompare`. The bcrypt verify defers to
`bcrypt.CompareHashAndPassword`, which is constant-time internally.

Create `credstore.go`:

```go
package credstore

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// Sentinel errors for structural failures. A wrong password is (false, "", nil),
// not an error.
var (
	// ErrUnknownAlgorithm means the stored hash has no recognized prefix.
	ErrUnknownAlgorithm = errors.New("credstore: unknown hash algorithm")
	// ErrVerification means the stored hash is structurally invalid.
	ErrVerification = errors.New("credstore: verification failed")
)

// Params are the argon2id cost parameters, stored inside every argon2id hash.
type Params struct {
	Memory  uint32
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// Hasher hashes and verifies a single algorithm's credentials.
type Hasher interface {
	Hash(password string) (string, error)
	Verify(password, encoded string) (bool, error)
}

// argon2Hasher is the preferred, memory-hard implementation.
type argon2Hasher struct {
	params Params
}

func (h *argon2Hasher) Hash(password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("credstore: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt,
		h.params.Time, h.params.Memory, h.params.Threads, h.params.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.Memory, h.params.Time, h.params.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func (h *argon2Hasher) Verify(password, encoded string) (bool, error) {
	p, salt, key, err := parseArgon2(encoded)
	if err != nil {
		return false, err
	}
	other := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(key)))
	return subtle.ConstantTimeCompare(other, key) == 1, nil
}

// needsUpgrade reports whether a stored argon2id hash is below current policy.
func (h *argon2Hasher) needsUpgrade(encoded string) bool {
	p, _, _, err := parseArgon2(encoded)
	if err != nil {
		return false
	}
	return p.Memory < h.params.Memory ||
		p.Time < h.params.Time ||
		p.Threads < h.params.Threads
}

// bcryptHasher is the legacy, CPU-hard implementation.
type bcryptHasher struct {
	cost int
}

func (h *bcryptHasher) Hash(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), h.cost)
	if err != nil {
		return "", fmt.Errorf("credstore: bcrypt hash: %w", err)
	}
	return string(b), nil
}

func (h *bcryptHasher) Verify(password, encoded string) (bool, error) {
	err := bcrypt.CompareHashAndPassword([]byte(encoded), []byte(password))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return false, nil
	default:
		return false, fmt.Errorf("%w: %w", ErrVerification, err)
	}
}

// Both implementations satisfy Hasher.
var (
	_ Hasher = (*argon2Hasher)(nil)
	_ Hasher = (*bcryptHasher)(nil)
)

// Service authenticates against stored credentials and migrates them to argon2id
// on successful login.
type Service struct {
	preferred *argon2Hasher
	legacy    *bcryptHasher
}

// NewService builds a service that prefers argon2id at params and can verify
// legacy bcrypt hashes.
func NewService(params Params, bcryptCost int) *Service {
	return &Service{
		preferred: &argon2Hasher{params: params},
		legacy:    &bcryptHasher{cost: bcryptCost},
	}
}

// Authenticate verifies password against the stored hash, detecting the algorithm
// from its prefix. On success it returns (true, upgraded, nil): upgraded is a
// fresh argon2id hash to re-store when the stored hash is bcrypt or below current
// argon2id policy, and empty otherwise. A wrong password is (false, "", nil).
func (s *Service) Authenticate(stored, password string) (bool, string, error) {
	switch {
	case strings.HasPrefix(stored, "$argon2id$"):
		ok, err := s.preferred.Verify(password, stored)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return false, "", nil
		}
		if s.preferred.needsUpgrade(stored) {
			up, err := s.preferred.Hash(password)
			if err != nil {
				return true, "", err
			}
			return true, up, nil
		}
		return true, "", nil

	case strings.HasPrefix(stored, "$2"):
		ok, err := s.legacy.Verify(password, stored)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return false, "", nil
		}
		// A legacy bcrypt hash always migrates to argon2id.
		up, err := s.preferred.Hash(password)
		if err != nil {
			return true, "", err
		}
		return true, up, nil

	default:
		return false, "", fmt.Errorf("credstore: %q: %w", prefix(stored), ErrUnknownAlgorithm)
	}
}

// prefix returns a short leading slice of s for error messages.
func prefix(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// parseArgon2 decodes a PHC argon2id string into params, salt, and key.
func parseArgon2(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, fmt.Errorf("credstore: bad layout: %w", ErrVerification)
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("credstore: bad version: %w", ErrVerification)
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, fmt.Errorf("credstore: bad params: %w", ErrVerification)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("credstore: bad salt: %w", ErrVerification)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("credstore: bad key: %w", ErrVerification)
	}
	return p, salt, key, nil
}
```

### The runnable demo

The demo seeds a legacy bcrypt hash directly (simulating a row from the old user
table), then authenticates through the service and watches the credential migrate
to argon2id in one login.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"example.com/credstore"
)

func main() {
	policy := credstore.Params{Memory: 19 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}
	svc := credstore.NewService(policy, bcrypt.DefaultCost)

	// A row from the legacy table: a bcrypt hash created long ago.
	legacy, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("legacy stored algorithm: bcrypt")

	ok, upgraded, err := svc.Authenticate(string(legacy), "hunter2")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("authenticated:", ok)
	fmt.Println("upgraded to argon2id:", strings.HasPrefix(upgraded, "$argon2id$"))

	// Re-store `upgraded` and confirm the next login verifies against it.
	ok2, _, err := svc.Authenticate(upgraded, "hunter2")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("new hash verifies same password:", ok2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
legacy stored algorithm: bcrypt
authenticated: true
upgraded to argon2id: true
new hash verifies same password: true
```

### Tests

The tests use small argon2 parameters and `bcrypt.MinCost` for speed. They cover
the five paths the brief calls for: (a) a legacy bcrypt user authenticates and
`upgraded` is a non-empty argon2id string that itself verifies the same password;
(b) an argon2id user already at policy authenticates with no upgrade; (c) an
argon2id user below policy authenticates and gets a refreshed hash; (d) a wrong
password on either algorithm returns `ok == false` with no error; (e) an
unrecognized prefix returns `ErrUnknownAlgorithm` via `errors.Is`. The `Example`
demonstrates the bcrypt-to-argon2id upgrade. The final "Your turn" test proves
there is no lockout window during migration: both the original stored hash and the
upgraded hash accept the password.

Create `credstore_test.go`:

```go
package credstore

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func policy() Params {
	return Params{Memory: 128, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}
}

func newTestService() *Service {
	return NewService(policy(), bcrypt.MinCost)
}

func TestMigratesBcrypt(t *testing.T) {
	t.Parallel()
	svc := newTestService()
	legacy, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	ok, upgraded, err := svc.Authenticate(string(legacy), "pw")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !ok {
		t.Fatal("legacy bcrypt user failed to authenticate")
	}
	if !strings.HasPrefix(upgraded, "$argon2id$") {
		t.Fatalf("upgraded = %q, want an $argon2id$ string", upgraded)
	}
	// The upgraded hash verifies the same password.
	ok2, _, err := svc.Authenticate(upgraded, "pw")
	if err != nil || !ok2 {
		t.Fatalf("upgraded hash re-auth = %v, %v; want true, nil", ok2, err)
	}
}

func TestArgon2AtPolicyIsNoOp(t *testing.T) {
	t.Parallel()
	svc := newTestService()
	stored, err := svc.preferred.Hash("pw") // at current policy
	if err != nil {
		t.Fatal(err)
	}
	ok, upgraded, err := svc.Authenticate(stored, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("argon2id user failed to authenticate")
	}
	if upgraded != "" {
		t.Fatalf("upgraded = %q, want empty (already at policy)", upgraded)
	}
}

func TestArgon2BelowPolicyUpgrades(t *testing.T) {
	t.Parallel()
	svc := newTestService()
	// A hash made with weaker-than-policy memory.
	weak := &argon2Hasher{params: Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}}
	stored, err := weak.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	ok, upgraded, err := svc.Authenticate(stored, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("below-policy argon2id user failed to authenticate")
	}
	if !strings.HasPrefix(upgraded, "$argon2id$") {
		t.Fatalf("upgraded = %q, want a refreshed $argon2id$ string", upgraded)
	}
	if upgraded == stored {
		t.Fatal("upgraded hash equals stored hash; not refreshed")
	}
}

func TestWrongPassword(t *testing.T) {
	t.Parallel()
	svc := newTestService()
	bc, err := bcrypt.GenerateFromPassword([]byte("right"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	ar, err := svc.preferred.Hash("right")
	if err != nil {
		t.Fatal(err)
	}
	for _, stored := range []string{string(bc), ar} {
		ok, upgraded, err := svc.Authenticate(stored, "wrong")
		if err != nil {
			t.Fatalf("wrong password returned error %v, want nil", err)
		}
		if ok {
			t.Fatal("wrong password authenticated")
		}
		if upgraded != "" {
			t.Fatalf("wrong password produced upgrade %q", upgraded)
		}
	}
}

func TestUnknownAlgorithm(t *testing.T) {
	t.Parallel()
	svc := newTestService()
	_, _, err := svc.Authenticate("$scrypt$whatever", "pw")
	if !errors.Is(err, ErrUnknownAlgorithm) {
		t.Fatalf("err = %v, want errors.Is ErrUnknownAlgorithm", err)
	}
}

// TestNoLockoutDuringMigration is the "Your turn" test: during a migration the
// original stored hash and the newly upgraded hash must BOTH accept the password,
// so a login race (old row read before the upgrade is persisted) never locks the
// user out. Add a case that also rejects a wrong password on both.
func TestNoLockoutDuringMigration(t *testing.T) {
	t.Parallel()
	svc := newTestService()
	legacy, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	ok, upgraded, err := svc.Authenticate(string(legacy), "pw")
	if err != nil || !ok {
		t.Fatalf("initial auth = %v, %v", ok, err)
	}
	for _, stored := range []string{string(legacy), upgraded} {
		ok, _, err := svc.Authenticate(stored, "pw")
		if err != nil || !ok {
			t.Fatalf("auth against %q = %v, %v; want true, nil", prefix(stored), ok, err)
		}
	}
}

func Example() {
	svc := NewService(Params{Memory: 128, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, bcrypt.MinCost)
	legacy, _ := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)

	ok, upgraded, _ := svc.Authenticate(string(legacy), "hunter2")
	reok, _, _ := svc.Authenticate(upgraded, "hunter2")

	fmt.Println("ok:", ok)
	fmt.Println("upgraded to argon2id:", strings.HasPrefix(upgraded, "$argon2id$"))
	fmt.Println("reverifies:", reok)
	// Output:
	// ok: true
	// upgraded to argon2id: true
	// reverifies: true
}
```

## Review

The service is correct when `Authenticate` returns `ok == true` for the right
password against either algorithm, `ok == false` with a nil error for the wrong
one, and `ErrUnknownAlgorithm` (assertable with `errors.Is`) for an unrecognized
prefix. The migration is correct when a bcrypt hash always yields a non-empty
argon2id `upgraded` that re-verifies the password, an argon2id hash at policy
yields an empty `upgraded`, and an argon2id hash below policy yields a refreshed
one.

The mistakes to avoid: do not treat "wrong password" as an error — a `500` on a bad
password both leaks information and breaks the caller's control flow; the tests pin
`ok == false, err == nil`. Do not rehash before verifying; you would be hashing an
attacker-supplied password and, worse, overwriting a good credential with a guess.
Do not compare argon2id keys with `==`; the bundled verifier uses
`subtle.ConstantTimeCompare` for the same timing-oracle reason as Exercise 1. And
keep the original hash valid until the upgrade is persisted — the "Your turn" test
proves both accept the password so a mid-migration read never locks anyone out. Run
`go test -race` to confirm the whole package is clean.

## Resources

- [golang.org/x/crypto/argon2](https://pkg.go.dev/golang.org/x/crypto/argon2) — `IDKey` and `Version` for the preferred hasher.
- [golang.org/x/crypto/bcrypt](https://pkg.go.dev/golang.org/x/crypto/bcrypt) — `CompareHashAndPassword` and `Cost` for the legacy path.
- [OWASP Password Storage Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html) — upgrade-on-login and algorithm-migration guidance.

---

Back to [02-bcrypt-cost-and-rehash.md](02-bcrypt-cost-and-rehash.md) | Next: [../05-paseto-vs-jwt-tokens/00-concepts.md](../05-paseto-vs-jwt-tokens/00-concepts.md)
