# Exercise 8: Deadlock-free balance transfer with lock ordering

A wallet or reservation service must debit one account and credit another as a
single atomic step, which means holding both accounts' locks at once — and the
moment a program holds two locks, the ABBA deadlock is on the table. This
module builds the ledger, enforces a stable global acquisition order with
`cmp.Compare`, and writes the test that actually surfaces the hang, because
`-race` never will: a deadlock is not a data race.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ledger/                      independent module: example.com/ledger
  go.mod                     go 1.26
  ledger.go                  type Ledger, type Account; New, Balance, Transfer
                             sentinels: ErrInsufficientFunds, ErrUnknownAccount,
                             ErrSameAccount, ErrNonPositiveAmount
  cmd/
    demo/
      main.go                runnable demo: transfers, a rejected overdraft,
                             and an opposite-direction storm that terminates
  ledger_test.go             ABBA probe under -timeout, conservation invariant,
                             error table, Example
```

- Files: `ledger.go`, `cmd/demo/main.go`, `ledger_test.go`.
- Implement: per-account mutexes and a `Transfer(from, to, amount)` that locks both accounts in `cmp.Compare` order of their IDs, rejects same-account transfers, and returns wrapped sentinel errors with no partial application.
- Test: 100 goroutines transferring A to B interleaved with 100 transferring B to A must terminate (run under `go test -timeout 30s`); total balance across all accounts is exactly conserved after the storm; every error path asserts `errors.Is` and unchanged balances.
- Verify: `go test -count=1 -race -timeout 30s ./...`

### Why two locks, and why order is the whole game

One mutex per account is the right granularity for a ledger: transfers between
disjoint account pairs run fully in parallel, and only transfers touching the
same account contend. But the debit and the credit must be one atomic step —
release the source's lock between them and a concurrent reader can observe
money that has left one account and not yet arrived in the other, or a
concurrent transfer can overdraw an account that a still-in-flight debit was
counting on. So `Transfer` holds both locks across the whole operation.

The trap is acquisition order. If one goroutine runs `Transfer("alice", "bob")`
as lock(alice) then lock(bob), while another runs `Transfer("bob", "alice")` as
lock(bob) then lock(alice), the interleaving where each takes its first lock
leaves both waiting forever on the second — the ABBA deadlock. Nothing panics.
No data race is reported, because a deadlock involves no unsynchronized memory
access at all; `-race` is structurally blind to it. In a running service the
symptom is a pair of goroutines pinned forever in `sync.Mutex.Lock` (visible in
a goroutine dump), a slowly draining worker pool, and eventually a full stall.

The fix is a rule, not a mechanism: every code path that takes more than one of
these locks must take them in the same global order. Any stable total order
works; here the natural key is the account ID, compared with `cmp.Compare`
(which returns -1, 0, or +1 for any ordered type). `Transfer` always locks the
account with the smaller ID first, regardless of which side is the source. The
equal-ID case — a transfer to itself — is rejected up front, both because it is
a client error and because `lock(a); lock(a)` on a non-reentrant mutex is an
instant self-deadlock.

Two more production details. First, error paths must not partially apply: the
insufficient-funds check happens while both locks are held, before either
balance moves, so a failed transfer leaves both accounts untouched. Second, the
account *set* is fixed at construction, which makes the `map[string]*Account`
read-only after `New` returns — concurrent lookups need no lock of their own.
If accounts could be added at runtime, the map would need its own mutex, and
that lock would sit *above* the per-account locks in the ordering hierarchy.

Create `ledger.go`:

```go
package ledger

import (
	"cmp"
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrInsufficientFunds means the source balance cannot cover the amount.
	ErrInsufficientFunds = errors.New("insufficient funds")
	// ErrUnknownAccount means an account ID is not in the ledger.
	ErrUnknownAccount = errors.New("unknown account")
	// ErrSameAccount means source and destination are the same account.
	ErrSameAccount = errors.New("transfer to same account")
	// ErrNonPositiveAmount means the amount is zero or negative.
	ErrNonPositiveAmount = errors.New("amount must be positive")
)

// Account is one wallet with its own lock. Balances are in minor units
// (cents), so int64 arithmetic is exact.
type Account struct {
	mu      sync.Mutex
	id      string
	balance int64
}

// Ledger holds a fixed set of accounts. The map is populated once in New and
// never mutated again, so concurrent lookups are safe without a ledger-level
// lock; all mutable state lives behind the per-account mutexes.
type Ledger struct {
	accounts map[string]*Account
}

// New builds a ledger with the given opening balances.
func New(balances map[string]int64) *Ledger {
	l := &Ledger{accounts: make(map[string]*Account, len(balances))}
	for id, b := range balances {
		l.accounts[id] = &Account{id: id, balance: b}
	}
	return l
}

// Balance returns the current balance of one account.
func (l *Ledger) Balance(id string) (int64, error) {
	a, ok := l.accounts[id]
	if !ok {
		return 0, fmt.Errorf("balance of %q: %w", id, ErrUnknownAccount)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.balance, nil
}

// Transfer atomically moves amount from one account to another. It holds both
// account locks for the duration, acquiring them in ascending ID order so that
// concurrent opposite transfers cannot ABBA-deadlock. A failed transfer leaves
// both balances untouched.
func (l *Ledger) Transfer(from, to string, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("transfer %d from %q: %w", amount, from, ErrNonPositiveAmount)
	}
	if from == to {
		return fmt.Errorf("transfer %q to itself: %w", from, ErrSameAccount)
	}
	src, ok := l.accounts[from]
	if !ok {
		return fmt.Errorf("transfer from %q: %w", from, ErrUnknownAccount)
	}
	dst, ok := l.accounts[to]
	if !ok {
		return fmt.Errorf("transfer to %q: %w", to, ErrUnknownAccount)
	}

	// The global lock order: lower account ID first, always.
	first, second := src, dst
	if cmp.Compare(src.id, dst.id) > 0 {
		first, second = dst, src
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()

	if src.balance < amount {
		return fmt.Errorf("transfer %d from %q (balance %d): %w",
			amount, from, src.balance, ErrInsufficientFunds)
	}
	src.balance -= amount
	dst.balance += amount
	return nil
}
```

### The runnable demo

The demo runs two sequential transfers, one rejected overdraft (printing the
wrapped sentinel), and then the storm that matters: 100 goroutines moving money
one way interleaved with 100 moving it the other way on a fresh two-account
ledger. With correct lock ordering the storm terminates with both balances
exactly where they started; with `from`-first locking it would hang here,
visibly, instead.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/ledger"
)

func main() {
	l := ledger.New(map[string]int64{"alice": 100, "bob": 50})

	_ = l.Transfer("alice", "bob", 30)
	_ = l.Transfer("bob", "alice", 10)
	a, _ := l.Balance("alice")
	b, _ := l.Balance("bob")
	fmt.Printf("alice=%d bob=%d\n", a, b)

	if err := l.Transfer("alice", "bob", 1000); err != nil {
		fmt.Printf("big transfer: %v\n", err)
	}

	// Opposite-direction storm: the ABBA shape, defused by lock ordering.
	storm := ledger.New(map[string]int64{"acct-1": 1000, "acct-2": 1000})
	var wg sync.WaitGroup
	wg.Add(200)
	for range 100 {
		go func() {
			defer wg.Done()
			_ = storm.Transfer("acct-1", "acct-2", 1)
		}()
		go func() {
			defer wg.Done()
			_ = storm.Transfer("acct-2", "acct-1", 1)
		}()
	}
	wg.Wait()

	s1, _ := storm.Balance("acct-1")
	s2, _ := storm.Balance("acct-2")
	fmt.Printf("storm done: acct-1=%d acct-2=%d total=%d\n", s1, s2, s1+s2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice=80 bob=70
big transfer: transfer 1000 from "alice" (balance 80): insufficient funds
storm done: acct-1=1000 acct-2=1000 total=2000
```

### Tests

`TestOppositeTransfersNoDeadlock` is the deadlock probe: it recreates the exact
ABBA shape and relies on `go test -timeout` to convert a hang into a failure —
this is the one bug class in this chapter where the assertion is "the test
finished at all". `TestConservationUnderStorm` hammers five accounts from 100
goroutines and asserts the grand total is exactly unchanged: money is neither
created nor destroyed, even when some transfers fail. The error table pins
every sentinel with `errors.Is` and proves failed transfers apply nothing.

Create `ledger_test.go`:

```go
package ledger

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOppositeTransfersNoDeadlock(t *testing.T) {
	t.Parallel()

	// The ABBA probe: run under `go test -timeout 30s`. A lock-ordering bug
	// does not fail an assertion here - it hangs, and the timeout turns the
	// hang into a test failure with a full goroutine dump.
	l := New(map[string]int64{"a": 10_000, "b": 10_000})

	var failures atomic.Int64
	var wg sync.WaitGroup
	wg.Add(200)
	for range 100 {
		go func() {
			defer wg.Done()
			if err := l.Transfer("a", "b", 1); err != nil {
				failures.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			if err := l.Transfer("b", "a", 1); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()

	if n := failures.Load(); n != 0 {
		t.Fatalf("%d transfers failed, want 0 (balances are ample)", n)
	}
	a, _ := l.Balance("a")
	b, _ := l.Balance("b")
	if a != 10_000 || b != 10_000 {
		t.Fatalf("balances after symmetric storm = %d/%d, want 10000/10000", a, b)
	}
}

func TestConservationUnderStorm(t *testing.T) {
	t.Parallel()

	ids := []string{"a", "b", "c", "d", "e"}
	opening := make(map[string]int64, len(ids))
	for _, id := range ids {
		opening[id] = 1000
	}
	l := New(opening)

	const goroutines, perG = 100, 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func() {
			defer wg.Done()
			for j := range perG {
				from := ids[(g+j)%len(ids)]
				to := ids[(g+j+1+j%3)%len(ids)] // offset 1..3: never == from
				amount := int64(j%3 + 1)
				if err := l.Transfer(from, to, amount); err != nil &&
					!errors.Is(err, ErrInsufficientFunds) {
					t.Errorf("Transfer(%q, %q, %d) = %v", from, to, amount, err)
				}
			}
		}()
	}
	wg.Wait()

	var total int64
	for _, id := range ids {
		b, err := l.Balance(id)
		if err != nil {
			t.Fatalf("Balance(%q) = %v", id, err)
		}
		total += b
	}
	if want := int64(len(ids)) * 1000; total != want {
		t.Fatalf("total after storm = %d, want exactly %d (money created or destroyed)", total, want)
	}
}

func TestTransferErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		from    string
		to      string
		amount  int64
		wantErr error
	}{
		{"same account", "a", "a", 1, ErrSameAccount},
		{"unknown source", "zz", "b", 1, ErrUnknownAccount},
		{"unknown destination", "a", "zz", 1, ErrUnknownAccount},
		{"zero amount", "a", "b", 0, ErrNonPositiveAmount},
		{"negative amount", "a", "b", -5, ErrNonPositiveAmount},
		{"insufficient funds", "a", "b", 101, ErrInsufficientFunds},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := New(map[string]int64{"a": 100, "b": 100})
			err := l.Transfer(tt.from, tt.to, tt.amount)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Transfer(%q, %q, %d) = %v, want errors.Is %v",
					tt.from, tt.to, tt.amount, err, tt.wantErr)
			}
			a, _ := l.Balance("a")
			b, _ := l.Balance("b")
			if a != 100 || b != 100 {
				t.Fatalf("failed transfer applied partially: a=%d b=%d, want 100/100", a, b)
			}
		})
	}
}

func ExampleLedger_Transfer() {
	l := New(map[string]int64{"a": 100, "b": 0})
	err := l.Transfer("a", "b", 40)
	a, _ := l.Balance("a")
	b, _ := l.Balance("b")
	fmt.Println(err, a, b)
	// Output: <nil> 60 40
}
```

## Review

The transfer is correct when both locks are held across the check and both
mutations, the acquisition order is derived from a stable key rather than from
argument position, and every error return happens before any balance moves.
The two storm tests split the proof: the symmetric A/B storm is the
termination proof (its only real assertion is that `wg.Wait` returns before
the `-timeout` deadline), and the five-account storm is the atomicity proof
(exact conservation of the grand total).

The mistakes this module inoculates against: locking `from` then `to` because
the arguments arrive in that order — correct-looking code that deadlocks only
under opposite concurrent traffic; forgetting the `from == to` guard, which
self-deadlocks a non-reentrant mutex with one caller and zero contention; and
expecting `-race` to save you — it reports unsynchronized memory access, and a
deadlock has none. Keep `-timeout` on every test run that exercises multi-lock
code; the goroutine dump it prints on expiry names the exact `Lock` lines of
the cycle. Run `go test -count=1 -race -timeout 30s ./...`.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the per-account lock; not reentrant, no owner.
- [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — the stable total order used for acquisition.
- [`go` command: testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-timeout`, the tool that surfaces hangs.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` does and does not detect.

---

Back to [07-copylocks-bug-fix.md](07-copylocks-bug-fix.md) | Next: [09-trylock-refresh-guard.md](09-trylock-refresh-guard.md)
