# Exercise 2: Non-Blocking Transfer with TryLock and Bounded Backoff

Ordered locking prevents deadlock but a `Move` still *blocks* on a contended account —
under heavy contention a burst of transfers can queue up behind one hot account. This
exercise builds `TryMove`, which acquires both account locks non-blockingly with
`sync.Mutex.TryLock`, releases the first lock if it cannot get the second (breaking
hold-and-wait), and wraps that in a retry-with-jittered-backoff loop bounded by a context
deadline so a persistently contended transfer fails cleanly instead of hanging.

This module is fully self-contained: its own `go mod init`, all types inline, its own
demo and tests.

## What you'll build

```text
trytransfer/               independent module: example.com/trytransfer
  go.mod                   go 1.25
  transfer.go              Account, Bank; TryMove (non-blocking), MoveWithBackoff (retry)
  cmd/
    demo/
      main.go              a contended transfer that fails fast, then succeeds after backoff
  transfer_test.go         contention -> ErrLocked; backoff success + DeadlineExceeded
```

- Files: `transfer.go`, `cmd/demo/main.go`, `transfer_test.go`.
- Implement: `TryMove` using `TryLock` with rollback of the first lock on failure, returning `ErrLocked` when contended; `MoveWithBackoff` that retries `TryMove` under a `context.Context` with jittered backoff.
- Test: hold one account's lock in a helper goroutine and assert `TryMove` returns `ErrLocked` with no partial mutation and no leaked lock; assert `MoveWithBackoff` succeeds once contention clears and returns `context.DeadlineExceeded` when it never does.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/14-deadlock-detection-and-prevention/02-trylock-transfer-backoff/cmd/demo
cd go-solutions/13-goroutines-and-channels/14-deadlock-detection-and-prevention/02-trylock-transfer-backoff
go mod edit -go=1.25
```

### Breaking hold-and-wait with TryLock

The blocking `Move` from Exercise 1 holds the first lock while blocking to acquire the
second — that is the hold-and-wait condition. Ordered locking makes that safe from
*deadlock*, but it still means a goroutine can be parked indefinitely on a busy account,
and there is no way to give up. `TryMove` attacks hold-and-wait directly: it takes the
first lock, then *tries* the second; if the try fails, it immediately releases the first
lock and returns `ErrLocked`. Because it never holds one lock while blocking on the other,
hold-and-wait never holds, and the caller is never parked.

Two correctness points are load-bearing. First, `TryLock` returning `false` must trigger a
*rollback*: if we got the first lock but not the second, we must `Unlock` the first before
returning, or we leak it and every future transfer touching that account wedges. Second,
`false` never means "proceed anyway" — `TryMove` returns an error and mutates nothing when
it cannot get both locks. We still acquire in the ordered sequence (lower ID first) so that
`TryMove` composes safely with the blocking `Move` if both are used on the same accounts.

`MoveWithBackoff` turns the non-blocking primitive into a bounded wait: it calls `TryMove`
in a loop; on `ErrLocked` it sleeps a short interval with random jitter and retries; it
gives up when the context deadline fires, returning `context.DeadlineExceeded`. Jitter (via
`math/rand/v2`) desynchronizes competing retriers so they do not collide in lockstep — a
livelock. The `time.Timer` is selected against `ctx.Done()` so cancellation interrupts the
sleep immediately rather than after the full backoff interval.

Create `transfer.go`:

```go
package trytransfer

import (
	"cmp"
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
	"unsafe"
)

// ErrInsufficientFunds is returned when the source lacks the amount.
var ErrInsufficientFunds = errors.New("insufficient funds")

// ErrSameAccount is returned when from and to are the same account.
var ErrSameAccount = errors.New("same account")

// ErrLocked is returned by TryMove when either account lock is contended.
var ErrLocked = errors.New("account locked")

// Account is a balance protected by its own mutex.
type Account struct {
	id      string
	mu      sync.Mutex
	balance int64
}

// NewAccount returns an account with the given id and opening balance.
func NewAccount(id string, balance int64) *Account {
	return &Account{id: id, balance: balance}
}

// ID returns the account identifier.
func (a *Account) ID() string { return a.id }

// Balance returns the current balance under the account's lock.
func (a *Account) Balance() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.balance
}

// Lock and Unlock expose the account mutex so tests can simulate contention.
func (a *Account) Lock()   { a.mu.Lock() }
func (a *Account) Unlock() { a.mu.Unlock() }

// Bank moves money with non-blocking, bounded-wait transfers.
type Bank struct{}

// NewBank returns a Bank.
func NewBank() *Bank { return &Bank{} }

func order(x, y *Account) (first, second *Account) {
	if c := cmp.Compare(x.id, y.id); c < 0 {
		return x, y
	} else if c > 0 {
		return y, x
	}
	if uintptr(unsafe.Pointer(x)) < uintptr(unsafe.Pointer(y)) {
		return x, y
	}
	return y, x
}

// TryMove attempts a transfer without blocking. If either lock is contended it
// rolls back any lock it took and returns ErrLocked, mutating nothing.
func (b *Bank) TryMove(from, to *Account, amount int64) error {
	if from == to {
		return ErrSameAccount
	}
	first, second := order(from, to)
	if !first.mu.TryLock() {
		return ErrLocked
	}
	defer first.mu.Unlock()
	if !second.mu.TryLock() {
		return ErrLocked // first is released by the deferred Unlock above
	}
	defer second.mu.Unlock()

	if from.balance < amount {
		return ErrInsufficientFunds
	}
	from.balance -= amount
	to.balance += amount
	return nil
}

// MoveWithBackoff retries TryMove under ctx, backing off with jitter between
// attempts. It returns nil on success, a domain error (ErrInsufficientFunds,
// ErrSameAccount) immediately, or ctx.Err() (e.g. DeadlineExceeded) if the
// deadline fires before the transfer succeeds.
func (b *Bank) MoveWithBackoff(ctx context.Context, from, to *Account, amount int64) error {
	const base = 200 * time.Microsecond
	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C // drain the initial fire

	for attempt := 0; ; attempt++ {
		err := b.TryMove(from, to, amount)
		if !errors.Is(err, ErrLocked) {
			return err // nil, ErrInsufficientFunds, or ErrSameAccount
		}
		// Contended: back off with exponential base and jitter, bounded by ctx.
		backoff := base << min(attempt, 6)
		jitter := time.Duration(rand.Int64N(int64(backoff) + 1))
		timer.Reset(backoff + jitter)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
```

### The runnable demo

The demo shows both behaviors: a `TryMove` against an account whose lock is held returns
`ErrLocked` immediately, and `MoveWithBackoff` succeeds once the holder releases the lock.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/trytransfer"
)

func main() {
	a := trytransfer.NewAccount("a", 100)
	b := trytransfer.NewAccount("b", 0)
	bank := trytransfer.NewBank()

	// Hold a's lock, then observe TryMove fail fast.
	a.Lock()
	if err := bank.TryMove(a, b, 50); errors.Is(err, trytransfer.ErrLocked) {
		fmt.Println("TryMove while contended: ErrLocked")
	}

	// Release after a short delay; MoveWithBackoff retries until it wins.
	go func() {
		time.Sleep(2 * time.Millisecond)
		a.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bank.MoveWithBackoff(ctx, a, b, 50); err == nil {
		fmt.Printf("MoveWithBackoff after release: a=%d b=%d\n", a.Balance(), b.Balance())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
TryMove while contended: ErrLocked
MoveWithBackoff after release: a=50 b=50
```

### Tests

`TestTryMoveContended` holds one account's lock in the test goroutine, calls `TryMove`,
asserts `ErrLocked`, and asserts no partial mutation happened. Crucially it then releases
the held lock and confirms a subsequent `TryMove` *succeeds* — proving the failed attempt
did not leak the first lock (if it had, the second attempt would also fail). `TestBackoff`
covers both branches of `MoveWithBackoff`: it succeeds once a helper releases the lock, and
returns `context.DeadlineExceeded` when the lock is never released before the deadline.

Create `transfer_test.go`:

```go
package trytransfer

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTryMoveContended(t *testing.T) {
	t.Parallel()

	a := NewAccount("a", 100)
	b := NewAccount("b", 0)
	bank := NewBank()

	a.Lock() // simulate another goroutine holding a's lock
	if err := bank.TryMove(a, b, 50); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryMove err = %v, want ErrLocked", err)
	}
	// b was ordered second and never touched money; nothing mutated.
	if b.balance != 0 {
		t.Fatalf("b mutated during failed TryMove: %d", b.balance)
	}
	a.Unlock()

	// If the failed attempt had leaked b's lock, this would fail.
	if err := bank.TryMove(a, b, 50); err != nil {
		t.Fatalf("TryMove after release err = %v, want nil (leaked lock?)", err)
	}
	if a.Balance() != 50 || b.Balance() != 50 {
		t.Fatalf("balances a=%d b=%d, want 50/50", a.Balance(), b.Balance())
	}
}

func TestBackoffSucceedsAfterRelease(t *testing.T) {
	t.Parallel()

	a := NewAccount("a", 100)
	b := NewAccount("b", 0)
	bank := NewBank()

	a.Lock()
	go func() {
		time.Sleep(5 * time.Millisecond)
		a.Unlock()
	}()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := bank.MoveWithBackoff(ctx, a, b, 50); err != nil {
		t.Fatalf("MoveWithBackoff err = %v, want nil", err)
	}
	if a.Balance() != 50 || b.Balance() != 50 {
		t.Fatalf("balances a=%d b=%d, want 50/50", a.Balance(), b.Balance())
	}
}

func TestBackoffDeadlineExceeded(t *testing.T) {
	t.Parallel()

	a := NewAccount("a", 100)
	b := NewAccount("b", 0)
	bank := NewBank()

	a.Lock() // never released: contention never clears
	defer a.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := bank.MoveWithBackoff(ctx, a, b, 50)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("MoveWithBackoff err = %v, want DeadlineExceeded", err)
	}
	if b.Balance() != 0 {
		t.Fatalf("b mutated despite deadline: %d", b.Balance())
	}
}
```

## Review

`TryMove` is correct when a `false` from either `TryLock` leaves the world untouched: no
balance changed and no lock held. The rollback is what makes that true — the deferred
`Unlock` of the first lock runs on the early `return ErrLocked` after the second `TryLock`
fails. `TestTryMoveContended`'s second attempt is the proof it did not leak: a leaked first
lock would make every later transfer on that account fail. `MoveWithBackoff` is a bounded
wait: it retries while `ErrLocked`, but the context deadline is a hard ceiling, so a
persistently contended transfer returns `DeadlineExceeded` instead of spinning forever.

The trap to avoid is using `TryLock`'s `false` as a signal to proceed without the lock —
that corrupts balances silently. The second trap is unbounded retry: without the context
branch in the `select`, a hot account would livelock the retriers. Jitter matters under real
contention; without it, two goroutines retrying on the same fixed interval can collide
repeatedly. Run `-race` to confirm the guarded mutation is actually protected on every path.

## Resources

- [`sync.Mutex.TryLock`](https://pkg.go.dev/sync#Mutex.TryLock) — non-blocking acquire and its intended use.
- [`context.Context`](https://pkg.go.dev/context#Context) — deadlines and cancellation for the retry ceiling.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — `Int64N` for backoff jitter.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-n-resource-lock-manager.md](03-n-resource-lock-manager.md)
