# Exercise 1: Argon2id hasher with PHC-encoded output

This exercise builds an argon2id hasher whose output is a self-describing PHC
string, so verification is stateless and parameters can evolve without a schema
change. The comparison of recomputed and stored keys is done in constant time,
because a hand-rolled verifier is a timing oracle unless you make it one that is
not.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
argon2id/                  independent module: example.com/argon2id
  go.mod                   go 1.26; requires golang.org/x/crypto
  argon2id.go              Params, DefaultParams, Hasher, New, Hash, Verify; sentinels
  cmd/
    demo/
      main.go              hashes and verifies a sample credential
  argon2id_test.go         round-trip, tamper, version, constant-time tests
```

- Files: `argon2id.go`, `cmd/demo/main.go`, `argon2id_test.go`.
- Implement: `Params`, `DefaultParams`, a `Hasher` with `Hash(password) (string, error)`, and a package-level `Verify(password, encoded) (bool, error)` that parses parameters back out of the PHC string, recomputes the key, and compares in constant time.
- Test: round-trip, wrong password, salt uniqueness, tampered/truncated strings asserted with `errors.Is`, a `v=18` string yielding `ErrIncompatibleVersion`, and equal-length constant-time inputs.
- Verify: `go test -count=1 -race ./...`

Set up the module. `golang.org/x/crypto/argon2` requires a recent toolchain, so
pin the language version:

```bash
mkdir -p ~/go-exercises/argon2id/cmd/demo
cd ~/go-exercises/argon2id
go mod init example.com/argon2id
go mod edit -go=1.26
go get golang.org/x/crypto/argon2
```

### Why the parameters live in the string

The core design decision is that `Hash` does not just return raw hash bytes; it
returns a PHC string that carries the algorithm, version, and every cost
parameter alongside the salt and the hash. That is what makes `Verify` a
package-level function that takes no configuration: it reads the parameters out of
the string it is verifying, so a hash made last year with weaker parameters still
verifies against *its own* parameters even after you raise the current defaults.
The alternative — storing parameters in a column or a config value — breaks the
moment those values change, because the verifier would recompute with the wrong
cost and never match.

`Hash` reads a fresh 16-byte salt from `crypto/rand`, calls `argon2.IDKey` with the
configured parameters, and formats both salt and hash with
`base64.RawStdEncoding` (standard alphabet, no padding — the PHC convention). The
`argon2.Version` constant is `0x13`, which the format records as the decimal `19`.

### Why Verify must compare in constant time

`Verify` reverses the process: split the string on `$`, check the variant is
`argon2id`, check the version equals `argon2.Version` (a `v=18` string is a
different algorithm generation and must be rejected, not silently accepted),
parse `m`, `t`, `p`, decode the salt and stored hash, then recompute the key from
the candidate password using the *parsed* parameters and the *stored* salt. The
final step compares the recomputed key against the stored key with
`subtle.ConstantTimeCompare`. A plain `==` or `bytes.Equal` returns as soon as it
finds a differing byte, leaking through timing how many leading bytes matched;
`ConstantTimeCompare` examines every byte and returns `1` only on a full match.
Because the recomputed key always has the same length as the stored key (both are
`len(key)` bytes, because `Verify` recomputes the key with `uint32(len(key))`),
the comparison inputs are equal length and the early-return-on-length-mismatch
branch of `ConstantTimeCompare` never fires on the happy path.

Every parse failure — wrong field count, wrong variant, bad base64, unparseable
numbers — is wrapped around the `ErrInvalidHash` sentinel with `%w`, so a caller
can classify it with `errors.Is`. A wrong-but-well-formed version is a distinct
condition and gets its own `ErrIncompatibleVersion`.

Create `argon2id.go`:

```go
package argon2id

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Sentinel errors, wrapped with %w so callers can classify with errors.Is.
var (
	// ErrInvalidHash means the encoded string is not a well-formed argon2id
	// PHC string.
	ErrInvalidHash = errors.New("argon2id: invalid encoded hash")
	// ErrIncompatibleVersion means the string is argon2id but a version this
	// package does not implement.
	ErrIncompatibleVersion = errors.New("argon2id: incompatible version")
)

// Params are the argon2id cost parameters. They are stored inside every hash so
// verification is stateless and the values can evolve over time.
type Params struct {
	Memory  uint32 // KiB of memory per hash
	Time    uint32 // number of iterations (passes)
	Threads uint8  // degree of parallelism
	SaltLen uint32 // salt length in bytes
	KeyLen  uint32 // derived key length in bytes
}

// DefaultParams returns OWASP-floor memory and time (m=19456 KiB, t=2) with
// parallelism set to the number of CPUs, a 16-byte salt and 32-byte key.
// Parallelism defaults to the number of CPUs, a common production choice; raise
// Memory/Time against your latency budget.
func DefaultParams() Params {
	threads := runtime.NumCPU()
	if threads > 255 {
		threads = 255
	}
	return Params{
		Memory:  19 * 1024,
		Time:    2,
		Threads: uint8(threads),
		SaltLen: 16,
		KeyLen:  32,
	}
}

// Hasher hashes passwords with a fixed set of parameters.
type Hasher struct {
	params Params
}

// New returns a Hasher using the given parameters.
func New(p Params) *Hasher { return &Hasher{params: p} }

// Hash reads a fresh random salt, derives the argon2id key, and returns a
// self-describing PHC string: $argon2id$v=19$m=...,t=...,p=...$salt$hash.
func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("argon2id: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt,
		h.params.Time, h.params.Memory, h.params.Threads, h.params.KeyLen)

	encoded := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.Memory, h.params.Time, h.params.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
	return encoded, nil
}

// Verify parses parameters out of encoded, recomputes the key from password and
// the stored salt, and compares the result to the stored key in constant time.
// A wrong password returns (false, nil); a malformed string returns an error
// wrapping ErrInvalidHash or ErrIncompatibleVersion.
func Verify(password, encoded string) (bool, error) {
	params, salt, key, err := decode(encoded)
	if err != nil {
		return false, err
	}
	other := argon2.IDKey([]byte(password), salt,
		params.Time, params.Memory, params.Threads, uint32(len(key)))
	return subtle.ConstantTimeCompare(other, key) == 1, nil
}

// decode parses a PHC argon2id string into its parameters, salt, and key.
func decode(encoded string) (Params, []byte, []byte, error) {
	// Layout: ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, key]
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, fmt.Errorf("argon2id: bad layout: %w", ErrInvalidHash)
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, fmt.Errorf("argon2id: bad version field: %w", ErrInvalidHash)
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("argon2id: v=%d: %w", version, ErrIncompatibleVersion)
	}

	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, fmt.Errorf("argon2id: bad params field: %w", ErrInvalidHash)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("argon2id: bad salt: %w", ErrInvalidHash)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("argon2id: bad key: %w", ErrInvalidHash)
	}
	if len(salt) == 0 || len(key) == 0 {
		return Params{}, nil, nil, fmt.Errorf("argon2id: empty salt or key: %w", ErrInvalidHash)
	}
	return p, salt, key, nil
}
```

### The runnable demo

The demo hashes a sample credential with `DefaultParams`, then verifies it against
the correct password and a wrong one, printing deterministic results. The full
hash string is not printed because it contains a random salt and would differ on
every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"strings"

	"example.com/argon2id"
)

func main() {
	h := argon2id.New(argon2id.DefaultParams())

	encoded, err := h.Hash("correct horse battery staple")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("prefix:", encoded[:len("$argon2id$v=19$")])
	fmt.Println("self-describing:", strings.HasPrefix(encoded, "$argon2id$v=19$m="))

	ok, err := argon2id.Verify("correct horse battery staple", encoded)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("verify correct password:", ok)

	ok, err = argon2id.Verify("wrong password", encoded)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("verify wrong password:", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
prefix: $argon2id$v=19$
self-describing: true
verify correct password: true
verify wrong password: false
```

### Tests

The tests use deliberately small parameters so the suite is fast; production uses
`DefaultParams`. `TestRoundTrip` proves Hash then Verify returns true and that a
wrong password returns `(false, nil)`. `TestSaltUniqueness` hashes the same
password twice and asserts the encoded strings differ, proving the salt is random.
`TestVerifyReadsEmbeddedParams` hashes the same password with two different
parameter sets and verifies both, proving Verify uses the parameters inside the
string rather than any current config. `TestTamperedStrings` feeds malformed
inputs and asserts each wraps the right sentinel via `errors.Is`, including a
`v=18` string that must yield `ErrIncompatibleVersion`. The final `Test` is the
"Your turn" case: it confirms the recomputed and stored keys are equal length, so
the constant-time comparison runs over equal-length inputs on both a matching and a
non-matching password.

Create `argon2id_test.go`:

```go
package argon2id

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func testParams() Params {
	return Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	h := New(testParams())
	tests := []struct {
		name     string
		password string
		guess    string
		want     bool
	}{
		{"correct", "s3cret-pass", "s3cret-pass", true},
		{"wrong", "s3cret-pass", "s3cret-Pass", false},
		{"empty guess", "s3cret-pass", "", false},
		{"empty password", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := h.Hash(tc.password)
			if err != nil {
				t.Fatalf("Hash: %v", err)
			}
			got, err := Verify(tc.guess, encoded)
			if err != nil {
				t.Fatalf("Verify: unexpected error %v", err)
			}
			if got != tc.want {
				t.Fatalf("Verify(%q) = %v, want %v", tc.guess, got, tc.want)
			}
		})
	}
}

func TestPrefix(t *testing.T) {
	t.Parallel()
	h := New(testParams())
	encoded, err := h.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Fatalf("prefix = %q, want $argon2id$v=19$...", encoded[:20])
	}
}

func TestSaltUniqueness(t *testing.T) {
	t.Parallel()
	h := New(testParams())
	a, err := h.Hash("same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Hash("same")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
}

func TestVerifyReadsEmbeddedParams(t *testing.T) {
	t.Parallel()
	weak := New(Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})
	strong := New(Params{Memory: 128, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32})

	for _, h := range []*Hasher{weak, strong} {
		encoded, err := h.Hash("pw")
		if err != nil {
			t.Fatal(err)
		}
		ok, err := Verify("pw", encoded)
		if err != nil || !ok {
			t.Fatalf("Verify with embedded params = %v, %v; want true, nil", ok, err)
		}
	}
}

func TestTamperedStrings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		encoded string
		want    error
	}{
		{"empty", "", ErrInvalidHash},
		{"too few fields", "$argon2id$v=19$m=64,t=1,p=1", ErrInvalidHash},
		{"wrong variant", "$argon2i$v=19$m=64,t=1,p=1$c2FsdA$aGFzaA", ErrInvalidHash},
		{"bad version field", "$argon2id$vv$m=64,t=1,p=1$c2FsdA$aGFzaA", ErrInvalidHash},
		{"bad params field", "$argon2id$v=19$m=x,t=1,p=1$c2FsdA$aGFzaA", ErrInvalidHash},
		{"bad salt base64", "$argon2id$v=19$m=64,t=1,p=1$!!!$aGFzaA", ErrInvalidHash},
		{"bad key base64", "$argon2id$v=19$m=64,t=1,p=1$c2FsdA$!!!", ErrInvalidHash},
		{"wrong version", "$argon2id$v=18$m=64,t=1,p=1$c2FsdA$aGFzaA", ErrIncompatibleVersion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Verify("pw", tc.encoded)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify(%q) err = %v, want errors.Is %v", tc.encoded, err, tc.want)
			}
		})
	}
}

// TestConstantTimeEqualLength is the "Your turn" test: the recomputed key must be
// the same length as the stored key so subtle.ConstantTimeCompare runs over
// equal-length inputs (it returns 0 immediately on a length mismatch, which would
// defeat the constant-time property). Add an assertion for a different KeyLen.
func TestConstantTimeEqualLength(t *testing.T) {
	t.Parallel()
	h := New(testParams())
	encoded, err := h.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	_, _, key, err := decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != int(testParams().KeyLen) {
		t.Fatalf("stored key len = %d, want %d", len(key), testParams().KeyLen)
	}
	// Both a matching and a non-matching guess derive a key of len(key), so the
	// comparison inputs are always equal length.
	for _, guess := range []string{"pw", "PW"} {
		if _, err := Verify(guess, encoded); err != nil {
			t.Fatalf("Verify(%q): %v", guess, err)
		}
	}
}

func ExampleHasher_Hash() {
	h := New(testParams())
	encoded, _ := h.Hash("pw")
	fmt.Println(encoded[:len("$argon2id$v=19$")])
	// Output: $argon2id$v=19$
}

func ExampleVerify() {
	h := New(testParams())
	encoded, _ := h.Hash("hunter2")
	ok, _ := Verify("hunter2", encoded)
	fmt.Println(ok)
	// Output: true
}
```

## Review

The hasher is correct when a round-trip verifies, two hashes of one password
differ, and a wrong password returns `(false, nil)` rather than an error — a
mismatch is not a failure of the function, it is the answer. The parse path is
correct when every malformed input maps to `ErrInvalidHash` and only a genuine
version mismatch maps to `ErrIncompatibleVersion`, both assertable with
`errors.Is` because they are wrapped with `%w`.

The mistakes to avoid are the ones the concepts warned about. Do not finish
`Verify` with `==` or `bytes.Equal`; that turns the verify latency into an oracle
for the stored hash. Do not read the salt from `math/rand`; `TestSaltUniqueness`
would still pass, but the salts would be predictable. Do not store the parameters
anywhere but the string; `TestVerifyReadsEmbeddedParams` proves the design goal
that verification uses the embedded parameters, so tuning `DefaultParams` never
breaks an existing hash. Run `go test -race` to confirm the package is clean under
the race detector even though there is no shared mutable state here.

## Resources

- [golang.org/x/crypto/argon2](https://pkg.go.dev/golang.org/x/crypto/argon2) — `IDKey` and the `Version` constant (0x13).
- [PHC string format specification](https://github.com/P-H-C/phc-string-format/blob/master/phc-sf-spec.md) — the `$argon2id$v=...$m=...` layout and base64 conventions.
- [crypto/subtle](https://pkg.go.dev/crypto/subtle) — `ConstantTimeCompare` and why comparisons must not short-circuit.
- [OWASP Password Storage Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html) — argon2id parameter floors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-bcrypt-cost-and-rehash.md](02-bcrypt-cost-and-rehash.md)
