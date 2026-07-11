# Exercise 2: Consistent Ordering for N-Resource Snapshots (Total)

Lock ordering is a property of the resource set, not of one function: the day
someone adds a totals endpoint that locks accounts in slice order, the
deadlock the transfer engine prevents comes back through the side door. This
exercise builds the aggregation paths — an exact `Total` that holds every lock
in ID order, and a lock-by-lock `Snapshot` that trades exactness for
contention — and tests them against a storm of concurrent transfers.

This module is fully self-contained. It begins with its own `go mod init`,
duplicates the small transfer engine it aggregates over, and ships its own
demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
banktotal/                 independent module: example.com/banktotal
  go.mod
  bank/
    account.go             Account, NewAccount, Transfer (ID-ordered), BalanceOf
    total.go               Total (holds ALL locks, ID order, exact) and
                           Snapshot (lock-by-lock, approximate under load)
  cmd/
    demo/
      main.go              runnable demo: transfers, exact total, snapshot
  bank/total_test.go       unsorted-input ordering test, caller-slice
                           non-mutation, Total vs concurrent transfer storm
```

- Files: `bank/account.go`, `bank/total.go`, `cmd/demo/main.go`, `bank/total_test.go`.
- Implement: `Total` that sorts a clone of the account slice with `slices.SortFunc` + `cmp.Compare`, acquires every mutex in ascending-ID order, sums, then releases; `Snapshot` that locks one account at a time and returns per-account balances.
- Test: `TestTotalOrdersByID` with unsorted input, proof that the caller's slice is not reordered, and `TestTotalDuringTransferStorm` — `Total` racing 8 goroutines of transfers, asserting every returned total equals the invariant sum.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/banktotal/bank ~/go-exercises/banktotal/cmd/demo
cd ~/go-exercises/banktotal
go mod init example.com/banktotal
```

### Two aggregations, two contracts

There are two honest ways to sum N mutex-guarded balances, and they differ in
a way that matters for real reconciliation jobs.

`Total` holds *all* the locks at once: sort by ID, lock every account in
ascending order, sum, unlock. Because no transfer can be mid-flight while
every lock is held, the result is a true point-in-time snapshot — if the
system conserves money, `Total` returns the invariant sum *always*, even while
transfers hammer the accounts. But holding N locks means `Total` participates
fully in the deadlock problem: it must follow the exact same ascending-ID
order as `Transfer`, or a transfer holding ID 2 and waiting for ID 7 can meet
a total holding ID 7 and waiting for ID 2. This is the whole point of the
exercise: the ordering discipline binds every multi-lock path in the package,
forever. The implementation sorts a *clone* (`slices.Clone`) so the caller's
slice order is untouched — mutating an input slice as a locking side effect is
the kind of surprise that breaks callers who relied on their ordering.

`Snapshot` locks one account at a time: lock, read, unlock, move on. It can
never deadlock against anything — it never holds two locks, so it cannot form
a hold-and-wait edge, and it does not even need the sort for safety (it keeps
it for deterministic output order). The price is exactness: between unlocking
account 1 and locking account 2, a transfer can move money *from* 2 *to* 1,
and the snapshot misses it — it read 1 before the credit landed and reads 2
after the debit, so the sum comes out low. The reverse interleaving comes out
high. Under load, `SnapshotSum` wobbles around the invariant. That is fine for
a metrics gauge or a dashboard, and wrong for a reconciliation job that
alerts when the books do not balance — that job needs `Total`, and it needs
`Total` to honor the ordering. Knowing which of the two contracts a consumer
needs is the design decision; this module gives you both and names them
honestly.

Create `bank/account.go`:

```go
// Package bank implements ID-ordered account locking: Transfer and Total
// both acquire multiple account mutexes in ascending Account.ID order, so
// their wait-for graph is a DAG and they can never deadlock each other.
package bank

import (
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrSameAccount rejects a transfer from an account to itself, which
	// would otherwise self-deadlock the non-reentrant mutex.
	ErrSameAccount = errors.New("bank: cannot transfer to same account")

	// ErrInsufficientFunds rejects a debit larger than the source balance.
	ErrInsufficientFunds = errors.New("bank: insufficient funds")

	// ErrNonPositiveAmount rejects zero and negative transfer amounts.
	ErrNonPositiveAmount = errors.New("bank: non-positive amount")
)

// Account is a bank account protected by its own mutex. ID is the global
// lock-ordering key shared by every multi-account operation.
type Account struct {
	ID      int
	mu      sync.Mutex
	balance float64
}

// NewAccount returns an account with the given id and starting balance.
func NewAccount(id int, balance float64) *Account {
	return &Account{ID: id, balance: balance}
}

// Transfer moves amount between accounts, locking both mutexes in
// ascending-ID order.
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

The original version of this helper hand-rolled an insertion sort; with
`slices.SortFunc` and `cmp.Compare` the ordering intent is one line and cannot
be wrong. Note that both functions sort a `slices.Clone` of the input — the
locking order is this package's concern, and it must not leak out as a
reordered argument.

Create `bank/total.go`:

```go
package bank

import (
	"cmp"
	"slices"
)

// sortedByID returns a copy of accounts sorted by ascending ID — the
// package-wide lock-acquisition order. It clones so the caller's slice
// order is never mutated as a locking side effect.
func sortedByID(accounts []*Account) []*Account {
	sorted := slices.Clone(accounts)
	slices.SortFunc(sorted, func(a, b *Account) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return sorted
}

// Total returns the exact point-in-time sum of all balances. It acquires
// EVERY account mutex in ascending-ID order and holds them all while
// summing, so no transfer can be mid-flight during the read. Because it
// holds multiple locks, it must follow the same ordering as Transfer —
// an unordered Total would deadlock against a concurrent Transfer.
func Total(accounts []*Account) float64 {
	sorted := sortedByID(accounts)
	for _, a := range sorted {
		a.mu.Lock()
	}
	var sum float64
	for _, a := range sorted {
		sum += a.balance
	}
	for _, a := range sorted {
		a.mu.Unlock()
	}
	return sum
}

// Balance pairs an account ID with the balance observed for it.
type Balance struct {
	ID     int
	Amount float64
}

// Snapshot returns per-account balances read lock-by-lock: each mutex is
// released before the next is acquired. It can never deadlock (it never
// holds two locks), but the balances are NOT a point-in-time view: a
// transfer landing between two reads makes the implied sum transiently
// wrong. Use it for gauges and dashboards; use Total for reconciliation.
func Snapshot(accounts []*Account) []Balance {
	sorted := sortedByID(accounts)
	out := make([]Balance, 0, len(sorted))
	for _, a := range sorted {
		a.mu.Lock()
		out = append(out, Balance{ID: a.ID, Amount: a.balance})
		a.mu.Unlock()
	}
	return out
}

// SnapshotSum sums a lock-by-lock Snapshot. Under concurrent transfers it
// wobbles around the true total; at quiescence it equals Total.
func SnapshotSum(accounts []*Account) float64 {
	var sum float64
	for _, b := range Snapshot(accounts) {
		sum += b.Amount
	}
	return sum
}
```

### The runnable demo

The demo deliberately passes the accounts in unsorted order, so the ID
ordering visibly comes from the package, not the caller.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/banktotal/bank"
)

func main() {
	c := bank.NewAccount(3, 1000)
	a := bank.NewAccount(1, 1000)
	b := bank.NewAccount(2, 1000)
	accounts := []*bank.Account{c, a, b} // unsorted on purpose

	if err := bank.Transfer(a, c, 250); err != nil {
		fmt.Println("err:", err)
		return
	}
	if err := bank.Transfer(c, b, 100); err != nil {
		fmt.Println("err:", err)
		return
	}

	fmt.Printf("total=%.2f\n", bank.Total(accounts))
	for _, bal := range bank.Snapshot(accounts) {
		fmt.Printf("account %d: %.2f\n", bal.ID, bal.Amount)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total=3000.00
account 1: 750.00
account 2: 1100.00
account 3: 1150.00
```

### Tests

`TestTotalOrdersByID` feeds accounts in scrambled order and checks the sum;
`TestTotalDoesNotReorderCallerSlice` pins the `slices.Clone` behavior. The
test that earns its keep is `TestTotalDuringTransferStorm`: eight goroutines
transfer continuously among four accounts while the main goroutine calls
`Total` two hundred times, asserting *every* result equals the invariant sum.
That assertion is only sound because `Total` holds all the locks — swap in
`SnapshotSum` and the test fails intermittently with sums slightly off, which
is exactly the difference between the two contracts. And the test is only
*safe* because `Total` orders its acquisitions: an unordered hold-all `Total`
racing ordered transfers is a deadlock, and the run would hang rather than
fail. Under `-race`, the storm also proves the mutexes cover every balance
access.

Create `bank/total_test.go`:

```go
package bank

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestTotalOrdersByID(t *testing.T) {
	t.Parallel()

	// Unsorted input: Total must sort internally before locking.
	accounts := []*Account{
		NewAccount(3, 100),
		NewAccount(1, 100),
		NewAccount(2, 100),
	}
	if got := Total(accounts); got != 300 {
		t.Fatalf("Total = %g, want 300", got)
	}
}

func TestTotalDoesNotReorderCallerSlice(t *testing.T) {
	t.Parallel()

	accounts := []*Account{
		NewAccount(3, 10),
		NewAccount(1, 20),
		NewAccount(2, 30),
	}
	Total(accounts)

	wantIDs := []int{3, 1, 2}
	for i, a := range accounts {
		if a.ID != wantIDs[i] {
			t.Fatalf("caller slice reordered: index %d has ID %d, want %d", i, a.ID, wantIDs[i])
		}
	}
}

func TestSnapshotSortedOutput(t *testing.T) {
	t.Parallel()

	accounts := []*Account{
		NewAccount(9, 1),
		NewAccount(4, 2),
		NewAccount(7, 3),
	}
	snap := Snapshot(accounts)
	if len(snap) != 3 {
		t.Fatalf("len(snap) = %d, want 3", len(snap))
	}
	wantIDs := []int{4, 7, 9}
	for i, b := range snap {
		if b.ID != wantIDs[i] {
			t.Errorf("snap[%d].ID = %d, want %d", i, b.ID, wantIDs[i])
		}
	}
	if got := SnapshotSum(accounts); got != 6 {
		t.Errorf("SnapshotSum = %g, want 6", got)
	}
}

func TestTotalDuringTransferStorm(t *testing.T) {
	t.Parallel()

	accounts := []*Account{
		NewAccount(1, 1000),
		NewAccount(2, 1000),
		NewAccount(3, 1000),
		NewAccount(4, 1000),
	}
	const want = 4000.0

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				from := accounts[(i+j)%len(accounts)]
				to := accounts[(i+j+1)%len(accounts)]
				err := Transfer(from, to, 1)
				if err != nil && !errors.Is(err, ErrInsufficientFunds) {
					t.Errorf("unexpected transfer error: %v", err)
					return
				}
				j++
			}
		}()
	}

	// Total holds every lock, so each call is an exact point-in-time
	// read: it must equal the invariant sum on every single call, even
	// mid-storm. SnapshotSum would wobble here.
	for range 200 {
		if got := Total(accounts); got != want {
			close(stop)
			wg.Wait()
			t.Fatalf("Total mid-storm = %g, want %g", got, want)
		}
	}
	close(stop)
	wg.Wait()

	if got := SnapshotSum(accounts); got != want {
		t.Fatalf("SnapshotSum at quiescence = %g, want %g", got, want)
	}
}

func ExampleTotal() {
	accounts := []*Account{
		NewAccount(2, 40),
		NewAccount(1, 60),
	}
	fmt.Printf("%.2f\n", Total(accounts))
	// Output: 100.00
}
```

## Review

The invariant to internalize: any function that locks more than one account is
bound by the package-wide ascending-ID order — `Transfer` and `Total` are the
same kind of animal, differing only in how many locks they hold. The common
mistakes here are subtle because each one still "works" in a quiet test.
Sorting the caller's slice in place works until a caller depends on its order.
A lock-by-lock `Total` works until a reconciliation job runs during peak
traffic and pages the on-call with a false imbalance. An unordered hold-all
`Total` works until it interleaves with a transfer just so — and then it does
not fail, it hangs. The storm test is designed so each of those regressions
becomes a visible failure (or, for the ordering one, a hang your CI watchdog
from exercise 9 should catch).

Confirm with `go test -count=1 -race ./...`. If you want to see the
`Snapshot` wobble with your own eyes, temporarily change the storm loop to
assert on `SnapshotSum` instead of `Total` and watch it fail within a few
hundred iterations.

## Resources

- [slices package — SortFunc and Clone](https://pkg.go.dev/slices) — the sorted-copy idiom used for the lock order.
- [cmp package — Compare](https://pkg.go.dev/cmp#Compare) — the comparator for `slices.SortFunc`.
- [sync package — Mutex](https://pkg.go.dev/sync#Mutex) — the per-account lock both aggregations acquire.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-trylock-backoff-reservation.md](03-trylock-backoff-reservation.md)
