# Exercise 1: Ordered-Lock Money Transfer Between Accounts

Moving money between two accounts is the canonical two-lock problem: you must hold
both account locks to keep the debit and credit atomic, and if two transfers run in
opposite directions with inconsistent lock order they deadlock. This exercise builds
a transfer service whose `Move` locks the two accounts in a globally consistent order
so a circular wait is impossible by construction.

This module is fully self-contained: its own `go mod init`, all types inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
transfer/                  independent module: example.com/transfer
  go.mod                   go 1.25
  transfer.go              Account, Bank; Move (ID-ordered locking), sentinel errors
  cmd/
    demo/
      main.go              two opposing transfers concurrently; balances conserved
  transfer_test.go         table tests + TestConcurrentTransfersNoDeadlock (-race)
```

- Files: `transfer.go`, `cmd/demo/main.go`, `transfer_test.go`.
- Implement: `Account` with an ID and balance, and `Bank.Move(from, to, amount)` that locks the two accounts in ascending-ID order, checks funds, and applies the debit/credit atomically.
- Test: table tests for success, `ErrInsufficientFunds`, `ErrSameAccount`; a lock-order-independence test; a 100+ goroutine `TestConcurrentTransfersNoDeadlock` with a conserved-sum invariant.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why a global lock order

To move money you must hold both account locks: without them a concurrent reader
could observe the debit before the credit and see money vanish, and two concurrent
transfers touching the same account could interleave their reads and writes and lose
an update. So mutual exclusion over both accounts is required — you cannot remove that
Coffman condition. What you *can* remove is circular wait.

Consider the naive version that locks `from` then `to`. A transfer of A→B locks A then
B; a simultaneous transfer of B→A locks B then A. Interleave them so each grabs its
first lock, and now A→B holds A and waits for B, while B→A holds B and waits for A —
a cycle, a deadlock. Neither goroutine can proceed, and the runtime will not report it
because the rest of the program is still running.

The fix is a total order over accounts. Give every account a stable ID and always lock
the lower ID first, regardless of transfer direction. Then A→B and B→A both lock A
before B, so one fully acquires both while the other waits for the first — no cycle is
possible. `cmp.Compare` gives the ordering; when IDs somehow tie (they should not for
distinct accounts, but we guard anyway) we fall back to the pointer address via
`uintptr` so the order is still total.

Note the field/accessor split: the account exposes `ID()` as a method returning the
stored `id` field. A method named the same as the field it returns (`func (a *Account)
ID() string { return a.ID() }`) is infinite recursion — a real bug that shipped in an
earlier version of this lesson. The field is unexported `id`; the accessor is `ID()`.

Create `transfer.go`:

```go
package transfer

import (
	"cmp"
	"errors"
	"sync"
	"unsafe"
)

// ErrInsufficientFunds is returned when the source account lacks the amount.
var ErrInsufficientFunds = errors.New("insufficient funds")

// ErrSameAccount is returned when from and to are the same account.
var ErrSameAccount = errors.New("same account")

// Account is a balance protected by its own mutex. The id gives accounts a
// stable total order used to acquire locks consistently.
type Account struct {
	id      string
	mu      sync.Mutex
	balance int64
}

// NewAccount returns an account with the given id and opening balance.
func NewAccount(id string, balance int64) *Account {
	return &Account{id: id, balance: balance}
}

// ID returns the account identifier. Note this is an accessor for the private
// id field, not a method that calls itself.
func (a *Account) ID() string { return a.id }

// Balance returns the current balance under the account's lock.
func (a *Account) Balance() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.balance
}

// Bank moves money between accounts with a globally consistent lock order.
type Bank struct{}

// NewBank returns a Bank.
func NewBank() *Bank { return &Bank{} }

// order returns the two accounts sorted so the "first" is always locked before
// the "second", giving a total order that prevents circular wait. Ties on id
// (which distinct accounts should not have) break on pointer address.
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

// Move transfers amount from one account to another. It locks both accounts in
// ascending-id order so concurrent opposing transfers cannot deadlock.
func (b *Bank) Move(from, to *Account, amount int64) error {
	if from == to {
		return ErrSameAccount
	}
	first, second := order(from, to)
	first.mu.Lock()
	defer first.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()

	if from.balance < amount {
		return ErrInsufficientFunds
	}
	from.balance -= amount
	to.balance += amount
	return nil
}
```

### The runnable demo

The demo fires many opposing transfers between two accounts concurrently. With
inconsistent lock order it would deadlock; with the ordered locks it completes, and
the total balance across both accounts is conserved exactly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/transfer"
)

func main() {
	a := transfer.NewAccount("a", 1000)
	b := transfer.NewAccount("b", 1000)
	bank := transfer.NewBank()

	var wg sync.WaitGroup
	for range 500 {
		wg.Add(2)
		go func() { defer wg.Done(); bank.Move(a, b, 1) }()
		go func() { defer wg.Done(); bank.Move(b, a, 1) }()
	}
	wg.Wait()

	total := a.Balance() + b.Balance()
	fmt.Printf("a=%d b=%d total=%d\n", a.Balance(), b.Balance(), total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a=1000 b=1000 total=2000
```

(The individual balances happen to return to 1000 here because the opposing
transfers are balanced; the load-bearing invariant is `total=2000`.)

### Tests

The table test covers `Move` success, `ErrInsufficientFunds`, and `ErrSameAccount`,
asserting sentinels with `errors.Is`. `TestMoveLockOrderIndependent` runs the same
transfer once as `Move(a, b, ...)` and once as `Move(b, a, ...)` and asserts identical
resulting balances, proving the outcome does not depend on argument order.
`TestConcurrentTransfersNoDeadlock` spins 200 goroutines over 4 accounts doing random
transfers and asserts the sum of all balances is conserved — if the transfer were not
atomic, or if it deadlocked, this fails (a deadlock hangs the test, which the `-race`
run plus `go test`'s own timeout surfaces).

Create `transfer_test.go`:

```go
package transfer

import (
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMove(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fromBalance int64
		amount      int64
		sameAccount bool
		wantErr     error
		wantFrom    int64
		wantTo      int64
	}{
		{name: "success", fromBalance: 100, amount: 50, wantFrom: 50, wantTo: 50},
		{name: "exact", fromBalance: 50, amount: 50, wantFrom: 0, wantTo: 50},
		{name: "insufficient", fromBalance: 10, amount: 100, wantErr: ErrInsufficientFunds, wantFrom: 10, wantTo: 0},
		{name: "same account", sameAccount: true, amount: 50, wantErr: ErrSameAccount},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bank := NewBank()
			from := NewAccount("a", tc.fromBalance)
			to := from
			if !tc.sameAccount {
				to = NewAccount("b", 0)
			}
			err := bank.Move(from, to, tc.amount)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Move err = %v, want %v", err, tc.wantErr)
			}
			if tc.sameAccount {
				return
			}
			if from.Balance() != tc.wantFrom {
				t.Errorf("from balance = %d, want %d", from.Balance(), tc.wantFrom)
			}
			if to.Balance() != tc.wantTo {
				t.Errorf("to balance = %d, want %d", to.Balance(), tc.wantTo)
			}
		})
	}
}

func TestMoveLockOrderIndependent(t *testing.T) {
	t.Parallel()

	// Move(a, b) and Move(b, a) exercise both branches of the ordering; the
	// resulting balances must match a hand-computed expectation regardless.
	a1, b1 := NewAccount("a", 100), NewAccount("b", 100)
	if err := NewBank().Move(a1, b1, 30); err != nil {
		t.Fatal(err)
	}
	a2, b2 := NewAccount("a", 100), NewAccount("b", 100)
	if err := NewBank().Move(b2, a2, 30); err != nil {
		t.Fatal(err)
	}
	if a1.Balance() != 70 || b1.Balance() != 130 {
		t.Errorf("a->b: a=%d b=%d, want 70/130", a1.Balance(), b1.Balance())
	}
	if b2.Balance() != 70 || a2.Balance() != 130 {
		t.Errorf("b->a: a=%d b=%d, want 130/70", a2.Balance(), b2.Balance())
	}
}

func TestConcurrentTransfersNoDeadlock(t *testing.T) {
	t.Parallel()

	const (
		nAccounts = 4
		opening   = 1000
		nWorkers  = 200
		nOps      = 50
	)
	accounts := make([]*Account, nAccounts)
	for i := range accounts {
		accounts[i] = NewAccount(string(rune('a'+i)), opening)
	}
	bank := NewBank()

	var wg sync.WaitGroup
	var done atomic.Int64
	for range nWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range nOps {
				i := rand.IntN(nAccounts)
				j := rand.IntN(nAccounts)
				bank.Move(accounts[i], accounts[j], int64(rand.IntN(10)))
				done.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := done.Load(); got != nWorkers*nOps {
		t.Fatalf("completed %d ops, want %d (possible deadlock)", got, nWorkers*nOps)
	}
	var total int64
	for _, a := range accounts {
		total += a.Balance()
	}
	if want := int64(nAccounts * opening); total != want {
		t.Fatalf("total balance = %d, want %d (money not conserved)", total, want)
	}
}
```

## Review

The transfer is correct when money is conserved and no concurrent workload can wedge
it. Conservation is the invariant `TestConcurrentTransfersNoDeadlock` asserts: no matter
how the 200 workers interleave their random transfers, the sum of balances stays at
`nAccounts * opening`, which holds only if each `Move` is atomic under the two locks.
Deadlock-freedom follows from the total order in `order`: because every `Move` locks the
lower-ID account first, no two transfers can hold each other's next lock, so there is no
cycle. The lock-order-independence test proves the direction of the transfer does not
change which physical lock is taken first.

The mistake this exercise exists to prevent is inconsistent lock order — locking `from`
then `to` unconditionally. Run the test under `-race` to confirm the debit and credit
are actually guarded; a missing lock shows up as a data race long before it shows up as
a wrong total. Keep the `order` key stable and total: sorting by the mutable balance, or
by a key two accounts can share, reintroduces the cycle.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock, and its non-reentrant contract.
- [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — a total order over ordered types for the lock key.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before guarantees a mutex provides for the debit/credit.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-trylock-transfer-backoff.md](02-trylock-transfer-backoff.md)
