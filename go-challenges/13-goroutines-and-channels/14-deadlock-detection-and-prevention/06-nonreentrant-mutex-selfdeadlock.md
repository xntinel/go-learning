# Exercise 6: Fixing a Self-Deadlock from a Re-Entrant Locked Method

`sync.Mutex` is not reentrant: a goroutine that already holds it self-deadlocks if it locks it
again. The classic way to trip this is an exported locked method that calls another exported
locked method. This exercise builds a ledger that exhibits the bug in a plain illustrative form,
then fixes it with the locked/unlocked method-pair convention — public methods that lock and
call only private lock-free helpers.

This module is fully self-contained: its own `go mod init`, all types inline, its own demo and
tests.

## What you'll build

```text
ledger/                    independent module: example.com/ledger
  go.mod                   go 1.25
  ledger.go                Ledger; public Withdraw/Balance lock, private helpers do not
  cmd/
    demo/
      main.go              a withdraw that internally reads the balance without re-locking
  ledger_test.go           watchdog-guarded test that would hang on the buggy version
```

- Files: `ledger.go`, `cmd/demo/main.go`, `ledger_test.go`.
- Implement: a `Ledger` whose exported `Withdraw` locks the mutex and internally consults the balance via a private lock-free helper, never re-acquiring the non-reentrant mutex.
- Test: a watchdog-guarded test that passes on the fixed version and would hang on the buggy one; unit tests asserting the private helper mutates state correctly while the public method owns the lock.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ledger/cmd/demo
cd ~/go-exercises/ledger
go mod init example.com/ledger
go mod edit -go=1.25
```

### The self-deadlock and its shape

Here is the bug, written as it usually ships — an exported method that locks, then calls
another exported method that also locks the same mutex:

```go
// BUGGY — do not build this.
func (l *Ledger) Balance() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balance
}

func (l *Ledger) Withdraw(amount int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Balance() < amount { // Balance() locks l.mu again -> self-deadlock
		return ErrInsufficientFunds
	}
	l.balance -= amount
	return nil
}
```

`Withdraw` holds `l.mu`. It calls `Balance`, which calls `l.mu.Lock()` on the mutex the same
goroutine already holds. `sync.Mutex` has no owner tracking and no recursion count — the second
`Lock` simply blocks, waiting for a release that can only come from this goroutine, which is now
stuck inside `Lock`. It waits for itself, forever. Only *this* goroutine wedges; the runtime's
"all goroutines asleep" detector never fires if anything else is running, so in a service this
manifests as one request that never returns, then two, then a slow leak.

The fix is the **locked/unlocked method pair**. Split each operation into a public method that
acquires the lock and a private helper that assumes the lock is already held and never locks it.
The public method does the locking exactly once; internal calls go to the helpers:

- `Balance()` (public) locks and returns `l.balance()`.
- `balance()` (private) assumes the lock is held and just reads the field.
- `Withdraw()` (public) locks once and calls `l.balance()` (the helper), never `l.Balance()`.

The convention is worth stating as a rule: from inside a locked section, call only lock-free
helpers, never an exported method that locks. Go deliberately ships no reentrant mutex precisely
because reentrancy hides the lock-scope confusion this convention makes explicit — the helper's
name and doc comment tell every reader "the lock must already be held."

Create `ledger.go`:

```go
package ledger

import (
	"errors"
	"sync"
)

// ErrInsufficientFunds is returned when a withdrawal exceeds the balance.
var ErrInsufficientFunds = errors.New("insufficient funds")

// Ledger is a single balance guarded by a non-reentrant mutex.
type Ledger struct {
	mu  sync.Mutex
	bal int64
}

// NewLedger returns a ledger with an opening balance.
func NewLedger(opening int64) *Ledger {
	return &Ledger{bal: opening}
}

// balance returns the balance. It assumes l.mu is already held and never locks
// it, so it is safe to call from other locked methods.
func (l *Ledger) balance() int64 {
	return l.bal
}

// deposit adds amount. It assumes l.mu is already held.
func (l *Ledger) deposit(amount int64) {
	l.bal += amount
}

// Balance returns the current balance, acquiring the lock.
func (l *Ledger) Balance() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balance()
}

// Deposit adds amount to the balance under the lock.
func (l *Ledger) Deposit(amount int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deposit(amount)
}

// Withdraw removes amount if the balance covers it. It locks once and consults
// the balance through the private helper l.balance(), NOT the exported
// l.Balance(), so the non-reentrant mutex is never acquired twice.
func (l *Ledger) Withdraw(amount int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.balance() < amount {
		return ErrInsufficientFunds
	}
	l.bal -= amount
	return nil
}

// Transfer moves amount from this ledger to dst. It demonstrates composing two
// locked operations without re-entrancy: it takes its own lock, uses the private
// helpers, and calls dst's exported Deposit only AFTER releasing its own lock is
// unnecessary here because dst is a different mutex — but note it must not call
// its own exported methods while holding l.mu.
func (l *Ledger) Transfer(dst *Ledger, amount int64) error {
	l.mu.Lock()
	if l.balance() < amount {
		l.mu.Unlock()
		return ErrInsufficientFunds
	}
	l.bal -= amount
	l.mu.Unlock()
	dst.Deposit(amount) // dst has its own lock; safe after releasing l.mu
	return nil
}
```

### The runnable demo

The demo performs a withdraw that internally reads the balance — the exact path that would
self-deadlock on the buggy version — and completes because it uses the lock-free helper.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/ledger"
)

func main() {
	l := ledger.NewLedger(100)

	if err := l.Withdraw(30); err != nil {
		fmt.Println("withdraw:", err)
		return
	}
	fmt.Printf("after withdraw 30: balance=%d\n", l.Balance())

	if err := l.Withdraw(1000); errors.Is(err, ledger.ErrInsufficientFunds) {
		fmt.Println("withdraw 1000: insufficient funds")
	}

	dst := ledger.NewLedger(0)
	if err := l.Transfer(dst, 20); err == nil {
		fmt.Printf("after transfer 20: src=%d dst=%d\n", l.Balance(), dst.Balance())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after withdraw 30: balance=70
withdraw 1000: insufficient funds
after transfer 20: src=50 dst=20
```

### Tests

The tests run `Withdraw` (which internally consults the balance) under a watchdog: the work runs
in a goroutine and the test fails with a diagnostic if it does not complete within a deadline.
On the fixed version it returns instantly; on the buggy version it would hang and the watchdog
would fail the test with "self-deadlock" instead of the whole `go test` binary timing out. Unit
tests assert the helpers mutate state correctly while the public method owns the lock.

Create `ledger_test.go`:

```go
package ledger

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// runWithWatchdog runs fn and fails the test if it does not return within d,
// which is how a self-deadlock is turned into an actionable failure.
func runWithWatchdog(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not complete within %s: likely self-deadlock", what, d)
	}
}

func TestWithdrawDoesNotSelfDeadlock(t *testing.T) {
	t.Parallel()

	l := NewLedger(100)
	runWithWatchdog(t, time.Second, "Withdraw", func() {
		if err := l.Withdraw(40); err != nil {
			t.Errorf("Withdraw err = %v, want nil", err)
		}
	})
	if got := l.Balance(); got != 60 {
		t.Fatalf("balance = %d, want 60", got)
	}
}

func TestWithdrawInsufficient(t *testing.T) {
	t.Parallel()

	l := NewLedger(10)
	if err := l.Withdraw(50); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if l.Balance() != 10 {
		t.Fatalf("balance mutated on rejected withdraw: %d", l.Balance())
	}
}

func TestConcurrentWithdrawDeposit(t *testing.T) {
	t.Parallel()

	l := NewLedger(0)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(2)
		go func() { defer wg.Done(); l.Deposit(1) }()
		go func() { defer wg.Done(); l.Withdraw(1) }()
	}
	wg.Wait()
	// Deposits and withdrawals are balanced; some withdrawals may fail when the
	// balance is momentarily zero, so the final balance is >= 0. The point of the
	// test is that it neither deadlocks nor races (run with -race).
	if l.Balance() < 0 {
		t.Fatalf("balance went negative: %d", l.Balance())
	}
}
```

## Review

The ledger is correct when every exported method acquires the lock exactly once and every
internal read/write goes through a lock-free helper. `TestWithdrawDoesNotSelfDeadlock` is the
proof: `Withdraw` internally needs the balance, and it gets it from `l.balance()` (the helper)
rather than `l.Balance()` (the locker), so the non-reentrant mutex is never taken twice. The
watchdog is what makes the test *diagnose* a regression — swap `l.balance()` back to `l.Balance()`
and the test fails with "likely self-deadlock" in one second instead of hanging the suite.

The mistake to avoid is calling an exported locking method from inside a locked section — the
single most common source of self-deadlock in Go services. The `Transfer` method shows the
related discipline for two objects: release your own lock before calling another object's
exported (locking) method, so you are never holding one lock while blocking on another. Run
`-race` to confirm the helpers only ever run under the lock.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the non-reentrant contract, stated in the docs.
- [Go Wiki: MutexOrChannel](https://go.dev/wiki/MutexOrChannel) — when a mutex is the right tool and how to scope it.
- [`runtime.Stack`](https://pkg.go.dev/runtime#Stack) — the basis of the watchdog's diagnosis (expanded in Exercise 10).

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-worker-pool-graceful-shutdown.md](07-worker-pool-graceful-shutdown.md)
