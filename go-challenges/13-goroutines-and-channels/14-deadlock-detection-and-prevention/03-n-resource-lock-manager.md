# Exercise 3: N-Resource Lock Ordering for a Batch Settlement

Pairwise ordered locking generalizes to N. A batch payout or netting settlement must
lock an arbitrary set of accounts atomically — debit some, credit others, all under one
consistent view. This exercise builds a settlement that sorts the involved accounts by a
stable key, acquires their locks in that order, and unlocks in reverse, so no batch of any
size over any overlapping subset can form a circular wait.

This module is fully self-contained: its own `go mod init`, all types inline, its own
demo and tests.

## What you'll build

```text
settle/                    independent module: example.com/settle
  go.mod                   go 1.25
  settle.go                Account, Ledger; Settle([]Entry) with N-lock ordering
  cmd/
    demo/
      main.go              a 3-account netting batch, then concurrent overlapping batches
  settle_test.go           deterministic-order unit test + concurrent conserved-sum test
```

- Files: `settle.go`, `cmd/demo/main.go`, `settle_test.go`.
- Implement: `Ledger.Settle(entries []Entry)` that locks every distinct account in the batch in ascending-ID order, verifies the batch nets to zero and leaves no account negative, then applies all deltas atomically.
- Test: a unit test asserting the acquire order is a deterministic sort of the input; a concurrent test where many goroutines settle overlapping random subsets, asserting completion and a conserved global sum under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### From two locks to N

The two-account transfer sorts a pair and locks the smaller first. A batch settlement
locks an arbitrary set, so the same idea has to scale: collect the distinct accounts the
batch touches, sort them by their stable ID, and acquire in that order. Unlock in reverse
so release mirrors acquisition — using a stack of deferred unlocks captured in a slice, not
individual `defer`s, because the set size is dynamic.

Why the total order still works for N: a circular wait needs a cycle of goroutines each
holding a lock the next wants. If every goroutine acquires strictly ascending IDs, then any
goroutine holding lock `h` and waiting on lock `w` has `w > h`. Following the cycle, the IDs
would have to strictly increase all the way around and return to the start — impossible.
So no cycle can form, regardless of which overlapping subsets different batches lock.

Two details matter. First, **deduplicate**: a batch that names the same account twice (a
correction that debits and credits it) must lock it once, or `Lock` on an already-held
non-reentrant mutex self-deadlocks. Second, **validate before mutating**: with all locks
held, check the batch nets to zero and no account would go negative, and only then apply —
a partial application under a lock would leave the ledger inconsistent if a later entry
failed.

Create `settle.go`:

```go
package settle

import (
	"cmp"
	"errors"
	"slices"
	"sync"
)

// ErrNotBalanced is returned when a settlement's deltas do not net to zero.
var ErrNotBalanced = errors.New("settlement does not net to zero")

// ErrWouldOverdraw is returned when applying the batch would leave an account
// with a negative balance.
var ErrWouldOverdraw = errors.New("settlement would overdraw an account")

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

// Entry is a single leg of a settlement: a delta applied to an account.
type Entry struct {
	Account *Account
	Delta   int64
}

// Ledger applies multi-account settlements atomically under an N-resource lock
// order.
type Ledger struct{}

// NewLedger returns a Ledger.
func NewLedger() *Ledger { return &Ledger{} }

// distinctSorted returns the distinct accounts in entries, sorted by id (ties
// broken by pointer identity), giving the global acquisition order.
func distinctSorted(entries []Entry) []*Account {
	seen := make(map[*Account]struct{}, len(entries))
	accts := make([]*Account, 0, len(entries))
	for _, e := range entries {
		if _, ok := seen[e.Account]; ok {
			continue
		}
		seen[e.Account] = struct{}{}
		accts = append(accts, e.Account)
	}
	slices.SortFunc(accts, func(x, y *Account) int {
		return cmp.Compare(x.id, y.id)
	})
	return accts
}

// Settle locks every distinct account in the batch in ascending-id order,
// validates the batch, applies all deltas, then unlocks in reverse order. Any
// batch, over any overlapping subset, is deadlock-free because acquisition is a
// total order.
func (l *Ledger) Settle(entries []Entry) error {
	accts := distinctSorted(entries)
	for _, a := range accts {
		a.mu.Lock()
	}
	defer func() {
		for i := len(accts) - 1; i >= 0; i-- {
			accts[i].mu.Unlock()
		}
	}()

	// Validate with all locks held.
	var net int64
	projected := make(map[*Account]int64, len(accts))
	for _, a := range accts {
		projected[a] = a.balance
	}
	for _, e := range entries {
		net += e.Delta
		projected[e.Account] += e.Delta
	}
	if net != 0 {
		return ErrNotBalanced
	}
	for _, a := range accts {
		if projected[a] < 0 {
			return ErrWouldOverdraw
		}
	}
	// Apply.
	for _, e := range entries {
		e.Account.balance += e.Delta
	}
	return nil
}
```

### The runnable demo

The demo settles a three-way netting batch (a pays b and c), then runs many concurrent
batches over overlapping random subsets to show they complete without wedging and conserve
the total.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"

	"example.com/settle"
)

func main() {
	a := settle.NewAccount("a", 100)
	b := settle.NewAccount("b", 100)
	c := settle.NewAccount("c", 100)
	ledger := settle.NewLedger()

	// a pays 30 to b and 20 to c, in one atomic settlement.
	if err := ledger.Settle([]settle.Entry{
		{Account: a, Delta: -50},
		{Account: b, Delta: 30},
		{Account: c, Delta: 20},
	}); err != nil {
		fmt.Println("settle:", err)
		return
	}
	fmt.Printf("after netting: a=%d b=%d c=%d\n", a.Balance(), b.Balance(), c.Balance())

	accts := []*settle.Account{a, b, c}
	var wg sync.WaitGroup
	for range 300 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			i, j := rand.IntN(3), rand.IntN(3)
			if i == j {
				return
			}
			ledger.Settle([]settle.Entry{
				{Account: accts[i], Delta: -1},
				{Account: accts[j], Delta: 1},
			})
		}()
	}
	wg.Wait()
	fmt.Printf("total conserved: %d\n", a.Balance()+b.Balance()+c.Balance())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after netting: a=50 b=130 c=120
total conserved: 300
```

### Tests

`TestAcquireOrderDeterministic` checks that `distinctSorted` returns accounts in a stable,
sorted order for a shuffled input with a duplicate — the property that makes the global lock
order work. `TestConcurrentSettlementsConserveSum` runs many goroutines settling overlapping
random subsets of a shared pool and asserts both completion (a deadlock would hang and be
caught by the test timeout) and a conserved total. `TestSettleValidation` covers the
`ErrNotBalanced` and `ErrWouldOverdraw` paths.

Create `settle_test.go`:

```go
package settle

import (
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAcquireOrderDeterministic(t *testing.T) {
	t.Parallel()

	d := NewAccount("d", 0)
	a := NewAccount("a", 0)
	c := NewAccount("c", 0)
	b := NewAccount("b", 0)
	// Shuffled input with a duplicate account (b appears twice).
	entries := []Entry{
		{Account: d}, {Account: b}, {Account: a}, {Account: c}, {Account: b},
	}
	got := distinctSorted(entries)
	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("got %d distinct accounts, want %d", len(got), len(want))
	}
	for i, a := range got {
		if a.ID() != want[i] {
			t.Errorf("order[%d] = %q, want %q", i, a.ID(), want[i])
		}
	}
}

func TestSettleValidation(t *testing.T) {
	t.Parallel()

	a := NewAccount("a", 10)
	b := NewAccount("b", 0)
	l := NewLedger()

	if err := l.Settle([]Entry{{Account: a, Delta: -5}, {Account: b, Delta: 3}}); !errors.Is(err, ErrNotBalanced) {
		t.Errorf("unbalanced err = %v, want ErrNotBalanced", err)
	}
	if err := l.Settle([]Entry{{Account: a, Delta: -50}, {Account: b, Delta: 50}}); !errors.Is(err, ErrWouldOverdraw) {
		t.Errorf("overdraw err = %v, want ErrWouldOverdraw", err)
	}
	if a.Balance() != 10 || b.Balance() != 0 {
		t.Fatalf("balances mutated on rejected settlement: a=%d b=%d", a.Balance(), b.Balance())
	}
}

func TestConcurrentSettlementsConserveSum(t *testing.T) {
	t.Parallel()

	const (
		nAccounts = 6
		opening   = 1000
		nWorkers  = 200
		nOps      = 40
	)
	accts := make([]*Account, nAccounts)
	for i := range accts {
		accts[i] = NewAccount(string(rune('a'+i)), opening)
	}
	l := NewLedger()

	var wg sync.WaitGroup
	var done atomic.Int64
	for range nWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range nOps {
				// A random balanced batch over 2 or 3 distinct accounts.
				i, j, k := rand.IntN(nAccounts), rand.IntN(nAccounts), rand.IntN(nAccounts)
				if i == j || i == k || j == k {
					done.Add(1)
					continue
				}
				l.Settle([]Entry{
					{Account: accts[i], Delta: -2},
					{Account: accts[j], Delta: 1},
					{Account: accts[k], Delta: 1},
				})
				done.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := done.Load(); got != nWorkers*nOps {
		t.Fatalf("completed %d ops, want %d (possible deadlock)", got, nWorkers*nOps)
	}
	var total int64
	for _, a := range accts {
		total += a.Balance()
	}
	if want := int64(nAccounts * opening); total != want {
		t.Fatalf("total = %d, want %d (money not conserved)", total, want)
	}
}
```

## Review

The settlement is correct when it is atomic and deadlock-free for any batch shape.
Atomicity comes from holding every involved account's lock across validation and
application, so no other settlement can observe a half-applied batch — the concurrent test's
conserved sum is the evidence. Deadlock-freedom comes from `distinctSorted`: because every
`Settle` acquires strictly ascending IDs, and duplicates are collapsed so a mutex is never
double-locked, no cycle can form across overlapping batches. The deterministic-order test
pins the one property everything rests on.

The mistakes to avoid: forgetting to deduplicate (a self-deadlock the moment a batch names
one account twice), sorting by something non-total, and mutating before validating (a
rejected batch must leave balances untouched, which `TestSettleValidation` checks). Run
`-race` at high `-count` to shake out any ordering bug the single run happened to miss.

## Resources

- [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) — sorting the batch's accounts by a stable key.
- [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — a total comparison for the lock key.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the per-account lock and its non-reentrant contract (why dedup is required).

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-context-bounded-channel-send.md](04-context-bounded-channel-send.md)
