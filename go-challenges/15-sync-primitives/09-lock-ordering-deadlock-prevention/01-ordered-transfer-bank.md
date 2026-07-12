# Exercise 1: Bank Transfers with Consistent Lock Ordering

Two goroutines transferring money between the same two accounts in opposite
directions is the smallest real deadlock there is, and a per-account mutex with
caller-order locking walks straight into it. This exercise builds the transfer
engine the correct way — lower-ID lock first, always — and pins the discipline
with a test that hangs without it.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
orderedbank/               independent module: example.com/orderedbank
  go.mod
  bank/
    account.go             type Account; NewAccount, Transfer (lower-ID-first),
                           BalanceOf; ErrSameAccount, ErrInsufficientFunds,
                           ErrNonPositiveAmount
  cmd/
    demo/
      main.go              runnable demo: two transfers, balances, same-account error
  bank/transfer_test.go    error-case table, ExampleTransfer, conservation under
                           50 goroutines, the opposite-direction deadlock test
```

- Files: `bank/account.go`, `cmd/demo/main.go`, `bank/transfer_test.go`.
- Implement: `Transfer(from, to, amount)` that guards `from == to`, validates the amount, acquires both account mutexes in ascending-ID order, and moves the money atomically; `BalanceOf` reads under the lock.
- Test: table-driven error cases asserted with `errors.Is`, a 50-goroutine conservation test, and `TestTransferBothDirections` — the test that deadlocks without ordering.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/01-ordered-transfer-bank/bank go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/01-ordered-transfer-bank/cmd/demo
cd go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/01-ordered-transfer-bank
```

### Why the order must belong to the account, not the caller

The naive transfer locks its arguments in the order it received them:

```
func Transfer(from, to *Account, amount float64) error {
	from.mu.Lock()          // goroutine 1: locks A     goroutine 2: locks B
	to.mu.Lock()            // goroutine 1: waits for B goroutine 2: waits for A
	...
}
```

Run `Transfer(a, b, 1)` and `Transfer(b, a, 1)` concurrently and each goroutine
holds one mutex while waiting for the other: a cycle in the wait-for graph,
and both goroutines are parked forever. Nothing crashes. In a server, requests
touching accounts A or B start queueing behind the wedge and the goroutine
count climbs while every health check stays green.

The fix is to make the acquisition order a stable property of the *resource*:
whichever account has the lower ID is locked first, no matter which argument
position it arrived in. Now every goroutine that needs both locks takes them
in the same order, so no goroutine can ever hold the higher-ID lock while
waiting for the lower-ID one. Every wait-for edge points from a lower ID to a
higher ID; the graph is a DAG; a DAG has no cycles; deadlock is impossible.
That is the entire proof, and it is why the ordering key must be total and
stable — an ID, never a pointer that could compare differently across runs or
an argument position the caller controls.

One more trap before the ordered path: `sync.Mutex` is not reentrant. If
`from == to`, "lock both in ID order" degenerates to locking the same mutex
twice, and the goroutine blocks on itself forever — a self-deadlock no
ordering can prevent. The identity guard at the top of `Transfer` is therefore
not input hygiene; it is deadlock prevention, and it gets its own sentinel
error so callers (an API handler validating a transfer request) can map it to
a 400 rather than a 500.

The money checks happen only after both locks are held: the insufficient-funds
test reads `from.balance` under the lock, so the check and the debit are one
atomic step. Validating the balance before locking would be a
time-of-check-to-time-of-use race — another goroutine could drain the account
between the check and the debit.

Create `bank/account.go`:

```go
// Package bank implements account-to-account transfers protected by
// per-account mutexes with a consistent lock-acquisition order.
//
// Ordering discipline: any code path that locks more than one account
// must acquire the mutexes in ascending Account.ID order. Transfer does;
// every future multi-account operation (totals, snapshots, admin tooling)
// must do the same, or the deadlock this package prevents comes back.
package bank

import (
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrSameAccount rejects a transfer whose source and destination are
	// the same account. Without this guard the ordered path would lock
	// one mutex twice and self-deadlock: sync.Mutex is not reentrant.
	ErrSameAccount = errors.New("bank: cannot transfer to same account")

	// ErrInsufficientFunds rejects a debit larger than the source balance.
	ErrInsufficientFunds = errors.New("bank: insufficient funds")

	// ErrNonPositiveAmount rejects zero and negative transfer amounts.
	ErrNonPositiveAmount = errors.New("bank: non-positive amount")
)

// Account is a bank account protected by its own mutex. ID is the global
// lock-ordering key: lower IDs are always locked before higher IDs.
type Account struct {
	ID      int
	mu      sync.Mutex
	balance float64
}

// NewAccount returns an account with the given id and starting balance.
// The zero-value mutex is ready for use.
func NewAccount(id int, balance float64) *Account {
	return &Account{ID: id, balance: balance}
}

// Transfer moves amount from one account to another atomically. It
// acquires both account mutexes in ascending-ID order, which makes the
// wait-for graph a DAG: no goroutine can hold a higher-ID lock while
// waiting for a lower-ID one, so no cycle — and no deadlock — can form.
func Transfer(from, to *Account, amount float64) error {
	if from == to {
		return ErrSameAccount
	}
	if amount <= 0 {
		return fmt.Errorf("%w: %g", ErrNonPositiveAmount, amount)
	}

	first, second := from, to
	if from.ID > to.ID {
		first, second = to, from
	}

	first.mu.Lock()
	defer first.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()

	if from.balance < amount {
		return fmt.Errorf("%w: have %.2f, need %.2f", ErrInsufficientFunds, from.balance, amount)
	}

	from.balance -= amount
	to.balance += amount
	return nil
}

// BalanceOf returns the current balance, read under the account's mutex.
func (a *Account) BalanceOf() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.balance
}
```

### The runnable demo

The demo moves money along a small chain, prints the balances and their sum,
and then exercises the same-account guard so the error path is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/orderedbank/bank"
)

func main() {
	a := bank.NewAccount(1, 1000)
	b := bank.NewAccount(2, 1000)
	c := bank.NewAccount(3, 1000)

	if err := bank.Transfer(a, b, 10); err != nil {
		fmt.Println("err:", err)
		return
	}
	if err := bank.Transfer(b, c, 25); err != nil {
		fmt.Println("err:", err)
		return
	}

	total := a.BalanceOf() + b.BalanceOf() + c.BalanceOf()
	fmt.Printf("a=%.2f b=%.2f c=%.2f total=%.2f\n",
		a.BalanceOf(), b.BalanceOf(), c.BalanceOf(), total)

	if err := bank.Transfer(a, a, 1); err != nil {
		fmt.Println("same-account err:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a=990.00 b=985.00 c=1025.00 total=3000.00
same-account err: bank: cannot transfer to same account
```

### Tests

The error cases go in one table asserted with `errors.Is`, so wrapping with
`%w` stays part of the contract. Two tests carry the concurrency weight.
`TestConcurrentTransfersConserveMoney` runs 50 goroutines making 100 transfers
each across four accounts and asserts the invariant that matters in a ledger:
the sum of balances never changes. `TestTransferBothDirections` is the
deadlock-proving test — two goroutines transferring between the same pair of
accounts in opposite directions, 1000 times each. With caller-order locking
this test hangs (the runtime will not save you: the test runner's goroutines
keep it alive, so the job stalls until the ten-minute test timeout); with
ID-order locking it completes in milliseconds and conserves the total.

Create `bank/transfer_test.go`:

```go
package bank

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestTransferSuccess(t *testing.T) {
	t.Parallel()

	a := NewAccount(1, 100)
	b := NewAccount(2, 50)
	if err := Transfer(a, b, 30); err != nil {
		t.Fatal(err)
	}
	if got := a.BalanceOf(); got != 70 {
		t.Errorf("a = %g, want 70", got)
	}
	if got := b.BalanceOf(); got != 80 {
		t.Errorf("b = %g, want 80", got)
	}
}

func TestTransferZeroesOutBalance(t *testing.T) {
	t.Parallel()

	a := NewAccount(1, 10)
	b := NewAccount(2, 0)
	if err := Transfer(a, b, 10); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := a.BalanceOf(); got != 0 {
		t.Errorf("a = %g, want 0", got)
	}
	if got := b.BalanceOf(); got != 10 {
		t.Errorf("b = %g, want 10", got)
	}
}

func TestTransferErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		balance float64
		same    bool
		amount  float64
		wantErr error
	}{
		{name: "same account", balance: 100, same: true, amount: 10, wantErr: ErrSameAccount},
		{name: "zero amount", balance: 100, amount: 0, wantErr: ErrNonPositiveAmount},
		{name: "negative amount", balance: 100, amount: -1, wantErr: ErrNonPositiveAmount},
		{name: "insufficient funds", balance: 5, amount: 10, wantErr: ErrInsufficientFunds},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			from := NewAccount(1, tc.balance)
			to := NewAccount(2, 0)
			if tc.same {
				to = from
			}

			err := Transfer(from, to, tc.amount)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Transfer() err = %v, want %v", err, tc.wantErr)
			}
			if got := from.BalanceOf(); got != tc.balance {
				t.Errorf("from balance changed despite error: %g, want %g", got, tc.balance)
			}
		})
	}
}

func TestConcurrentTransfersConserveMoney(t *testing.T) {
	t.Parallel()

	accounts := []*Account{
		NewAccount(1, 1000),
		NewAccount(2, 1000),
		NewAccount(3, 1000),
		NewAccount(4, 1000),
	}

	const goroutines = 50
	const perGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			for j := range perGoroutine {
				from := accounts[i%len(accounts)]
				to := accounts[(i+j+1)%len(accounts)]
				if err := Transfer(from, to, 1); err != nil {
					// Insufficient funds and same-account are expected
					// under this access pattern; anything else is a bug.
					if !errors.Is(err, ErrInsufficientFunds) && !errors.Is(err, ErrSameAccount) {
						t.Errorf("unexpected transfer error: %v", err)
					}
				}
			}
		}()
	}
	wg.Wait()

	var got float64
	for _, a := range accounts {
		got += a.BalanceOf()
	}
	want := float64(len(accounts)) * 1000
	if got != want {
		t.Fatalf("total = %g, want %g (money not conserved)", got, want)
	}
}

func TestTransferBothDirections(t *testing.T) {
	t.Parallel()

	// The lesson's hardest case: two goroutines transferring between the
	// same accounts in opposite directions, simultaneously. With
	// caller-order locking this deadlocks; with ID-order locking both
	// streams complete and the total is conserved.
	a := NewAccount(1, 1000)
	b := NewAccount(2, 1000)

	const n = 1000

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range n {
			if err := Transfer(a, b, 1); err != nil {
				t.Errorf("a->b: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range n {
			if err := Transfer(b, a, 1); err != nil {
				t.Errorf("b->a: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	if got := a.BalanceOf() + b.BalanceOf(); got != 2000 {
		t.Fatalf("total = %g, want 2000", got)
	}
}

func TestTransferFractionalAmount(t *testing.T) {
	t.Parallel()

	// Pins float64 arithmetic under the lock. 0.25 and 0.75 are exactly
	// representable in binary floating point, so equality is exact; a
	// production ledger would use integer cents instead (exercise 10 does).
	a := NewAccount(1, 100)
	b := NewAccount(2, 0)
	if err := Transfer(a, b, 0.25); err != nil {
		t.Fatal(err)
	}
	if got := a.BalanceOf(); got != 99.75 {
		t.Errorf("a = %g, want 99.75", got)
	}
	if got := b.BalanceOf(); got != 0.25 {
		t.Errorf("b = %g, want 0.25", got)
	}
}

func ExampleTransfer() {
	a := NewAccount(1, 100)
	b := NewAccount(2, 0)
	if err := Transfer(a, b, 25); err != nil {
		fmt.Println("err:", err)
		return
	}
	fmt.Println(a.BalanceOf(), b.BalanceOf())
	// Output: 75 25
}
```

## Review

The heart of the module is four lines: pick `first, second` by comparing IDs,
then lock `first` before `second`. Everything else defends that core. The
`from == to` guard runs before any locking because the ordered path locks the
same mutex twice for identical accounts and `sync.Mutex` is not reentrant. The
balance check happens after both locks are held so check-and-debit is atomic;
moving it earlier reintroduces a time-of-check-to-time-of-use race that the
conservation test will catch as a negative balance or a wrong total. Note also
what the errors do: the sentinel values are wrapped with `%w` and asserted
with `errors.Is`, so a caller can branch on the category while the message
still carries the amounts.

Confirm correctness by running `go test -count=1 -race ./...`. The
race detector proves the mutexes actually guard `balance`;
`TestTransferBothDirections` proves the ordering — if you flip the lock
acquisition back to argument order, that test wedges rather than fails, which
is itself the lesson: deadlocks do not produce assertions, they produce
silence. Exercise 9 builds the watchdog that turns that silence into a failing
test with a goroutine dump.

## Resources

- [sync package — Mutex](https://pkg.go.dev/sync#Mutex) — Lock/Unlock semantics; note there is no reentrancy.
- [Go FAQ: goroutines and deadlock](https://go.dev/doc/faq#goroutines) — why the runtime detector only fires when every goroutine sleeps.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before guarantees the mutex provides for `balance`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-multi-lock-aggregation.md](02-multi-lock-aggregation.md)
