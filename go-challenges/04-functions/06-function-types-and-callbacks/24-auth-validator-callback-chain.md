# Exercise 24: Chained Authentication Validators with Function Type Composition

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A login endpoint enforcing multi-factor authentication needs a password
check and a TOTP check to both pass, in that order, and needs the whole
thing wrapped in a lockout guard that locks a username out after too many
failures — even if the requests hammering it are concurrent. This module
builds the policy as a `Chain` of `Validator` callbacks and the lockout as
a `LockoutGuard` whose check-then-act is a single atomic step under one
mutex.

## What you'll build

```text
authchain/                  independent module: example.com/auth-validator-callback-chain
  go.mod                     go 1.24
  authchain.go                 type Credentials, type Validator, ErrLockedOut, func Chain, PasswordValidator, TOTPValidator, type LockoutGuard, func NewLockoutGuard, (LockoutGuard) Wrap, (LockoutGuard) Attempts
  cmd/
    demo/
      main.go                  runnable demo: bad password, bad TOTP, then a lockout on a fourth attempt
  authchain_test.go            table test: chain outcomes, chain short-circuit, lockout threshold, reset on success, concurrent failures clipped at max (-race)
```

Files: `authchain.go`, `cmd/demo/main.go`, `authchain_test.go`.
Implement: `type Credentials struct { Username, Password, TOTP string }`, `type Validator func(creds Credentials) error`, a sentinel `ErrLockedOut`, `func Chain(validators ...Validator) Validator`, `PasswordValidator(passwords map[string]string) Validator`, `TOTPValidator(codes map[string]string) Validator`, a `LockoutGuard` with `NewLockoutGuard(max int) *LockoutGuard`, `(*LockoutGuard) Wrap(next Validator) Validator`, and `(*LockoutGuard) Attempts(username string) int`.
Test: a table of chain outcomes (both factors correct, either wrong, both wrong), the chain stopping before the TOTP validator runs when the password validator already failed, the lockout guard tripping at exactly `max` failures and staying tripped even for correct credentials afterward, the guard resetting a username's count on success, and 30 goroutines hammering the same username concurrently proving the failed-attempt count clips at exactly `max` no matter the interleaving.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the lockout guard holds its lock across the call to `next`

`Chain` composing `PasswordValidator` and `TOTPValidator` is the
straightforward half of this module: each factor is a `Validator`, and
"both factors must pass, in order, and stop at the first failure" is just a
loop that returns on the first non-nil error — no need to check the second
factor once the first has already failed. `LockoutGuard` is where a naive
implementation breaks under load. The obvious version reads "check if
locked, unlock, call `next`, lock again, record the result" — a classic
check-then-act split into two critical sections with a gap in between.
Under concurrent login attempts for the same username, several goroutines
can all read "not locked out yet" during that gap, all proceed to call
`next`, and all increment the counter afterward — pushing the recorded
attempt count past `max` and, worse, letting more attempts through than the
policy intended between the check and the lock re-acquisition. `Wrap` holds
`g.mu` for the read, the call to `next`, and the write, so "is this user
locked out" and "record this attempt's outcome" are one atomic step: no
interleaving of concurrent calls can ever let two goroutines both pass the
threshold check before either one's outcome is recorded.

Create `authchain.go`:

```go
// Package authchain composes authentication validators as function-type
// callbacks, chaining them to enforce a multi-factor policy and guarding
// the whole chain with a per-user lockout after repeated failures.
package authchain

import (
	"fmt"
	"sync"
)

// Credentials is everything a login attempt supplies.
type Credentials struct {
	Username string
	Password string
	TOTP     string
}

// Validator checks one factor of a login attempt.
type Validator func(creds Credentials) error

// ErrLockedOut is returned once a username has failed too many attempts.
var ErrLockedOut = fmt.Errorf("authchain: account locked out")

// Chain runs validators in order, stopping at the first error. A
// multi-factor policy is just a Chain of one Validator per factor:
// password first, then TOTP, so a bad password never even reaches the
// TOTP check.
func Chain(validators ...Validator) Validator {
	return func(creds Credentials) error {
		for _, v := range validators {
			if err := v(creds); err != nil {
				return err
			}
		}
		return nil
	}
}

// PasswordValidator checks creds.Password against a fixed table of known
// passwords, keyed by username.
func PasswordValidator(passwords map[string]string) Validator {
	return func(creds Credentials) error {
		if passwords[creds.Username] != creds.Password {
			return fmt.Errorf("authchain: invalid password for %q", creds.Username)
		}
		return nil
	}
}

// TOTPValidator checks creds.TOTP against a fixed table of expected
// one-time codes, keyed by username. A real implementation would derive
// the expected code from a shared secret and the current time step; this
// module takes the expected code directly so the exercise and its tests
// stay deterministic and free of any wall-clock dependency.
func TOTPValidator(codes map[string]string) Validator {
	return func(creds Credentials) error {
		if codes[creds.Username] != creds.TOTP {
			return fmt.Errorf("authchain: invalid TOTP code for %q", creds.Username)
		}
		return nil
	}
}

// LockoutGuard tracks failed attempts per username and locks an account
// out after max consecutive failures. It is safe for concurrent use.
type LockoutGuard struct {
	mu       sync.Mutex
	attempts map[string]int
	max      int
}

// NewLockoutGuard builds a LockoutGuard that locks a username out after
// max consecutive failed attempts.
func NewLockoutGuard(max int) *LockoutGuard {
	return &LockoutGuard{attempts: make(map[string]int), max: max}
}

// Wrap returns a Validator that checks the lockout state, runs next, and
// updates the lockout state, all under one lock. Holding the lock across
// the call to next keeps "is this user locked out" and "record this
// attempt's outcome" as a single atomic check-then-act step: two
// concurrent attempts for the same user can never both read "not locked
// out yet" and both proceed past the threshold.
func (g *LockoutGuard) Wrap(next Validator) Validator {
	return func(creds Credentials) error {
		g.mu.Lock()
		defer g.mu.Unlock()

		if g.attempts[creds.Username] >= g.max {
			return ErrLockedOut
		}

		err := next(creds)
		if err != nil {
			g.attempts[creds.Username]++
		} else {
			g.attempts[creds.Username] = 0
		}
		return err
	}
}

// Attempts reports the current failed-attempt count for a username
// (test and observability helper).
func (g *LockoutGuard) Attempts(username string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attempts[username]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/auth-validator-callback-chain"
)

func main() {
	passwords := map[string]string{"alice": "correct-horse"}
	codes := map[string]string{"alice": "123456"}

	guard := authchain.NewLockoutGuard(3)
	login := guard.Wrap(authchain.Chain(
		authchain.PasswordValidator(passwords),
		authchain.TOTPValidator(codes),
	))

	attempts := []authchain.Credentials{
		{Username: "alice", Password: "wrong", TOTP: "123456"},
		{Username: "alice", Password: "wrong", TOTP: "123456"},
		{Username: "alice", Password: "correct-horse", TOTP: "000000"},
		{Username: "alice", Password: "correct-horse", TOTP: "123456"},
	}

	for i, creds := range attempts {
		err := login(creds)
		switch {
		case err == nil:
			fmt.Printf("attempt %d: success\n", i)
		case errors.Is(err, authchain.ErrLockedOut):
			fmt.Printf("attempt %d: locked out\n", i)
		default:
			fmt.Printf("attempt %d: rejected: %v\n", i, err)
		}
	}

	// A fourth failure (after three prior ones) should now be locked out
	// regardless of correct credentials.
	err := login(authchain.Credentials{Username: "alice", Password: "correct-horse", TOTP: "123456"})
	fmt.Println("final attempt locked out:", errors.Is(err, authchain.ErrLockedOut))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempt 0: rejected: authchain: invalid password for "alice"
attempt 1: rejected: authchain: invalid password for "alice"
attempt 2: rejected: authchain: invalid TOTP code for "alice"
attempt 3: locked out
final attempt locked out: true
```

### Tests

Create `authchain_test.go`:

```go
package authchain

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func testPolicy() Validator {
	return Chain(
		PasswordValidator(map[string]string{"alice": "secret"}),
		TOTPValidator(map[string]string{"alice": "123456"}),
	)
}

func TestChainRequiresEveryValidator(t *testing.T) {
	t.Parallel()
	policy := testPolicy()

	tests := []struct {
		name    string
		creds   Credentials
		wantErr bool
	}{
		{"correct password and TOTP", Credentials{"alice", "secret", "123456"}, false},
		{"wrong password", Credentials{"alice", "nope", "123456"}, true},
		{"correct password, wrong TOTP", Credentials{"alice", "secret", "000000"}, true},
		{"both wrong", Credentials{"alice", "nope", "000000"}, true},
	}
	for _, tc := range tests {
		err := policy(tc.creds)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestChainStopsAtFirstFailingValidator(t *testing.T) {
	t.Parallel()
	var totpCalled bool
	policy := Chain(
		PasswordValidator(map[string]string{"alice": "secret"}),
		func(creds Credentials) error {
			totpCalled = true
			return nil
		},
	)
	_ = policy(Credentials{Username: "alice", Password: "wrong-password"})
	if totpCalled {
		t.Fatal("TOTP validator should not run after the password validator fails")
	}
}

func TestLockoutGuardLocksOutAfterMaxFailures(t *testing.T) {
	t.Parallel()
	guard := NewLockoutGuard(3)
	login := guard.Wrap(testPolicy())
	bad := Credentials{Username: "alice", Password: "wrong", TOTP: "123456"}

	for i := 0; i < 3; i++ {
		if err := login(bad); errors.Is(err, ErrLockedOut) {
			t.Fatalf("attempt %d: locked out too early", i)
		}
	}
	if got := guard.Attempts("alice"); got != 3 {
		t.Fatalf("Attempts = %d, want 3", got)
	}

	err := login(Credentials{Username: "alice", Password: "secret", TOTP: "123456"})
	if !errors.Is(err, ErrLockedOut) {
		t.Fatalf("expected lockout on 4th attempt even with correct credentials, got %v", err)
	}
}

func TestLockoutGuardResetsOnSuccess(t *testing.T) {
	t.Parallel()
	guard := NewLockoutGuard(3)
	login := guard.Wrap(testPolicy())
	bad := Credentials{Username: "alice", Password: "wrong", TOTP: "123456"}
	good := Credentials{Username: "alice", Password: "secret", TOTP: "123456"}

	_ = login(bad)
	_ = login(bad)
	if got := guard.Attempts("alice"); got != 2 {
		t.Fatalf("Attempts before success = %d, want 2", got)
	}
	if err := login(good); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got := guard.Attempts("alice"); got != 0 {
		t.Fatalf("Attempts after success = %d, want 0 (reset)", got)
	}
}

// TestLockoutGuardConcurrentFailuresClipAtMax drives many goroutines at
// the same username concurrently, all with wrong credentials, and proves
// the check-then-act (is-locked, then record-failure) pair never lets the
// attempt count exceed max: across every possible interleaving, exactly
// max calls reach the real validator and every other call is rejected as
// locked out before it does. Run with -race to confirm the shared
// attempts map has no unsynchronized access.
func TestLockoutGuardConcurrentFailuresClipAtMax(t *testing.T) {
	const (
		maxAttempts = 5
		goroutines  = 30
	)
	guard := NewLockoutGuard(maxAttempts)
	var realAttempts, lockedOut atomic.Int64

	login := guard.Wrap(Validator(func(creds Credentials) error {
		realAttempts.Add(1)
		return errors.New("always fails")
	}))

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			err := login(Credentials{Username: "alice"})
			if errors.Is(err, ErrLockedOut) {
				lockedOut.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := realAttempts.Load(); got != maxAttempts {
		t.Fatalf("realAttempts = %d, want exactly %d", got, maxAttempts)
	}
	if got := lockedOut.Load(); got != goroutines-maxAttempts {
		t.Fatalf("lockedOut = %d, want %d", got, goroutines-maxAttempts)
	}
	if got := guard.Attempts("alice"); got != maxAttempts {
		t.Fatalf("Attempts(alice) = %d, want %d", got, maxAttempts)
	}
}
```

## Review

`Chain` guarantees ordering and short-circuiting for free: `TestChainStops
AtFirstFailingValidator` proves the second validator in a chain never even
runs once an earlier one has already failed, which matters whenever a
later factor is expensive (a network call to a TOTP provider, say) and
shouldn't be paid for on a request that was already going to be rejected.
`TestLockoutGuardConcurrentFailuresClipAtMax` is the test that actually
exercises the concurrency guarantee: with the lock held across the entire
"check, call `next`, record" sequence, 30 concurrent attempts against a
`max` of 5 always produce exactly 5 real validator calls and exactly 25
lockouts — not "5, give or take a couple," which is what a version with a
gap between the check and the update would produce under `-race` or under
heavier scheduling pressure. `TestLockoutGuardResetsOnSuccess` guards the
other direction: a legitimate login has to clear the failure count, or a
user who mistyped a password twice and then got it right would still be
one attempt away from a lockout on their next slip.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [sync/atomic](https://pkg.go.dev/sync/atomic)
- [RFC 6238: TOTP: Time-Based One-Time Password Algorithm](https://www.rfc-editor.org/rfc/rfc6238)
- [OWASP: Credential Stuffing / Account Lockout guidance](https://cheatsheetseries.owasp.org/cheatsheets/Credential_Stuffing_Prevention_Cheat_Sheet.html)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-tenant-context-callback-extractor.md](23-tenant-context-callback-extractor.md) | Next: [25-data-pipeline-transform-callback.md](25-data-pipeline-transform-callback.md)
