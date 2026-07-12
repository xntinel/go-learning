# Exercise 2: bcrypt hashing — cost calibration, inspection, and rehash policy

This exercise wraps `x/crypto/bcrypt` into a small package that treats cost as a
tunable security control: it calibrates cost against a latency budget, inspects
the cost embedded in a stored hash to decide whether it is below current policy,
maps library errors to package sentinels, and refuses to silently truncate a
passphrase longer than bcrypt's 72-byte limit.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
bcryptauth/                independent module: example.com/bcryptauth
  go.mod                   go 1.26; requires golang.org/x/crypto
  bcryptauth.go            Hash, Verify, Cost, NeedsRehash, CalibrateCost; sentinels
  cmd/
    demo/
      main.go              hash at a legacy cost, inspect, rehash to policy
  bcryptauth_test.go       verify, mismatch, too-long, cost, rehash, leniency
```

- Files: `bcryptauth.go`, `cmd/demo/main.go`, `bcryptauth_test.go`.
- Implement: `Hash(password, cost)`, `Verify(encoded, password)`, `Cost(encoded)`, `NeedsRehash(encoded, targetCost)`, and `CalibrateCost(target)`, with sentinels `ErrMismatch` and `ErrPasswordTooLong` mapped via `errors.Is`.
- Test: correct/wrong password, a 73-byte password rejected by Hash, cost inspection, rehash policy, and the lenient compare path for a long password.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get golang.org/x/crypto/bcrypt
```

### Cost is the whole security control

bcrypt takes one parameter, a logarithmic *cost*: cost `n` runs `2^n` rounds of the
key schedule, so each increment doubles the work. `bcrypt.DefaultCost` is 10,
`MinCost` is 4, `MaxCost` is 31. The right value is not a constant you copy; it is
the highest cost whose one-verify latency still fits your budget on today's
hardware. `CalibrateCost` encodes that: it times a single `GenerateFromPassword`
at increasing costs and returns the first cost whose call meets or exceeds the
target latency. In production you run it once at startup (or offline) and store the
result as policy; you do not calibrate on every login.

`Cost` inspects the cost embedded in an existing hash via `bcrypt.Cost`, which
parses the `$2a$NN$` prefix without needing the password. `NeedsRehash` compares
that embedded cost against the current policy cost: if the stored hash was made at
a lower cost than policy now demands, it must be re-hashed on the next successful
login. This is the same self-describing-hash idea as argon2's PHC string — the
parameters travel with the hash, so upgrading policy never breaks verification of
old hashes.

### Mapping library errors to sentinels

Callers should classify failures with `errors.Is`, not by comparing against the
library's error values directly. So `Verify` maps `bcrypt.ErrMismatchedHashAndPassword`
to the package sentinel `ErrMismatch`, and `Hash` maps `bcrypt.ErrPasswordTooLong`
to `ErrPasswordTooLong`. Both are wrapped with a doubled `%w` verb
(`fmt.Errorf("%w: %w", ...)`) so `errors.Is` matches *either* the package sentinel
or the underlying library error — the caller picks whichever it wants to depend on.
Critically, a mismatch is *not* a server error: it means "invalid credentials." A
handler that treats every non-nil error from the compare as a 500, or worse lets it
fall through to "authorized," is the classic fail-open bug.

### The 72-byte limit: reject on Hash, lenient on Compare

Modern `x/crypto/bcrypt.GenerateFromPassword` rejects a password longer than 72
bytes with `ErrPasswordTooLong` rather than silently truncating (golang/go#36546).
`Hash` surfaces that as `ErrPasswordTooLong` so a long passphrase becomes a handled
error, not an invisible truncation. `CompareHashAndPassword`, by contrast, does not
length-check: it still verifies hashes created in the truncating era, so a password
whose first 72 bytes match an old hash will authenticate. That asymmetry is
deliberate — Generate is strict so you never *create* a truncated hash, Compare is
lenient so you never *lock out* a user whose hash predates the fix — and the "Your
turn" test pins it down.

Create `bcryptauth.go`:

```go
package bcryptauth

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Sentinel errors. Verify/Hash wrap both the package sentinel and the underlying
// library error with %w, so errors.Is matches either one.
var (
	// ErrMismatch means the password does not match the stored hash. It is
	// "invalid credentials", never a server error.
	ErrMismatch = errors.New("bcryptauth: password does not match hash")
	// ErrPasswordTooLong means the password exceeds bcrypt's 72-byte limit.
	ErrPasswordTooLong = errors.New("bcryptauth: password exceeds 72 bytes")
)

// DefaultCost re-exports bcrypt.DefaultCost (10) as the policy floor.
const DefaultCost = bcrypt.DefaultCost

// Hash returns a bcrypt hash of password at the given cost. A password longer
// than 72 bytes is rejected with ErrPasswordTooLong rather than truncated.
func Hash(password string, cost int) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return "", fmt.Errorf("%w: %w", ErrPasswordTooLong, err)
		}
		return "", fmt.Errorf("bcryptauth: hash: %w", err)
	}
	return string(h), nil
}

// Verify reports whether password matches the stored bcrypt hash. A mismatch is
// returned as ErrMismatch; a structurally invalid hash is returned wrapped.
func Verify(encoded, password string) error {
	err := bcrypt.CompareHashAndPassword([]byte(encoded), []byte(password))
	if err == nil {
		return nil
	}
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return fmt.Errorf("%w: %w", ErrMismatch, err)
	}
	return fmt.Errorf("bcryptauth: verify: %w", err)
}

// Cost returns the cost embedded in a stored hash without needing the password.
func Cost(encoded string) (int, error) {
	c, err := bcrypt.Cost([]byte(encoded))
	if err != nil {
		return 0, fmt.Errorf("bcryptauth: cost: %w", err)
	}
	return c, nil
}

// NeedsRehash reports whether a stored hash was created below the current policy
// cost and should be re-hashed on the next successful login.
func NeedsRehash(encoded string, targetCost int) (bool, error) {
	c, err := Cost(encoded)
	if err != nil {
		return false, err
	}
	return c < targetCost, nil
}

// CalibrateCost returns the lowest cost whose single GenerateFromPassword call
// takes at least target. Run it once at startup to derive policy from the actual
// hardware, never on the login path. The result is clamped to bcrypt's range.
func CalibrateCost(target time.Duration) (int, error) {
	const sample = "calibration-sample-password"
	for cost := bcrypt.MinCost; cost < bcrypt.MaxCost; cost++ {
		start := time.Now()
		if _, err := bcrypt.GenerateFromPassword([]byte(sample), cost); err != nil {
			return 0, fmt.Errorf("bcryptauth: calibrate: %w", err)
		}
		if time.Since(start) >= target {
			return cost, nil
		}
	}
	return bcrypt.MaxCost, nil
}
```

### The runnable demo

The demo tells the upgrade-on-login story with fixed costs so the output is
deterministic: it hashes at a legacy cost of 6, inspects it, finds it below a
policy cost of 12, re-hashes at 12, and confirms the rehash is no longer needed. It
also calibrates a cost against a tiny budget and asserts the result falls inside
bcrypt's allowed range.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"golang.org/x/crypto/bcrypt"

	"example.com/bcryptauth"
)

func main() {
	const policyCost = 12

	legacy, err := bcryptauth.Hash("hunter2", 6)
	if err != nil {
		log.Fatal(err)
	}
	c, _ := bcryptauth.Cost(legacy)
	fmt.Println("legacy stored cost:", c)

	need, _ := bcryptauth.NeedsRehash(legacy, policyCost)
	fmt.Printf("needs rehash to policy cost %d: %v\n", policyCost, need)

	upgraded, err := bcryptauth.Hash("hunter2", policyCost)
	if err != nil {
		log.Fatal(err)
	}
	c, _ = bcryptauth.Cost(upgraded)
	fmt.Println("upgraded stored cost:", c)

	need, _ = bcryptauth.NeedsRehash(upgraded, policyCost)
	fmt.Printf("needs rehash to policy cost %d: %v\n", policyCost, need)

	cal, err := bcryptauth.CalibrateCost(time.Nanosecond)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("calibrated cost within allowed range:",
		cal >= bcrypt.MinCost && cal <= bcrypt.MaxCost)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
legacy stored cost: 6
needs rehash to policy cost 12: true
upgraded stored cost: 12
needs rehash to policy cost 12: false
calibrated cost within allowed range: true
```

### Tests

The tests keep costs low (`bcrypt.MinCost`) so the suite stays fast — production
cost is calibrated, not hardcoded at 4. `TestVerify` covers a correct password
(nil) and a wrong one (`ErrMismatch` via `errors.Is`). `TestHashRejectsLong`
proves a 73-byte password to `Hash` returns `ErrPasswordTooLong` — Go rejects
rather than truncates. `TestCost` confirms `Cost` reads back the cost a hash was
made at. `TestNeedsRehash` checks the policy comparison in both directions. The
`Example` prints `DefaultCost`. The final test is the "Your turn" case: it proves
`Verify`'s compare path is lenient — a 73-byte password still authenticates
against a hash of its first 72 bytes, because `CompareHashAndPassword` does not
length-check.

Create `bcryptauth_test.go`:

```go
package bcryptauth

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestVerify(t *testing.T) {
	t.Parallel()
	encoded, err := Hash("s3cret", bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		guess   string
		wantErr error
	}{
		{"correct", "s3cret", nil},
		{"wrong", "S3cret", ErrMismatch},
		{"empty", "", ErrMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Verify(encoded, tc.guess)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Verify = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Verify = %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}

func TestMismatchWrapsLibraryError(t *testing.T) {
	t.Parallel()
	encoded, err := Hash("pw", bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	err = Verify(encoded, "nope")
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("want ErrMismatch, got %v", err)
	}
	if !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		t.Fatalf("want wrapped bcrypt.ErrMismatchedHashAndPassword, got %v", err)
	}
}

func TestHashRejectsLong(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 73)
	_, err := Hash(long, bcrypt.MinCost)
	if !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("Hash(73 bytes) = %v, want errors.Is ErrPasswordTooLong", err)
	}
	// A 72-byte password is accepted.
	if _, err := Hash(strings.Repeat("a", 72), bcrypt.MinCost); err != nil {
		t.Fatalf("Hash(72 bytes) = %v, want nil", err)
	}
}

func TestCost(t *testing.T) {
	t.Parallel()
	const cost = 5
	encoded, err := Hash("pw", cost)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Cost(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != cost {
		t.Fatalf("Cost = %d, want %d", got, cost)
	}
	if _, err := Cost("not-a-bcrypt-hash"); err == nil {
		t.Fatal("Cost of a non-bcrypt string: want error, got nil")
	}
}

func TestNeedsRehash(t *testing.T) {
	t.Parallel()
	encoded, err := Hash("pw", 5)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		target int
		want   bool
	}{
		{"below policy", 6, true},
		{"at policy", 5, false},
		{"above stored", 4, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NeedsRehash(encoded, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("NeedsRehash(target=%d) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

func TestCalibrateCost(t *testing.T) {
	t.Parallel()
	// A tiny target returns the minimum cost quickly.
	cost, err := CalibrateCost(1)
	if err != nil {
		t.Fatal(err)
	}
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		t.Fatalf("CalibrateCost = %d, want within [%d,%d]", cost, bcrypt.MinCost, bcrypt.MaxCost)
	}
}

// TestCompareIsLenientForLong is the "Your turn" test: bcrypt's compare path does
// not length-check, so a password longer than 72 bytes still authenticates
// against a hash of its first 72 bytes. This is why old hashes never lock users
// out even though Hash now rejects long inputs. Add a case for a 100-byte input.
func TestCompareIsLenientForLong(t *testing.T) {
	t.Parallel()
	first72 := strings.Repeat("a", 72)
	long := strings.Repeat("a", 73) // same first 72 bytes

	encoded, err := Hash(first72, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(encoded, long); err != nil {
		t.Fatalf("Verify with 73-byte password = %v, want nil (compare is lenient)", err)
	}
}

func ExampleHash() {
	fmt.Println(DefaultCost)
	// Output: 10
}
```

## Review

The package is correct when `Verify` returns nil for the right password and
`ErrMismatch` (assertable with `errors.Is`, and also matching the wrapped
`bcrypt.ErrMismatchedHashAndPassword`) for the wrong one; when `Hash` rejects a
73-byte password with `ErrPasswordTooLong` instead of truncating; and when `Cost`
and `NeedsRehash` read the embedded cost so policy can rise without breaking old
hashes.

The mistakes to avoid: do not treat a mismatch as a server error — it is invalid
credentials, and `TestVerify` exists to keep that mapping honest. Do not hardcode a
cost; calibrate it, which is why `CalibrateCost` measures rather than assumes.
Remember the asymmetry the "Your turn" test captures: `Hash` rejects long
passwords but `Verify` accepts them against old hashes, so migrating to a length
policy must go through the upgrade-on-login path, not a mass rejection that would
lock out existing users. Run `go test -race` to confirm the package is clean.

## Resources

- [golang.org/x/crypto/bcrypt](https://pkg.go.dev/golang.org/x/crypto/bcrypt) — `GenerateFromPassword`, `CompareHashAndPassword`, `Cost`, `ErrPasswordTooLong`.
- [golang/go#36546](https://github.com/golang/go/issues/36546) — the change to reject rather than truncate passwords over 72 bytes.
- [OWASP Password Storage Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html) — bcrypt cost floor and the 72-byte / pre-hashing guidance.

---

Back to [01-argon2id-phc-hasher.md](01-argon2id-phc-hasher.md) | Next: [03-password-service-upgrade-on-login.md](03-password-service-upgrade-on-login.md)
